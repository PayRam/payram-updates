package manifest

import (
	"fmt"
)

// BuildDockerRunArgs constructs docker run arguments from a manifest.
// Returns a slice of arguments suitable for passing to docker run command.
//
// STATELESS DESIGN: All configuration comes from the manifest.
// Runtime inspection of the existing container is the source of truth.
// No persisted installation config or script-based assumptions are used.
func BuildDockerRunArgs(manifest *Manifest, resolvedTag string) ([]string, error) {
	args := []string{"run", "-d"}

	// Container name from manifest
	containerName := manifest.Defaults.ContainerName
	if containerName == "" {
		containerName = "payram" // fallback default
	}
	args = append(args, "--name", containerName)

	// Restart policy (from manifest defaults or fallback to "no")
	restartPolicy := manifest.Defaults.RestartPolicy
	if restartPolicy == "" {
		restartPolicy = "no"
	}
	args = append(args, "--restart", restartPolicy)

	// Port mappings from manifest
	for _, port := range manifest.Defaults.Ports {
		portMapping := fmt.Sprintf("%d:%d", port.Host, port.Container)
		if port.Protocol != "" {
			portMapping = fmt.Sprintf("%d:%d/%s", port.Host, port.Container, port.Protocol)
		}
		args = append(args, "-p", portMapping)
	}

	// Volume mappings from manifest (only if volumes are defined)
	// VALIDATION: Skip any volume with empty source or destination to prevent
	// "invalid spec: :rw: empty section between colons" errors
	for _, volume := range manifest.Defaults.Volumes {
		// Skip if source or destination is empty
		if volume.Source == "" || volume.Destination == "" {
			continue // Silently skip invalid volumes
		}
		volumeMapping := fmt.Sprintf("%s:%s", volume.Source, volume.Destination)
		if volume.ReadOnly {
			volumeMapping = fmt.Sprintf("%s:%s:ro", volume.Source, volume.Destination)
		}
		args = append(args, "-v", volumeMapping)
	}

	// Image name with resolved tag
	imageName := fmt.Sprintf("%s:%s", manifest.Image.Repo, resolvedTag)
	args = append(args, imageName)

	return args, nil
}
