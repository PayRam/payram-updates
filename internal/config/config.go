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

// Load reads configuration from environment variables first, then falls back to
// /etc/payram/updater.env. Required fields are validated.
func Load() (*Config, error) {
	// Try to load from env file if it exists
	envFilePath := "/etc/payram/updater.env"
	if _, err := os.Stat(envFilePath); err == nil {
		if err := loadEnvFile(envFilePath); err != nil {
			return nil, fmt.Errorf("failed to load env file: %w", err)
		}
	}

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
