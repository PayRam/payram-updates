package config

import (
	"fmt"
	"os"
	"strconv"
)

// BackupConfig holds configuration for database backups.
// Backups are always enabled.
type BackupConfig struct {
	Dir        string
	Retention  int
	PGHost     string
	PGPort     int
	PGDB       string
	PGUser     string
	PGPassword string
}

const (
	// DefaultAutoUpdateEnabled controls the default auto-update setting.
	// Change this constant to flip the default behavior globally.
	DefaultAutoUpdateEnabled = false
	// DefaultAutoUpdateIntervalHours is the default check interval in hours.
	DefaultAutoUpdateIntervalHours = 24
)

// Config holds all configuration for the payram-updater service.
// STATELESS DESIGN: This updater does not persist runtime configuration.
// Container runtime details (ports, env vars, mounts, networks) are discovered
// via Docker inspection and overlaid with manifest settings. Only job state,
// logs, and backups are persisted.
type Config struct {
	Port                int
	PolicyURL           string
	RuntimeManifestURL  string
	FetchTimeoutSeconds int
	StateDir            string // For job state persistence only
	LogDir              string // For log persistence only
	CoreBaseURL         string
	ExecutionMode       string
	DockerBin           string
	TargetContainerName string // Optional: overrides manifest container_name
	ImageRepoOverride   string // Optional: for testing with different image repos (e.g., payram-dummy)
	DebugVersionMode    bool   // When true, allows arbitrary version names and uses release list ordering
	AutoUpdateEnabled   bool
	AutoUpdateInterval  int // Hours
	Backup              BackupConfig
}

// Load reads configuration with the following precedence order:
//  1. OS environment variables (highest priority)
//  2. .env file in current working directory (if present)
//  3. /etc/payram/updater.env (if present)
//  4. Default values (lowest priority)
//
// Required fields are validated.
func Load() (*Config, error) {
	// Load config files in reverse precedence order (lowest to highest priority)
	// so that higher priority sources can override lower priority ones.

	// Try to load from /etc/payram/updater.env if it exists (lowest priority file)
	etcEnvFilePath := "/etc/payram/updater.env"
	if _, err := os.Stat(etcEnvFilePath); err == nil {
		if err := loadEnvFile(etcEnvFilePath); err != nil {
			return nil, fmt.Errorf("failed to load env file: %w", err)
		}
	}

	// Try to load from .env in current working directory if it exists (higher priority)
	cwdEnvFilePath := ".env"
	if _, err := os.Stat(cwdEnvFilePath); err == nil {
		if err := loadEnvFile(cwdEnvFilePath); err != nil {
			return nil, fmt.Errorf("failed to load .env file: %w", err)
		}
	}

	// Build config from environment variables (OS env vars have highest priority)
	cfg := &Config{
		Port:                getEnvInt("UPDATER_PORT", 2567),
		PolicyURL:           os.Getenv("POLICY_URL"),
		RuntimeManifestURL:  os.Getenv("RUNTIME_MANIFEST_URL"),
		FetchTimeoutSeconds: getEnvInt("FETCH_TIMEOUT_SECONDS", 10),
		StateDir:            getEnvString("STATE_DIR", "/var/lib/payram-updater"),
		LogDir:              getEnvString("LOG_DIR", "/var/log/payram-updater"),
		CoreBaseURL:         os.Getenv("CORE_BASE_URL"), // Optional: will be discovered if not provided
		ExecutionMode:       getEnvString("EXECUTION_MODE", "dry-run"),
		DockerBin:           getEnvString("DOCKER_BIN", "docker"),
		TargetContainerName: os.Getenv("TARGET_CONTAINER_NAME"), // Optional: no default
		ImageRepoOverride:   os.Getenv("IMAGE_REPO_OVERRIDE"),   // Optional: for testing (e.g., "payram-dummy")
		DebugVersionMode:    getEnvString("DEBUG_VERSION_MODE", "") == "false",
		AutoUpdateEnabled:   DefaultAutoUpdateEnabled,
		AutoUpdateInterval:  DefaultAutoUpdateIntervalHours,
		Backup: BackupConfig{
			Dir:        getEnvString("BACKUP_DIR", "data/backups"),
			Retention:  getEnvInt("BACKUP_RETENTION", 10),
			PGHost:     getEnvString("PG_HOST", "127.0.0.1"),
			PGPort:     getEnvInt("PG_PORT", 5432),
			PGDB:       getEnvString("PG_DB", "payram"),
			PGUser:     getEnvString("PG_USER", "payram"),
			PGPassword: getEnvString("PG_PASSWORD", ""),
		},
	}

	// Validate required fields
	if cfg.PolicyURL == "" {
		return nil, fmt.Errorf("POLICY_URL is required")
	}
	if cfg.RuntimeManifestURL == "" {
		return nil, fmt.Errorf("RUNTIME_MANIFEST_URL is required")
	}

	// Validate EXECUTION_MODE
	if cfg.ExecutionMode != "dry-run" && cfg.ExecutionMode != "execute" {
		return nil, fmt.Errorf("EXECUTION_MODE must be 'dry-run' or 'execute', got '%s'", cfg.ExecutionMode)
	}

	if cfg.AutoUpdateEnabled && cfg.AutoUpdateInterval < 1 {
		return nil, fmt.Errorf("AUTO_UPDATE_INTERVAL_HOURS must be at least 1 when auto update is enabled, got %d", cfg.AutoUpdateInterval)
	}

	return cfg, nil
}

// getEnvString returns the environment variable value or a default.
func getEnvString(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// getEnvInt returns the environment variable as an integer or a default.
func getEnvInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

// getEnvBool returns the environment variable as a boolean or a default.
// Accepts "true", "1", "yes" (case-insensitive) as true; everything else is false.
func getEnvBool(key string, defaultValue bool) bool {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	switch valueStr {
	case "true", "TRUE", "True", "1", "yes", "YES", "Yes":
		return true
	case "false", "FALSE", "False", "0", "no", "NO", "No":
		return false
	default:
		return defaultValue
	}
}

// ValidateBackupConfig validates backup configuration.
// Backups are always enabled, so all fields must be valid.
func (c *Config) ValidateBackupConfig() error {
	// Backups are always enabled, validate all fields
	if c.Backup.Dir == "" {
		return fmt.Errorf("BACKUP_DIR is required when backup is enabled")
	}

	if c.Backup.Retention < 1 {
		return fmt.Errorf("BACKUP_RETENTION must be at least 1, got %d", c.Backup.Retention)
	}

	if c.Backup.PGHost == "" {
		return fmt.Errorf("PG_HOST is required when backup is enabled")
	}

	if c.Backup.PGPort < 1 || c.Backup.PGPort > 65535 {
		return fmt.Errorf("PG_PORT must be between 1 and 65535, got %d", c.Backup.PGPort)
	}

	if c.Backup.PGDB == "" {
		return fmt.Errorf("PG_DB is required when backup is enabled")
	}

	if c.Backup.PGUser == "" {
		return fmt.Errorf("PG_USER is required when backup is enabled")
	}

	// Note: PG_PASSWORD can be empty if using .pgpass or trust auth

	return nil
}
