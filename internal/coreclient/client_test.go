package coreclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNewClient tests client creation.
func TestNewClient(t *testing.T) {
	client := NewClient("http://localhost:8080")

	if client.BaseURL != "http://localhost:8080" {
		t.Errorf("expected BaseURL 'http://localhost:8080', got %s", client.BaseURL)
	}

	if client.HTTPClient == nil {
		t.Error("HTTPClient should not be nil")
	}

	if client.HTTPClient.Timeout != DefaultTimeout {
		t.Errorf("expected timeout %v, got %v", DefaultTimeout, client.HTTPClient.Timeout)
	}
}

// TestNewClient_TrailingSlash tests that trailing slashes are removed.
func TestNewClient_TrailingSlash(t *testing.T) {
	client := NewClient("http://localhost:8080/")

	if client.BaseURL != "http://localhost:8080" {
		t.Errorf("expected trailing slash removed, got %s", client.BaseURL)
	}
}

// TestHealth_Success tests successful health check.
func TestHealth_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("expected path /health, got %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET method, got %s", r.Method)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(HealthResponse{Status: "ok", DB: "ok"})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", response.Status)
	}
	if response.DB != "ok" {
		t.Errorf("expected db 'ok', got %s", response.DB)
	}
}

// TestHealth_WithDBField tests that db field is captured.
func TestHealth_WithDBField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","db":"ok"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", response.Status)
	}
	if response.DB != "ok" {
		t.Errorf("expected db 'ok', got %s", response.DB)
	}
}

// TestVersion_Success tests successful version retrieval.
func TestVersion_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("expected path /version, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(VersionResponse{Version: "1.2.3"})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Version(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response.Version != "1.2.3" {
		t.Errorf("expected version '1.2.3', got %s", response.Version)
	}
}

// TestMigrationsStatus_Success tests successful migrations status retrieval.
func TestMigrationsStatus_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/migrations/status" {
			t.Errorf("expected path /admin/migrations/status, got %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(MigrationsStatusResponse{
			State: "complete",
		})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.MigrationsStatus(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response.State != "complete" {
		t.Errorf("expected state 'complete', got %s", response.State)
	}
}

// TestHealth_Non200Status tests handling of non-200 status codes.
func TestHealth_Non200Status(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	_, err := client.Health(ctx)
	if err == nil {
		t.Fatal("expected error for 500 status")
	}

	if !strings.Contains(err.Error(), "unexpected status code 500") {
		t.Errorf("expected error to contain status code, got: %v", err)
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("expected error to contain response body, got: %v", err)
	}
}

// TestVersion_InvalidJSON tests handling of invalid JSON responses.
func TestVersion_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not valid json"))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	_, err := client.Version(ctx)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}

	if !strings.Contains(err.Error(), "failed to decode JSON") {
		t.Errorf("expected JSON decode error, got: %v", err)
	}
}

// TestHealth_UnknownFields tests lenient JSON parsing with unknown fields.
// The health endpoint intentionally ignores unknown fields to allow payram-core
// to evolve its health response without breaking the updater.
func TestHealth_UnknownFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","extra_field":"value","another_field":123}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("unexpected error: health should ignore unknown fields, got: %v", err)
	}

	if response.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", response.Status)
	}
}

// TestMigrationsStatus_ContextCancellation tests context cancellation.
func TestMigrationsStatus_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Delay to allow context cancellation
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(MigrationsStatusResponse{State: "complete"})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := client.MigrationsStatus(ctx)
	if err == nil {
		t.Fatal("expected error for context timeout")
	}

	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("expected context deadline error, got: %v", err)
	}
}

// TestHealth_ErrorWrapping tests that errors are properly wrapped.
func TestHealth_ErrorWrapping(t *testing.T) {
	// Test with invalid URL
	client := NewClient("http://invalid-host-that-does-not-exist:99999")
	ctx := context.Background()

	_, err := client.Health(ctx)
	if err == nil {
		t.Fatal("expected error for invalid host")
	}

	if !strings.Contains(err.Error(), "health check failed") {
		t.Errorf("expected wrapped error with context, got: %v", err)
	}
}

// TestVersion_ErrorWrapping tests version error wrapping.
func TestVersion_ErrorWrapping(t *testing.T) {
	client := NewClient("http://invalid-host:99999")
	ctx := context.Background()

	_, err := client.Version(ctx)
	if err == nil {
		t.Fatal("expected error for invalid host")
	}

	if !strings.Contains(err.Error(), "version check failed") {
		t.Errorf("expected wrapped error with context, got: %v", err)
	}
}

// TestMigrationsStatus_ErrorWrapping tests migrations status error wrapping.
func TestMigrationsStatus_ErrorWrapping(t *testing.T) {
	client := NewClient("http://invalid-host:99999")
	ctx := context.Background()

	_, err := client.MigrationsStatus(ctx)
	if err == nil {
		t.Fatal("expected error for invalid host")
	}

	if !strings.Contains(err.Error(), "migrations status check failed") {
		t.Errorf("expected wrapped error with context, got: %v", err)
	}
}

