package network

import (
	"log"
	"net"
	"net/http"
)

// AllowedIPsMiddleware creates middleware that restricts access to specific IP addresses.
// This ensures only localhost and the Payram container can access the updater API.
func AllowedIPsMiddleware(allowedIPs []string, logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientIP := getClientIP(r)

			// Check if client IP is in the allowed list
			allowed := false
			for _, allowedIP := range allowedIPs {
				if clientIP == allowedIP {
					allowed = true
					break
				}
			}

			if !allowed {
				logger.Printf("ACCESS DENIED: Request from unauthorized IP %s to %s %s", clientIP, r.Method, r.URL.Path)
				http.Error(w, "Access forbidden: unauthorized source IP", http.StatusForbidden)
				return
			}

			// IP is allowed, continue to the handler
			next.ServeHTTP(w, r)
		})
	}
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
