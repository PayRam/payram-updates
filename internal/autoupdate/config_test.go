package autoupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	settings := &Settings{
		AutoUpdateEnabled:       true,
		AutoUpdateIntervalHours: 6,
		Initialized:             true,
	}

	if err := Save(path, settings); err != nil {
		t.Fatalf("failed to save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	if loaded.AutoUpdateEnabled != settings.AutoUpdateEnabled {
		t.Errorf("expected AutoUpdateEnabled %v, got %v", settings.AutoUpdateEnabled, loaded.AutoUpdateEnabled)
	}
	if loaded.AutoUpdateIntervalHours != settings.AutoUpdateIntervalHours {
		t.Errorf("expected AutoUpdateIntervalHours %d, got %d", settings.AutoUpdateIntervalHours, loaded.AutoUpdateIntervalHours)
	}
	if loaded.Initialized != settings.Initialized {
		t.Errorf("expected Initialized %v, got %v", settings.Initialized, loaded.Initialized)
	}
}

func TestSave_Validation(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.json")

	settings := &Settings{
		AutoUpdateEnabled:       true,
		AutoUpdateIntervalHours: 0,
		Initialized:             true,
	}

	if err := Save(path, settings); err == nil {
		t.Fatal("expected error for invalid interval")
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected no file written, got err=%v", err)
	}
}
