package network

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
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
