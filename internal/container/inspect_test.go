package container

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

// TestExtractRuntimeState tests full runtime state extraction.
func TestExtractRuntimeState(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create mock docker inspect output
	inspectJSON := `[{
		"Id": "abc123def456",
		"Name": "/payram-core",
		"Image": "sha256:fedcba987654",
		"Config": {
			"Image": "payramapp/payram:1.2.3",
			"Env": [
				"PATH=/usr/local/bin:/usr/bin",
				"POSTGRES_HOST=db.example.com",
				"POSTGRES_PORT=5432"
			]
		},
		"HostConfig": {
			"RestartPolicy": {
				"Name": "unless-stopped",
				"MaximumRetryCount": 0
			},
			"PortBindings": {
				"80/tcp": [
					{"HostIp": "0.0.0.0", "HostPort": "8080"}
				],
				"443/tcp": [
					{"HostIp": "0.0.0.0", "HostPort": "8443"}
				]
			}
		},
		"Mounts": [
			{
				"Type": "volume",
				"Source": "payram-data",
				"Destination": "/var/lib/payram",
				"Mode": "",
				"RW": true
			},
			{
				"Type": "bind",
				"Source": "/etc/ssl/certs",
				"Destination": "/certs",
				"Mode": "ro",
				"RW": false
			}
		],
		"NetworkSettings": {
			"Networks": {
				"bridge": {
					"IPAddress": "172.17.0.2",
					"Gateway": "172.17.0.1",
					"MacAddress": "02:42:ac:11:00:02"
				}
			}
		}
	}]`

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "inspect" ]]; then
	cat <<'EOF'
`+inspectJSON+`
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	inspector := NewInspector(dockerScript, logger)

	state, err := inspector.ExtractRuntimeState(context.Background(), "payram-core")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Validate ID
	if state.ID != "abc123def456" {
		t.Errorf("Expected ID 'abc123def456', got '%s'", state.ID)
	}

	// Validate Name
	if state.Name != "/payram-core" {
		t.Errorf("Expected name '/payram-core', got '%s'", state.Name)
	}

	// Validate Image
	if state.Image != "payramapp/payram:1.2.3" {
		t.Errorf("Expected image 'payramapp/payram:1.2.3', got '%s'", state.Image)
	}

	// Validate ImageTag
	if state.ImageTag != "1.2.3" {
		t.Errorf("Expected tag '1.2.3', got '%s'", state.ImageTag)
	}

	// Validate Environment variables
	if len(state.Env) != 3 {
		t.Errorf("Expected 3 env vars, got %d", len(state.Env))
	}

	// Validate Ports
	if len(state.Ports) != 2 {
		t.Fatalf("Expected 2 ports, got %d", len(state.Ports))
	}

	// Check first port
	port80 := findPort(state.Ports, "80")
	if port80 == nil {
		t.Fatal("Expected port 80 not found")
	}
	if port80.HostPort != "8080" {
		t.Errorf("Expected host port 8080, got '%s'", port80.HostPort)
	}
	if port80.Protocol != "tcp" {
		t.Errorf("Expected protocol tcp, got '%s'", port80.Protocol)
	}

	// Validate Mounts
	if len(state.Mounts) != 2 {
		t.Fatalf("Expected 2 mounts, got %d", len(state.Mounts))
	}

	// Check volume mount
	volumeMount := findMount(state.Mounts, "/var/lib/payram")
	if volumeMount == nil {
		t.Fatal("Expected volume mount not found")
	}
	if volumeMount.Type != "volume" {
		t.Errorf("Expected type 'volume', got '%s'", volumeMount.Type)
	}
	if volumeMount.Source != "payram-data" {
		t.Errorf("Expected source 'payram-data', got '%s'", volumeMount.Source)
	}
	if !volumeMount.RW {
		t.Error("Expected RW to be true")
	}

	// Check bind mount
	bindMount := findMount(state.Mounts, "/certs")
	if bindMount == nil {
		t.Fatal("Expected bind mount not found")
	}
	if bindMount.Type != "bind" {
		t.Errorf("Expected type 'bind', got '%s'", bindMount.Type)
	}
	if bindMount.RW {
		t.Error("Expected RW to be false for read-only mount")
	}

	// Validate Networks
	if len(state.Networks) != 1 {
		t.Fatalf("Expected 1 network, got %d", len(state.Networks))
	}
	if state.Networks[0].NetworkName != "bridge" {
		t.Errorf("Expected network 'bridge', got '%s'", state.Networks[0].NetworkName)
	}
	if state.Networks[0].IPAddress != "172.17.0.2" {
		t.Errorf("Expected IP '172.17.0.2', got '%s'", state.Networks[0].IPAddress)
	}

	// Validate Restart Policy
	if state.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("Expected restart policy 'unless-stopped', got '%s'", state.RestartPolicy.Name)
	}
}

