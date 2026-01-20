package policy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	timeout := 5 * time.Second
	client := NewClient(timeout)

	if client == nil {
		t.Fatal("expected non-nil client")
	}

	if client.timeout != timeout {
		t.Errorf("expected timeout %v, got %v", timeout, client.timeout)
	}

	if client.httpClient == nil {
		t.Error("expected non-nil http client")
	}
}

func TestFetch_Success(t *testing.T) {
	policy := Policy{
		Latest:   "v1.2.3",
		Releases: []string{"v1.2.3", "v1.2.2", "v1.2.1"},
		Breakpoints: []Breakpoint{
			{Version: "v1.0.0", Reason: "Major change", Docs: "https://example.com/docs"},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(policy)
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	result, err := client.Fetch(context.Background(), server.URL)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Latest != policy.Latest {
		t.Errorf("expected latest %q, got %q", policy.Latest, result.Latest)
	}

	if len(result.Releases) != len(policy.Releases) {
		t.Errorf("expected %d releases, got %d", len(policy.Releases), len(result.Releases))
	}

	if len(result.Breakpoints) != len(policy.Breakpoints) {
		t.Errorf("expected %d breakpoints, got %d", len(policy.Breakpoints), len(result.Breakpoints))
	}

	if result.Breakpoints[0].Version != policy.Breakpoints[0].Version {
		t.Errorf("expected breakpoint version %q, got %q", policy.Breakpoints[0].Version, result.Breakpoints[0].Version)
	}
}

func TestFetch_Non200Status(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"404 Not Found", http.StatusNotFound},
		{"500 Internal Server Error", http.StatusInternalServerError},
		{"403 Forbidden", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer server.Close()

			client := NewClient(5 * time.Second)
			_, err := client.Fetch(context.Background(), server.URL)

			if err == nil {
				t.Fatal("expected error for non-200 status")
			}

			if !errors.Is(err, ErrNon200Status) {
				t.Errorf("expected ErrNon200Status, got: %v", err)
			}
		})
	}
}

func TestFetch_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not valid json"))
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	_, err := client.Fetch(context.Background(), server.URL)

	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}

	if !errors.Is(err, ErrInvalidJSON) {
		t.Errorf("expected ErrInvalidJSON, got: %v", err)
	}
}

func TestFetch_ResponseTooBig(t *testing.T) {
	largeData := strings.Repeat("x", maxResponseSize+1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(largeData))
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	_, err := client.Fetch(context.Background(), server.URL)

	if err == nil {
		t.Fatal("expected error for response too big")
	}

	if !errors.Is(err, ErrResponseTooBig) {
		t.Errorf("expected ErrResponseTooBig, got: %v", err)
	}
}

func TestFetch_ContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(Policy{Latest: "v1.0.0"})
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := client.Fetch(ctx, server.URL)

	if err == nil {
		t.Fatal("expected error for context timeout")
	}
}

func TestFetch_EmptyPolicy(t *testing.T) {
	policy := Policy{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(policy)
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	result, err := client.Fetch(context.Background(), server.URL)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Latest != "" {
		t.Errorf("expected empty latest, got %q", result.Latest)
	}

	if result.Releases != nil {
		t.Errorf("expected nil releases, got %v", result.Releases)
	}

	if result.Breakpoints != nil {
		t.Errorf("expected nil breakpoints, got %v", result.Breakpoints)
	}
}
