// Package container provides container discovery and runtime inspection.
package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
)

// DiscoveredContainer represents a discovered Payram container.
type DiscoveredContainer struct {
	ID        string // Full container ID
	Name      string // Container name
	ImageTag  string // Image tag (version)
	ImageFull string // Full image name (e.g., payramapp/payram:1.2.3)
}

// Discoverer handles dynamic container discovery.
type Discoverer struct {
	dockerBin    string
	imagePattern string // e.g., "payramapp/payram:" or "payram-dummy:"
	logger       Logger
}

// Logger defines the interface for logging.
type Logger interface {
	Printf(format string, v ...interface{})
}

// NewDiscoverer creates a new container discoverer.
// imagePattern is the image prefix to match (e.g., "payramapp/payram:" or "payram-dummy:").
// If empty, defaults to "payramapp/payram:".
func NewDiscoverer(dockerBin string, imagePattern string, logger Logger) *Discoverer {
	if imagePattern == "" {
		imagePattern = "payramapp/payram:"
	}
	return &Discoverer{
		dockerBin:    dockerBin,
		imagePattern: imagePattern,
		logger:       logger,
	}
}

// containerListEntry represents a single container from docker ps JSON output.
type containerListEntry struct {
	ID      string `json:"ID"`
	Names   string `json:"Names"`
	Image   string `json:"Image"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Created string `json:"CreatedAt"`
}

// DiscoverPayramContainer finds all running Payram containers and returns the one
// with the highest semantic version.
//
// Process:
// 1. Lists all running containers
// 2. Filters for images matching the configured image pattern (default: "payramapp/payram:*")
// 3. Parses image tags as semantic versions
// 4. Selects container with highest version
//
// Returns PAYRAM_CONTAINER_NOT_FOUND error if no Payram containers are found.
func (d *Discoverer) DiscoverPayramContainer(ctx context.Context) (*DiscoveredContainer, error) {
	d.logger.Printf("Discovering Payram containers...")

	// Create timeout context
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// List all running containers in JSON format
	cmd := exec.CommandContext(cmdCtx, d.dockerBin, "ps", "--format", "{{json .}}")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w: %s", err, string(output))
	}

	// Parse container list
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		d.logger.Printf("No running containers found")
		return nil, &DiscoveryError{
			FailureCode: "PAYRAM_CONTAINER_NOT_FOUND",
			Message:     "No Payram containers found (no running containers)",
		}
	}

	var candidates []DiscoveredContainer

	for _, line := range lines {
		if line == "" {
			continue
		}

		var entry containerListEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			d.logger.Printf("Warning: failed to parse container entry: %v", err)
			continue
		}

		// Filter for configured image pattern
		if !strings.HasPrefix(entry.Image, d.imagePattern) {
			continue
		}

		// Extract tag from image
		parts := strings.Split(entry.Image, ":")
		if len(parts) != 2 {
			d.logger.Printf("Warning: unexpected image format: %s", entry.Image)
			continue
		}

		tag := parts[1]

		// Skip "latest" tag - only accept semantic versions
		if tag == "latest" {
			d.logger.Printf("Skipping container with 'latest' tag: %s", entry.Names)
			continue
		}

		candidates = append(candidates, DiscoveredContainer{
			ID:        entry.ID,
			Name:      strings.TrimPrefix(entry.Names, "/"), // Docker prefixes names with /
			ImageTag:  tag,
			ImageFull: entry.Image,
		})
	}

	if len(candidates) == 0 {
		d.logger.Printf("No Payram containers found matching %s*", d.imagePattern)
		return nil, &DiscoveryError{
			FailureCode: "PAYRAM_CONTAINER_NOT_FOUND",
			Message:     fmt.Sprintf("No Payram containers found (no containers match image %s*)", d.imagePattern),
		}
	}

	// Parse versions and select highest
	highestContainer, err := selectHighestVersion(candidates)
	if err != nil {
		return nil, fmt.Errorf("failed to select highest version: %w", err)
	}

	// Safely truncate container ID for logging
	idDisplay := highestContainer.ID
	if len(idDisplay) > 12 {
		idDisplay = idDisplay[:12]
	}

	d.logger.Printf("Discovered Payram container: %s (ID: %s, Tag: %s)",
		highestContainer.Name, idDisplay, highestContainer.ImageTag)

	return highestContainer, nil
}

// selectHighestVersion parses semantic versions and returns the container with the highest version.
func selectHighestVersion(candidates []DiscoveredContainer) (*DiscoveredContainer, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates provided")
	}

	// Single candidate - return it
	if len(candidates) == 1 {
		return &candidates[0], nil
	}

	// Parse versions
	type versionedCandidate struct {
		container DiscoveredContainer
		version   *version.Version
	}

	var versioned []versionedCandidate

	for _, candidate := range candidates {
		v, err := version.NewVersion(candidate.ImageTag)
		if err != nil {
			// Skip containers with non-semantic version tags
			continue
		}

		versioned = append(versioned, versionedCandidate{
			container: candidate,
			version:   v,
		})
	}

	if len(versioned) == 0 {
		return nil, &DiscoveryError{
			FailureCode: "PAYRAM_VERSION_PARSE_FAILED",
			Message:     "No Payram containers have valid semantic version tags",
		}
	}

	// Sort by version (descending - highest first)
	sort.Slice(versioned, func(i, j int) bool {
		return versioned[i].version.GreaterThan(versioned[j].version)
	})

	// Return highest version
	return &versioned[0].container, nil
}

// DiscoveryError represents a container discovery error.
type DiscoveryError struct {
	FailureCode string
	Message     string
}

func (e *DiscoveryError) Error() string {
	return fmt.Sprintf("%s: %s", e.FailureCode, e.Message)
}
