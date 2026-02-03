// Package container provides container name resolution and validation.
package container

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/manifest"
)

// ResolutionSource indicates where the container name was resolved from.
type ResolutionSource string

const (
	SourceEnv      ResolutionSource = "env"
	SourceManifest ResolutionSource = "manifest"
)

// ResolvedContainer contains the resolved container name and its source.
type ResolvedContainer struct {
	Name   string
	Source ResolutionSource
}

// Resolver handles container name resolution.
type Resolver struct {
	envContainerName string
	dockerBin        string
	logger           *log.Logger
}

// NewResolver creates a new container resolver.
// envContainerName is the value from TARGET_CONTAINER_NAME environment variable.
func NewResolver(envContainerName string, dockerBin string, logger *log.Logger) *Resolver {
	if logger == nil {
		logger = log.Default()
	}
	return &Resolver{
		envContainerName: envContainerName,
		dockerBin:        dockerBin,
		logger:           logger,
	}
}

// Resolve determines the target container name using the following resolution order:
//  1. If env TARGET_CONTAINER_NAME is set and non-empty, use it
//  2. Else if runtime manifest specifies a container name, use manifest.container_name
//  3. Else fail with CONTAINER_NAME_UNRESOLVED
//
// Returns the resolved container info or an error with failure code.
func (r *Resolver) Resolve(manifestData *manifest.Manifest) (*ResolvedContainer, error) {
	// Priority 1: Environment variable
	if r.envContainerName != "" {
		r.logger.Printf("Using target container from env: %s", r.envContainerName)
		return &ResolvedContainer{
			Name:   r.envContainerName,
			Source: SourceEnv,
		}, nil
	}

	// Priority 2: Manifest
	if manifestData != nil && manifestData.Defaults.ContainerName != "" {
		r.logger.Printf("Using target container from manifest: %s", manifestData.Defaults.ContainerName)
		return &ResolvedContainer{
			Name:   manifestData.Defaults.ContainerName,
			Source: SourceManifest,
		}, nil
	}

	// Priority 3: Fail - no container name available
	return nil, &ResolutionError{
		FailureCode: "CONTAINER_NAME_UNRESOLVED",
		Message:     "Target container name not specified (env TARGET_CONTAINER_NAME or manifest.container_name required)",
	}
}

// ValidateExists verifies that the container exists using docker inspect.
// Returns nil if the container exists, or an error with CONTAINER_NOT_FOUND failure code.
func (r *Resolver) ValidateExists(ctx context.Context, containerName string) error {
	r.logger.Printf("Checking container %s exists...", containerName)

	// Create a timeout context for the docker inspect command
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, r.dockerBin, "inspect", "--format", "{{.State.Status}}", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if it's specifically a "not found" error
		outputStr := strings.ToLower(string(output))
		if strings.Contains(outputStr, "no such") || strings.Contains(outputStr, "not found") {
			return &ResolutionError{
				FailureCode: "CONTAINER_NOT_FOUND",
				Message:     fmt.Sprintf("Container '%s' not found", containerName),
			}
		}
		// Generic docker error
		return &ResolutionError{
			FailureCode: "CONTAINER_NOT_FOUND",
			Message:     fmt.Sprintf("Failed to inspect container '%s': %v", containerName, err),
		}
	}

	status := strings.TrimSpace(string(output))
	r.logger.Printf("Container %s status: %s", containerName, status)
	return nil
}

// ResolveAndValidate performs both resolution and existence validation.
// This is a convenience method that combines Resolve and ValidateExists.
func (r *Resolver) ResolveAndValidate(ctx context.Context, manifestData *manifest.Manifest) (*ResolvedContainer, error) {
	resolved, err := r.Resolve(manifestData)
	if err != nil {
		return nil, err
	}

	if err := r.ValidateExists(ctx, resolved.Name); err != nil {
		return nil, err
	}

	return resolved, nil
}

// ResolutionError represents a container resolution failure with a specific failure code.
type ResolutionError struct {
	FailureCode string
	Message     string
}

func (e *ResolutionError) Error() string {
	return e.Message
}

// GetFailureCode returns the failure code for this error.
func (e *ResolutionError) GetFailureCode() string {
	return e.FailureCode
}
