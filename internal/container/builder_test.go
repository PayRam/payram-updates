package container

import (
	"fmt"
	"strings"
	"testing"

	"github.com/payram/payram-updater/internal/manifest"
)

// upgradeArgs builds the new container's settings and returns the arguments
// an update creates it with, under the running container's name.
func upgradeArgs(builder *DockerRunBuilder, state *RuntimeState, m *manifest.Manifest, tag string) ([]string, error) {
	if state == nil {
		return nil, fmt.Errorf("runtime state is required (cannot infer configuration)")
	}
	spec, err := builder.BuildContainerSpec(state, m, tag)
	if err != nil {
		return nil, err
	}
	return spec.CreateArgs(ContainerName(state.Name)), nil
}

// TestBuildUpgradeArgs_PreservesRuntimeState tests that runtime state is preserved.
func TestBuildUpgradeArgs_PreservesRuntimeState(t *testing.T) {
	state := &RuntimeState{
		Name:  "payram-core",
		Image: "payramapp/payram:1.8.0",
		Ports: []PortMapping{
			{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
			{HostPort: "8443", ContainerPort: "443", Protocol: "tcp"},
		},
		Mounts: []Mount{
			{Type: "volume", Source: "payram-data", Destination: "/data", RW: true},
			{Type: "bind", Source: "/host/config", Destination: "/config", RW: false, Mode: "ro"},
		},
		Env: []string{
			"AES_KEY=secret123",
			"POSTGRES_PASSWORD=dbsecret",
			"CUSTOM_VAR=value",
		},
		Networks: []NetworkConfig{
			{NetworkName: "payram-net"},
		},
		RestartPolicy: RestartPolicy{Name: "unless-stopped"},
	}

	m := &manifest.Manifest{
		Image: manifest.Image{Repo: "payramapp/payram"},
		Defaults: manifest.Defaults{
			ContainerName: "payram-core",
		},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	args, err := upgradeArgs(builder, state, m, "1.9.0")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Check container name preserved
	if !containsArgs(args, "--name", "payram-core") {
		t.Error("Container name not preserved")
	}

	// Check restart policy preserved (applied when the switch starts it)
	if spec, _ := builder.BuildContainerSpec(state, m, "1.9.0"); spec.RestartPolicy != "unless-stopped" {
		t.Errorf("Restart policy not preserved: %q", spec.RestartPolicy)
	}

	// Check ports preserved
	if !containsArg(args, "-p", "8080:80/tcp") {
		t.Error("Port 8080:80 not preserved")
	}
	if !containsArg(args, "-p", "8443:443/tcp") {
		t.Error("Port 8443:443 not preserved")
	}

	// Check mounts preserved
	if !containsArg(args, "-v", "payram-data:/data") {
		t.Error("Volume mount not preserved")
	}
	if !containsArg(args, "-v", "/host/config:/config:ro") {
		t.Error("Bind mount not preserved")
	}

	// Check env vars preserved: named on the command line, valued in Env
	spec, _ := builder.BuildContainerSpec(state, m, "1.9.0")
	for _, pair := range []string{"AES_KEY=secret123", "POSTGRES_PASSWORD=dbsecret", "CUSTOM_VAR=value"} {
		key := strings.SplitN(pair, "=", 2)[0]
		if !containsArg(args, "-e", key) {
			t.Errorf("%s not passed to the new container", key)
		}
		if !containsString(spec.Env, pair) {
			t.Errorf("%s missing from the environment docker gets: %v", pair, spec.Env)
		}
	}
	if joined := strings.Join(args, " "); strings.Contains(joined, "secret123") || strings.Contains(joined, "dbsecret") {
		t.Errorf("a secret value is on the docker command line: %s", joined)
	}

	// Check network preserved
	if !containsArgs(args, "--network", "payram-net") {
		t.Error("Network not preserved")
	}

	// Check image changed
	if !containsArg(args, "", "payramapp/payram:1.9.0") {
		t.Error("Image not upgraded to 1.9.0")
	}
}

// TestBuildUpgradeArgs_AddsManifestRequirements tests additive overlay.
func TestBuildUpgradeArgs_AddsManifestRequirements(t *testing.T) {
	state := &RuntimeState{
		Name:  "payram",
		Image: "payramapp/payram:1.8.0",
		Ports: []PortMapping{
			{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
		},
		Mounts: []Mount{
			{Type: "volume", Source: "data", Destination: "/data", RW: true},
		},
		Env: []string{
			"EXISTING_VAR=value",
		},
		RestartPolicy: RestartPolicy{Name: "no"},
	}

	m := &manifest.Manifest{
		Image: manifest.Image{Repo: "payramapp/payram"},
		Defaults: manifest.Defaults{
			ContainerName: "payram",
			Ports: []manifest.Port{
				{Container: 9090, Host: 9090, Protocol: "tcp"}, // New port
			},
			Volumes: []manifest.Volume{
				{Destination: "/logs"}, // New mount
			},
		},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	args, err := upgradeArgs(builder, state, m, "1.9.0")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Check original port preserved
	if !containsArg(args, "-p", "8080:80/tcp") {
		t.Error("Original port not preserved")
	}

	// Check new port added
	if !containsArg(args, "-p", "9090:9090/tcp") {
		t.Error("Manifest-required port not added")
	}

	// Check original mount preserved
	if !containsArg(args, "-v", "data:/data") {
		t.Error("Original mount not preserved")
	}

	// Check new mount added
	foundLogsMount := false
	for i, arg := range args {
		if arg == "-v" && i+1 < len(args) && strings.HasPrefix(args[i+1], "/logs") {
			foundLogsMount = true
			break
		}
	}
	if !foundLogsMount {
		t.Error("Manifest-required mount not added")
	}
}

// TestBuildUpgradeArgs_OnlyImageChanges tests that only image tag changes.
func TestBuildUpgradeArgs_OnlyImageChanges(t *testing.T) {
	state := &RuntimeState{
		Name:  "payram",
		Image: "payramapp/payram:1.8.0",
		Ports: []PortMapping{
			{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
		},
		Mounts:        []Mount{},
		Env:           []string{"VAR=value"},
		RestartPolicy: RestartPolicy{Name: "always"},
	}

	m := &manifest.Manifest{
		Image:    manifest.Image{Repo: "payramapp/payram"},
		Defaults: manifest.Defaults{ContainerName: "payram"},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	args, err := upgradeArgs(builder, state, m, "2.0.0")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Verify new image
	lastArg := args[len(args)-1]
	if lastArg != "payramapp/payram:2.0.0" {
		t.Errorf("Expected image 'payramapp/payram:2.0.0', got '%s'", lastArg)
	}

	// Verify all other config unchanged
	if !containsArgs(args, "--name", "payram") {
		t.Error("Container name changed")
	}
	if spec, _ := builder.BuildContainerSpec(state, m, "2.0.0"); spec.RestartPolicy != "always" {
		t.Errorf("Restart policy changed: %q", spec.RestartPolicy)
	}
	if !containsArg(args, "-p", "8080:80/tcp") {
		t.Error("Port changed")
	}
}

// TestBuildUpgradeArgs_NilRuntimeState tests error handling for nil state.
func TestBuildUpgradeArgs_NilRuntimeState(t *testing.T) {
	m := &manifest.Manifest{
		Image:    manifest.Image{Repo: "payramapp/payram"},
		Defaults: manifest.Defaults{ContainerName: "payram"},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	_, err := upgradeArgs(builder, nil, m, "1.9.0")
	if err == nil {
		t.Fatal("Expected error for nil runtime state, got nil")
	}

	if !strings.Contains(err.Error(), "runtime state is required") {
		t.Errorf("Expected 'runtime state is required' error, got: %v", err)
	}
}

// TestBuildUpgradeArgs_NilManifest tests error handling for nil manifest.
func TestBuildUpgradeArgs_NilManifest(t *testing.T) {
	state := &RuntimeState{
		Name:  "payram",
		Image: "payramapp/payram:1.8.0",
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	_, err := upgradeArgs(builder, state, nil, "1.9.0")
	if err == nil {
		t.Fatal("Expected error for nil manifest, got nil")
	}

	if !strings.Contains(err.Error(), "manifest is required") {
		t.Errorf("Expected 'manifest is required' error, got: %v", err)
	}
}

// TestBuildUpgradeArgs_EmptyImageTag tests error handling for empty tag.
func TestBuildUpgradeArgs_EmptyImageTag(t *testing.T) {
	state := &RuntimeState{
		Name:  "payram",
		Image: "payramapp/payram:1.8.0",
	}

	m := &manifest.Manifest{
		Image:    manifest.Image{Repo: "payramapp/payram"},
		Defaults: manifest.Defaults{ContainerName: "payram"},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	_, err := upgradeArgs(builder, state, m, "")
	if err == nil {
		t.Fatal("Expected error for empty image tag, got nil")
	}

	if !strings.Contains(err.Error(), "image tag is required") {
		t.Errorf("Expected 'image tag is required' error, got: %v", err)
	}
}

// TestBuildUpgradeArgs_MissingContainerName tests error for missing name.
func TestBuildUpgradeArgs_MissingContainerName(t *testing.T) {
	state := &RuntimeState{
		Name:  "", // Missing
		Image: "payramapp/payram:1.8.0",
	}

	m := &manifest.Manifest{
		Image:    manifest.Image{Repo: "payramapp/payram"},
		Defaults: manifest.Defaults{},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	_, err := upgradeArgs(builder, state, m, "1.9.0")
	if err == nil {
		t.Fatal("Expected error for missing container name, got nil")
	}

	if !strings.Contains(err.Error(), "container name missing") {
		t.Errorf("Expected 'container name missing' error, got: %v", err)
	}
}

// TestBuildUpgradeArgs_RestartPolicyFormatting tests restart policy formats.
func TestBuildUpgradeArgs_RestartPolicyFormatting(t *testing.T) {
	tests := []struct {
		name     string
		policy   RestartPolicy
		expected string
	}{
		{"no restart", RestartPolicy{Name: ""}, "no"},
		{"always", RestartPolicy{Name: "always"}, "always"},
		{"unless-stopped", RestartPolicy{Name: "unless-stopped"}, "unless-stopped"},
		{"on-failure no count", RestartPolicy{Name: "on-failure"}, "on-failure"},
		{"on-failure with count", RestartPolicy{Name: "on-failure", MaximumRetryCount: 5}, "on-failure:5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &RuntimeState{
				Name:          "test",
				Image:         "test:1.0",
				RestartPolicy: tt.policy,
			}

			m := &manifest.Manifest{
				Image:    manifest.Image{Repo: "test"},
				Defaults: manifest.Defaults{ContainerName: "test"},
			}

			logger := &mockLogger{}
			builder := NewDockerRunBuilder(logger)

			spec, err := builder.BuildContainerSpec(state, m, "1.1")
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if spec.RestartPolicy != tt.expected {
				t.Errorf("restart policy = %q, want %q", spec.RestartPolicy, tt.expected)
			}
			// Created with "no"; the switch applies the policy when it starts it.
			if !containsArgs(spec.CreateArgs("test-next"), "--restart", "no") {
				t.Errorf("the new container must be created with restart policy no: %v", spec.CreateArgs("test-next"))
			}
		})
	}
}

// TestBuildUpgradeArgs_PortFormatting tests port mapping formats.
func TestBuildUpgradeArgs_PortFormatting(t *testing.T) {
	tests := []struct {
		name     string
		port     PortMapping
		expected string
	}{
		{
			"simple tcp",
			PortMapping{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
			"8080:80/tcp",
		},
		{
			"with host IP",
			PortMapping{HostIP: "127.0.0.1", HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
			"127.0.0.1:8080:80/tcp",
		},
		{
			"udp protocol",
			PortMapping{HostPort: "5353", ContainerPort: "53", Protocol: "udp"},
			"5353:53/udp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &RuntimeState{
				Name:  "test",
				Image: "test:1.0",
				Ports: []PortMapping{tt.port},
			}

			m := &manifest.Manifest{
				Image:    manifest.Image{Repo: "test"},
				Defaults: manifest.Defaults{ContainerName: "test"},
			}

			logger := &mockLogger{}
			builder := NewDockerRunBuilder(logger)

			args, err := upgradeArgs(builder, state, m, "1.1")
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if !containsArg(args, "-p", tt.expected) {
				t.Errorf("Expected port format '%s', not found in args", tt.expected)
			}
		})
	}
}

// TestBuildUpgradeArgs_MountFormatting tests mount formatting.
func TestBuildUpgradeArgs_MountFormatting(t *testing.T) {
	tests := []struct {
		name     string
		mount    Mount
		expected string
	}{
		{
			"volume without source",
			Mount{Type: "volume", Source: "", Destination: "/data", RW: true},
			"/data",
		},
		{
			"volume with source",
			Mount{Type: "volume", Source: "mydata", Destination: "/data", RW: true},
			"mydata:/data",
		},
		{
			"bind mount rw",
			Mount{Type: "bind", Source: "/host/path", Destination: "/container/path", RW: true},
			"/host/path:/container/path",
		},
		{
			"bind mount ro",
			Mount{Type: "bind", Source: "/host/path", Destination: "/container/path", Mode: "ro"},
			"/host/path:/container/path:ro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &RuntimeState{
				Name:   "test",
				Image:  "test:1.0",
				Mounts: []Mount{tt.mount},
			}

			m := &manifest.Manifest{
				Image:    manifest.Image{Repo: "test"},
				Defaults: manifest.Defaults{ContainerName: "test"},
			}

			logger := &mockLogger{}
			builder := NewDockerRunBuilder(logger)

			args, err := upgradeArgs(builder, state, m, "1.1")
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if !containsArg(args, "-v", tt.expected) {
				t.Errorf("Expected mount format '%s', not found in args", tt.expected)
			}
		})
	}
}

// TestBuildUpgradeArgs_NetworkPreservation tests network preservation.
func TestBuildUpgradeArgs_NetworkPreservation(t *testing.T) {
	state := &RuntimeState{
		Name:  "test",
		Image: "test:1.0",
		Networks: []NetworkConfig{
			{NetworkName: "custom-network"},
		},
	}

	m := &manifest.Manifest{
		Image:    manifest.Image{Repo: "test"},
		Defaults: manifest.Defaults{ContainerName: "test"},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	args, err := upgradeArgs(builder, state, m, "1.1")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !containsArgs(args, "--network", "custom-network") {
		t.Error("Custom network not preserved")
	}
}

// TestBuildUpgradeArgs_SkipsBridgeNetwork tests that bridge network is not explicitly set.
func TestBuildUpgradeArgs_SkipsBridgeNetwork(t *testing.T) {
	state := &RuntimeState{
		Name:  "test",
		Image: "test:1.0",
		Networks: []NetworkConfig{
			{NetworkName: "bridge"}, // Default network
		},
	}

	m := &manifest.Manifest{
		Image:    manifest.Image{Repo: "test"},
		Defaults: manifest.Defaults{ContainerName: "test"},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	args, err := upgradeArgs(builder, state, m, "1.1")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Bridge network should not be explicitly set
	if containsArgs(args, "--network", "bridge") {
		t.Error("Bridge network should not be explicitly set (it's the default)")
	}
}

// TestNewDockerRunBuilder tests constructor.
func TestNewDockerRunBuilder(t *testing.T) {
	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	if builder == nil {
		t.Fatal("NewDockerRunBuilder returned nil")
	}

	if builder.logger != logger {
		t.Error("Logger not set correctly")
	}
}

// TestNewDockerRunBuilder_NilLogger tests default logger.
func TestNewDockerRunBuilder_NilLogger(t *testing.T) {
	builder := NewDockerRunBuilder(nil)

	if builder == nil {
		t.Fatal("NewDockerRunBuilder returned nil")
	}

	if builder.logger == nil {
		t.Error("Default logger not set")
	}
}

// Helper functions

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func containsArgs(args []string, flag, value string) bool {
	for i, arg := range args {
		if arg == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

func containsArg(args []string, flag, value string) bool {
	if flag == "" {
		// Just check if value exists
		for _, arg := range args {
			if arg == value {
				return true
			}
		}
		return false
	}

	// Check if flag followed by value exists
	for i, arg := range args {
		if arg == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestBuildUpgradeArgs_SkipsEmptyMounts tests that mounts with empty source/destination are skipped.
func TestBuildUpgradeArgs_SkipsEmptyMounts(t *testing.T) {
	state := &RuntimeState{
		Name:  "test-container",
		Image: "test:1.0.0",
		Mounts: []Mount{
			{Type: "bind", Source: "/host/valid", Destination: "/container/valid", RW: true},
			{Type: "bind", Source: "", Destination: "/container/empty-source", RW: true}, // Should be skipped
			{Type: "bind", Source: "/host/another", Destination: "", RW: true},           // Should be skipped
			{Type: "volume", Source: "valid-vol", Destination: "/container/vol", RW: true},
			{Type: "volume", Source: "", Destination: "", RW: true}, // Should be skipped
		},
		RestartPolicy: RestartPolicy{Name: "no"},
	}

	m := &manifest.Manifest{
		Image: manifest.Image{Repo: "test"},
		Defaults: manifest.Defaults{
			ContainerName: "test-container",
		},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	args, err := upgradeArgs(builder, state, m, "1.1.0")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should have exactly 2 valid mounts
	mountCount := 0
	for i, arg := range args {
		if arg == "-v" {
			mountCount++
			if i+1 >= len(args) {
				t.Fatal("Expected value after -v flag")
			}
			mountSpec := args[i+1]

			// Verify no empty sections (no "::" or starting/ending with ":")
			if strings.Contains(mountSpec, "::") {
				t.Errorf("Mount spec contains empty section: %s", mountSpec)
			}
			if strings.HasPrefix(mountSpec, ":") {
				t.Errorf("Mount spec starts with colon: %s", mountSpec)
			}
			if strings.HasSuffix(mountSpec, ":") && !strings.HasSuffix(mountSpec, ":ro") && !strings.HasSuffix(mountSpec, ":rw") {
				t.Errorf("Mount spec ends with colon: %s", mountSpec)
			}
		}
	}

	if mountCount != 2 {
		t.Errorf("Expected 2 valid mounts, got %d", mountCount)
	}

	// Verify the valid mounts are present
	if !containsArg(args, "-v", "/host/valid:/container/valid") {
		t.Error("Expected valid bind mount not found")
	}
	if !containsArg(args, "-v", "valid-vol:/container/vol") {
		t.Error("Expected valid volume mount not found")
	}
}

// TestBuildUpgradeArgs_DeduplicatesMounts tests that duplicate mounts by destination are removed.
func TestBuildUpgradeArgs_DeduplicatesMounts(t *testing.T) {
	state := &RuntimeState{
		Name:  "test-container",
		Image: "test:1.0.0",
		Mounts: []Mount{
			{Type: "volume", Source: "vol1", Destination: "/data", RW: true},
			{Type: "volume", Source: "vol2", Destination: "/data", RW: true}, // Duplicate destination
			{Type: "bind", Source: "/host/logs", Destination: "/logs", RW: true},
			{Type: "bind", Source: "/host/other", Destination: "/logs", RW: false}, // Duplicate destination
		},
		RestartPolicy: RestartPolicy{Name: "no"},
	}

	m := &manifest.Manifest{
		Image: manifest.Image{Repo: "test"},
		Defaults: manifest.Defaults{
			ContainerName: "test-container",
		},
	}

	logger := &mockLogger{}
	builder := NewDockerRunBuilder(logger)

	args, err := upgradeArgs(builder, state, m, "1.1.0")
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should have exactly 2 mounts (duplicates removed)
	mountCount := 0
	seenDestinations := make(map[string]bool)

	for i, arg := range args {
		if arg == "-v" {
			mountCount++
			if i+1 >= len(args) {
				t.Fatal("Expected value after -v flag")
			}
			mountSpec := args[i+1]

			// Extract destination (part before last : or the whole thing)
			parts := strings.Split(mountSpec, ":")
			var dest string
			if len(parts) >= 2 {
				// Check if last part is ro/rw
				if parts[len(parts)-1] == "ro" || parts[len(parts)-1] == "rw" {
					dest = parts[len(parts)-2]
				} else {
					dest = parts[len(parts)-1]
				}
			} else {
				dest = mountSpec
			}

			if seenDestinations[dest] {
				t.Errorf("Found duplicate mount for destination: %s (spec: %s)", dest, mountSpec)
			}
			seenDestinations[dest] = true
		}
	}

	if mountCount != 2 {
		t.Errorf("Expected 2 mounts after deduplication, got %d", mountCount)
	}
}

// TestBuildUpgradeArgs_NoInvalidMountSpecs tests that no invalid mount specs are generated.
func TestBuildUpgradeArgs_NoInvalidMountSpecs(t *testing.T) {
	// Test with various edge cases
	testCases := []struct {
		name   string
		mounts []Mount
	}{
		{
			name: "empty source bind mount",
			mounts: []Mount{
				{Type: "bind", Source: "", Destination: "/test", RW: true},
			},
		},
		{
			name: "empty destination",
			mounts: []Mount{
				{Type: "volume", Source: "vol", Destination: "", RW: true},
			},
		},
		{
			name: "both empty",
			mounts: []Mount{
				{Type: "bind", Source: "", Destination: "", RW: true},
			},
		},
		{
			name: "mixed valid and invalid",
			mounts: []Mount{
				{Type: "volume", Source: "valid", Destination: "/valid", RW: true},
				{Type: "bind", Source: "", Destination: "/invalid", RW: true},
				{Type: "volume", Source: "another", Destination: "", RW: true},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			state := &RuntimeState{
				Name:          "test",
				Image:         "test:1.0",
				Mounts:        tc.mounts,
				RestartPolicy: RestartPolicy{Name: "no"},
			}

			m := &manifest.Manifest{
				Image: manifest.Image{Repo: "test"},
				Defaults: manifest.Defaults{
					ContainerName: "test",
				},
			}

			logger := &mockLogger{}
			builder := NewDockerRunBuilder(logger)

			args, err := upgradeArgs(builder, state, m, "1.1")
			if err != nil {
				t.Fatalf("Expected no error, got: %v", err)
			}

			// Check all -v arguments
			for i, arg := range args {
				if arg == "-v" {
					if i+1 >= len(args) {
						t.Fatal("Expected value after -v flag")
					}
					mountSpec := args[i+1]

					// CRITICAL: No invalid patterns allowed
					if strings.Contains(mountSpec, "::") {
						t.Errorf("INVALID: Mount spec contains '::' - %s", mountSpec)
					}
					if mountSpec == ":" {
						t.Errorf("INVALID: Mount spec is bare colon - %s", mountSpec)
					}
					if strings.HasPrefix(mountSpec, ":") {
						t.Errorf("INVALID: Mount spec starts with colon - %s", mountSpec)
					}
					// Allow trailing : only if followed by ro/rw
					if strings.HasSuffix(mountSpec, ":") && !strings.HasSuffix(mountSpec, ":ro") && !strings.HasSuffix(mountSpec, ":rw") {
						t.Errorf("INVALID: Mount spec ends with bare colon - %s", mountSpec)
					}
				}
			}
		})
	}
}

// TestBuildUpgradeArgs_DropsDatabasePort: an upgrade or rollback must not carry
// the bundled database's port forward, in any binding form, and keeps the rest.
func TestBuildUpgradeArgs_DropsDatabasePort(t *testing.T) {
	state := &RuntimeState{
		Name:  "payram",
		Image: "payramapp/payram:3.8.1",
		Ports: []PortMapping{
			{HostPort: "80", ContainerPort: "80", Protocol: "tcp"},
			{HostPort: "5432", ContainerPort: "5432", Protocol: "tcp"},
			{HostIP: "127.0.0.1", HostPort: "15432", ContainerPort: "5432", Protocol: "tcp"},
			{HostIP: "::", HostPort: "5432", ContainerPort: "5432", Protocol: "tcp"},
			{HostPort: "5432", ContainerPort: "6000", Protocol: "tcp"},
		},
	}
	m := &manifest.Manifest{
		Image: manifest.Image{Repo: "payramapp/payram"},
		Defaults: manifest.Defaults{
			ContainerName: "payram",
			Ports:         []manifest.Port{{Host: 5432, Container: 5432}},
		},
	}

	args, err := upgradeArgs(NewDockerRunBuilder(&mockLogger{}), state, m, "3.10.0")
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	var published []string
	for i, a := range args {
		if a == "-p" && i+1 < len(args) {
			published = append(published, args[i+1])
		}
	}
	want := []string{"80:80/tcp", "5432:6000/tcp"}
	if len(published) != len(want) {
		t.Fatalf("published %v, want %v", published, want)
	}
	for i := range want {
		if published[i] != want[i] {
			t.Fatalf("published %v, want %v", published, want)
		}
	}
}

// TestBuildCreateArgs_RecreatesTheContainerFaithfully covers the settings an
// update must carry over exactly, built for `docker create` under a new name.
func TestBuildCreateArgs_RecreatesTheContainerFaithfully(t *testing.T) {
	state := &RuntimeState{
		Name:          "/payram",
		Image:         "payramapp/payram:3.9.0",
		ImageTag:      "3.9.0",
		RestartPolicy: RestartPolicy{Name: "unless-stopped"},
		NetworkMode:   "payramnet",
		Networks: []NetworkConfig{
			{NetworkName: "payramnet"},
			{NetworkName: "monitoring"},
		},
		Ports: []PortMapping{
			{HostIP: "0.0.0.0", HostPort: "443", ContainerPort: "443", Protocol: "tcp"},
			{HostIP: "127.0.0.1", HostPort: "8080", ContainerPort: "8080", Protocol: "tcp"},
			{HostIP: "::1", HostPort: "8443", ContainerPort: "8443", Protocol: "tcp"},
		},
		Mounts: []Mount{
			{Type: "volume", Name: "payram-postgres-data", Source: "/var/lib/docker/volumes/payram-postgres-data/_data", Destination: "/var/lib/payram/db"},
			{Type: "bind", Source: "/root/payram/log", Destination: "/var/log", Mode: "rw"},
		},
		Env: []string{
			"AES_KEY=secret",
			"PATH=/usr/lib/postgresql/17/bin:/usr/local/bin:/usr/bin",
			"POSTGRES_USER=payram",
		},
		ImageEnv: []string{"PATH=/usr/lib/postgresql/17/bin:/usr/local/bin:/usr/bin"},
	}
	m := &manifest.Manifest{Image: manifest.Image{Repo: "payramapp/payram"}}

	builder := NewDockerRunBuilder(&mockLogger{})
	spec, err := builder.BuildContainerSpec(state, m, "3.10.0")
	if err != nil {
		t.Fatalf("BuildContainerSpec: %v", err)
	}
	args := strings.Join(spec.CreateArgs("payram-next"), " ")

	for _, want := range []string{
		"create --name payram-next --restart no ",
		"-p 443:443/tcp",
		"-p 127.0.0.1:8080:8080/tcp",
		"-p [::1]:8443:8443/tcp",
		"-v payram-postgres-data:/var/lib/payram/db",
		"-v /root/payram/log:/var/log:rw",
		"-e AES_KEY ",
		"-e POSTGRES_USER ",
		"--network payramnet",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("create args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "secret") {
		t.Errorf("the AES key's value is on the docker command line:\n%s", args)
	}
	if spec.RestartPolicy != "unless-stopped" {
		t.Errorf("restart policy = %q, want unless-stopped for the switch to apply", spec.RestartPolicy)
	}
	if want := []string{"AES_KEY=secret", "POSTGRES_USER=payram"}; strings.Join(spec.Env, " ") != strings.Join(want, " ") {
		t.Errorf("env = %v, want %v (and not the old image's PATH)", spec.Env, want)
	}
	if strings.Contains(args, "PATH") {
		t.Errorf("the old image's default PATH must be left to the new image:\n%s", args)
	}
	if strings.Contains(args, "/var/lib/docker/volumes") {
		t.Errorf("a named volume must be mounted by name, not by its host path:\n%s", args)
	}
	if !strings.HasSuffix(args, " payramapp/payram:3.10.0") {
		t.Errorf("image must be last: %s", args)
	}
	if len(spec.ExtraNetworks) != 1 || spec.ExtraNetworks[0] != "monitoring" {
		t.Errorf("extra networks = %v, want [monitoring]", spec.ExtraNetworks)
	}
}

// TestBuildContainerSpec_HostNetworkKeepsHostMode: a host-network container
// must not be rebuilt on the bridge with published ports.
func TestBuildContainerSpec_HostNetworkKeepsHostMode(t *testing.T) {
	state := &RuntimeState{
		Name:        "payram",
		NetworkMode: "host",
		Networks:    []NetworkConfig{{NetworkName: "host"}},
		Ports:       []PortMapping{{HostPort: "80", ContainerPort: "80", Protocol: "tcp"}},
	}
	m := &manifest.Manifest{Image: manifest.Image{Repo: "payramapp/payram"}}

	spec, err := NewDockerRunBuilder(&mockLogger{}).BuildContainerSpec(state, m, "3.10.0")
	if err != nil {
		t.Fatalf("BuildContainerSpec: %v", err)
	}
	args := strings.Join(spec.Options, " ")
	if !strings.Contains(args, "--network host") || strings.Contains(args, "-p ") {
		t.Errorf("host network container rebuilt wrongly: %s", args)
	}
}

// TestBuildContainerSpec_SecretsTravelInTheEnvironmentOnly: values go to
// docker through its environment, except settings docker itself reads,
// which would steer docker (another daemon, config or proxy) if put there.
func TestBuildContainerSpec_SecretsTravelInTheEnvironmentOnly(t *testing.T) {
	state := &RuntimeState{
		Name:  "payram",
		Image: "payramapp/payram:3.9.0",
		Env: []string{
			"AES_KEY=hot-wallet-key",
			"POSTGRES_PASSWORD=db-pass",
			"SSL_CERT_PATH=",
			"HTTPS_PROXY=http://user:proxy-pass@proxy:3128",
			"HOME=/srv/payram",
			"DOCKER_HOST=tcp://elsewhere:2375",
		},
	}
	m := &manifest.Manifest{Image: manifest.Image{Repo: "payramapp/payram"}}
	spec, err := NewDockerRunBuilder(&mockLogger{}).BuildContainerSpec(state, m, "3.10.0")
	if err != nil {
		t.Fatalf("BuildContainerSpec: %v", err)
	}
	args := strings.Join(spec.CreateArgs("payram-next"), " ")
	for _, secret := range []string{"hot-wallet-key", "db-pass", "proxy-pass"} {
		if strings.Contains(args, secret) {
			t.Errorf("%s is on the docker command line: %s", secret, args)
		}
	}
	for _, named := range []string{"-e AES_KEY ", "-e POSTGRES_PASSWORD ", "-e SSL_CERT_PATH ", "-e HTTPS_PROXY "} {
		if !strings.Contains(args, named) {
			t.Errorf("args lack %q: %s", named, args)
		}
	}
	for _, inline := range []string{"-e HOME=/srv/payram", "-e DOCKER_HOST=tcp://elsewhere:2375"} {
		if !strings.Contains(args, inline) {
			t.Errorf("args lack %q (docker reads it, so it must not go in its environment): %s", inline, args)
		}
	}
	want := []string{"AES_KEY=hot-wallet-key", "POSTGRES_PASSWORD=db-pass", "SSL_CERT_PATH=", "HTTPS_PROXY=http://user:proxy-pass@proxy:3128"}
	if strings.Join(spec.Env, "|") != strings.Join(want, "|") {
		t.Errorf("env = %v, want %v", spec.Env, want)
	}
}

func TestPublishesDatabasePort(t *testing.T) {
	published := &RuntimeState{Ports: []PortMapping{{HostPort: "443", ContainerPort: "443"}, {HostPort: "5432", ContainerPort: "5432", Protocol: "tcp"}}}
	if !PublishesDatabasePort(published) {
		t.Error("a container that maps 5432 publishes the database")
	}
	if PublishesDatabasePort(&RuntimeState{Ports: []PortMapping{{HostPort: "443", ContainerPort: "443"}}}) {
		t.Error("443 only is not the database")
	}
	if PublishesDatabasePort(&RuntimeState{NetworkMode: "host", Ports: published.Ports}) || PublishesDatabasePort(nil) {
		t.Error("a host-network container publishes nothing")
	}
}
