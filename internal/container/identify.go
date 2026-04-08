// Package container provides port and service identification.
package container

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// PayramCoreWelcomeMessage is the string that identifies Payram Core on the root endpoint
	PayramCoreWelcomeMessage = "Welcome to Payram Core"

	// PortIdentificationTimeout is the timeout for checking each port
	PortIdentificationTimeout = 3 * time.Second
)

// IdentifiedPort represents a successfully identified Payram Core port.
type IdentifiedPort struct {
	HostPort      string // The host port (e.g., "8080")
	ContainerPort string // The container port (e.g., "80")
	Protocol      string // The protocol (e.g., "tcp")
	Scheme        string // The URL scheme ("http" or "https")
}

// PortIdentifier handles identification of Payram Core service ports.
type PortIdentifier struct {
	httpClient  *http.Client
	httpsClient *http.Client
	logger      Logger
}

// NewPortIdentifier creates a new port identifier.
func NewPortIdentifier(logger Logger) *PortIdentifier {
	noFollow := func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &PortIdentifier{
		httpClient: &http.Client{
			Timeout: PortIdentificationTimeout,
			// Don't follow redirects - we want to check the root endpoint directly
			CheckRedirect: noFollow,
		},
		// httpsClient skips TLS verification because the container's certificate
		// may be self-signed or not include 127.0.0.1 as a SAN.  This is
		// acceptable here since we only ever probe loopback addresses.
		httpsClient: &http.Client{
			Timeout:       PortIdentificationTimeout,
			CheckRedirect: noFollow,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		},
		logger: logger,
	}
}

// IdentifyPayramCorePort identifies which exposed port is running Payram Core.
//
// Process:
// 1. Iterates through all exposed host ports from RuntimeState
// 2. Sends HTTP GET request to http://localhost:<port>/
// 3. Checks if response contains "Welcome to Payram Core"
// 4. Returns the first port that matches
//
// Returns PAYRAM_CORE_PORT_NOT_FOUND error if no port responds with the welcome message.
func (p *PortIdentifier) IdentifyPayramCorePort(ctx context.Context, state *RuntimeState) (*IdentifiedPort, error) {
	if state == nil {
		return nil, fmt.Errorf("runtime state is nil")
	}

	if len(state.Ports) == 0 {
		p.logger.Printf("No ports exposed in container")
		return nil, &IdentificationError{
			FailureCode: "PAYRAM_CORE_PORT_NOT_FOUND",
			Message:     "No ports exposed in container",
		}
	}

	p.logger.Printf("Identifying Payram Core port among %d exposed ports", len(state.Ports))

	// Try each exposed port
	for _, portMapping := range state.Ports {
		// Skip non-TCP ports
		if portMapping.Protocol != "tcp" && portMapping.Protocol != "" {
			p.logger.Printf("Skipping non-TCP port: %s/%s", portMapping.HostPort, portMapping.Protocol)
			continue
		}

		if portMapping.HostPort == "" {
			p.logger.Printf("Skipping port with empty host port")
			continue
		}

		p.logger.Printf("Checking port %s...", portMapping.HostPort)

		if scheme, ok := p.checkPort(ctx, portMapping.HostPort); ok {
			p.logger.Printf("Identified Payram Core on port %s (scheme: %s)", portMapping.HostPort, scheme)
			return &IdentifiedPort{
				HostPort:      portMapping.HostPort,
				ContainerPort: portMapping.ContainerPort,
				Protocol:      portMapping.Protocol,
				Scheme:        scheme,
			}, nil
		}
	}

	// No port matched
	p.logger.Printf("No port responded with Payram Core welcome message")
	return nil, &IdentificationError{
		FailureCode: "PAYRAM_CORE_PORT_NOT_FOUND",
		Message:     fmt.Sprintf("No port responded with '%s' message", PayramCoreWelcomeMessage),
	}
}

// checkPort checks if a specific port is running Payram Core.
// It tries HTTP first, then HTTPS (with TLS verification disabled for localhost).
// Returns the working scheme ("http" or "https") and true if found.
func (p *PortIdentifier) checkPort(ctx context.Context, hostPort string) (string, bool) {
	if p.checkScheme(ctx, "http", hostPort, p.httpClient) {
		return "http", true
	}
	if p.checkScheme(ctx, "https", hostPort, p.httpsClient) {
		return "https", true
	}
	return "", false
}

// checkScheme performs a single HTTP/HTTPS probe against the root path and
// returns true if the response contains the Payram Core welcome message.
func (p *PortIdentifier) checkScheme(ctx context.Context, scheme, hostPort string, client *http.Client) bool {
	url := fmt.Sprintf("%s://localhost:%s/", scheme, hostPort)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		p.logger.Printf("Failed to create %s request for port %s: %v", scheme, hostPort, err)
		return false
	}

	resp, err := client.Do(req)
	if err != nil {
		p.logger.Printf("Port %s not responding on %s: %v", hostPort, scheme, err)
		return false
	}
	defer resp.Body.Close()

	// Read response body (limit to 10KB to prevent memory issues)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024))
	if err != nil {
		p.logger.Printf("Port %s - failed to read %s response: %v", hostPort, scheme, err)
		return false
	}

	if strings.Contains(string(body), PayramCoreWelcomeMessage) {
		p.logger.Printf("Port %s - found Payram Core welcome message via %s", hostPort, scheme)
		return true
	}

	p.logger.Printf("Port %s - %s response does not contain welcome message", hostPort, scheme)
	return false
}

// IdentificationError represents a port identification error.
type IdentificationError struct {
	FailureCode string
	Message     string
}

func (e *IdentificationError) Error() string {
	return fmt.Sprintf("%s: %s", e.FailureCode, e.Message)
}
