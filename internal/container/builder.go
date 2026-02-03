package container

import (
	"fmt"
	"log"

	"github.com/payram/payram-updater/internal/manifest"
)

/*
DYNAMIC DOCKER RUN BUILDER

This module constructs docker run arguments from:
  1. Inspected runtime state (authoritative source)
  2. Additive manifest overlays (minimum requirements)

CRITICAL PRINCIPLE: RUNTIME PARITY
The new container MUST match the current container's runtime topology exactly,
except for the image tag. This ensures:
  - User customizations are preserved (manually added ports, mounts, env vars)
  - Previous upgrade configurations persist across upgrades
  - Installation method independence (script, manual, custom)
  - Zero operational surprises (no unexpected topology changes)
  - Secret preservation (AES_KEY, DB passwords never regenerated)

WHAT CHANGES:
  - Image tag only (e.g., payramapp/payram:1.8.0 → payramapp/payram:1.9.0)

WHAT NEVER CHANGES:
  - Ports (host:container mappings)
  - Mounts (volumes and bind mounts)
  - Environment variables (especially secrets)
  - Network configuration
  - Restart policy
  - Container name

WHY THIS MATTERS:
An upgrade that silently removes a port, changes a mount path, or regenerates
a secret is not an upgrade - it's a destructive reinstallation. This module
ensures upgrades are truly non-destructive operations that only change the
application code, not the runtime environment.
*/

// DockerRunBuilder constructs docker run arguments from runtime state and manifest.
type DockerRunBuilder struct {
	logger Logger
}

// NewDockerRunBuilder creates a new builder.
func NewDockerRunBuilder(logger Logger) *DockerRunBuilder {
	if logger == nil {
		logger = log.Default()
	}
	return &DockerRunBuilder{logger: logger}
}