// TestExtractRuntimeState_MinimalContainer tests with minimal configuration.
func TestExtractRuntimeState_MinimalContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	inspectJSON := `[{
		"Id": "minimal123",
		"Name": "/minimal",
		"Image": "sha256:abc",
		"Config": {
			"Image": "payramapp/payram:1.0.0",
			"Env": []
		},
		"HostConfig": {
			"RestartPolicy": {
				"Name": "no",
				"MaximumRetryCount": 0
			},
			"PortBindings": {}
		},
		"Mounts": [],
		"NetworkSettings": {
			"Networks": {}
		}
	}]`

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "inspect" ]]; then
	cat <<'EOF'
`+inspectJSON+`
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	inspector := NewInspector(dockerScript, logger)

	state, err := inspector.ExtractRuntimeState(context.Background(), "minimal")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(state.Ports) != 0 {
		t.Errorf("Expected 0 ports, got %d", len(state.Ports))
	}

	if len(state.Mounts) != 0 {
		t.Errorf("Expected 0 mounts, got %d", len(state.Mounts))
	}

	if len(state.Networks) != 0 {
		t.Errorf("Expected 0 networks, got %d", len(state.Networks))
	}

	if state.RestartPolicy.Name != "no" {
		t.Errorf("Expected restart policy 'no', got '%s'", state.RestartPolicy.Name)
	}
}

// TestExtractRuntimeState_MultipleNetworks tests multiple network attachments.
func TestExtractRuntimeState_MultipleNetworks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	inspectJSON := `[{
		"Id": "multi123",
		"Name": "/multi-net",
		"Image": "sha256:abc",
		"Config": {
			"Image": "payramapp/payram:2.0.0",
			"Env": []
		},
		"HostConfig": {
			"RestartPolicy": {"Name": "always", "MaximumRetryCount": 0},
			"PortBindings": {}
		},
		"Mounts": [],
		"NetworkSettings": {
			"Networks": {
				"frontend": {
					"IPAddress": "10.0.1.5",
					"Gateway": "10.0.1.1",
					"MacAddress": "02:42:0a:00:01:05"
				},
				"backend": {
					"IPAddress": "10.0.2.5",
					"Gateway": "10.0.2.1",
					"MacAddress": "02:42:0a:00:02:05"
				}
			}
		}
	}]`

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "inspect" ]]; then
	cat <<'EOF'
`+inspectJSON+`
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	inspector := NewInspector(dockerScript, logger)

	state, err := inspector.ExtractRuntimeState(context.Background(), "multi-net")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(state.Networks) != 2 {
		t.Fatalf("Expected 2 networks, got %d", len(state.Networks))
	}

	// Check both networks exist (order may vary due to map iteration)
	foundFrontend := false
	foundBackend := false

	for _, net := range state.Networks {
		if net.NetworkName == "frontend" {
			foundFrontend = true
			if net.IPAddress != "10.0.1.5" {
				t.Errorf("Expected frontend IP '10.0.1.5', got '%s'", net.IPAddress)
			}
		}
		if net.NetworkName == "backend" {
			foundBackend = true
			if net.IPAddress != "10.0.2.5" {
				t.Errorf("Expected backend IP '10.0.2.5', got '%s'", net.IPAddress)
			}
		}
	}

	if !foundFrontend {
		t.Error("Frontend network not found")
	}
	if !foundBackend {
		t.Error("Backend network not found")
	}
}

