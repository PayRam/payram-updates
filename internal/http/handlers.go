package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/policy"
)

// HealthResponse represents the health check response.
type HealthResponse struct {
	Status string `json:"status"`
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

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(job)
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

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(job)
	}
}

// isJobActive returns true if the job is in an active state.
func isJobActive(job *jobs.Job) bool {
	return job.State != jobs.JobStateIdle &&
		job.State != jobs.JobStateReady &&
		job.State != jobs.JobStateFailed
}
