package network

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// AllowList admits requests from this machine and from the PayRam container.
// The container's addresses are looked up again when an unknown address
// knocks (at most once per refreshEvery): an update recreates the container,
// and Docker may give it a new IP, which used to lock the dashboard out until
// the updater was restarted.
type AllowList struct {
	lookup       func(ctx context.Context) ([]string, error)
	refreshEvery time.Duration
	logger       *log.Logger

	mu         sync.Mutex
	ips        map[string]bool
	lastLookup time.Time
}

// NewAllowList creates an allow list whose container addresses come from
// lookup. A nil lookup admits this machine only.
func NewAllowList(lookup func(ctx context.Context) ([]string, error), refreshEvery time.Duration, logger *log.Logger) *AllowList {
	return &AllowList{lookup: lookup, refreshEvery: refreshEvery, logger: logger, ips: map[string]bool{}}
}

// Allows reports whether a request from ip may use the updater API.
func (a *AllowList) Allows(ip string) bool {
	if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ips[ip] {
		return true
	}
	if a.lookup == nil || time.Since(a.lastLookup) < a.refreshEvery {
		return false
	}
	a.refreshLocked()
	return a.ips[ip]
}

// Refresh looks the container's addresses up now.
func (a *AllowList) Refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshLocked()
}

func (a *AllowList) refreshLocked() {
	a.lastLookup = time.Now()
	if a.lookup == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ips, err := a.lookup(ctx)
	if err != nil {
		a.logger.Printf("Could not look up the PayRam container's address (%v); only this machine can reach the updater until it can", err)
		return
	}
	fresh := make(map[string]bool, len(ips))
	for _, ip := range ips {
		fresh[ip] = true
	}
	a.ips = fresh
}

// Middleware refuses requests from any other address.
func (a *AllowList) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := getClientIP(r)
		if !a.Allows(clientIP) {
			a.logger.Printf("ACCESS DENIED: Request from unauthorized IP %s to %s %s", clientIP, r.Method, r.URL.Path)
			http.Error(w, "Access forbidden: unauthorized source IP", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AllowedIPsMiddleware creates middleware that restricts access to a fixed
// set of addresses (plus this machine).
func AllowedIPsMiddleware(allowedIPs []string, logger *log.Logger) func(http.Handler) http.Handler {
	fixed := append([]string(nil), allowedIPs...)
	list := NewAllowList(func(context.Context) ([]string, error) { return fixed, nil }, time.Hour, logger)
	list.Refresh()
	return list.Middleware
}

// IsLoopbackRequest reports whether a request came from this machine.
func IsLoopbackRequest(r *http.Request) bool {
	parsed := net.ParseIP(getClientIP(r))
	return parsed != nil && parsed.IsLoopback()
}

// getClientIP extracts the client's IP address from the request.
// Only RemoteAddr is trusted to avoid spoofed proxy headers.
func getClientIP(r *http.Request) string {
	// First try RemoteAddr (most reliable for local connections)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && ip != "" {
		return ip
	}

	return ""
}
