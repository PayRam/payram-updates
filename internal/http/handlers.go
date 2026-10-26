package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/cli"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/inspect"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/network"
	"github.com/payram/payram-updater/internal/recovery"
)

// HealthResponse represents the health check response.
type HealthResponse struct {
	Status string `json:"status"`
}

// UpgradeStatusResponse extends Job with recovery playbook for FAILED states.
type UpgradeStatusResponse struct {
	*jobs.Job
	RecoveryPlaybook *recovery.Playbook `json:"recoveryPlaybook,omitempty"`
}

// HistoryResponse represents the response for history queries.
type HistoryResponse struct {
	Events []history.Event `json:"events"`
	Count  int             `json:"count"`
}

// PlanRequest represents the request body for POST /upgrade/plan. The running
// version is never taken from the caller: it is looked up (see
// lookupCurrentVersion), so the downgrade and breakpoint checks cannot be
// moved by claiming an older one.
type PlanRequest struct {
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requestedTarget"`
	Source          string `json:"source"`
}

// PlanResponse represents the response for POST /upgrade/plan.
type PlanResponse struct {
	State           string `json:"state"`
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requestedTarget"`
	ResolvedTarget  string `json:"resolvedTarget,omitempty"`
	FailureCode     string `json:"failureCode,omitempty"`
	Message         string `json:"message"`
	ImageRepo       string `json:"imageRepo,omitempty"`
	ContainerName   string `json:"containerName,omitempty"`
}

// RunRequest represents the request body for POST /upgrade/run.
type RunRequest struct {
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requestedTarget"`
	Source          string `json:"source"` // Origin of request, defaults to "UNKNOWN"
	// Attended says a person at root's command line follows the update and
	// answers its question before a backup is restored. Honoured only for
	// root's CLI (the token); any other caller's update is unattended.
	Attended bool `json:"attended"`
}

// AnswerRequest is the body of POST /upgrade/answer.
type AnswerRequest struct {
	QuestionID string `json:"questionId"`
	// Restore is the answer: true restores the backup and switches back.
	Restore bool `json:"restore"`
}

func parseJobMode(value string) (jobs.JobMode, error) {
	if strings.TrimSpace(value) == "" {
		return jobs.JobModeDashboard, nil
	}

	upper := strings.ToUpper(strings.TrimSpace(value))
	switch upper {
	case string(jobs.JobModeDashboard):
		return jobs.JobModeDashboard, nil
	case string(jobs.JobModeManual):
		return jobs.JobModeManual, nil
	default:
		return "", fmt.Errorf("invalid mode %q", value)
	}
}

// errManualNeedsRoot refuses manual mode to a caller that is not root's CLI.
var errManualNeedsRoot = errors.New("manual mode skips the release policy's checks, so only root's payram-updater command may use it: run it with sudo")

// resolveMode decides a request's mode. MANUAL skips the release policy's
// gates, so only root's CLI may ask for it (fromRootCLI: a request from this
// machine carrying the token only root can read). Any other request is
// DASHBOARD, except that a request claiming to be the CLI and asking for
// MANUAL without the token is refused, so the operator learns to use sudo
// instead of silently getting the dashboard's checks.
func resolveMode(requestedMode, source string, fromRootCLI bool) (jobs.JobMode, error) {
	if !strings.EqualFold(strings.TrimSpace(source), "CLI") {
		return jobs.JobModeDashboard, nil
	}
	mode, err := parseJobMode(requestedMode)
	if err != nil {
		return "", err
	}
	if mode == jobs.JobModeManual && !fromRootCLI {
		return "", errManualNeedsRoot
	}
	return mode, nil
}

// isRootCLIRequest reports whether r comes from root's payram-updater command
// on this machine.
func (s *Server) isRootCLIRequest(r *http.Request) bool {
	return network.IsLoopbackRequest(r) && cli.TokenMatches(s.cliToken, r.Header.Get(cli.TokenHeader))
}

// writeModeError answers a request whose mode was refused.
func writeModeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errManualNeedsRoot) {
		status = http.StatusForbidden
	}
	http.Error(w, sentence(err.Error()), status)
}

// lookupCurrentVersion asks the running PayRam which version it is (its API,
// or its image label). "" when it cannot be told; the release checks that
// need it are then skipped, as for an updater that never knew it.
func (s *Server) lookupCurrentVersion(ctx context.Context) string {
	if s.versionLookup != nil {
		return s.versionLookup(ctx)
	}
	version, err := s.runningVersion(ctx, s.fetchPolicyInitVersion(ctx))
	if err != nil {
		logger.Warnf("Server", "lookupCurrentVersion", "Could not tell which version PayRam runs (%v); the breakpoint and downgrade checks are skipped for this request", err)
		return ""
	}
	return version
}