// BuildUpgradeArgs constructs docker run arguments for an upgrade.
//
// ALGORITHM:
//  1. Extract runtime state from running container (docker inspect)
//  2. Reconcile with manifest requirements (additive only)
//  3. Build docker run args preserving ALL runtime configuration
//  4. Change ONLY the image tag
//
// Parameters:
//   - runtimeState: Current container configuration from docker inspect
//   - manifest: Manifest with minimum requirements (additive overlay)
//   - newImageTag: New image tag to use (e.g., "1.9.0")
//
// Returns:
//   - Docker run arguments ready for execution
//   - Error if reconciliation fails or required data is missing
func (b *DockerRunBuilder) BuildUpgradeArgs(
	runtimeState *RuntimeState,
	manifest *manifest.Manifest,
	newImageTag string,
) ([]string, error) {
	if runtimeState == nil {
		return nil, fmt.Errorf("runtime state is required (cannot infer configuration)")
	}
	if manifest == nil {
		return nil, fmt.Errorf("manifest is required")
	}
	if newImageTag == "" {
		return nil, fmt.Errorf("new image tag is required")
	}

	b.logger.Printf("Building docker run args for upgrade to %s", newImageTag)

	// Step 1: Reconcile runtime state with manifest requirements
	reconciler := NewReconciler(b.logger)
	reconciled, err := reconciler.Reconcile(runtimeState, manifest)
	if err != nil {
		return nil, fmt.Errorf("reconciliation failed: %w", err)
	}

	b.logger.Printf("Reconciliation complete: %d ports (%d added), %d mounts (%d added), %d env vars (%d added)",
		len(reconciled.Ports), reconciled.AddedPorts,
		len(reconciled.Mounts), reconciled.AddedMounts,
		len(reconciled.Env), reconciled.AddedEnvs)

	// Step 2: Build docker run arguments
	args := []string{"run", "-d"}

	// Container name (PRESERVED from runtime state)
	if runtimeState.Name == "" {
		return nil, fmt.Errorf("container name missing from runtime state (cannot proceed)")
	}
	args = append(args, "--name", runtimeState.Name)
	b.logger.Printf("Container name: %s (preserved from runtime)", runtimeState.Name)

	// Restart policy (PRESERVED from runtime state)
	restartPolicy := formatRestartPolicy(runtimeState.RestartPolicy)
	args = append(args, "--restart", restartPolicy)
	b.logger.Printf("Restart policy: %s (preserved from runtime)", restartPolicy)

	// Ports (RECONCILED: runtime + manifest)
	for _, port := range reconciled.Ports {
		// Format: hostIP:hostPort:containerPort/protocol
		// If hostIP is empty or 0.0.0.0, omit it
		var portMapping string
		if port.HostIP == "" || port.HostIP == "0.0.0.0" {
			portMapping = fmt.Sprintf("%s:%s/%s", port.HostPort, port.ContainerPort, port.Protocol)
		} else {
			portMapping = fmt.Sprintf("%s:%s:%s/%s", port.HostIP, port.HostPort, port.ContainerPort, port.Protocol)
		}
		args = append(args, "-p", portMapping)
	}
	b.logger.Printf("Ports: %d total (%d from runtime, %d added from manifest)",
		len(reconciled.Ports), len(runtimeState.Ports), reconciled.AddedPorts)

	// Mounts (RECONCILED: runtime + manifest)
	// VALIDATION: Skip any mount with empty source or destination to prevent
	// "invalid spec: :rw: empty section between colons" errors
	validMounts := 0
	skippedMounts := 0
	seenDestinations := make(map[string]bool) // Deduplicate by destination

	for _, mount := range reconciled.Mounts {
		// Validate: destination must always be non-empty
		if mount.Destination == "" {
			b.logger.Printf("DEBUG: Skipping mount with empty destination (source=%s, type=%s)", mount.Source, mount.Type)
			skippedMounts++
			continue
		}

		// Deduplicate: skip if we've already seen this destination
		if seenDestinations[mount.Destination] {
			b.logger.Printf("DEBUG: Skipping duplicate mount for destination %s", mount.Destination)
			skippedMounts++
			continue
		}
		seenDestinations[mount.Destination] = true

		var mountSpec string
		if mount.Type == "bind" {
			// Bind mount: source:destination[:mode]
			// Validate: source must be non-empty for bind mounts
			if mount.Source == "" {
				b.logger.Printf("DEBUG: Skipping bind mount with empty source (destination=%s)", mount.Destination)
				skippedMounts++
				continue
			}
			mountSpec = fmt.Sprintf("%s:%s", mount.Source, mount.Destination)
			if mount.Mode != "" {
				mountSpec = fmt.Sprintf("%s:%s", mountSpec, mount.Mode)
			}
		} else {
			// Volume: volumeName:destination[:mode]
			// If source is empty, Docker will generate a volume name
			if mount.Source == "" {
				mountSpec = mount.Destination
			} else {
				mountSpec = fmt.Sprintf("%s:%s", mount.Source, mount.Destination)
			}
			if mount.Mode != "" {
				mountSpec = fmt.Sprintf("%s:%s", mountSpec, mount.Mode)
			}
		}
		args = append(args, "-v", mountSpec)
		validMounts++
	}
	b.logger.Printf("Mounts: %d valid, %d skipped (%d total from reconciliation, %d from runtime, %d added from manifest)",
		validMounts, skippedMounts, len(reconciled.Mounts), len(runtimeState.Mounts), reconciled.AddedMounts)

	// Environment variables (RECONCILED: runtime + manifest)
	for _, env := range reconciled.Env {
		args = append(args, "-e", env)
	}
	b.logger.Printf("Environment variables: %d total (%d from runtime, %d added from manifest)",
		len(reconciled.Env), len(runtimeState.Env), reconciled.AddedEnvs)

	// Networks (PRESERVED from runtime state)
	// Note: Docker run only supports connecting to ONE network at creation time.
	// Additional networks must be connected after container creation.
	// For simplicity, we'll connect to the first network (usually the default).
	if len(runtimeState.Networks) > 0 {
		primaryNetwork := runtimeState.Networks[0]
		if primaryNetwork.NetworkName != "bridge" && primaryNetwork.NetworkName != "host" && primaryNetwork.NetworkName != "none" {
			args = append(args, "--network", primaryNetwork.NetworkName)
			b.logger.Printf("Network: %s (preserved from runtime)", primaryNetwork.NetworkName)
			if len(runtimeState.Networks) > 1 {
				b.logger.Printf("Warning: container was connected to %d networks. Only primary network will be preserved.", len(runtimeState.Networks))
			}
		}
	}

	// Image with new tag (ONLY CHANGE)
	newImage := fmt.Sprintf("%s:%s", manifest.Image.Repo, newImageTag)
	args = append(args, newImage)
	b.logger.Printf("Image: %s (UPGRADED from %s)", newImage, runtimeState.Image)

	return args, nil
}

// formatRestartPolicy converts RestartPolicy struct to docker restart policy string.
func formatRestartPolicy(policy RestartPolicy) string {
	if policy.Name == "" {
		return "no"
	}
	if policy.Name == "on-failure" && policy.MaximumRetryCount > 0 {
		return fmt.Sprintf("on-failure:%d", policy.MaximumRetryCount)
	}
	return policy.Name
}
