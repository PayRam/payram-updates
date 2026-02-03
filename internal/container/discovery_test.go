package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// mockLogger implements Logger interface for testing.
type mockLogger struct {
	logs []string
}

func (m *mockLogger) Printf(format string, v ...interface{}) {
	m.logs = append(m.logs, fmt.Sprintf(format, v...))
}

// TestDiscoverPayramContainer_SingleContainer tests discovery with one container.
func TestDiscoverPayramContainer_SingleContainer(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create mock docker script
	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<EOF
{"ID":"abc123","Names":"payram","Image":"payramapp/payram:1.0.0","State":"running","Status":"Up 5 hours","CreatedAt":"2026-01-01"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if container.Name != "payram" {
		t.Errorf("Expected name 'payram', got '%s'", container.Name)
	}

	if container.ImageTag != "1.0.0" {
		t.Errorf("Expected tag '1.0.0', got '%s'", container.ImageTag)
	}

	if container.ID != "abc123" {
		t.Errorf("Expected ID 'abc123', got '%s'", container.ID)
	}
}

// TestDiscoverPayramContainer_MultipleContainers tests selecting highest version.
func TestDiscoverPayramContainer_MultipleContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Create mock docker script with multiple containers
	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<EOF
{"ID":"abc123","Names":"payram-old","Image":"payramapp/payram:1.0.0","State":"running","Status":"Up 5 hours","CreatedAt":"2026-01-01"}
{"ID":"def456","Names":"payram-new","Image":"payramapp/payram:2.5.1","State":"running","Status":"Up 3 hours","CreatedAt":"2026-01-02"}
{"ID":"ghi789","Names":"payram-mid","Image":"payramapp/payram:1.9.0","State":"running","Status":"Up 4 hours","CreatedAt":"2026-01-01"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should select highest version: 2.5.1
	if container.ImageTag != "2.5.1" {
		t.Errorf("Expected highest version '2.5.1', got '%s'", container.ImageTag)
	}

	if container.Name != "payram-new" {
		t.Errorf("Expected name 'payram-new', got '%s'", container.Name)
	}
}

// TestDiscoverPayramContainer_NoContainers tests error when no containers exist.
func TestDiscoverPayramContainer_NoContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	echo ""
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	_, err := discoverer.DiscoverPayramContainer(context.Background())
	if err == nil {
		t.Fatal("Expected error for no containers, got nil")
	}

	discoveryErr, ok := err.(*DiscoveryError)
	if !ok {
		t.Fatalf("Expected DiscoveryError, got %T", err)
	}

	if discoveryErr.FailureCode != "PAYRAM_CONTAINER_NOT_FOUND" {
		t.Errorf("Expected PAYRAM_CONTAINER_NOT_FOUND, got '%s'", discoveryErr.FailureCode)
	}
}

// TestDiscoverPayramContainer_NoPayramContainers tests when only non-Payram containers exist.
func TestDiscoverPayramContainer_NoPayramContainers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<EOF
{"ID":"abc123","Names":"postgres","Image":"postgres:14","State":"running","Status":"Up 5 hours","CreatedAt":"2026-01-01"}
{"ID":"def456","Names":"redis","Image":"redis:latest","State":"running","Status":"Up 3 hours","CreatedAt":"2026-01-02"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	_, err := discoverer.DiscoverPayramContainer(context.Background())
	if err == nil {
		t.Fatal("Expected error for no Payram containers, got nil")
	}

	discoveryErr, ok := err.(*DiscoveryError)
	if !ok {
		t.Fatalf("Expected DiscoveryError, got %T", err)
	}

	if discoveryErr.FailureCode != "PAYRAM_CONTAINER_NOT_FOUND" {
		t.Errorf("Expected PAYRAM_CONTAINER_NOT_FOUND, got '%s'", discoveryErr.FailureCode)
	}
}

