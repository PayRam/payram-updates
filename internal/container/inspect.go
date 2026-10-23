// Package container provides runtime inspection of Docker containers.
package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// RuntimeState represents the complete runtime configuration of a container
// as observed via docker inspect. This is the source of truth for how a
// container is actually running.
type RuntimeState struct {
	// Container identification
	ID   string
	Name string

	// Image information
	Image    string // Full image name
	ImageTag string // Parsed tag

	// Port mappings (host:container)
	Ports []PortMapping

	// Mounts (volumes and bind mounts)
	Mounts []Mount

	// Environment variables
	Env []string

	// ImageEnv is the environment baked into the image the container was
	// created from (its Dockerfile ENV lines). Docker copies these into the
	// container's Env, so they are told apart here: an update must take the
	// new image's defaults, not pin the old image's (e.g. a PATH that names an
	// older Postgres). Empty when the image could not be inspected.
	ImageEnv []string

	// NetworkMode is HostConfig.NetworkMode: "bridge", "host", "none", a
	// user-defined network name, or "container:<id>".
	NetworkMode string

	// Image labels
	Labels map[string]string

	// Network configuration
	Networks []NetworkConfig

	// Restart policy
	RestartPolicy RestartPolicy
}

// PortMapping represents a port mapping from host to container.
type PortMapping struct {
	HostIP        string // Host IP (e.g., "0.0.0.0")
	HostPort      string // Host port (e.g., "8080")
	ContainerPort string // Container port (e.g., "80")
	Protocol      string // Protocol (tcp, udp)
}

// Mount represents a volume or bind mount.
type Mount struct {
	Type        string // "bind" or "volume"
	Name        string // Volume name (volumes only)
	Source      string // Host path (for a volume: where Docker keeps it)
	Destination string // Container path
	Mode        string // Mount options (e.g., "rw", "ro")
	RW          bool   // Read-write flag
}

// NetworkConfig represents network configuration.
type NetworkConfig struct {
	NetworkName string
	IPAddress   string
	Gateway     string
	MacAddress  string
}

// RestartPolicy represents the container restart policy.
type RestartPolicy struct {
	Name              string // "no", "always", "on-failure", "unless-stopped"
	MaximumRetryCount int    // For "on-failure" policy
}

// Inspector handles runtime inspection of containers.
type Inspector struct {
	dockerBin string
	logger    Logger
}

// NewInspector creates a new runtime inspector.
func NewInspector(dockerBin string, logger Logger) *Inspector {
	return &Inspector{
		dockerBin: dockerBin,
		logger:    logger,
	}
}

// dockerInspectOutput represents the JSON structure from docker inspect.
type dockerInspectOutput struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode   string `json:"NetworkMode"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress  string `json:"IPAddress"`
			Gateway    string `json:"Gateway"`
			MacAddress string `json:"MacAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ExtractRuntimeState inspects a container and extracts its full runtime configuration.
//
// This function uses `docker inspect` to retrieve:
// - Exposed ports (host ↔ container mappings)
// - Mounts (bind mounts and volumes)
// - Environment variables
// - Network configuration
// - Restart policy
//
// Returns a RuntimeState struct representing the container exactly as it is running.
func (i *Inspector) ExtractRuntimeState(ctx context.Context, containerNameOrID string) (*RuntimeState, error) {
	i.logger.Printf("Extracting runtime state for container: %s", containerNameOrID)

	// Create timeout context
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Execute docker inspect
	cmd := exec.CommandContext(cmdCtx, i.dockerBin, "inspect", containerNameOrID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker inspect failed: %w: %s", err, string(output))
	}

	// Parse JSON output (docker inspect returns an array)
	var inspectData []dockerInspectOutput
	if err := json.Unmarshal(output, &inspectData); err != nil {
		return nil, fmt.Errorf("failed to parse docker inspect output: %w", err)
	}

	if len(inspectData) == 0 {
		return nil, fmt.Errorf("docker inspect returned no data")
	}

	data := inspectData[0]

	// Build RuntimeState
	state := &RuntimeState{
		ID:     data.ID,
		Name:   data.Name,
		Image:  data.Config.Image,
		Env:    data.Config.Env,
		Labels: data.Config.Labels,
	}

	// Parse image tag
	if imageParts := parseImageTag(data.Config.Image); imageParts != nil {
		state.ImageTag = imageParts["tag"]
	}

	// Extract ports
	state.Ports = extractPorts(data.HostConfig.PortBindings)

	// Extract mounts
	state.Mounts = extractMounts(data.Mounts)

	// Extract networks
	state.NetworkMode = data.HostConfig.NetworkMode
	state.Networks = extractNetworks(data.NetworkSettings.Networks, data.HostConfig.NetworkMode)

	// Best effort: the image's own environment, to tell its defaults apart
	// from settings made for this container.
	state.ImageEnv = i.imageEnv(cmdCtx, data.Image)

	// Extract restart policy
	state.RestartPolicy = RestartPolicy{
		Name:              data.HostConfig.RestartPolicy.Name,
		MaximumRetryCount: data.HostConfig.RestartPolicy.MaximumRetryCount,
	}

	i.logger.Printf("Extracted runtime state: %d ports, %d mounts, %d env vars, %d networks",
		len(state.Ports), len(state.Mounts), len(state.Env), len(state.Networks))

	return state, nil
}

