package config

import (
	"os"
	"testing"
)

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
					t.Errorf("expected error but got none")
				} else if err.Error() != tt.errMsg {
					t.Errorf("expected error %q, got %q", tt.errMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if cfg == nil {
					t.Errorf("expected config but got nil")
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

	if cfg.Port != 2359 {
		t.Errorf("expected default port 2359, got %d", cfg.Port)
	}
	if cfg.FetchTimeoutSeconds != 10 {
		t.Errorf("expected default timeout 10, got %d", cfg.FetchTimeoutSeconds)
	}
	if cfg.StateDir != "/var/lib/payram-updater" {
		t.Errorf("expected default state dir, got %s", cfg.StateDir)
	}
	if cfg.LogDir != "/var/log/payram-updater" {
		t.Errorf("expected default log dir, got %s", cfg.LogDir)
	}
}

func TestLoad_CustomValues(t *testing.T) {
	os.Clearenv()
	os.Setenv("POLICY_URL", "https://example.com/policy")
	os.Setenv("RUNTIME_MANIFEST_URL", "https://example.com/manifest")
	os.Setenv("UPDATER_PORT", "8080")
	os.Setenv("FETCH_TIMEOUT_SECONDS", "30")
	os.Setenv("STATE_DIR", "/tmp/state")
	os.Setenv("LOG_DIR", "/tmp/logs")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Port)
	}
	if cfg.FetchTimeoutSeconds != 30 {
		t.Errorf("expected timeout 30, got %d", cfg.FetchTimeoutSeconds)
	}
	if cfg.StateDir != "/tmp/state" {
		t.Errorf("expected state dir /tmp/state, got %s", cfg.StateDir)
	}
	if cfg.LogDir != "/tmp/logs" {
		t.Errorf("expected log dir /tmp/logs, got %s", cfg.LogDir)
	}
}

func TestLoad_Precedence_OSEnvOverridesDotEnv(t *testing.T) {
	// Create temp directory and .env file
	tmpDir := t.TempDir()
	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(tmpDir)

	// Create .env file with specific values
	dotEnvContent := `POLICY_URL=https://dotenv.example.com/policy
RUNTIME_MANIFEST_URL=https://dotenv.example.com/manifest
UPDATER_PORT=3000
`
	if err := os.WriteFile(".env", []byte(dotEnvContent), 0644); err != nil {
		t.Fatalf("failed to create .env file: %v", err)
	}

	// Set OS env vars (should override .env)
	os.Clearenv()
	os.Setenv("POLICY_URL", "https://osenv.example.com/policy")
	os.Setenv("UPDATER_PORT", "4000")
	// Note: RUNTIME_MANIFEST_URL not set in OS env, should come from .env

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// OS env should override .env
	if cfg.PolicyURL != "https://osenv.example.com/policy" {
		t.Errorf("expected policy URL from OS env, got %s", cfg.PolicyURL)
	}
	if cfg.Port != 4000 {
		t.Errorf("expected port from OS env (4000), got %d", cfg.Port)
	}

	// .env should provide value when OS env doesn't have it
	if cfg.RuntimeManifestURL != "https://dotenv.example.com/manifest" {
		t.Errorf("expected manifest URL from .env, got %s", cfg.RuntimeManifestURL)
	}
}

func TestLoad_Precedence_DotEnvOverridesEtcFile(t *testing.T) {
	// This test verifies that .env in cwd is loaded after /etc/payram/updater.env
	// We can't easily test the full precedence with /etc in unit tests,
	// but we can verify that .env is loaded and provides values

	// Create temp directory for working directory with .env
	tmpDir := t.TempDir()
	originalWd, _ := os.Getwd()
	defer os.Chdir(originalWd)
	os.Chdir(tmpDir)

	// Create .env file with specific values
	dotEnvContent := `POLICY_URL=https://dotenv.example.com/policy
RUNTIME_MANIFEST_URL=https://dotenv.example.com/manifest
UPDATER_PORT=6000
FETCH_TIMEOUT_SECONDS=25
`
	if err := os.WriteFile(".env", []byte(dotEnvContent), 0644); err != nil {
		t.Fatalf("failed to create .env file: %v", err)
	}

	// Clear env (no OS env vars, no /etc file in temp dir)
	os.Clearenv()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All values should come from .env
	if cfg.PolicyURL != "https://dotenv.example.com/policy" {
		t.Errorf("expected policy URL from .env, got %s", cfg.PolicyURL)
	}
	if cfg.RuntimeManifestURL != "https://dotenv.example.com/manifest" {
		t.Errorf("expected manifest URL from .env, got %s", cfg.RuntimeManifestURL)
	}
	if cfg.Port != 6000 {
		t.Errorf("expected port from .env (6000), got %d", cfg.Port)
	}
	if cfg.FetchTimeoutSeconds != 25 {
		t.Errorf("expected timeout from .env (25), got %d", cfg.FetchTimeoutSeconds)
	}
}