// TestClient_DefaultTimeout tests the default timeout value.
func TestClient_DefaultTimeout(t *testing.T) {
	if DefaultTimeout != 3*time.Second {
		t.Errorf("expected default timeout 3s, got %v", DefaultTimeout)
	}

	client := NewClient("http://localhost:8080")
	if client.HTTPClient.Timeout != 3*time.Second {
		t.Errorf("expected client timeout 3s, got %v", client.HTTPClient.Timeout)
	}
}

// TestHealth_MissingFields tests handling of responses with missing required fields.
func TestHealth_MissingFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`)) // Empty JSON
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should successfully parse with empty status
	if response.Status != "" {
		t.Errorf("expected empty status, got %s", response.Status)
	}
}

// TestVersion_LargeResponse tests handling of large responses.
func TestVersion_LargeResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write a large response that exceeds MaxResponseSize
		largeData := strings.Repeat("a", MaxResponseSize+1000)
		json.NewEncoder(w).Encode(map[string]string{"version": largeData})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	_, err := client.Version(ctx)
	if err == nil {
		t.Fatal("expected error for large response")
	}

	// Should get some error related to parsing or reading
	if !strings.Contains(err.Error(), "failed to") {
		t.Errorf("expected parse/read error, got: %v", err)
	}
}

// TestMigrationsStatus_DifferentStates tests various migration states.
func TestMigrationsStatus_DifferentStates(t *testing.T) {
	testCases := []struct {
		name     string
		response MigrationsStatusResponse
	}{
		{
			name: "complete",
			response: MigrationsStatusResponse{
				State: "complete",
			},
		},
		{
			name: "failed",
			response: MigrationsStatusResponse{
				State: "failed",
			},
		},
		{
			name: "pending",
			response: MigrationsStatusResponse{
				State: "pending",
			},
		},
		{
			name: "running",
			response: MigrationsStatusResponse{
				State: "running",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(tc.response)
			}))
			defer server.Close()

			client := NewClient(server.URL)
			ctx := context.Background()

			response, err := client.MigrationsStatus(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if response.State != tc.response.State {
				t.Errorf("expected state %s, got %s", tc.response.State, response.State)
			}
		})
	}
}

// TestHealth_WithExtraFields tests that health check accepts extra fields (F1).
// This ensures forward compatibility with future payram-core versions.
func TestHealth_WithExtraFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","db":"ok","time":"2026-02-02T12:00:00Z","build":"abc123","extra_field":true}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("unexpected error: health should ignore extra fields, got: %v", err)
	}

	if response.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", response.Status)
	}
	if response.DB != "ok" {
		t.Errorf("expected db 'ok', got %s", response.DB)
	}
}

// TestHealth_WithoutDBField tests that health check works without db field (F1).
// The db field is optional - only status is required.
func TestHealth_WithoutDBField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", response.Status)
	}
	if response.DB != "" {
		t.Errorf("expected empty db field, got %s", response.DB)
	}
}

// TestHealth_WithDBFieldNotOk tests health response with db != ok.
// This is handled at the validation layer (server.go), not the client.
func TestHealth_WithDBFieldNotOk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok","db":"degraded"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Client successfully parses the response
	if response.Status != "ok" {
		t.Errorf("expected status 'ok', got %s", response.Status)
	}
	if response.DB != "degraded" {
		t.Errorf("expected db 'degraded', got %s", response.DB)
	}
}

// TestVersion_WithExtraFields tests that version check accepts extra fields (F2).
// This ensures forward compatibility with future payram-core versions.
func TestVersion_WithExtraFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"version":"1.2.3","build":"abc123","image":"payramapp/payram:1.2.3","timestamp":"2026-02-02"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.Version(ctx)
	if err != nil {
		t.Fatalf("unexpected error: version should ignore extra fields, got: %v", err)
	}

	if response.Version != "1.2.3" {
		t.Errorf("expected version '1.2.3', got %s", response.Version)
	}
}

// TestMigrationsStatus_WithExtraFields tests that migrations status accepts extra fields (F3).
func TestMigrationsStatus_WithExtraFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"state":"complete","pending_count":0,"applied_count":5,"last_migration":"20260202120000"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.MigrationsStatus(ctx)
	if err != nil {
		t.Fatalf("unexpected error: migrations should ignore extra fields, got: %v", err)
	}

	if response.State != "complete" {
		t.Errorf("expected state 'complete', got %s", response.State)
	}
}

// TestMigrationsStatus_RunningState tests the running state (F3).
func TestMigrationsStatus_RunningState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(MigrationsStatusResponse{State: "running"})
	}))
	defer server.Close()

	client := NewClient(server.URL)
	ctx := context.Background()

	response, err := client.MigrationsStatus(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if response.State != "running" {
		t.Errorf("expected state 'running', got %s", response.State)
	}
}
