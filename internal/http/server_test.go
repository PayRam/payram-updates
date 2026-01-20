package http

import (
	"testing"
)

func TestNew(t *testing.T) {
	port := 8080
	server := New(port)

	if server == nil {
		t.Fatal("expected server to be created, got nil")
	}

	if server.port != port {
		t.Errorf("expected port %d, got %d", port, server.port)
	}

	if server.httpServer == nil {
		t.Fatal("expected httpServer to be created, got nil")
	}

	expectedAddr := "127.0.0.1:8080"
	if server.httpServer.Addr != expectedAddr {
		t.Errorf("expected addr %q, got %q", expectedAddr, server.httpServer.Addr)
	}
}
