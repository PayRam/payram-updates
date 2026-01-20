package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/jobs"
)

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		wantStatusCode int
		wantResponse   *HealthResponse
	}{
		{
			name:           "GET request returns ok",
			method:         http.MethodGet,
			wantStatusCode: http.StatusOK,
			wantResponse:   &HealthResponse{Status: "ok"},
		},
		{
			name:           "POST request returns method not allowed",
			method:         http.MethodPost,
			wantStatusCode: http.StatusMethodNotAllowed,
			wantResponse:   nil,
		},
		{
			name:           "PUT request returns method not allowed",
			method:         http.MethodPut,
			wantStatusCode: http.StatusMethodNotAllowed,
			wantResponse:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/health", nil)
			w := httptest.NewRecorder()

			handler := HandleHealth()
			handler(w, req)

			resp := w.Result()
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatusCode {
				t.Errorf("expected status %d, got %d", tt.wantStatusCode, resp.StatusCode)
			}

			if tt.wantResponse != nil {
				var got HealthResponse
				if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}

				if got.Status != tt.wantResponse.Status {
					t.Errorf("expected status %q, got %q", tt.wantResponse.Status, got.Status)
				}

				contentType := resp.Header.Get("Content-Type")
				if contentType != "application/json" {
					t.Errorf("expected Content-Type 'application/json', got %q", contentType)
				}
			}
		})
	}
}

func TestHandleUpgradeStatus_NoJob(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/status", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeStatus()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var job jobs.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if job.State != jobs.JobStateIdle {
		t.Errorf("expected state IDLE, got %s", job.State)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}
}

func TestHandleUpgradeStatus_WithJob(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)

	// Create and save a job
	testJob := jobs.NewJob("test-123", jobs.JobModeManual, "v1.0.0")
	testJob.State = jobs.JobStateReady
	testJob.ResolvedTarget = "v1.0.0"
	testJob.Message = "Update ready"

	if err := jobStore.Save(testJob); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/status", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeStatus()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var job jobs.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if job.JobID != testJob.JobID {
		t.Errorf("expected JobID %q, got %q", testJob.JobID, job.JobID)
	}
	if job.State != testJob.State {
		t.Errorf("expected state %s, got %s", testJob.State, job.State)
	}
	if job.Message != testJob.Message {
		t.Errorf("expected message %q, got %q", testJob.Message, job.Message)
	}
}

func TestHandleUpgradeStatus_MethodNotAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodPost, "/upgrade/status", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeStatus()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, resp.StatusCode)
	}
}

func TestHandleUpgradeLogs_NoLogs(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/logs", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeLogs()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "text/plain" {
		t.Errorf("expected Content-Type 'text/plain', got %q", contentType)
	}
}

func TestHandleUpgradeLogs_WithLogs(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)

	// Append some logs
	testLogs := []string{
		"Starting update",
		"Fetching policy",
		"Update complete",
	}
	for _, line := range testLogs {
		if err := jobStore.AppendLog(line); err != nil {
			t.Fatalf("failed to append log: %v", err)
		}
	}

	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/logs", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeLogs()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	body := w.Body.String()
	for _, line := range testLogs {
		if !strings.Contains(body, line) {
			t.Errorf("expected logs to contain %q", line)
		}
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType != "text/plain" {
		t.Errorf("expected Content-Type 'text/plain', got %q", contentType)
	}
}

func TestHandleUpgradeLogs_MethodNotAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodPost, "/upgrade/logs", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeLogs()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, resp.StatusCode)
	}
}
