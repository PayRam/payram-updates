package manifest

import (
	"strings"
	"testing"
)

// TestBuildDockerRunArgs_Basic tests basic argument construction.
func TestBuildDockerRunArgs_Basic(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "payramapp/payram",
		},
		Defaults: Defaults{
			ContainerName: "payram-test",
			RestartPolicy: "always",
			Ports: []Port{
				{Host: 80, Container: 80, Protocol: "tcp"},
				{Host: 443, Container: 443, Protocol: "tcp"},
			},
			Volumes: []Volume{
				{Source: "/var/lib/payram", Destination: "/data"},
			},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "1.2.3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it starts with run -d
	if len(args) < 2 || args[0] != "run" || args[1] != "-d" {
		t.Errorf("expected args to start with 'run -d', got %v", args[:2])
	}

	// Verify container name
	if !containsSequence(args, "--name", "payram-test") {
		t.Error("expected --name payram-test in args")
	}

	// Verify restart policy
	if !containsSequence(args, "--restart", "always") {
		t.Error("expected --restart always in args")
	}

	// Verify port mappings
	if !containsSequence(args, "-p", "80:80/tcp") {
		t.Error("expected -p 80:80/tcp in args")
	}
	if !containsSequence(args, "-p", "443:443/tcp") {
		t.Error("expected -p 443:443/tcp in args")
	}

	// Verify volume mapping
	if !containsSequence(args, "-v", "/var/lib/payram:/data") {
		t.Error("expected -v /var/lib/payram:/data in args")
	}

	// Verify image name is last
	if args[len(args)-1] != "payramapp/payram:1.2.3" {
		t.Errorf("expected last arg to be 'payramapp/payram:1.2.3', got %s", args[len(args)-1])
	}
}

// TestBuildDockerRunArgs_DefaultRestartPolicy tests fallback restart policy.
func TestBuildDockerRunArgs_DefaultRestartPolicy(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "payramapp/payram",
		},
		Defaults: Defaults{
			ContainerName: "payram",
			// No restart policy specified
			RestartPolicy: "",
			Ports:         []Port{},
			Volumes:       []Volume{},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use default "no" (changed from "unless-stopped" for dummy testing)
	if !containsSequence(args, "--restart", "no") {
		t.Error("expected default --restart no in args")
	}
}

// TestBuildDockerRunArgs_DefaultContainerName tests fallback container name.
func TestBuildDockerRunArgs_DefaultContainerName(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "payramapp/payram",
		},
		Defaults: Defaults{
			ContainerName: "", // No container name specified
			RestartPolicy: "no",
			Ports:         []Port{},
			Volumes:       []Volume{},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use default "payram"
	if !containsSequence(args, "--name", "payram") {
		t.Error("expected default --name payram in args")
	}
}

// TestBuildDockerRunArgs_PortsWithoutProtocol tests ports without protocol specified.
func TestBuildDockerRunArgs_PortsWithoutProtocol(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "myrepo/myimage",
		},
		Defaults: Defaults{
			ContainerName: "test-container",
			RestartPolicy: "always",
			Ports: []Port{
				{Host: 8080, Container: 80}, // No protocol
			},
			Volumes: []Volume{},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "2.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should include port without protocol suffix
	if !containsSequence(args, "-p", "8080:80") {
		t.Error("expected -p 8080:80 in args (without protocol)")
	}
}

// TestBuildDockerRunArgs_ReadOnlyVolumes tests read-only volume mappings.
func TestBuildDockerRunArgs_ReadOnlyVolumes(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "test/image",
		},
		Defaults: Defaults{
			ContainerName: "test",
			RestartPolicy: "no",
			Ports:         []Port{},
			Volumes: []Volume{
				{Source: "/host/data", Destination: "/container/data", ReadOnly: true},
				{Source: "/host/logs", Destination: "/container/logs", ReadOnly: false},
			},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "3.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify read-only volume
	if !containsSequence(args, "-v", "/host/data:/container/data:ro") {
		t.Error("expected -v /host/data:/container/data:ro in args")
	}

	// Verify read-write volume (no :ro suffix)
	if !containsSequence(args, "-v", "/host/logs:/container/logs") {
		t.Error("expected -v /host/logs:/container/logs in args")
	}
}

// TestBuildDockerRunArgs_MultiplePorts tests multiple port mappings.
func TestBuildDockerRunArgs_MultiplePorts(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "app/server",
		},
		Defaults: Defaults{
			ContainerName: "multi-port",
			RestartPolicy: "always",
			Ports: []Port{
				{Host: 80, Container: 8000, Protocol: "tcp"},
				{Host: 443, Container: 8443, Protocol: "tcp"},
				{Host: 8080, Container: 8080},
				{Host: 5432, Container: 5432, Protocol: "tcp"},
			},
			Volumes: []Volume{},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "1.5.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all ports are present
	expectedPorts := []string{
		"80:8000/tcp",
		"443:8443/tcp",
		"8080:8080",
		"5432:5432/tcp",
	}

	for _, expectedPort := range expectedPorts {
		if !containsSequence(args, "-p", expectedPort) {
			t.Errorf("expected -p %s in args", expectedPort)
		}
	}
}

