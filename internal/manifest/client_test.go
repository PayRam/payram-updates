package manifest

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
	manifest := Manifest{
		Image: Image{
			Repo: "ghcr.io/payram/runtime",
		},
		Defaults: Defaults{
			ContainerName: "payram-runtime",
			RestartPolicy: "unless-stopped",
			Ports: []Port{
				{Container: 8080, Host: 8080, Protocol: "tcp"},
			},
			Volumes: []Volume{
				{Source: "/var/lib/payram", Destination: "/data", ReadOnly: false},
			},
		},
		Overrides: []Override{
			{
				Version:       "v2.0.0",
				ContainerName: "payram-runtime-v2",
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(manifest)
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	result, err := client.Fetch(context.Background(), server.URL)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Image.Repo != manifest.Image.Repo {
		t.Errorf("expected image repo %q, got %q", manifest.Image.Repo, result.Image.Repo)
	}

	if result.Defaults.ContainerName != manifest.Defaults.ContainerName {
		t.Errorf("expected container name %q, got %q", manifest.Defaults.ContainerName, result.Defaults.ContainerName)
	}

	if result.Defaults.RestartPolicy != manifest.Defaults.RestartPolicy {
		t.Errorf("expected restart policy %q, got %q", manifest.Defaults.RestartPolicy, result.Defaults.RestartPolicy)
	}

	if len(result.Defaults.Ports) != len(manifest.Defaults.Ports) {
		t.Errorf("expected %d ports, got %d", len(manifest.Defaults.Ports), len(result.Defaults.Ports))
	}

	if len(result.Defaults.Volumes) != len(manifest.Defaults.Volumes) {
		t.Errorf("expected %d volumes, got %d", len(manifest.Defaults.Volumes), len(result.Defaults.Volumes))
	}

	if len(result.Overrides) != len(manifest.Overrides) {
		t.Errorf("expected %d overrides, got %d", len(manifest.Overrides), len(result.Overrides))
	}

	if result.Overrides[0].Version != manifest.Overrides[0].Version {
		t.Errorf("expected override version %q, got %q", manifest.Overrides[0].Version, result.Overrides[0].Version)
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
		json.NewEncoder(w).Encode(Manifest{})
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

func TestFetch_MinimalManifest(t *testing.T) {
	manifest := Manifest{
		Image: Image{
			Repo: "ghcr.io/payram/runtime",
		},
		Defaults: Defaults{
			ContainerName: "payram",
			RestartPolicy: "always",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(manifest)
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	result, err := client.Fetch(context.Background(), server.URL)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Image.Repo != manifest.Image.Repo {
		t.Errorf("expected image repo %q, got %q", manifest.Image.Repo, result.Image.Repo)
	}

	if result.Defaults.ContainerName != manifest.Defaults.ContainerName {
		t.Errorf("expected container name %q, got %q", manifest.Defaults.ContainerName, result.Defaults.ContainerName)
	}
}

func TestFetch_WithMultipleOverrides(t *testing.T) {
	manifest := Manifest{
		Image: Image{Repo: "ghcr.io/payram/runtime"},
		Defaults: Defaults{
			ContainerName: "payram-runtime",
			RestartPolicy: "unless-stopped",
		},
		Overrides: []Override{
			{Version: "v2.0.0", ContainerName: "payram-v2"},
			{Version: "v3.0.0", RestartPolicy: "always"},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(manifest)
	}))
	defer server.Close()

	client := NewClient(5 * time.Second)
	result, err := client.Fetch(context.Background(), server.URL)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Overrides) != 2 {
		t.Fatalf("expected 2 overrides, got %d", len(result.Overrides))
	}

	if result.Overrides[0].Version != "v2.0.0" {
		t.Errorf("expected first override version v2.0.0, got %q", result.Overrides[0].Version)
	}

	if result.Overrides[1].Version != "v3.0.0" {
		t.Errorf("expected second override version v3.0.0, got %q", result.Overrides[1].Version)
	}
}