// TestExtractRuntimeState_OnFailureRestartPolicy tests on-failure restart policy.
func TestExtractRuntimeState_OnFailureRestartPolicy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	inspectJSON := `[{
		"Id": "restart123",
		"Name": "/restart-test",
		"Image": "sha256:abc",
		"Config": {
			"Image": "payramapp/payram:1.0.0",
			"Env": []
		},
		"HostConfig": {
			"RestartPolicy": {
				"Name": "on-failure",
				"MaximumRetryCount": 5
			},
			"PortBindings": {}
		},
		"Mounts": [],
		"NetworkSettings": {"Networks": {}}
	}]`

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "inspect" ]]; then
	cat <<'EOF'
`+inspectJSON+`
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	inspector := NewInspector(dockerScript, logger)

	state, err := inspector.ExtractRuntimeState(context.Background(), "restart-test")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if state.RestartPolicy.Name != "on-failure" {
		t.Errorf("Expected restart policy 'on-failure', got '%s'", state.RestartPolicy.Name)
	}

	if state.RestartPolicy.MaximumRetryCount != 5 {
		t.Errorf("Expected max retry count 5, got %d", state.RestartPolicy.MaximumRetryCount)
	}
}

// TestParseImageTag tests image tag parsing.
func TestParseImageTag(t *testing.T) {
	tests := []struct {
		image       string
		expectedTag string
	}{
		{"payramapp/payram:1.2.3", "1.2.3"},
		{"payramapp/payram:latest", "latest"},
		{"ubuntu:20.04", "20.04"},
		{"localhost:5000/myimage:v1.0", "v1.0"},
		{"myimage", "latest"},
		{"registry.example.com/app/service:prod-v2", "prod-v2"},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			result := parseImageTag(tt.image)
			if result["tag"] != tt.expectedTag {
				t.Errorf("Expected tag '%s', got '%s'", tt.expectedTag, result["tag"])
			}
		})
	}
}

// TestExtractPorts tests port extraction from Docker format.
func TestExtractPorts(t *testing.T) {
	portBindings := map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}{
		"80/tcp": {
			{HostIP: "0.0.0.0", HostPort: "8080"},
		},
		"443/tcp": {
			{HostIP: "127.0.0.1", HostPort: "8443"},
		},
		"53/udp": {
			{HostIP: "0.0.0.0", HostPort: "5353"},
		},
	}

	ports := extractPorts(portBindings)

	if len(ports) != 3 {
		t.Fatalf("Expected 3 ports, got %d", len(ports))
	}

	// Find and check each port
	port80 := findPort(ports, "80")
	if port80 == nil {
		t.Fatal("Port 80 not found")
	}
	if port80.Protocol != "tcp" {
		t.Errorf("Expected protocol 'tcp' for port 80, got '%s'", port80.Protocol)
	}
	if port80.HostPort != "8080" {
		t.Errorf("Expected host port '8080', got '%s'", port80.HostPort)
	}

	port53 := findPort(ports, "53")
	if port53 == nil {
		t.Fatal("Port 53 not found")
	}
	if port53.Protocol != "udp" {
		t.Errorf("Expected protocol 'udp' for port 53, got '%s'", port53.Protocol)
	}
}

// TestExtractMounts tests mount extraction.
func TestExtractMounts(t *testing.T) {
	dockerMounts := []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Mode        string `json:"Mode"`
		RW          bool   `json:"RW"`
	}{
		{
			Type:        "volume",
			Source:      "data-vol",
			Destination: "/data",
			Mode:        "",
			RW:          true,
		},
		{
			Type:        "bind",
			Source:      "/host/config",
			Destination: "/config",
			Mode:        "ro",
			RW:          false,
		},
	}

	mounts := extractMounts(dockerMounts)

	if len(mounts) != 2 {
		t.Fatalf("Expected 2 mounts, got %d", len(mounts))
	}

	if mounts[0].Type != "volume" {
		t.Errorf("Expected type 'volume', got '%s'", mounts[0].Type)
	}

	if mounts[1].Mode != "ro" {
		t.Errorf("Expected mode 'ro', got '%s'", mounts[1].Mode)
	}
}

