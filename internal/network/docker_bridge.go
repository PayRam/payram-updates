package network

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/logger"
)

// GetDockerBridgeIP retrieves the IPv4 address of the docker0 bridge interface.
// This allows the updater to be accessible from Docker containers.
// Returns the IP address (e.g., "172.17.0.1") or an error if not found.
func GetDockerBridgeIP() (string, error) {
	cmd := exec.Command("ip", "-4", "addr", "show", "docker0")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to execute 'ip -4 addr show docker0': %w", err)
	}

	// Parse output to extract the IP address
	// Example line: "    inet 172.17.0.1/16 brd 172.17.255.255 scope global docker0"
	re := regexp.MustCompile(`inet\s+(\d+\.\d+\.\d+\.\d+)/`)
	matches := re.FindStringSubmatch(string(output))
	if len(matches) < 2 {
		return "", fmt.Errorf("docker0 interface not found or has no IPv4 address")
	}

	ip := strings.TrimSpace(matches[1])
	return ip, nil
}

// GetPayramContainerIP retrieves the IP address of the Payram container.
// This is used to restrict API access to only the Payram container.
func GetPayramContainerIP(dockerBin string, imagePattern string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Step 1: Discover the Payram container using the same selection logic as the updater
	discoverer := container.NewDiscoverer(dockerBin, imagePattern, logger.StdLogger())
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to discover Payram container: %w", err)
	}

	// Step 2: Inspect the container to read network IPs
	inspector := container.NewInspector(dockerBin, logger.StdLogger())
	runtimeState, err := inspector.ExtractRuntimeState(ctx, discovered.Name)
	if err != nil {
		return "", fmt.Errorf("failed to inspect Payram container %s: %w", discovered.Name, err)
	}

	// Step 3: Choose the first non-empty IP from the container networks
	for _, network := range runtimeState.Networks {
		if strings.TrimSpace(network.IPAddress) != "" {
			return strings.TrimSpace(network.IPAddress), nil
		}
	}

	return "", fmt.Errorf("Payram container %s has no IP address in any network", discovered.Name)
}
