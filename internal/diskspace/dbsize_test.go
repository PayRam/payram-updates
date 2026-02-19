package diskspace

import (
	"context"
	"testing"
)

func TestDBConfig_IsLocalDB(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		expected bool
	}{
		{"localhost", "localhost", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"ipv6 localhost", "::1", true},
		{"external host", "192.168.1.100", false},
		{"remote host", "db.example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &DBConfig{Host: tt.host}
			if got := config.IsLocalDB(); got != tt.expected {
				t.Errorf("IsLocalDB() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestNewDBSizeChecker(t *testing.T) {
	// Test with empty dockerBin (should default to "docker")
	checker := NewDBSizeChecker("")
	if checker.DockerBin != "docker" {
		t.Errorf("Expected default docker bin to be 'docker', got %s", checker.DockerBin)
	}

	// Test with custom dockerBin
	custom := "/usr/local/bin/docker"
	checker = NewDBSizeChecker(custom)
	if checker.DockerBin != custom {
		t.Errorf("Expected docker bin to be %s, got %s", custom, checker.DockerBin)
	}
}

func TestGetDatabaseSize_InvalidContainer(t *testing.T) {
	checker := NewDBSizeChecker("docker")
	ctx := context.Background()

	dbConfig := &DBConfig{
		Host:     "localhost",
		Port:     "5432",
		Database: "testdb",
		Username: "testuser",
		Password: "testpass",
	}

	// This should fail because the container doesn't exist
	_, err := checker.GetDatabaseSize(ctx, "nonexistent-container", dbConfig)
	if err == nil {
		t.Error("Expected error for nonexistent container, got nil")
	}
}
