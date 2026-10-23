package container

import (
	"fmt"
	"strings"

	"github.com/payram/payram-updater/internal/logger"
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
  - Ports (host:container mappings), except the bundled database's (below)
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

// databaseContainerPort is the bundled PostgreSQL's port inside the container,
// the one runtime setting an upgrade does not carry over. Installs made before
// 28 May 2026 published it on every interface, where older images accepted the
// shared default database password from anywhere. Nothing outside the container
// needs it: backups and restores run through docker exec. setup_payram.sh drops
// it on updates too.
const databaseContainerPort = "5432"

// DockerRunBuilder constructs docker run arguments from runtime state and manifest.
type DockerRunBuilder struct {
	logger Logger
}

// NewDockerRunBuilder creates a new builder.
func NewDockerRunBuilder(logSink Logger) *DockerRunBuilder {
	if logSink == nil {
		logSink = logger.StdLogger()
	}
	return &DockerRunBuilder{logger: logSink}
}

// BuildUpgradeArgs constructs `docker run -d --name <name> ...` arguments for
// an upgrade.
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
	spec, err := b.BuildContainerSpec(runtimeState, manifest, newImageTag)
	if err != nil {
		return nil, err
	}
	return append([]string{"run", "-d", "--name", ContainerName(runtimeState.Name)}, spec.Options...), nil
}

// ContainerSpec is everything needed to recreate a container under any name:
// the options for `docker create`/`docker run` (ending with the image), and
// the networks to attach after creation (docker can only attach one at create).
type ContainerSpec struct {
	Options       []string
	ExtraNetworks []string
	Image         string
}

// BuildCreateArgs constructs `docker create --name <name> ...` arguments, so
// the new version's container can be created (and its image, mounts and
// network checked) while the current one keeps running.
func (b *DockerRunBuilder) BuildCreateArgs(spec *ContainerSpec, name string) []string {
	return append([]string{"create", "--name", name}, spec.Options...)
}