// TestBuildDockerRunArgs_MultipleVolumes tests multiple volume mappings.
func TestBuildDockerRunArgs_MultipleVolumes(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "app/data",
		},
		Defaults: Defaults{
			ContainerName: "multi-vol",
			RestartPolicy: "always",
			Ports:         []Port{},
			Volumes: []Volume{
				{Source: "/data1", Destination: "/mnt/data1"},
				{Source: "/data2", Destination: "/mnt/data2", ReadOnly: true},
				{Source: "/data3", Destination: "/mnt/data3"},
			},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "2.1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all volumes are present
	if !containsSequence(args, "-v", "/data1:/mnt/data1") {
		t.Error("expected -v /data1:/mnt/data1 in args")
	}
	if !containsSequence(args, "-v", "/data2:/mnt/data2:ro") {
		t.Error("expected -v /data2:/mnt/data2:ro in args")
	}
	if !containsSequence(args, "-v", "/data3:/mnt/data3") {
		t.Error("expected -v /data3:/mnt/data3 in args")
	}
}

// TestBuildDockerRunArgs_EmptyPortsAndVolumes tests manifest with no ports or volumes.
func TestBuildDockerRunArgs_EmptyPortsAndVolumes(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "minimal/app",
		},
		Defaults: Defaults{
			ContainerName: "minimal",
			RestartPolicy: "on-failure",
			Ports:         []Port{},
			Volumes:       []Volume{},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "0.1.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should still have basic structure
	if !containsSequence(args, "--name", "minimal") {
		t.Error("expected --name minimal in args")
	}
	if !containsSequence(args, "--restart", "on-failure") {
		t.Error("expected --restart on-failure in args")
	}

	// Should not have any -p flags
	portCount := 0
	for _, arg := range args {
		if arg == "-p" {
			portCount++
		}
	}
	if portCount != 0 {
		t.Errorf("expected 0 -p flags, got %d", portCount)
	}

	// Should not have any -v flags
	volumeCount := 0
	for _, arg := range args {
		if arg == "-v" {
			volumeCount++
		}
	}
	if volumeCount != 0 {
		t.Errorf("expected 0 -v flags, got %d", volumeCount)
	}

	// Verify image name
	if args[len(args)-1] != "minimal/app:0.1.0" {
		t.Errorf("expected image 'minimal/app:0.1.0', got %s", args[len(args)-1])
	}
}

// TestBuildDockerRunArgs_ArgumentOrder tests that arguments are in correct order.
func TestBuildDockerRunArgs_ArgumentOrder(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "test/order",
		},
		Defaults: Defaults{
			ContainerName: "order-test",
			RestartPolicy: "always",
			Ports:         []Port{{Host: 80, Container: 80}},
			Volumes:       []Volume{{Source: "/data", Destination: "/data"}},
		},
	}

	args, err := BuildDockerRunArgs(manifest, "1.0.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	argsStr := strings.Join(args, " ")

	// Image should be at the end
	if !strings.HasSuffix(argsStr, "test/order:1.0.0") {
		t.Error("image should be the last argument")
	}

	// run -d should be at the beginning
	if !strings.HasPrefix(argsStr, "run -d") {
		t.Error("args should start with 'run -d'")
	}
}

// TestBuildDockerRunArgs_DifferentTags tests different resolved tags.
func TestBuildDockerRunArgs_DifferentTags(t *testing.T) {
	manifest := &Manifest{
		Image: Image{
			Repo: "app/test",
		},
		Defaults: Defaults{
			ContainerName: "test",
			RestartPolicy: "always",
			Ports:         []Port{},
			Volumes:       []Volume{},
		},
	}

	testCases := []struct {
		tag      string
		expected string
	}{
		{"1.2.3", "app/test:1.2.3"},
		{"latest", "app/test:latest"},
		{"v2.0.0-beta", "app/test:v2.0.0-beta"},
		{"sha-abc123", "app/test:sha-abc123"},
	}

	for _, tc := range testCases {
		t.Run(tc.tag, func(t *testing.T) {
			args, err := BuildDockerRunArgs(manifest, tc.tag)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if args[len(args)-1] != tc.expected {
				t.Errorf("expected image %s, got %s", tc.expected, args[len(args)-1])
			}
		})
	}
}

// containsSequence checks if args contains a flag followed by a specific value.
func containsSequence(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
