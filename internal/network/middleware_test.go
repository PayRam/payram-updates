package network

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAllowedIPsMiddleware_AllowedIP(t *testing.T) {
	allowedIPs := []string{"127.0.0.1", "172.17.0.2"}
	logger := log.Default()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	middleware := AllowedIPsMiddleware(allowedIPs, logger)(handler)

	// Test request from allowed IP
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "127.0.0.1:12345"

	w := httptest.NewRecorder()
	middleware.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	if w.Body.String() != "success" {
		t.Errorf("expected body 'success', got '%s'", w.Body.String())
	}
}

func TestAllowedIPsMiddleware_BlockedIP(t *testing.T) {
	allowedIPs := []string{"127.0.0.1", "172.17.0.2"}
	logger := log.Default()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	middleware := AllowedIPsMiddleware(allowedIPs, logger)(handler)

	// Test request from blocked IP
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "192.168.1.100:54321"

	w := httptest.NewRecorder()
	middleware.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", w.Code)
	}
}

func TestAllowedIPsMiddleware_IPv6Localhost(t *testing.T) {
	allowedIPs := []string{"127.0.0.1", "::1"}
	logger := log.Default()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	middleware := AllowedIPsMiddleware(allowedIPs, logger)(handler)

	// Test request from IPv6 localhost
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.RemoteAddr = "[::1]:12345"

	w := httptest.NewRecorder()
	middleware.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 for IPv6 localhost, got %d", w.Code)
	}
}

func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		expectedIP string
	}{
		{
			name:       "RemoteAddr IPv4",
			remoteAddr: "192.168.1.100:12345",
			expectedIP: "192.168.1.100",
		},
		{
			name:       "RemoteAddr IPv6",
			remoteAddr: "[::1]:12345",
			expectedIP: "::1",
		},
		{
			name:       "X-Real-IP header",
			remoteAddr: "127.0.0.1:12345",
			headers:    map[string]string{"X-Real-IP": "10.0.0.1"},
			expectedIP: "127.0.0.1", // RemoteAddr takes priority
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			ip := getClientIP(req)
			if ip != tt.expectedIP {
				t.Errorf("expected IP %s, got %s", tt.expectedIP, ip)
			}
		})
	}
}

func TestAllowList_LooksTheContainerUpAgainWhenItsAddressChanges(t *testing.T) {
	current := []string{"172.17.0.2"}
	lookups := 0
	list := NewAllowList(func(context.Context) ([]string, error) {
		lookups++
		return current, nil
	}, 0, log.New(io.Discard, "", 0))

	if !list.Allows("172.17.0.2") {
		t.Fatal("the container's address must be allowed")
	}
	// An update recreated the container with a new address.
	current = []string{"172.17.0.3"}
	if !list.Allows("172.17.0.3") {
		t.Fatal("the recreated container's new address must be allowed after a lookup")
	}
	if list.Allows("172.17.0.2") {
		t.Fatal("the old address must no longer be allowed")
	}
	if !list.Allows("127.0.0.1") || !list.Allows("::1") {
		t.Fatal("this machine is always allowed")
	}
	if lookups < 2 {
		t.Fatalf("lookups = %d, want a fresh lookup for the unknown address", lookups)
	}
}

func TestAllowList_LookupsAreRateLimited(t *testing.T) {
	lookups := 0
	list := NewAllowList(func(context.Context) ([]string, error) {
		lookups++
		return []string{"172.17.0.2"}, nil
	}, time.Hour, log.New(io.Discard, "", 0))

	for i := 0; i < 50; i++ {
		list.Allows("10.9.9.9")
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want 1 (an unknown address must not trigger a docker inspect per request)", lookups)
	}
}