// BuildContainerSpec reconciles the runtime state with the manifest and builds
// the container options. The only intended change is the image tag; the
// bundled database's port is the one setting deliberately not carried over.
func (b *DockerRunBuilder) BuildContainerSpec(
	runtimeState *RuntimeState,
	manifest *manifest.Manifest,
	newImageTag string,
) (*ContainerSpec, error) {
	if runtimeState == nil {
		return nil, fmt.Errorf("runtime state is required (cannot infer configuration)")
	}
	if manifest == nil {
		return nil, fmt.Errorf("manifest is required")
	}
	if newImageTag == "" {
		return nil, fmt.Errorf("new image tag is required")
	}
	if runtimeState.Name == "" {
		return nil, fmt.Errorf("container name missing from runtime state (cannot proceed)")
	}

	b.logger.Printf("Building container settings for upgrade to %s", newImageTag)

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

	var args []string

	// Restart policy (PRESERVED from runtime state)
	restartPolicy := FormatRestartPolicy(runtimeState.RestartPolicy)
	args = append(args, "--restart", restartPolicy)
	b.logger.Printf("Restart policy: %s (preserved from runtime)", restartPolicy)

	// Network mode first: a host-network container publishes no ports.
	hostNetwork := runtimeState.NetworkMode == "host"

	// Ports (RECONCILED: runtime + manifest), without the database's
	published := 0
	for _, port := range reconciled.Ports {
		if hostNetwork {
			break
		}
		if port.ContainerPort == databaseContainerPort {
			b.logger.Printf("Not publishing %s:%s/%s: the PayRam database is only reachable from inside the container",
				port.HostPort, port.ContainerPort, port.Protocol)
			continue
		}
		published++
		args = append(args, "-p", formatPortMapping(port))
	}
	b.logger.Printf("Ports: %d published (%d from runtime, %d added from manifest)",
		published, len(runtimeState.Ports), reconciled.AddedPorts)

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
		} else {
			// Volume: name:destination. Use the volume's NAME, not the host
			// path Docker keeps it at: mounting that path would turn the
			// volume into a bind mount (and on Docker Desktop the path only
			// exists inside Docker's VM).
			volume := mount.Name
			if volume == "" {
				volume = mount.Source
			}
			if volume == "" {
				mountSpec = mount.Destination
			} else {
				mountSpec = fmt.Sprintf("%s:%s", volume, mount.Destination)
			}
		}
		if mount.Mode != "" {
			mountSpec = fmt.Sprintf("%s:%s", mountSpec, mount.Mode)
		}
		args = append(args, "-v", mountSpec)
		validMounts++
	}
	b.logger.Printf("Mounts: %d valid, %d skipped (%d total from reconciliation, %d from runtime, %d added from manifest)",
		validMounts, skippedMounts, len(reconciled.Mounts), len(runtimeState.Mounts), reconciled.AddedMounts)

	// Environment variables (RECONCILED: runtime + manifest), minus the old
	// image's own defaults so the new image's defaults apply.
	imageDefaults := make(map[string]bool, len(runtimeState.ImageEnv))
	for _, env := range runtimeState.ImageEnv {
		imageDefaults[env] = true
	}
	passedEnv := 0
	for _, env := range reconciled.Env {
		if imageDefaults[env] {
			continue
		}
		args = append(args, "-e", env)
		passedEnv++
	}
	b.logger.Printf("Environment variables: %d passed (%d from runtime, %d added from manifest, %d left to the new image's defaults)",
		passedEnv, len(runtimeState.Env), reconciled.AddedEnvs, len(reconciled.Env)-passedEnv)

	// Networks (PRESERVED from runtime state). docker create attaches one
	// network; the others are connected right after.
	var extraNetworks []string
	switch mode := runtimeState.NetworkMode; {
	case mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:"):
		args = append(args, "--network", mode)
		b.logger.Printf("Network mode: %s (preserved from runtime)", mode)
	default:
		primary := mode
		if primary == "" && len(runtimeState.Networks) > 0 {
			primary = runtimeState.Networks[0].NetworkName
		}
		if primary == "" || primary == "default" {
			primary = "bridge"
		}
		if primary != "bridge" {
			args = append(args, "--network", primary)
		}
		for _, network := range runtimeState.Networks {
			if network.NetworkName != primary {
				extraNetworks = append(extraNetworks, network.NetworkName)
			}
		}
		b.logger.Printf("Network: %s (preserved from runtime), %d more to connect after create", primary, len(extraNetworks))
	}

	// Image with new tag (ONLY CHANGE)
	newImage := fmt.Sprintf("%s:%s", manifest.Image.Repo, newImageTag)
	args = append(args, newImage)
	b.logger.Printf("Image: %s (UPGRADED from %s)", newImage, runtimeState.Image)

	return &ContainerSpec{Options: args, ExtraNetworks: extraNetworks, Image: newImage}, nil
}

// ContainerName returns a container name without docker inspect's leading "/".
func ContainerName(name string) string {
	return strings.TrimPrefix(name, "/")
}

// formatPortMapping renders hostIP:hostPort:containerPort/protocol, omitting an
// all-interfaces host IP and bracketing IPv6 host IPs ("[::1]:80:80/tcp").
func formatPortMapping(port PortMapping) string {
	switch {
	case port.HostIP == "" || port.HostIP == "0.0.0.0" || port.HostIP == "::":
		return fmt.Sprintf("%s:%s/%s", port.HostPort, port.ContainerPort, port.Protocol)
	case strings.Contains(port.HostIP, ":"):
		return fmt.Sprintf("[%s]:%s:%s/%s", port.HostIP, port.HostPort, port.ContainerPort, port.Protocol)
	default:
		return fmt.Sprintf("%s:%s:%s/%s", port.HostIP, port.HostPort, port.ContainerPort, port.Protocol)
	}
}

// FormatRestartPolicy converts RestartPolicy struct to docker restart policy string.
func FormatRestartPolicy(policy RestartPolicy) string {
	if policy.Name == "" {
		return "no"
	}
	if policy.Name == "on-failure" && policy.MaximumRetryCount > 0 {
		return fmt.Sprintf("on-failure:%d", policy.MaximumRetryCount)
	}
	return policy.Name
}