// runningVersion is the version the PayRam container runs, from its API or
// its image label. It has its own time budget (the policy fetch before it
// has another).
func (s *Server) runningVersion(ctx context.Context, initVersion string) (string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel()
	containerName, err := s.discoverContainerName(lookupCtx)
	if err != nil {
		return "", err
	}
	version, _, err := s.resolveCoreVersion(lookupCtx, containerName, initVersion)
	return version, err
}

// maxRequestBody bounds a plan/run request body; both are a few fields.
const maxRequestBody = 64 << 10

// RunResponse represents the response for POST /upgrade/run.
type RunResponse struct {
	JobID           string `json:"jobId,omitempty"`
	State           string `json:"state"`
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requestedTarget"`
	ResolvedTarget  string `json:"resolvedTarget,omitempty"`
	FailureCode     string `json:"failureCode,omitempty"`
	Message         string `json:"message"`
}

// HandleHealth returns a handler for the /health endpoint.
func HandleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		response := HealthResponse{Status: "ok"}
		json.NewEncoder(w).Encode(response)
	}
}

// HandleUpgradeStatus returns a handler for the /upgrade/status endpoint.
func (s *Server) HandleUpgradeStatus() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Load the latest job
		job, err := s.jobStore.LoadLatest()
		if err != nil {
			logger.Error("Server", "HandleUpgradeStatus", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// If no job exists, return an IDLE job
		if job == nil {
			job = &jobs.Job{
				State: jobs.JobStateIdle,
			}
		}

		// Build response with recovery playbook if job failed, or if it is
		// still marked active but its updater stopped saving progress.
		response := UpgradeStatusResponse{Job: job}
		if job.State == jobs.JobStateFailed && job.FailureCode != "" {
			playbook := recovery.RenderJobPlaybook(job, s.buildPlaybookContext(job))
			response.RecoveryPlaybook = &playbook
		} else if recovery.IsStale(job, time.Now()) {
			playbook := recovery.RenderPlaybook(recovery.StalePlaybookCode(job.State), s.buildPlaybookContext(job).WithJob(job))
			response.RecoveryPlaybook = &playbook
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}
}

// HandleUpgradeLogs returns a handler for the /upgrade/logs endpoint.
func (s *Server) HandleUpgradeLogs() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read logs
		logs, err := s.jobStore.ReadLogs()
		if err != nil {
			logger.Error("Server", "HandleUpgradeLogs", err)
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		// The log is cleaned when the updater starts; this also covers a
		// line that slipped past that (the dashboard shows the log).
		w.Write([]byte(dockerexec.RedactLines(logs)))
	}
}

// HandleHistory returns a handler for history queries.
// Supports query params: ?type=upgrade|backup|restore&status=started|succeeded|failed&limit=100
func (s *Server) HandleHistory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		q := r.URL.Query()
		typeFilter := strings.TrimSpace(q.Get("type"))
		statusFilter := strings.TrimSpace(q.Get("status"))
		limit := 100
		if rawLimit := strings.TrimSpace(q.Get("limit")); rawLimit != "" {
			parsed, err := strconv.Atoi(rawLimit)
			if err != nil || parsed <= 0 {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
			limit = parsed
		}

		events, err := s.historyStore.List(limit, typeFilter, statusFilter)
		if err != nil {
			logger.Error("Server", "HandleHistory", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(HistoryResponse{Events: events, Count: len(events)})
	}
}

// HandleUpgradeLast returns a handler for the /upgrade/last endpoint.
// Returns only the last job state without recovery playbook.
func (s *Server) HandleUpgradeLast() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Load the latest job
		job, err := s.jobStore.LoadLatest()
		if err != nil {
			logger.Error("Server", "HandleUpgradeLast", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if job == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"message": "No upgrade job found",
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(job)
	}
}

// HandleUpgradePlaybook returns a handler for the /upgrade/playbook endpoint.
// Returns the recovery playbook for the last failed job, if any.
func (s *Server) HandleUpgradePlaybook() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Load the latest job
		job, err := s.jobStore.LoadLatest()
		if err != nil {
			logger.Error("Server", "HandleUpgradePlaybook", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// No job or job not failed - no playbook
		if job == nil || job.State != jobs.JobStateFailed || job.FailureCode == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"playbook": nil,
				"message":  "No recovery playbook needed",
			})
			return
		}

		playbook := recovery.RenderJobPlaybook(job, s.buildPlaybookContext(job))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"playbook":    &playbook,
			"failureCode": job.FailureCode,
			"backupPath":  job.BackupPath,
		})
	}
}

