package policy

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

// Breakpoint represents a version breakpoint in the policy.
type Breakpoint struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
	Docs    string `json:"docs"`
}

// Policy represents the update policy fetched from GitHub.
type Policy struct {
	Latest      string       `json:"latest"`
	Releases    []string     `json:"releases"`
	Breakpoints []Breakpoint `json:"breakpoints"`
}

// Client is an HTTP client for fetching policy data.
type Client struct {
	httpClient *http.Client
	timeout    time.Duration
}

// NewClient creates a new policy client with the specified timeout.
func NewClient(timeout time.Duration) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: timeout,
		},
		timeout: timeout,
	}
}

// Fetch retrieves and parses the policy from the given URL or local file path.
// Local file support is provided for development and testing.
// If the URL starts with "http://" or "https://", it is fetched via HTTP.
// Otherwise, it is treated as a local file path.
func (c *Client) Fetch(ctx context.Context, url string) (*Policy, error) {
	var body []byte
	var err error

	// Check if this is an HTTP(S) URL or a local file path
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		// HTTP fetch (production)
		body, err = c.fetchHTTP(ctx, url)
	} else {
		// Local file fetch (development/testing)
		body, err = c.fetchLocal(url)
	}

	if err != nil {
		return nil, err
	}

	// Parse JSON with strict unmarshaling
	var policy Policy
	if err := json.Unmarshal(body, &policy); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	return &policy, nil
}

// fetchHTTP retrieves policy data from an HTTP(S) URL.
func (c *Client) fetchHTTP(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch policy: %w", err)
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

// fetchLocal retrieves policy data from a local file path.
func (c *Client) fetchLocal(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read local policy file: %w", err)
	}

	if len(body) > maxResponseSize {
		return nil, ErrResponseTooBig
	}

	return body, nil
}