// SplitImage splits an image reference into repository and tag:
// "payramapp/payram:3.9.0" → ("payramapp/payram", "3.9.0"),
// "localhost:5000/payram:3.10.0" → ("localhost:5000/payram", "3.10.0").
// A digest reference ("...@sha256:...") or a reference without a tag gives an
// empty tag.
func SplitImage(image string) (repo, tag string) {
	if at := strings.Index(image, "@"); at >= 0 {
		return image[:at], ""
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		return image[:colon], image[colon+1:]
	}
	return image, ""
}

// parseImageTag extracts the tag from an image name.
// Returns a map with "repository", "name", and "tag" keys.
func parseImageTag(image string) map[string]string {
	// Split by last colon to separate tag
	lastColon := -1
	for i := len(image) - 1; i >= 0; i-- {
		if image[i] == ':' {
			lastColon = i
			break
		}
	}

	if lastColon == -1 {
		// No tag specified
		return map[string]string{
			"repository": image,
			"name":       image,
			"tag":        "latest",
		}
	}

	return map[string]string{
		"repository": image[:lastColon],
		"name":       image[:lastColon],
		"tag":        image[lastColon+1:],
	}
}

// extractPorts converts Docker port bindings to PortMapping structs.
func extractPorts(portBindings map[string][]struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}) []PortMapping {
	var ports []PortMapping

	for containerPort, bindings := range portBindings {
		// containerPort format: "80/tcp" or "53/udp"
		portAndProto := containerPort
		protocol := "tcp"
		if idx := len(portAndProto) - 4; idx > 0 && portAndProto[idx] == '/' {
			protocol = portAndProto[idx+1:]
			portAndProto = portAndProto[:idx]
		}

		for _, binding := range bindings {
			ports = append(ports, PortMapping{
				HostIP:        binding.HostIP,
				HostPort:      binding.HostPort,
				ContainerPort: portAndProto,
				Protocol:      protocol,
			})
		}
	}

	return ports
}

// extractMounts converts Docker mounts to Mount structs.
func extractMounts(dockerMounts []struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}) []Mount {
	mounts := make([]Mount, len(dockerMounts))

	for i, m := range dockerMounts {
		mounts[i] = Mount{
			Type:        m.Type,
			Name:        m.Name,
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			RW:          m.RW,
		}
	}

	return mounts
}

// extractNetworks converts Docker networks to NetworkConfig structs, the
// container's primary network (HostConfig.NetworkMode) first and the rest by
// name, so every caller sees the same order (Go map order is random).
func extractNetworks(dockerNetworks map[string]struct {
	IPAddress  string `json:"IPAddress"`
	Gateway    string `json:"Gateway"`
	MacAddress string `json:"MacAddress"`
}, primary string) []NetworkConfig {
	var networks []NetworkConfig

	for name, net := range dockerNetworks {
		networks = append(networks, NetworkConfig{
			NetworkName: name,
			IPAddress:   net.IPAddress,
			Gateway:     net.Gateway,
			MacAddress:  net.MacAddress,
		})
	}

	sort.Slice(networks, func(a, b int) bool {
		if (networks[a].NetworkName == primary) != (networks[b].NetworkName == primary) {
			return networks[a].NetworkName == primary
		}
		return networks[a].NetworkName < networks[b].NetworkName
	})
	return networks
}

// imageEnv returns the Config.Env of an image, or nil if it cannot be read.
func (i *Inspector) imageEnv(ctx context.Context, imageID string) []string {
	if imageID == "" {
		return nil
	}
	output, err := exec.CommandContext(ctx, i.dockerBin, "image", "inspect", "--format", "{{json .Config.Env}}", imageID).Output()
	if err != nil {
		i.logger.Printf("Could not read the image's default environment (%v); keeping every container setting", err)
		return nil
	}
	var env []string
	if err := json.Unmarshal(output, &env); err != nil {
		return nil
	}
	return env
}
