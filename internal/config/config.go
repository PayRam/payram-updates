package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all configuration for the payram-updater service.
type Config struct {
	Port                int
	PolicyURL           string
	RuntimeManifestURL  string
	FetchTimeoutSeconds int
	StateDir            string
	LogDir              string
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
		Port:                getEnvInt("UPDATER_PORT", 2359),
		PolicyURL:           os.Getenv("POLICY_URL"),
		RuntimeManifestURL:  os.Getenv("RUNTIME_MANIFEST_URL"),
		FetchTimeoutSeconds: getEnvInt("FETCH_TIMEOUT_SECONDS", 10),
		StateDir:            getEnvString("STATE_DIR", "/var/lib/payram-updater"),
		LogDir:              getEnvString("LOG_DIR", "/var/log/payram-updater"),
	}

	// Validate required fields
	if cfg.PolicyURL == "" {
		return nil, fmt.Errorf("POLICY_URL is required")
	}
	if cfg.RuntimeManifestURL == "" {
		return nil, fmt.Errorf("RUNTIME_MANIFEST_URL is required")
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
