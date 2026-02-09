package config

import (
	"os"
	"testing"
)

// TestLoad_RequiredFields tests that required configuration fields are validated.
// STATELESS DESIGN: Only Policy URL and Manifest URL are required.
// No installation config paths are required.
func TestLoad_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		envVars map[string]string
		wantErr bool
		errMsg  string
	}{
		{
			name: "missing POLICY_URL",
			envVars: map[string]string{
				"RUNTIME_MANIFEST_URL": "https://example.com/manifest",
			},
			wantErr: true,
			errMsg:  "POLICY_URL is required",
		},
		{
			name: "missing RUNTIME_MANIFEST_URL",
			envVars: map[string]string{
				"POLICY_URL": "https://example.com/policy",
			},
			wantErr: true,
			errMsg:  "RUNTIME_MANIFEST_URL is required",
		},
		{
			name: "all required fields present",
			envVars: map[string]string{
				"POLICY_URL":           "https://example.com/policy",
				"RUNTIME_MANIFEST_URL": "https://example.com/manifest",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Clearenv()
			for k, v := range tt.envVars {
				os.Setenv(k, v)
			}

			cfg, err := Load()

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error %q, got nil", tt.errMsg)
				} else if err.Error() != tt.errMsg {
					t.Errorf("expected error %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if cfg == nil {
					t.Error("expected non-nil config")
				}
			}
		})
	}
}

func TestLoad_Defaults(t *testing.T) {
	os.Clearenv()
	os.Setenv("POLICY_URL", "https://example.com/policy")
	os.Setenv("RUNTIME_MANIFEST_URL", "https://example.com/manifest")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != 2567 {
		t.Errorf("expected default port 2567, got %d", cfg.Port)
	}
	if cfg.ExecutionMode != "dry-run" {
		t.Errorf("expected default execution mode 'dry-run', got %s", cfg.ExecutionMode)
	}
	if cfg.StateDir != "/var/lib/payram-updater" {
		t.Errorf("expected default state dir '/var/lib/payram-updater', got %s", cfg.StateDir)
	}
	if cfg.AutoUpdateEnabled != DefaultAutoUpdateEnabled {
		t.Errorf("expected default auto update enabled %v, got %v", DefaultAutoUpdateEnabled, cfg.AutoUpdateEnabled)
	}
	if cfg.AutoUpdateInterval != DefaultAutoUpdateIntervalHours {
		t.Errorf("expected default auto update interval %d, got %d", DefaultAutoUpdateIntervalHours, cfg.AutoUpdateInterval)
	}
}

func TestLoad_BackupDefaults(t *testing.T) {
	os.Clearenv()
	os.Setenv("POLICY_URL", "https://example.com/policy")
	os.Setenv("RUNTIME_MANIFEST_URL", "https://example.com/manifest")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify backup defaults
	// Backups are always enabled
	if cfg.Backup.Dir != "data/backups" {
		t.Errorf("expected default backup dir 'data/backups', got %s", cfg.Backup.Dir)
	}
	if cfg.Backup.Retention != 10 {
		t.Errorf("expected default retention 10, got %d", cfg.Backup.Retention)
	}
	if cfg.Backup.PGHost != "127.0.0.1" {
		t.Errorf("expected default PG_HOST '127.0.0.1', got %s", cfg.Backup.PGHost)
	}
	if cfg.Backup.PGPort != 5432 {
		t.Errorf("expected default PG_PORT 5432, got %d", cfg.Backup.PGPort)
	}
	if cfg.Backup.PGDB != "payram" {
		t.Errorf("expected default PG_DB 'payram', got %s", cfg.Backup.PGDB)
	}
	if cfg.Backup.PGUser != "payram" {
		t.Errorf("expected default PG_USER 'payram', got %s", cfg.Backup.PGUser)
	}
}

func TestValidateBackupConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid backup config",
			config: Config{
				Backup: BackupConfig{
					Dir:        "/var/lib/backups",
					Retention:  10,
					PGHost:     "localhost",
					PGPort:     5432,
					PGDB:       "mydb",
					PGUser:     "user",
					PGPassword: "pass",
				},
			},
			wantErr: false,
		},
		{
			name: "missing backup dir",
			config: Config{
				Backup: BackupConfig{
					Dir:       "",
					Retention: 10,
					PGHost:    "localhost",
					PGPort:    5432,
					PGDB:      "mydb",
					PGUser:    "user",
				},
			},
			wantErr: true,
			errMsg:  "BACKUP_DIR is required when backup is enabled",
		},
		{
			name: "retention too low",
			config: Config{
				Backup: BackupConfig{
					Dir:       "/backups",
					Retention: 0,
					PGHost:    "localhost",
					PGPort:    5432,
					PGDB:      "mydb",
					PGUser:    "user",
				},
			},
			wantErr: true,
			errMsg:  "BACKUP_RETENTION must be at least 1, got 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.ValidateBackupConfig()

			if tt.wantErr {
				if err == nil {
					t.Error("expected error but got none")
				} else if err.Error() != tt.errMsg {
					t.Errorf("expected error %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}
