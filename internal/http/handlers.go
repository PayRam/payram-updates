package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/inspect"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/policy"
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

// PlanRequest represents the request body for POST /upgrade/plan.
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

func resolveMode(requestedMode, source string) (jobs.JobMode, error) {
	if strings.EqualFold(strings.TrimSpace(source), "CLI") {
		return parseJobMode(requestedMode)
	}

	return jobs.JobModeDashboard, nil
}

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
			// Log the error but don't crash - return IDLE state
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// If no job exists, return an IDLE job
		if job == nil {
			job = &jobs.Job{
				State: jobs.JobStateIdle,
			}
		}

		// Build response with recovery playbook if job failed
		response := UpgradeStatusResponse{Job: job}
		if job.State == jobs.JobStateFailed && job.FailureCode != "" {
			ctx := s.buildPlaybookContext(job.BackupPath)
			playbook := recovery.RenderPlaybook(job.FailureCode, ctx)
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
			// Log the error but don't crash - return empty logs
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(logs))
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
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(HistoryResponse{Events: events, Count: len(events)})
	}
}

// UpgradeRequest represents the request body for POST /upgrade.
type UpgradeRequest struct {
	RequestedTarget string `json:"requestedTarget"`
}

// HandleUpgrade returns a handler for the POST /upgrade endpoint.
func (s *Server) HandleUpgrade() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse request body
		var req UpgradeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Legacy endpoint always uses DASHBOARD mode
		mode := jobs.JobModeDashboard

		// Validate requestedTarget
		if req.RequestedTarget == "" {
			http.Error(w, "requestedTarget is required", http.StatusBadRequest)
			return
		}

		// Check for active job (concurrency check)
		existingJob, err := s.jobStore.LoadLatest()
		if err != nil {
			log.Printf("Failed to load existing job: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if existingJob != nil && isJobActive(existingJob) {
			http.Error(w, "An active job already exists", http.StatusConflict)
			return
		}

		// Create new job with unique ID
		jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
		job := jobs.NewJob(jobID, mode, req.RequestedTarget)
		job.State = jobs.JobStatePolicyFetching

		// Save job in POLICY_FETCHING state
		if err := s.jobStore.Save(job); err != nil {
			log.Printf("Failed to save job: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Log start
		s.jobStore.AppendLog(fmt.Sprintf("Starting upgrade job %s: mode=%s target=%s", jobID, mode, req.RequestedTarget))

		// Fetch policy
		policyClient := policy.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
		defer cancel()

		policyData, err := policyClient.Fetch(ctx, s.config.PolicyURL)
		if err != nil {
			if mode == jobs.JobModeDashboard {
				// DASHBOARD mode: policy fetch failure is fatal
				job.State = jobs.JobStateFailed
				if err == policy.ErrInvalidJSON {
					job.FailureCode = "POLICY_INVALID_JSON"
				} else {
					job.FailureCode = "POLICY_FETCH_FAILED"
				}
				job.Message = fmt.Sprintf("Failed to fetch policy: %v", err)
				job.UpdatedAt = time.Now().UTC()
				s.jobStore.Save(job)
				s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(job)
				return
			} else {
				// MANUAL mode: log warning and continue
				s.jobStore.AppendLog(fmt.Sprintf("WARNING: Policy fetch failed (continuing in MANUAL mode): %v", err))
			}
		} else {
			s.jobStore.AppendLog(fmt.Sprintf("Policy fetched: latest=%s, %d releases", policyData.Latest, len(policyData.Releases)))

			// Check for breakpoints (DASHBOARD mode only)
			if mode == jobs.JobModeDashboard && policyData != nil {
				for _, breakpoint := range policyData.Breakpoints {
					if breakpoint.Version == req.RequestedTarget {
						// Breakpoint hit - manual upgrade required
						job.State = jobs.JobStateFailed
						job.FailureCode = "MANUAL_UPGRADE_REQUIRED"
						job.Message = fmt.Sprintf("%s %s", breakpoint.Reason, breakpoint.Docs)
						job.UpdatedAt = time.Now().UTC()
						s.jobStore.Save(job)
						s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - Target %s requires manual upgrade: %s", job.FailureCode, req.RequestedTarget, job.Message))

						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(job)
						return
					}
				}
			}
		}

		// Fetch runtime manifest
		job.State = jobs.JobStateManifestFetching
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)

		manifestClient := manifest.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
		manifestData, err := manifestClient.Fetch(ctx, s.config.RuntimeManifestURL)
		if err != nil {
			// Manifest fetch failure is fatal for both modes
			job.State = jobs.JobStateFailed
			if err == manifest.ErrInvalidJSON {
				job.FailureCode = "MANIFEST_INVALID_JSON"
			} else {
				job.FailureCode = "MANIFEST_FETCH_FAILED"
			}
			job.Message = fmt.Sprintf("Failed to fetch manifest: %v", err)
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(job)
			return
		}

		s.jobStore.AppendLog(fmt.Sprintf("Manifest fetched: repo=%s", manifestData.Image.Repo))

		// Apply IMAGE_REPO_OVERRIDE if configured (for testing with dummy repos)
		if s.config.ImageRepoOverride != "" {
			s.jobStore.AppendLog(fmt.Sprintf("Applying IMAGE_REPO_OVERRIDE: %s -> %s", manifestData.Image.Repo, s.config.ImageRepoOverride))
			manifestData.Image.Repo = s.config.ImageRepoOverride
		}

		// Resolve target (for now, just use requested_target)
		job.ResolvedTarget = req.RequestedTarget
		job.State = jobs.JobStateReady
		job.Message = "Upgrade ready"
		job.UpdatedAt = time.Now().UTC()

		if err := s.jobStore.Save(job); err != nil {
			log.Printf("Failed to save job: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		s.jobStore.AppendLog(fmt.Sprintf("Job ready: resolved_target=%s", job.ResolvedTarget))

		// Launch background execution goroutine
		go s.executeUpgrade(job, manifestData)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(job)
	}
}

// isJobActive returns true if the job is in an active state.
func isJobActive(job *jobs.Job) bool {
	// Active states are those that indicate ongoing work
	return job.State == jobs.JobStatePolicyFetching ||
		job.State == jobs.JobStateManifestFetching ||
		job.State == jobs.JobStateExecuting ||
		job.State == jobs.JobStateVerifying
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

		playbook := recovery.RenderPlaybook(job.FailureCode, s.buildPlaybookContext(job.BackupPath))
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
		resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, log.Default())
		resolved, err := resolver.Resolve(manifestData)
		if err != nil {
			if resErr, ok := err.(*container.ResolutionError); ok && resErr.GetFailureCode() == "CONTAINER_NAME_UNRESOLVED" {
				imagePattern := "payramapp/payram:"
				if s.config.ImageRepoOverride != "" {
					imagePattern = s.config.ImageRepoOverride + ":"
				}
				discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, log.Default())
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
		log.Printf("Target container resolved as: %s", containerName)

		inspector := inspect.NewInspector(
			s.jobStore,
			s.dockerRunner.DockerBin,
			containerName,
			s.coreClient.BaseURL, // Use resolved BaseURL from coreClient (handles auto-discovery)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		mode, err := resolveMode(req.Mode, req.Source)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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

		plan := s.PlanUpgrade(ctx, mode, req.RequestedTarget)

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
					discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, log.Default())
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		mode, err := resolveMode(req.Mode, req.Source)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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

		// Check for active job (concurrency check)
		existingJob, err := s.jobStore.LoadLatest()
		if err != nil {
			log.Printf("Failed to load existing job: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if existingJob != nil && isJobActive(existingJob) {
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

		plan := s.PlanUpgrade(ctx, mode, req.RequestedTarget)
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
		job.State = jobs.JobStateReady
		job.Message = "Upgrade job created"
		job.UpdatedAt = time.Now().UTC()

		// Save job
		if err := s.jobStore.Save(job); err != nil {
			log.Printf("Failed to save job: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Log start with source
		s.jobStore.AppendLog(fmt.Sprintf("Starting upgrade job %s: mode=%s target=%s (resolved: %s) source=%s",
			jobID, mode, req.RequestedTarget, plan.ResolvedTarget, source))

		// Launch background execution goroutine
		go s.executeUpgrade(job, plan.Manifest)

		// Return response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(RunResponse{
			JobID:           job.JobID,
			State:           string(job.State),
			Mode:            string(job.Mode),
			RequestedTarget: job.RequestedTarget,
			ResolvedTarget:  job.ResolvedTarget,
			Message:         "Upgrade job started",
		})
	}
}

// buildPlaybookContext constructs a PlaybookContext by discovering runtime information.
// It attempts to discover the running Payram container and extract dynamic values.
// Falls back to empty values if discovery fails (placeholders will remain in playbook).
func (s *Server) buildPlaybookContext(backupPath string) recovery.PlaybookContext {
	ctx := recovery.PlaybookContext{
		BackupPath: backupPath,
		ImageRepo:  "payramapp/payram", // default
	}

	// Determine image pattern for discovery
	imagePattern := "payramapp/payram:"
	if s.config.ImageRepoOverride != "" {
		imagePattern = s.config.ImageRepoOverride + ":"
		ctx.ImageRepo = s.config.ImageRepoOverride
	}

	// Try to discover running container
	discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, log.Default())
	discovered, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		// Container not found or discovery failed - return partial context
		// Placeholders will remain in the playbook
		return ctx
	}

	// Extract container name
	ctx.ContainerName = discovered.Name

	// Try to use the core client's base URL for port extraction
	// The coreClient was initialized with discovered port in New()
	if s.coreClient != nil {
		// Extract port from base URL (e.g., "http://127.0.0.1:8080")
		baseURL := s.coreClient.BaseURL
		ctx.BaseURL = baseURL

		// Simple port extraction from URL
		for i := len(baseURL) - 1; i >= 0; i-- {
			if baseURL[i] == ':' {
				ctx.HTTPPort = baseURL[i+1:]
				break
			}
		}
	}

	return ctx
}