// TestExtractNetworks tests network extraction.
func TestExtractNetworks(t *testing.T) {
	dockerNetworks := map[string]struct {
		IPAddress  string `json:"IPAddress"`
		Gateway    string `json:"Gateway"`
		MacAddress string `json:"MacAddress"`
	}{
		"bridge": {
			IPAddress:  "172.17.0.2",
			Gateway:    "172.17.0.1",
			MacAddress: "02:42:ac:11:00:02",
		},
	}

	networks := extractNetworks(dockerNetworks)

	if len(networks) != 1 {
		t.Fatalf("Expected 1 network, got %d", len(networks))
	}

	if networks[0].NetworkName != "bridge" {
		t.Errorf("Expected network 'bridge', got '%s'", networks[0].NetworkName)
	}

	if networks[0].IPAddress != "172.17.0.2" {
		t.Errorf("Expected IP '172.17.0.2', got '%s'", networks[0].IPAddress)
	}
}

// TestRuntimeStateStructure validates struct accessibility.
func TestRuntimeStateStructure(t *testing.T) {
	state := RuntimeState{
		ID:       "test123",
		Name:     "test-container",
		Image:    "test:1.0",
		ImageTag: "1.0",
		Ports: []PortMapping{
			{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
		},
		Mounts: []Mount{
			{Type: "volume", Source: "vol", Destination: "/data"},
		},
		Env: []string{"VAR=value"},
		Networks: []NetworkConfig{
			{NetworkName: "bridge", IPAddress: "172.17.0.2"},
		},
		RestartPolicy: RestartPolicy{Name: "always"},
	}

	// Verify all fields are accessible
	if state.ID == "" || len(state.Ports) == 0 || len(state.Mounts) == 0 {
		t.Error("RuntimeState fields should be accessible")
	}
}

// TestNewInspector validates constructor.
func TestNewInspector(t *testing.T) {
	logger := &mockLogger{}
	inspector := NewInspector("docker", logger)

	if inspector == nil {
		t.Fatal("NewInspector returned nil")
	}

	if inspector.dockerBin != "docker" {
		t.Errorf("Expected dockerBin 'docker', got '%s'", inspector.dockerBin)
	}

	if inspector.logger != logger {
		t.Error("Logger not set correctly")
	}
}

// TestExtractRuntimeState_RealDocker tests against real Docker if available.
func TestExtractRuntimeState_RealDocker(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") != "1" {
		t.Skip("Skipping real Docker integration test (set INTEGRATION_TEST=1 to run)")
	}

	logger := &mockLogger{}
	discoverer := NewDiscoverer("docker", "", logger)

	// Try to discover a Payram container
	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		t.Skipf("No Payram container found: %v", err)
	}

	// Now inspect it
	inspector := NewInspector("docker", logger)
	state, err := inspector.ExtractRuntimeState(context.Background(), container.ID)
	if err != nil {
		t.Fatalf("Failed to extract runtime state: %v", err)
	}

	t.Logf("Extracted state for %s:", container.Name)
	t.Logf("  ID: %s", state.ID[:12])
	t.Logf("  Image: %s", state.Image)
	t.Logf("  Ports: %d", len(state.Ports))
	t.Logf("  Mounts: %d", len(state.Mounts))
	t.Logf("  Env vars: %d", len(state.Env))
	t.Logf("  Networks: %d", len(state.Networks))
	t.Logf("  Restart: %s", state.RestartPolicy.Name)
}

// TestDockerInspectOutput tests JSON unmarshaling.
func TestDockerInspectOutput(t *testing.T) {
	jsonData := `[{
		"Id": "abc123",
		"Name": "/test",
		"Image": "sha256:def456",
		"Config": {
			"Image": "test:1.0",
			"Env": ["PATH=/usr/bin"]
		},
		"HostConfig": {
			"RestartPolicy": {"Name": "always", "MaximumRetryCount": 0},
			"PortBindings": {}
		},
		"Mounts": [],
		"NetworkSettings": {"Networks": {}}
	}]`

	var output []dockerInspectOutput
	if err := json.Unmarshal([]byte(jsonData), &output); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if len(output) != 1 {
		t.Fatalf("Expected 1 entry, got %d", len(output))
	}

	if output[0].ID != "abc123" {
		t.Errorf("Expected ID 'abc123', got '%s'", output[0].ID)
	}
}

// Helper functions

func findPort(ports []PortMapping, containerPort string) *PortMapping {
	for i := range ports {
		if ports[i].ContainerPort == containerPort {
			return &ports[i]
		}
	}
	return nil
}

func findMount(mounts []Mount, destination string) *Mount {
	for i := range mounts {
		if mounts[i].Destination == destination {
			return &mounts[i]
		}
	}
	return nil
}
