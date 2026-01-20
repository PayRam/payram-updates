package http

import (
	"encoding/json"
	"net/http"
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