// TestDiscoverPayramContainer_SkipsLatestTag tests that latest tag is skipped.
func TestDiscoverPayramContainer_SkipsLatestTag(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<EOF
{"ID":"abc123","Names":"payram-latest","Image":"payramapp/payram:latest","State":"running","Status":"Up 5 hours","CreatedAt":"2026-01-01"}
{"ID":"def456","Names":"payram-versioned","Image":"payramapp/payram:1.5.0","State":"running","Status":"Up 3 hours","CreatedAt":"2026-01-02"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should skip "latest" and select versioned container
	if container.ImageTag != "1.5.0" {
		t.Errorf("Expected tag '1.5.0', got '%s'", container.ImageTag)
	}

	if container.Name != "payram-versioned" {
		t.Errorf("Expected name 'payram-versioned', got '%s'", container.Name)
	}
}

// TestDiscoverPayramContainer_OnlyLatestTag tests error when only latest tag exists.
func TestDiscoverPayramContainer_OnlyLatestTag(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<EOF
{"ID":"abc123","Names":"payram","Image":"payramapp/payram:latest","State":"running","Status":"Up 5 hours","CreatedAt":"2026-01-01"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	_, err := discoverer.DiscoverPayramContainer(context.Background())
	if err == nil {
		t.Fatal("Expected error when only latest tag, got nil")
	}

	// Should fail because latest is skipped and no other containers exist
	discoveryErr, ok := err.(*DiscoveryError)
	if !ok {
		t.Fatalf("Expected DiscoveryError, got %T", err)
	}

	if discoveryErr.FailureCode != "PAYRAM_CONTAINER_NOT_FOUND" {
		t.Errorf("Expected PAYRAM_CONTAINER_NOT_FOUND, got '%s'", discoveryErr.FailureCode)
	}
}

// TestSelectHighestVersion tests version comparison logic.
func TestSelectHighestVersion(t *testing.T) {
	tests := []struct {
		name          string
		candidates    []DiscoveredContainer
		expectedTag   string
		expectedError bool
	}{
		{
			name: "simple comparison",
			candidates: []DiscoveredContainer{
				{ID: "1", Name: "c1", ImageTag: "1.0.0"},
				{ID: "2", Name: "c2", ImageTag: "2.0.0"},
				{ID: "3", Name: "c3", ImageTag: "1.5.0"},
			},
			expectedTag:   "2.0.0",
			expectedError: false,
		},
		{
			name: "patch version differences",
			candidates: []DiscoveredContainer{
				{ID: "1", Name: "c1", ImageTag: "1.0.1"},
				{ID: "2", Name: "c2", ImageTag: "1.0.5"},
				{ID: "3", Name: "c3", ImageTag: "1.0.3"},
			},
			expectedTag:   "1.0.5",
			expectedError: false,
		},
		{
			name: "pre-release versions",
			candidates: []DiscoveredContainer{
				{ID: "1", Name: "c1", ImageTag: "1.0.0-alpha"},
				{ID: "2", Name: "c2", ImageTag: "1.0.0"},
				{ID: "3", Name: "c3", ImageTag: "1.0.0-beta"},
			},
			expectedTag:   "1.0.0",
			expectedError: false,
		},
		{
			name: "single candidate",
			candidates: []DiscoveredContainer{
				{ID: "1", Name: "c1", ImageTag: "3.2.1"},
			},
			expectedTag:   "3.2.1",
			expectedError: false,
		},
		{
			name: "invalid versions skipped",
			candidates: []DiscoveredContainer{
				{ID: "1", Name: "c1", ImageTag: "invalid"},
				{ID: "2", Name: "c2", ImageTag: "1.2.3"},
				{ID: "3", Name: "c3", ImageTag: "also-invalid"},
			},
			expectedTag:   "1.2.3",
			expectedError: false,
		},
		{
			name: "all invalid versions",
			candidates: []DiscoveredContainer{
				{ID: "1", Name: "c1", ImageTag: "invalid"},
				{ID: "2", Name: "c2", ImageTag: "also-invalid"},
			},
			expectedTag:   "",
			expectedError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := selectHighestVersion(tt.candidates)

			if tt.expectedError {
				if err == nil {
					t.Fatal("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("Expected no error, got: %v", err)
			}

			if result.ImageTag != tt.expectedTag {
				t.Errorf("Expected tag '%s', got '%s'", tt.expectedTag, result.ImageTag)
			}
		})
	}
}

// TestDiscoveryError tests error formatting.
func TestDiscoveryError(t *testing.T) {
	err := &DiscoveryError{
		FailureCode: "TEST_ERROR",
		Message:     "This is a test error",
	}

	expected := "TEST_ERROR: This is a test error"
	if err.Error() != expected {
		t.Errorf("Expected '%s', got '%s'", expected, err.Error())
	}
}

// createMockDockerScript creates a temporary executable script for testing.
func createMockDockerScript(t *testing.T, content string) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "mock-docker-*.sh")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	if err := tmpFile.Close(); err != nil {
		t.Fatalf("Failed to close file: %v", err)
	}

	if err := os.Chmod(tmpFile.Name(), 0755); err != nil {
		t.Fatalf("Failed to make script executable: %v", err)
	}

	return tmpFile.Name()
}

// TestDiscoverPayramContainer_RealDocker tests against real Docker if available.
// This test is skipped unless INTEGRATION_TEST=1 is set.
func TestDiscoverPayramContainer_RealDocker(t *testing.T) {
	if os.Getenv("INTEGRATION_TEST") != "1" {
		t.Skip("Skipping real Docker integration test (set INTEGRATION_TEST=1 to run)")
	}

	// Check if docker is available
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker not available")
	}

	logger := &mockLogger{}
	discoverer := NewDiscoverer("docker", "", logger)

	// This will only work if a Payram container is actually running
	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		// If no Payram container exists, that's expected
		if discoveryErr, ok := err.(*DiscoveryError); ok {
			if discoveryErr.FailureCode == "PAYRAM_CONTAINER_NOT_FOUND" {
				t.Logf("No Payram container found (expected): %v", err)
				return
			}
		}
		t.Fatalf("Unexpected error: %v", err)
	}

	t.Logf("Discovered container: %s (ID: %s, Tag: %s)",
		container.Name, container.ID[:12], container.ImageTag)
}

