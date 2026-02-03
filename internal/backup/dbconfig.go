// Package backup provides database backup and restore functionality.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// DBEnvFile is the path to the persisted database credentials file (relative to backup dir)
	DBEnvFile = "../state/db.env"
	// DBEnvFilePerms is the required file permissions for db.env (0600 = owner read/write only)
	DBEnvFilePerms = 0600
)

// IsLocalDB returns true if the database host is localhost or 127.0.0.1.
// Local databases run inside the container and should have credentials persisted.
func IsLocalDB(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == ""
}

// ContainerDBConfig holds database configuration extracted from a running container.
type ContainerDBConfig struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
	SSLMode  string
}

// IsLocalDB returns true if the database is running locally (inside the container).
func (c *ContainerDBConfig) IsLocalDB() bool {
	host := strings.ToLower(c.Host)
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Validate checks that all required fields are present.
func (c *ContainerDBConfig) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("POSTGRES_HOST not found in container environment")
	}
	if c.Port == "" {
		return fmt.Errorf("POSTGRES_PORT not found in container environment")
	}
	if c.Database == "" {
		return fmt.Errorf("POSTGRES_DB or POSTGRES_DATABASE not found in container environment")
	}
	if c.Username == "" {
		return fmt.Errorf("POSTGRES_USER or POSTGRES_USERNAME not found in container environment")
	}
	// Password can be empty for trust authentication
	return nil
}

// DockerInspector provides methods to inspect Docker containers.
type DockerInspector struct {
	DockerBin string
	Executor  CommandExecutor
}

// NewDockerInspector creates a new DockerInspector.
func NewDockerInspector(dockerBin string, executor CommandExecutor) *DockerInspector {
	if dockerBin == "" {
		dockerBin = "docker"
	}
	if executor == nil {
		executor = &RealExecutor{}
	}
	return &DockerInspector{
		DockerBin: dockerBin,
		Executor:  executor,
	}
}

// CheckDaemon verifies that the Docker daemon is running.
// Returns nil if running, error otherwise.
func (d *DockerInspector) CheckDaemon(ctx context.Context) error {
	output, err := d.Executor.Execute(ctx, d.DockerBin, []string{"info"}, nil)
	if err != nil {
		return fmt.Errorf("docker daemon not running: %w: %s", err, string(output))
	}
	return nil
}

// ContainerExists checks if a container exists (running or stopped).
func (d *DockerInspector) ContainerExists(ctx context.Context, container string) (bool, error) {
	_, err := d.Executor.Execute(ctx, d.DockerBin, []string{"inspect", container}, nil)
	if err != nil {
		// Container doesn't exist - executor returns error for non-zero exit
		return false, nil
	}
	return true, nil
}

