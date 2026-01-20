package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// Fetch retrieves and parses the policy from the given URL.
func (c *Client) Fetch(ctx context.Context, url string) (*Policy, error) {
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

	var policy Policy
	if err := json.Unmarshal(body, &policy); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	return &policy, nil
}