// TestDiscoverPayramContainer_MixedVersionFormats tests various version formats.
func TestDiscoverPayramContainer_MixedVersionFormats(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<EOF
{"ID":"abc","Names":"c1","Image":"payramapp/payram:v1.0.0","State":"running","Status":"Up","CreatedAt":"2026-01-01"}
{"ID":"def","Names":"c2","Image":"payramapp/payram:2.0.0","State":"running","Status":"Up","CreatedAt":"2026-01-01"}
{"ID":"ghi","Names":"c3","Image":"payramapp/payram:1.5.0-beta","State":"running","Status":"Up","CreatedAt":"2026-01-01"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// v1.0.0 is parsed as 1.0.0, so 2.0.0 should be highest
	if container.ImageTag != "2.0.0" {
		t.Errorf("Expected highest version '2.0.0', got '%s'", container.ImageTag)
	}
}

// TestDiscoveredContainerStructure validates the structure.
func TestDiscoveredContainerStructure(t *testing.T) {
	container := DiscoveredContainer{
		ID:        "abc123def456",
		Name:      "payram-core",
		ImageTag:  "1.2.3",
		ImageFull: "payramapp/payram:1.2.3",
	}

	// Verify all fields are accessible
	if container.ID == "" || container.Name == "" || container.ImageTag == "" || container.ImageFull == "" {
		t.Error("DiscoveredContainer fields should be populated")
	}
}

// Benchmark version selection performance
func BenchmarkSelectHighestVersion(b *testing.B) {
	candidates := make([]DiscoveredContainer, 100)
	for i := 0; i < 100; i++ {
		candidates[i] = DiscoveredContainer{
			ID:       fmt.Sprintf("id-%d", i),
			Name:     fmt.Sprintf("container-%d", i),
			ImageTag: fmt.Sprintf("%d.%d.%d", i/10, i%10, i%3),
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = selectHighestVersion(candidates)
	}
}

// TestDiscoverPayramContainer_JSONParsing tests robust JSON parsing.
func TestDiscoverPayramContainer_JSONParsing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Test with extra whitespace and formatting
	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<'EOF'
{"ID":"abc123","Names":"payram","Image":"payramapp/payram:1.0.0","State":"running","Status":"Up 5 hours","CreatedAt":"2026-01-01"}

{"ID":"def456","Names":"other","Image":"postgres:14","State":"running","Status":"Up","CreatedAt":"2026-01-01"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		t.Fatalf("Expected no error with extra whitespace, got: %v", err)
	}

	if container.Name != "payram" {
		t.Errorf("Expected name 'payram', got '%s'", container.Name)
	}
}

// TestNewDiscoverer validates constructor.
func TestNewDiscoverer(t *testing.T) {
	logger := &mockLogger{}
	discoverer := NewDiscoverer("docker", "", logger)

	if discoverer == nil {
		t.Fatal("NewDiscoverer returned nil")
	}

	if discoverer.dockerBin != "docker" {
		t.Errorf("Expected dockerBin 'docker', got '%s'", discoverer.dockerBin)
	}

	if discoverer.logger != logger {
		t.Error("Logger not set correctly")
	}
}

// Test container list JSON serialization
func TestContainerListEntry(t *testing.T) {
	jsonData := `{"ID":"abc123","Names":"test","Image":"payramapp/payram:1.0.0","State":"running","Status":"Up","CreatedAt":"2026-01-01"}`

	var entry containerListEntry
	if err := json.Unmarshal([]byte(jsonData), &entry); err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}

	if entry.ID != "abc123" {
		t.Errorf("Expected ID 'abc123', got '%s'", entry.ID)
	}

	if entry.Image != "payramapp/payram:1.0.0" {
		t.Errorf("Expected image 'payramapp/payram:1.0.0', got '%s'", entry.Image)
	}
}

// TestDiscoverPayramContainer_ContainerNamePrefix tests stripping / prefix.
func TestDiscoverPayramContainer_ContainerNamePrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	dockerScript := createMockDockerScript(t, `#!/bin/bash
if [[ "$1" == "ps" ]]; then
	cat <<EOF
{"ID":"abc123","Names":"/payram-core","Image":"payramapp/payram:1.0.0","State":"running","Status":"Up","CreatedAt":"2026-01-01"}
EOF
fi
`)
	defer os.Remove(dockerScript)

	logger := &mockLogger{}
	discoverer := NewDiscoverer(dockerScript, "", logger)

	container, err := discoverer.DiscoverPayramContainer(context.Background())
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Docker prefixes names with /, should be stripped
	if container.Name != "payram-core" {
		t.Errorf("Expected name 'payram-core' (without /), got '%s'", container.Name)
	}

	if strings.HasPrefix(container.Name, "/") {
		t.Error("Container name should not have / prefix")
	}
}