// GetContainerEnv extracts environment variables from a running container.
func (d *DockerInspector) GetContainerEnv(ctx context.Context, container string) (map[string]string, error) {
	// Use docker inspect to get container config
	output, err := d.Executor.Execute(ctx, d.DockerBin,
		[]string{"inspect", "--format", "{{json .Config.Env}}", container}, nil)
	if err != nil {
		outputStr := string(output)
		if strings.Contains(outputStr, "No such object") ||
			strings.Contains(outputStr, "No such container") {
			return nil, fmt.Errorf("container not found: %s", container)
		}
		return nil, fmt.Errorf("failed to inspect container: %w: %s", err, outputStr)
	}

	// Parse JSON array of "KEY=VALUE" strings
	var envArray []string
	if err := json.Unmarshal(output, &envArray); err != nil {
		return nil, fmt.Errorf("failed to parse container env: %w", err)
	}

	envMap := make(map[string]string)
	for _, env := range envArray {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	return envMap, nil
}

// GetDBConfig extracts database configuration from a running container.
// It looks for POSTGRES_* environment variables.
// Supports both common naming conventions:
//   - POSTGRES_DB / POSTGRES_DATABASE
//   - POSTGRES_USER / POSTGRES_USERNAME
func (d *DockerInspector) GetDBConfig(ctx context.Context, container string) (*ContainerDBConfig, error) {
	env, err := d.GetContainerEnv(ctx, container)
	if err != nil {
		return nil, err
	}

	// Support both naming conventions for database name
	database := env["POSTGRES_DB"]
	if database == "" {
		database = env["POSTGRES_DATABASE"]
	}

	// Support both naming conventions for username
	username := env["POSTGRES_USER"]
	if username == "" {
		username = env["POSTGRES_USERNAME"]
	}

	config := &ContainerDBConfig{
		Host:     env["POSTGRES_HOST"],
		Port:     env["POSTGRES_PORT"],
		Database: database,
		Username: username,
		Password: env["POSTGRES_PASSWORD"],
		SSLMode:  env["POSTGRES_SSLMODE"],
	}

	// Validate required fields
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}

// DiscoverPayramContainer discovers the running Payram container.
// Returns the container name or error if not found.
func (d *DockerInspector) DiscoverPayramContainer(ctx context.Context) (string, error) {
	// List running containers with Payram image
	output, err := d.Executor.Execute(ctx, d.DockerBin,
		[]string{"ps", "--filter", "ancestor=payram/payram", "--format", "{{.Names}}"}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to list containers: %w: %s", err, string(output))
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", fmt.Errorf("no running Payram container found")
	}

	// Return first container found
	containerName := strings.TrimSpace(lines[0])
	return containerName, nil
}

// PersistDBCredentials writes database credentials to data/state/db.env.
// Only call this for LOCAL databases (localhost/127.0.0.1) after successful backup.
// File is created with 0600 permissions (owner read/write only).
func PersistDBCredentials(backupDir string, config *ContainerDBConfig) error {
	// Only persist for local databases
	if !IsLocalDB(config.Host) {
		return fmt.Errorf("refusing to persist credentials for non-local database: %s", config.Host)
	}

	// Ensure state directory exists
	stateDir := filepath.Join(backupDir, "../state")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	dbEnvPath := filepath.Join(backupDir, DBEnvFile)

	// Build env file content
	content := fmt.Sprintf("POSTGRES_HOST=%s\n", config.Host)
	content += fmt.Sprintf("POSTGRES_PORT=%s\n", config.Port)
	content += fmt.Sprintf("POSTGRES_DATABASE=%s\n", config.Database)
	content += fmt.Sprintf("POSTGRES_USERNAME=%s\n", config.Username)
	content += fmt.Sprintf("POSTGRES_PASSWORD=%s\n", config.Password)
	if config.SSLMode != "" {
		content += fmt.Sprintf("POSTGRES_SSLMODE=%s\n", config.SSLMode)
	}

	// Write with restricted permissions
	if err := os.WriteFile(dbEnvPath, []byte(content), DBEnvFilePerms); err != nil {
		return fmt.Errorf("failed to write db.env: %w", err)
	}

	return nil
}

// LoadPersistedCredentials loads database credentials from data/state/db.env.
// Returns error if file doesn't exist or cannot be read.
func LoadPersistedCredentials(backupDir string) (*ContainerDBConfig, error) {
	dbEnvPath := filepath.Join(backupDir, DBEnvFile)

	// Check file exists
	if _, err := os.Stat(dbEnvPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("no persisted credentials found at %s", dbEnvPath)
	}

	// Read file
	content, err := os.ReadFile(dbEnvPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read db.env: %w", err)
	}

	// Parse env vars
	envMap := make(map[string]string)
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	config := &ContainerDBConfig{
		Host:     envMap["POSTGRES_HOST"],
		Port:     envMap["POSTGRES_PORT"],
		Database: envMap["POSTGRES_DATABASE"],
		Username: envMap["POSTGRES_USERNAME"],
		Password: envMap["POSTGRES_PASSWORD"],
		SSLMode:  envMap["POSTGRES_SSLMODE"],
	}

	// Validate required fields
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid persisted credentials: %w", err)
	}

	return config, nil
}
