package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestHandleUpgradeStatus_NotFailedNoPlaybook(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)

	// Create and save a successful job
	testJob := jobs.NewJob("test-success", jobs.JobModeManual, "v1.0.0")
	testJob.State = jobs.JobStateReady
	testJob.Message = "Upgrade complete"

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

	var statusResp UpgradeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Playbook should NOT be included for non-failed jobs
	if statusResp.RecoveryPlaybook != nil {
		t.Error("expected no recovery playbook for non-failed job")
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

func TestHandleUpgradeLast_NoJob(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/last", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeLast()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["message"] != "No upgrade job found" {
		t.Errorf("expected message about no job, got %q", result["message"])
	}
}

func TestHandleUpgradeLast_WithJob(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)

	// Create a job
	testJob := jobs.NewJob("test-123", jobs.JobModeManual, "v1.0.0")
	testJob.State = jobs.JobStateReady
	if err := jobStore.Save(testJob); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/last", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeLast()
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
}

func TestHandleUpgradePlaybook_NoJob(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/playbook", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradePlaybook()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["playbook"] != nil {
		t.Error("expected no playbook for no job")
	}
}

func TestHandleUpgradePlaybook_FailedJob(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)

	// Create a failed job
	testJob := jobs.NewJob("test-123", jobs.JobModeManual, "v2.0.0")
	testJob.State = jobs.JobStateFailed
	testJob.FailureCode = "HEALTHCHECK_FAILED"
	testJob.Message = "health check error"
	if err := jobStore.Save(testJob); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/playbook", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradePlaybook()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["playbook"] == nil {
		t.Error("expected playbook for failed job")
	}

	if result["failureCode"] != "HEALTHCHECK_FAILED" {
		t.Errorf("expected failureCode HEALTHCHECK_FAILED, got %v", result["failureCode"])
	}
}

func TestHandleUpgradeInspect(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{
		Port:               8080,
		PolicyURL:          "http://example.com/policy.json",
		RuntimeManifestURL: "http://example.com/manifest.json",
		CoreBaseURL:        "http://localhost:8080",
		DockerBin:          "docker",
	}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/inspect", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeInspect()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Should have overallState
	if result["overallState"] == nil {
		t.Error("expected overallState in response")
	}

	// Should have checks map
	if result["checks"] == nil {
		t.Error("expected checks in response")
	}
}

func TestHandleUpgradeLast_MethodNotAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodPost, "/upgrade/last", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeLast()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, resp.StatusCode)
	}
}

func TestHandleUpgradePlan_Success(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock policy server
	policyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"latest":   "v1.7.0",
			"releases": []string{"v1.7.0", "v1.6.0"},
		})
	}))
	defer policyServer.Close()

	// Create mock manifest server
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"image": map[string]interface{}{
				"repo": "ghcr.io/payram/runtime",
			},
			"defaults": map[string]interface{}{
				"container_name": "payram-core",
			},
		})
	}))
	defer manifestServer.Close()

	cfg := &config.Config{
		Port:                8080,
		PolicyURL:           policyServer.URL,
		RuntimeManifestURL:  manifestServer.URL,
		FetchTimeoutSeconds: 5,
	}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	body := strings.NewReader(`{"requestedTarget":"v1.7.0"}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/plan", body)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradePlan()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var planResp PlanResponse
	if err := json.NewDecoder(resp.Body).Decode(&planResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if planResp.State != "READY" {
		t.Errorf("expected state READY, got %s", planResp.State)
	}
	if planResp.Mode != "DASHBOARD" {
		t.Errorf("expected mode DASHBOARD, got %s", planResp.Mode)
	}
	if planResp.RequestedTarget != "v1.7.0" {
		t.Errorf("expected requestedTarget v1.7.0, got %s", planResp.RequestedTarget)
	}
	if planResp.ImageRepo != "ghcr.io/payram/runtime" {
		t.Errorf("expected imageRepo ghcr.io/payram/runtime, got %s", planResp.ImageRepo)
	}
	if planResp.ContainerName != "payram-core" {
		t.Errorf("expected containerName payram-core, got %s", planResp.ContainerName)
	}

	// Verify no job was created (dry-run is read-only)
	job, err := jobStore.LoadLatest()
	if err != nil {
		t.Fatalf("failed to load job: %v", err)
	}
	if job != nil {
		t.Error("expected no job to be created during plan, but found one")
	}
}

func TestHandleUpgradePlan_PolicyFetchFailed_Dashboard(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock policy server that fails
	policyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer policyServer.Close()

	cfg := &config.Config{
		Port:                8080,
		PolicyURL:           policyServer.URL,
		RuntimeManifestURL:  "http://localhost:1/manifest", // Will not be reached
		FetchTimeoutSeconds: 5,
	}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	body := strings.NewReader(`{"requestedTarget":"v1.7.0"}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/plan", body)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradePlan()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var planResp PlanResponse
	if err := json.NewDecoder(resp.Body).Decode(&planResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if planResp.State != "FAILED" {
		t.Errorf("expected state FAILED, got %s", planResp.State)
	}
	if planResp.FailureCode != "POLICY_FETCH_FAILED" {
		t.Errorf("expected failureCode POLICY_FETCH_FAILED, got %s", planResp.FailureCode)
	}
}

func TestHandleUpgradePlan_MethodNotAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	req := httptest.NewRequest(http.MethodGet, "/upgrade/plan", nil)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradePlan()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status %d, got %d", http.StatusMethodNotAllowed, resp.StatusCode)
	}
}

