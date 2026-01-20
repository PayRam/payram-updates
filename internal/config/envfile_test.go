package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantVars map[string]string
		wantErr  bool
	}{
		{
			name: "simple key=value",
			content: `KEY1=value1
KEY2=value2`,
			wantVars: map[string]string{
				"KEY1": "value1",
				"KEY2": "value2",
			},
			wantErr: false,
		},
		{
			name: "quoted values",
			content: `KEY1="value with spaces"
KEY2="value2"`,
			wantVars: map[string]string{
				"KEY1": "value with spaces",
				"KEY2": "value2",
			},
			wantErr: false,
		},
		{
			name: "comments and blank lines",
			content: `# This is a comment
KEY1=value1

# Another comment
KEY2=value2

`,
			wantVars: map[string]string{
				"KEY1": "value1",
				"KEY2": "value2",
			},
			wantErr: false,
		},
		{
			name: "mixed formats",
			content: `KEY1=value1
KEY2="quoted value"
# comment
KEY3=value3`,
			wantVars: map[string]string{
				"KEY1": "value1",
				"KEY2": "quoted value",
				"KEY3": "value3",
			},
			wantErr: false,
		},
		{
			name:     "invalid format missing equals",
			content:  `INVALID LINE WITHOUT EQUALS`,
			wantVars: nil,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			tmpFile := filepath.Join(tmpDir, "test.env")
			if err := os.WriteFile(tmpFile, []byte(tt.content), 0644); err != nil {
				t.Fatalf("failed to create temp file: %v", err)
			}

			os.Clearenv()

			err := loadEnvFile(tmpFile)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for k, v := range tt.wantVars {
				got := os.Getenv(k)
				if got != v {
					t.Errorf("expected %s=%q, got %q", k, v, got)
				}
			}
		})
	}
}

func TestLoadEnvFile_EnvVarsPrecedence(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test.env")
	content := `KEY1=from_file
KEY2=from_file`
	if err := os.WriteFile(tmpFile, []byte(content), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	os.Clearenv()
	os.Setenv("KEY1", "from_env")

	if err := loadEnvFile(tmpFile); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := os.Getenv("KEY1"); got != "from_env" {
		t.Errorf("expected KEY1 to remain as env value 'from_env', got %q", got)
	}

	if got := os.Getenv("KEY2"); got != "from_file" {
		t.Errorf("expected KEY2 to be 'from_file', got %q", got)
	}
}
