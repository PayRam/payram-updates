package http

import (
	"testing"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/jobs"
)

func TestNew(t *testing.T) {
	cfg := &config.Config{
		Port: 8080,
	}
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	server := New(cfg, jobStore)

	if server == nil {
		t.Fatal("expected server to be created, got nil")
	}

	if server.port != 8080 {
		t.Errorf("expected port %d, got %d", 8080, server.port)
	}

	if server.httpServer == nil {
		t.Fatal("expected httpServer to be created, got nil")
	}

	expectedAddr := "127.0.0.1:8080"
	if server.httpServer.Addr != expectedAddr {
		t.Errorf("expected addr %q, got %q", expectedAddr, server.httpServer.Addr)
	}

	if server.config == nil {
		t.Fatal("expected config to be set, got nil")
	}

	if server.jobStore == nil {
		t.Fatal("expected jobStore to be set, got nil")
	}
}
