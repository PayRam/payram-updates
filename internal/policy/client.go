package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Breakpoint represents a version that requires an automatic stepping-stone
// upgrade: the dashboard will stop at the highest release below this version,
// then on the next run advance through it automatically. No SSH needed.
type Breakpoint struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
	Docs    string `json:"docs"`
}

// StopPoint represents a version that requires a manual SSH upgrade.
// The dashboard will stop at the highest release below this version and
// return MANUAL_UPGRADE_REQUIRED — the operator must SSH and upgrade
// through this version before the dashboard can continue.
type StopPoint struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
	Docs    string `json:"docs"`
}

// Policy represents the update policy fetched from GitHub.
type Policy struct {
	Latest                string            `json:"latest"`
	UpdaterAPIInitVersion string            `json:"updater_api_init_version"`
	Releases              []string          `json:"releases"`
	Breakpoints           []Breakpoint      `json:"breakpoints"`
	StopPoints            []StopPoint       `json:"stop_points"`
	ArchSupport           map[string]string `json:"arch_support,omitempty"` // e.g. {"arm64": "1.9.1"}
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
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return checkFetchURL(req.URL)
			},
		},
		timeout: timeout,
	}
}

// checkFetchURL allows https, and plain http only to this machine (local test
// servers). The policy decides which image and version every merchant's next
// update runs, so it must not be readable or replaceable on the way.
func checkFetchURL(u *url.URL) error {
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" {
		switch u.Hostname() {
		case "127.0.0.1", "localhost", "::1":
			return nil
		}
	}
	return fmt.Errorf("refusing to fetch the policy from %s: only https:// addresses are allowed", u.Redacted())
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
	if err := checkFetchURL(req.URL); err != nil {
		return nil, err
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
