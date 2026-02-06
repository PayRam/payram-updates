package network

import (
	"strings"
	"testing"
)

func TestGetDockerBridgeIP(t *testing.T) {
	ip, err := GetDockerBridgeIP()
	if err != nil {
		t.Logf("Docker bridge not available (this is OK in test environments): %v", err)
		return
	}

	// Validate IP format
	if ip == "" {
		t.Error("expected non-empty IP address")
	}

	// Check if it's a valid IPv4 format
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		t.Errorf("expected IPv4 format, got: %s", ip)
	}

	// Docker bridge typically uses 172.17.0.1 but could be different
	t.Logf("Docker bridge IP: %s", ip)
}
