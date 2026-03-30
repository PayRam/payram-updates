package autoupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings stores auto update configuration.
type Settings struct {
	AutoUpdateEnabled       bool `json:"autoUpdateEnabled"`
	AutoUpdateIntervalHours int  `json:"autoUpdateIntervalHours"`
	Initialized             bool `json:"initialized"`
}

// DefaultStateDir is the default location for updater state.
const DefaultStateDir = "/var/lib/payram"

// DefaultPath returns the default path for auto update configuration.
// Uses STATE_DIR env var if set, otherwise falls back to /var/lib/payram.
func DefaultPath() (string, error) {
	stateDir := os.Getenv("STATE_DIR")
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	return filepath.Join(stateDir, "updater-config.json"), nil
}

// Load reads settings from the provided path.
func Load(path string) (*Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var settings Settings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("failed to parse auto update config: %w", err)
	}

	return &settings, nil
}

// Save writes settings to the provided path.
// Creates parent directories if they don't exist.
func Save(path string, settings *Settings) error {
	if settings.AutoUpdateEnabled && settings.AutoUpdateIntervalHours < 1 {
		return fmt.Errorf("auto_update_interval_hours must be at least 1 when auto updates are enabled")
	}

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode auto update config: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write auto update config: %w", err)
	}

	return nil
}
