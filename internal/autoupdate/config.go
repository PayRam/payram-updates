package autoupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings stores auto update configuration.
type Settings struct {
	AutoUpdateEnabled       bool `json:"auto_update_enabled"`
	AutoUpdateIntervalHours int  `json:"auto_update_interval_hours"`
	Initialized             bool `json:"initialized"`
}

// DefaultPath returns the default path for auto update configuration.
func DefaultPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home dir: %w", err)
	}
	return filepath.Join(homeDir, ".payram-updates.config"), nil
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
func Save(path string, settings *Settings) error {
	if settings.AutoUpdateEnabled && settings.AutoUpdateIntervalHours < 1 {
		return fmt.Errorf("auto_update_interval_hours must be at least 1 when auto updates are enabled")
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
