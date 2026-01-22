package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const maxResponseSize = 1 * 1024 * 1024 // 1MB

var (
	ErrNon200Status   = errors.New("non-200 HTTP status")
	ErrResponseTooBig = errors.New("response exceeds 1MB limit")
	ErrInvalidJSON    = errors.New("invalid JSON response")
)

// Port represents a container port mapping.
type Port struct {
	Container int    `json:"container"`
	Host      int    `json:"host,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

// Volume represents a container volume mapping.
type Volume struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"readonly,omitempty"`
}

// Defaults represents default container configuration.
type Defaults struct {
	ContainerName string   `json:"container_name"`
	RestartPolicy string   `json:"restart_policy"`
	Ports         []Port   `json:"ports"`
	Volumes       []Volume `json:"volumes"`
}

// Override represents version-specific configuration overrides.
type Override struct {
	Version       string   `json:"version"`
	ContainerName string   `json:"container_name,omitempty"`
	RestartPolicy string   `json:"restart_policy,omitempty"`
	Ports         []Port   `json:"ports,omitempty"`
	Volumes       []Volume `json:"volumes,omitempty"`
}

// Image represents container image information.
type Image struct {
	Repo string `json:"repo"`
}

// Manifest represents the runtime manifest fetched from GitHub.
type Manifest struct {
	Image     Image      `json:"image"`
	Defaults  Defaults   `json:"defaults"`
	Overrides []Override `json:"overrides,omitempty"`
}

// Client is an HTTP client for fetching manifest data.
type Client struct {
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient creates a new manifest client with the specified timeout.
func NewClient(timeout time.Duration) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

// Fetch retrieves and parses the manifest from the given URL or local file path.
// Phase 1: Supports both HTTP(S) URLs and local filesystem paths.
// If the URL starts with "http://" or "https://", it is fetched via HTTP.
// Otherwise, it is treated as a local file path.
func (c *Client) Fetch(ctx context.Context, url string) (*Manifest, error) {
	var body []byte
	var err error

	// Check if this is an HTTP(S) URL or a local file path
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// HTTP fetch (existing behavior)
		body, err = c.fetchHTTP(ctx, url)
	} else {
		// Local file fetch (Phase 1 support)
		body, err = c.fetchLocal(url)
	}

	if err != nil {
		return nil, err
	}

	// Parse JSON with strict unmarshaling
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	return &manifest, nil
}

// fetchHTTP retrieves manifest data from an HTTP(S) URL.
func (c *Client) fetchHTTP(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: got %d", ErrNon200Status, resp.StatusCode)
	}

	// Limit response size to 1MB
	limitedReader := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if len(body) > maxResponseSize {
		return nil, ErrResponseTooBig
	}

	return body, nil
}

// fetchLocal retrieves manifest data from a local file path.
func (c *Client) fetchLocal(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read local manifest file: %w", err)
	}

	if len(body) > maxResponseSize {
		return nil, ErrResponseTooBig
	}

	return body, nil
}
