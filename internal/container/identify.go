// Package container provides port and service identification.
package container

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// HealthPath is the endpoint used to identify Payram Core on a port.
	HealthPath = "/api/v1/health"

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
// 2. Sends a GET request to <scheme>://127.0.0.1:<port>/api/v1/health
// 3. Checks the response is 200 with a JSON body containing a "status" field
// 4. Returns the first port that matches
//
// Returns PAYRAM_CORE_PORT_NOT_FOUND error if no port responds as Payram Core.
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
	p.logger.Printf("No port responded as Payram Core health")
	return nil, &IdentificationError{
		FailureCode: "PAYRAM_CORE_PORT_NOT_FOUND",
		Message:     fmt.Sprintf("No port responded on %s", HealthPath),
	}
}

// checkPort checks if a specific port is running Payram Core.
// It tries HTTPS first, then HTTP (with TLS verification disabled for localhost).
// HTTPS is tried first because new installs serve the API over HTTPS and
// redirect plain HTTP to it.
// Returns the working scheme ("http" or "https") and true if found.
func (p *PortIdentifier) checkPort(ctx context.Context, hostPort string) (string, bool) {
	if p.checkScheme(ctx, "https", hostPort, p.httpsClient) {
		return "https", true
	}
	if p.checkScheme(ctx, "http", hostPort, p.httpClient) {
		return "http", true
	}
	return "", false
}

// checkScheme probes the Payram Core health endpoint on the given scheme+port
// and returns true if it responds with 200 and a JSON body containing a
// "status" field. This identifies Payram Core across all versions, since the
// health endpoint is served on the same port as the rest of the API.
func (p *PortIdentifier) checkScheme(ctx context.Context, scheme, hostPort string, client *http.Client) bool {
	url := fmt.Sprintf("%s://127.0.0.1:%s%s", scheme, hostPort, HealthPath)

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

	if resp.StatusCode != http.StatusOK {
		p.logger.Printf("Port %s - %s health returned status %d", hostPort, scheme, resp.StatusCode)
		return false
	}

	// Read response body (limit to 10KB to prevent memory issues)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024))
	if err != nil {
		p.logger.Printf("Port %s - failed to read %s response: %v", hostPort, scheme, err)
		return false
	}

	var probe struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Status != "" {
		p.logger.Printf("Port %s - identified Payram Core health via %s", hostPort, scheme)
		return true
	}

	p.logger.Printf("Port %s - %s response is not a Payram Core health response", hostPort, scheme)
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