// HandleUpgradeInspect returns a handler for the /upgrade/inspect endpoint.
// Returns full system inspection results.
func (s *Server) HandleUpgradeInspect() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Resolve container name for inspection
		// For inspect, we need to fetch the manifest first to get the container name
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		// Fetch manifest to get container name
		manifestClient := manifest.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
		manifestData, _ := manifestClient.Fetch(ctx, s.config.RuntimeManifestURL)

		// Resolve container name
		resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, logger.StdLogger())
		resolved, err := resolver.Resolve(manifestData)
		if err != nil {
			if resErr, ok := err.(*container.ResolutionError); ok && resErr.GetFailureCode() == "CONTAINER_NAME_UNRESOLVED" {
				imagePattern := "payramapp/payram:"
				if s.config.ImageRepoOverride != "" {
					imagePattern = s.config.ImageRepoOverride + ":"
				}
				discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, logger.StdLogger())
				discovered, discoverErr := discoverer.DiscoverPayramContainer(ctx)
				if discoverErr != nil {
					// For inspect, return error in JSON instead of failing
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(inspect.InspectResult{
						OverallState: inspect.StateBroken,
						Issues: []inspect.Issue{
							{
								Component:   "container",
								Description: err.Error(),
								Severity:    "CRITICAL",
							},
						},
						Checks: map[string]inspect.CheckResult{
							"container_name": {Status: "FAILED", Message: err.Error()},
						},
					})
					return
				}
				resolved = &container.ResolvedContainer{Name: discovered.Name}
			} else {
				// For inspect, return error in JSON instead of failing
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(inspect.InspectResult{
					OverallState: inspect.StateBroken,
					Issues: []inspect.Issue{
						{
							Component:   "container",
							Description: err.Error(),
							Severity:    "CRITICAL",
						},
					},
					Checks: map[string]inspect.CheckResult{
						"container_name": {Status: "FAILED", Message: err.Error()},
					},
				})
				return
			}
		}

		containerName := resolved.Name
		logger.Infof("Server", "HandleUpgradeInspect", "Target container resolved as: %s", containerName)

		inspector := inspect.NewInspector(
			s.jobStore,
			s.dockerRunner.DockerBin,
			containerName,
			s.currentCoreBaseURL(ctx, containerName),
			s.config.PolicyURL,
			s.config.RuntimeManifestURL,
			s.config.DebugVersionMode,
		)

		result := inspector.Run(ctx)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(result)
	}
}

// HandleUpgradePlan returns a handler for the POST /upgrade/plan endpoint.
// This is a READ-ONLY endpoint that validates upgrade parameters without
// creating jobs, mutating state, or executing docker commands.
func (s *Server) HandleUpgradePlan() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse request body
		var req PlanRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		mode, err := resolveMode(req.Mode, req.Source, s.isRootCLIRequest(r))
		if err != nil {
			writeModeError(w, err)
			return
		}

		// Validate requestedTarget
		if req.RequestedTarget == "" {
			http.Error(w, "requestedTarget is required", http.StatusBadRequest)
			return
		}

		// Perform read-only planning
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		plan := s.PlanUpgrade(ctx, mode, req.RequestedTarget, s.lookupCurrentVersion(r.Context()))

		// Build response
		response := PlanResponse{
			State:           string(plan.State),
			Mode:            string(plan.Mode),
			RequestedTarget: plan.RequestedTarget,
			ResolvedTarget:  plan.ResolvedTarget,
			FailureCode:     plan.FailureCode,
			Message:         plan.Message,
		}

		// Add manifest info if available
		if plan.Manifest != nil {
			response.ImageRepo = plan.Manifest.Image.Repo
			// Resolve container name using the resolver (env > manifest), then fallback to discovery
			resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, nil)
			if resolved, err := resolver.Resolve(plan.Manifest); err == nil {
				response.ContainerName = resolved.Name
			} else {
				if resErr, ok := err.(*container.ResolutionError); ok && resErr.GetFailureCode() == "CONTAINER_NAME_UNRESOLVED" {
					imagePattern := "payramapp/payram:"
					if s.config.ImageRepoOverride != "" {
						imagePattern = s.config.ImageRepoOverride + ":"
					}
					discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, logger.StdLogger())
					if discovered, discoverErr := discoverer.DiscoverPayramContainer(ctx); discoverErr == nil {
						response.ContainerName = discovered.Name
					} else {
						response.ContainerName = ""
						response.FailureCode = "CONTAINER_NAME_UNRESOLVED"
						response.Message = err.Error()
					}
				} else {
					// If resolution fails, report it in the response
					response.ContainerName = ""
					response.FailureCode = "CONTAINER_NAME_UNRESOLVED"
					response.Message = err.Error()
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}
}

