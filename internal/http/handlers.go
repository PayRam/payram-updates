package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/payram/payram-updater/internal/container"
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
	RecoveryPlaybook *recovery.Playbook `json:"recovery_playbook,omitempty"`
}

// PlanRequest represents the request body for POST /upgrade/plan.
type PlanRequest struct {
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requested_target"`
}

// PlanResponse represents the response for POST /upgrade/plan.
type PlanResponse struct {
	State           string `json:"state"`
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requested_target"`
	ResolvedTarget  string `json:"resolved_target,omitempty"`
	FailureCode     string `json:"failure_code,omitempty"`
	Message         string `json:"message"`
	ImageRepo       string `json:"image_repo,omitempty"`
	ContainerName   string `json:"container_name,omitempty"`
}

// RunRequest represents the request body for POST /upgrade/run.
type RunRequest struct {
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requested_target"`
	Source          string `json:"source"` // "CLI" or "DASHBOARD"
}

// RunResponse represents the response for POST /upgrade/run.
type RunResponse struct {
	JobID           string `json:"job_id,omitempty"`
	State           string `json:"state"`
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requested_target"`
	ResolvedTarget  string `json:"resolved_target,omitempty"`
	FailureCode     string `json:"failure_code,omitempty"`
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
			playbook := recovery.GetPlaybookWithBackup(job.FailureCode, job.BackupPath)
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

// UpgradeRequest represents the request body for POST /upgrade.
type UpgradeRequest struct {
	Mode            string `json:"mode"`
	RequestedTarget string `json:"requested_target"`
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

		// Validate mode
		var mode jobs.JobMode
		switch req.Mode {
		case "DASHBOARD":
			mode = jobs.JobModeDashboard
		case "MANUAL":
			mode = jobs.JobModeManual
		default:
			http.Error(w, "Invalid mode: must be DASHBOARD or MANUAL", http.StatusBadRequest)
			return
		}

		// Validate requested_target
		if req.RequestedTarget == "" {
			http.Error(w, "requested_target is required", http.StatusBadRequest)
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
		s.jobStore.AppendLog(fmt.Sprintf("Starting upgrade job %s: mode=%s target=%s", jobID, req.Mode, req.RequestedTarget))

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

		playbook := recovery.GetPlaybookWithBackup(job.FailureCode, job.BackupPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"playbook":     &playbook,
			"failure_code": job.FailureCode,
			"backup_path":  job.BackupPath,
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

		containerName := resolved.Name
		log.Printf("Target container resolved as: %s", containerName)

		// Default ports
		defaultPorts := []int{8080, 443}

		inspector := inspect.NewInspector(
			s.jobStore,
			s.dockerRunner.DockerBin,
			containerName,
			s.coreClient.BaseURL, // Use resolved BaseURL from coreClient (handles auto-discovery)
			s.config.PolicyURL,
			s.config.RuntimeManifestURL,
			defaultPorts,
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

		// Validate mode
		var mode jobs.JobMode
		switch req.Mode {
		case "DASHBOARD":
			mode = jobs.JobModeDashboard
		case "MANUAL":
			mode = jobs.JobModeManual
		default:
			http.Error(w, "Invalid mode: must be DASHBOARD or MANUAL", http.StatusBadRequest)
			return
		}

		// Validate requested_target
		if req.RequestedTarget == "" {
			http.Error(w, "requested_target is required", http.StatusBadRequest)
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
			// Resolve container name using the resolver (env > manifest)
			resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, nil)
			if resolved, err := resolver.Resolve(plan.Manifest); err == nil {
				response.ContainerName = resolved.Name
			} else {
				// If resolution fails, report it in the response
				response.ContainerName = ""
				response.FailureCode = "CONTAINER_NAME_UNRESOLVED"
				response.Message = err.Error()
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

		// Validate mode
		var mode jobs.JobMode
		switch req.Mode {
		case "DASHBOARD":
			mode = jobs.JobModeDashboard
		case "MANUAL":
			mode = jobs.JobModeManual
		default:
			http.Error(w, "Invalid mode: must be DASHBOARD or MANUAL", http.StatusBadRequest)
			return
		}

		// Validate requested_target
		if req.RequestedTarget == "" {
			http.Error(w, "requested_target is required", http.StatusBadRequest)
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
				"job_id":  existingJob.JobID,
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
		s.jobStore.AppendLog(fmt.Sprintf("Starting upgrade job %s: mode=%s target=%s source=%s",
			jobID, req.Mode, req.RequestedTarget, source))

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
