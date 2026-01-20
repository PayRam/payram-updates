package http

import (
	"encoding/json"
	"net/http"

	"github.com/payram/payram-updater/internal/jobs"
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
