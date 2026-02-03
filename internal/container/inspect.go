// Package container provides runtime inspection of Docker containers.
package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
	Source      string // Host path or volume name
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
		Image string   `json:"Image"`
		Env   []string `json:"Env"`
	} `json:"Config"`
	HostConfig struct {
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
		ID:    data.ID,
		Name:  data.Name,
		Image: data.Config.Image,
		Env:   data.Config.Env,
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
	state.Networks = extractNetworks(data.NetworkSettings.Networks)

	// Extract restart policy
	state.RestartPolicy = RestartPolicy{
		Name:              data.HostConfig.RestartPolicy.Name,
		MaximumRetryCount: data.HostConfig.RestartPolicy.MaximumRetryCount,
	}

	i.logger.Printf("Extracted runtime state: %d ports, %d mounts, %d env vars, %d networks",
		len(state.Ports), len(state.Mounts), len(state.Env), len(state.Networks))

	return state, nil
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
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}) []Mount {
	mounts := make([]Mount, len(dockerMounts))

	for i, m := range dockerMounts {
		mounts[i] = Mount{
			Type:        m.Type,
			Source:      m.Source,
			Destination: m.Destination,
			Mode:        m.Mode,
			RW:          m.RW,
		}
	}

	return mounts
}

// extractNetworks converts Docker networks to NetworkConfig structs.
func extractNetworks(dockerNetworks map[string]struct {
	IPAddress  string `json:"IPAddress"`
	Gateway    string `json:"Gateway"`
	MacAddress string `json:"MacAddress"`
}) []NetworkConfig {
	var networks []NetworkConfig

	for name, net := range dockerNetworks {
		networks = append(networks, NetworkConfig{
			NetworkName: name,
			IPAddress:   net.IPAddress,
			Gateway:     net.Gateway,
			MacAddress:  net.MacAddress,
		})
	}

	return networks
}