func TestHandleUpgradeRun_Success(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock policy server
	policyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"latest":   "v1.7.0",
			"releases": []string{"v1.7.0", "v1.6.0"},
		})
	}))
	defer policyServer.Close()

	// Create mock manifest server
	manifestServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"image": map[string]interface{}{
				"repo": "ghcr.io/payram/runtime",
			},
			"defaults": map[string]interface{}{
				"containerName": "payram-core",
			},
		})
	}))
	defer manifestServer.Close()

	cfg := &config.Config{
		Port:                8080,
		PolicyURL:           policyServer.URL,
		RuntimeManifestURL:  manifestServer.URL,
		FetchTimeoutSeconds: 5,
		DockerBin:           "echo", // Mock docker with echo
	}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	body := strings.NewReader(`{"requestedTarget":"v1.7.0","source":"CLI"}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", body)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeRun()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	var runResp RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&runResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if runResp.JobID == "" {
		t.Error("expected jobId to be set")
	}
	if runResp.Mode != "DASHBOARD" {
		t.Errorf("expected mode DASHBOARD, got %s", runResp.Mode)
	}
	if runResp.RequestedTarget != "v1.7.0" {
		t.Errorf("expected requestedTarget v1.7.0, got %s", runResp.RequestedTarget)
	}

	// Wait a bit for background execution
	time.Sleep(100 * time.Millisecond)

	// Verify job was created
	job, err := jobStore.LoadLatest()
	if err != nil {
		t.Fatalf("failed to load job: %v", err)
	}
	if job == nil {
		t.Fatal("expected job to be created")
	}
	if job.JobID != runResp.JobID {
		t.Errorf("job ID mismatch: expected %s, got %s", runResp.JobID, job.JobID)
	}
}

func TestHandleUpgradeRun_Conflict(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := &config.Config{
		Port:                8080,
		PolicyURL:           "http://localhost:1/policy",
		RuntimeManifestURL:  "http://localhost:1/manifest",
		FetchTimeoutSeconds: 5,
	}
	jobStore := jobs.NewStore(tmpDir)

	// Create an active job
	activeJob := jobs.NewJob("existing-job", jobs.JobModeDashboard, "v1.6.0")
	activeJob.State = jobs.JobStateExecuting
	activeJob.UpdatedAt = time.Now().UTC()
	jobStore.Save(activeJob)

	server := New(cfg, jobStore)

	body := strings.NewReader(`{"requestedTarget":"v1.7.0","source":"CLI"}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", body)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeRun()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected status %d, got %d", http.StatusConflict, resp.StatusCode)
	}

	var errResp map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if errResp["error"] != "An active job already exists" {
		t.Errorf("expected error message about active job, got %s", errResp["error"])
	}
	if errResp["jobId"] != "existing-job" {
		t.Errorf("expected jobId existing-job, got %s", errResp["jobId"])
	}
}

func TestHandleUpgradeRun_InvalidBody(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	body := strings.NewReader(`{invalid json}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", body)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeRun()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, resp.StatusCode)
	}
}

func TestHandleUpgradeRun_MissingTarget(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := &config.Config{Port: 8080}
	jobStore := jobs.NewStore(tmpDir)
	server := New(cfg, jobStore)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", body)
	w := httptest.NewRecorder()

	handler := server.HandleUpgradeRun()
	handler(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d", http.StatusBadRequest, resp.StatusCode)
	}
}
