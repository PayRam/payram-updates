package coreclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultTimeout is the default timeout for HTTP requests.
	DefaultTimeout = 3 * time.Second
	// MaxResponseSize is the maximum response body size (1MB).
	MaxResponseSize = 1 * 1024 * 1024
)

// Client is an HTTP client for communicating with payram-core API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// HealthResponse represents the response from the /health endpoint.
// Note: This struct intentionally only captures fields we care about.
// The health endpoint may return additional fields that we ignore,
// allowing payram-core to evolve its health response without breaking the updater.
// Required fields: status == "ok" for a healthy state.
// Optional fields: db (if present, must be "ok" for healthy state).
type HealthResponse struct {
	Status string `json:"status"`
	DB     string `json:"db,omitempty"`
}

// VersionResponse represents the response from the /version endpoint.
// Note: This struct only captures the version field we need.
// Additional fields like "build" and "image" are ignored.
type VersionResponse struct {
	Version string `json:"version"`
}

// MigrationsStatusResponse represents the response from the /admin/migrations/status endpoint.
// The state field indicates migration status:
//   - "complete" = migrations finished successfully
//   - "running" = migrations currently in progress (caller should wait)
//   - "failed" = migrations failed (requires manual recovery)
type MigrationsStatusResponse struct {
	State string `json:"state"`
}

// NewClient creates a new core API client with default timeout.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTPClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}
}

// Health checks the health status of payram-core.
// The health endpoint response is parsed leniently - unknown fields are ignored.
// This allows payram-core to add new fields without breaking the updater.
// Required: status == "ok" for a healthy state.
// Optional: db (if present, must be "ok" for healthy state).
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	url := c.BaseURL + "/health"
	var response HealthResponse
	if err := c.doRequestLenient(ctx, url, &response); err != nil {
		return nil, fmt.Errorf("health check failed: %w", err)
	}
	return &response, nil
}

// Version retrieves the current version of payram-core.
// The response is parsed leniently - only the "version" field is captured.
// Additional fields like "build" and "image" are ignored.
func (c *Client) Version(ctx context.Context) (*VersionResponse, error) {
	url := c.BaseURL + "/version"
	var response VersionResponse
	if err := c.doRequestLenient(ctx, url, &response); err != nil {
		return nil, fmt.Errorf("version check failed: %w", err)
	}
	return &response, nil
}

// MigrationsStatus retrieves the database migrations status.
// The response is parsed leniently - only the "state" field is captured.
func (c *Client) MigrationsStatus(ctx context.Context) (*MigrationsStatusResponse, error) {
	url := c.BaseURL + "/admin/migrations/status"
	var response MigrationsStatusResponse
	if err := c.doRequestLenient(ctx, url, &response); err != nil {
		return nil, fmt.Errorf("migrations status check failed: %w", err)
	}
	return &response, nil
}

// doRequest performs an HTTP GET request and decodes the JSON response strictly.
// Unknown fields in the JSON response will cause an error.
func (c *Client) doRequest(ctx context.Context, url string, target interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	// Limit response size
	limitedReader := io.LimitReader(resp.Body, MaxResponseSize)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse JSON strictly
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("failed to decode JSON response: %w (body: %s)", err, string(body))
	}

	return nil
}

// doRequestLenient performs an HTTP GET request and decodes the JSON response leniently.
// Unknown fields in the JSON response are ignored, allowing the remote service
// to evolve its API without breaking the client. Use this for endpoints like /health
// where we only care about specific fields and want to be resilient to schema changes.
func (c *Client) doRequestLenient(ctx context.Context, url string, target interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	// Limit response size
	limitedReader := io.LimitReader(resp.Body, MaxResponseSize)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse JSON leniently - unknown fields are ignored
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("failed to decode JSON response: %w (body: %s)", err, string(body))
	}

	return nil
}