// HandleUpgradeRun returns a handler for the POST /upgrade/run endpoint.
// This endpoint creates a new upgrade job and executes it.
func (s *Server) HandleUpgradeRun() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse request body
		var req RunRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		mode, err := resolveMode(req.Mode, req.Source, s.isRootCLIRequest(r))
		if err != nil {
			writeModeError(w, err)
			return
		}

		// Validate requestedTarget
		if req.RequestedTarget == "" {
			http.Error(w, "requestedTarget is required", http.StatusBadRequest)
			return
		}

		// Validate source
		source := req.Source
		if source == "" {
			source = "UNKNOWN"
		}

		// One update at a time: the slot is claimed before anything else and
		// held until the job ends (released below if no job is started).
		if err := s.claimUpgradeSlot(); err != nil {
			response := map[string]string{
				"error":   "An active job already exists",
				"message": fmt.Sprintf("%s; wait for it to finish or check its status", sentence(err.Error())),
			}
			if existingJob, loadErr := s.jobStore.LoadLatest(); loadErr == nil && existingJob != nil {
				response["jobId"] = existingJob.JobID
				response["state"] = string(existingJob.State)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(response)
			return
		}
		started := false
		defer func() {
			if !started {
				s.releaseUpgradeSlot()
			}
		}()
		// A job still marked active on disk belongs to an updater that
		// stopped; the startup repair finishes it. Until then, refuse.
		if existingJob, err := s.jobStore.LoadLatest(); err == nil && existingJob != nil && existingJob.IsActive() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "An active job already exists",
				"jobId":   existingJob.JobID,
				"state":   string(existingJob.State),
				"message": "Wait for the current job to complete or check its status",
			})
			return
		}

		// First, do a read-only plan to validate
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		plan := s.PlanUpgrade(ctx, mode, req.RequestedTarget, s.lookupCurrentVersion(r.Context()))
		if plan.State == jobs.JobStateFailed {
			// Planning failed - return error without creating a job
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(RunResponse{
				State:           string(plan.State),
				Mode:            string(plan.Mode),
				RequestedTarget: plan.RequestedTarget,
				FailureCode:     plan.FailureCode,
				Message:         plan.Message,
			})
			return
		}

		// Planning succeeded - create and execute job
		jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
		job := jobs.NewJob(jobID, mode, req.RequestedTarget)
		job.ResolvedTarget = plan.ResolvedTarget
		job.State = jobs.JobStateExecuting
		job.Message = "Preparing the update"
		job.UpdatedAt = time.Now().UTC()
		job.Attended = req.Attended && s.isRootCLIRequest(r)

		// Save job
		if err := s.jobStore.Save(job); err != nil {
			logger.Error("Server", "HandleUpgradeRun", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Log start with source
		s.jobStore.AppendLog(fmt.Sprintf("Starting upgrade job %s: mode=%s target=%s (resolved: %s) source=%s",
			jobID, mode, req.RequestedTarget, plan.ResolvedTarget, source))

		// The reply is built before the job starts: from then on the job
		// belongs to the update goroutine.
		response := RunResponse{
			JobID:           job.JobID,
			State:           string(job.State),
			Mode:            string(job.Mode),
			RequestedTarget: job.RequestedTarget,
			ResolvedTarget:  job.ResolvedTarget,
			Message:         "Upgrade job started",
		}
		started = true
		s.startUpgrade(job, plan)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}
}

// HandleUpgradeAnswer returns the handler for POST /upgrade/answer: root's
// payram-updater command answers the question an attended update waits on
// (whether to restore the backup and switch back). Only root's CLI may
// answer; an answer to a question that no longer waits (answered, or past
// its deadline, when the default was taken) is refused with 409.
func (s *Server) HandleUpgradeAnswer() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.isRootCLIRequest(r) {
			http.Error(w, "Only root's payram-updater command can answer an update's question: run it with sudo", http.StatusForbidden)
			return
		}
		var req AnswerRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil || req.QuestionID == "" {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
		if !s.answers.deliver(req.QuestionID, req.Restore) {
			http.Error(w, "That question is no longer waiting for an answer (it was answered, or its time ran out and the default was taken)", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"accepted": true})
	}
}

// buildPlaybookContext fills the values a playbook's steps name. The job's
// own record (container, address, backup) wins; only what it lacks is
// looked up.
func (s *Server) buildPlaybookContext(job *jobs.Job) recovery.PlaybookContext {
	ctx := recovery.PlaybookContext{
		ImageRepo: "payramapp/payram",
		BackupDir: s.config.Backup.Dir,
	}
	if s.config.ImageRepoOverride != "" {
		ctx.ImageRepo = s.config.ImageRepoOverride
	}
	if job != nil {
		ctx = ctx.WithJob(job)
	}

	if ctx.ContainerName == "" {
		if name, err := s.discoverContainerName(context.Background()); err == nil {
			ctx.ContainerName = name
		}
	}
	if ctx.BaseURL == "" && ctx.ContainerName != "" {
		ctx.BaseURL = s.currentCoreBaseURL(context.Background(), ctx.ContainerName)
	}
	return ctx
}
