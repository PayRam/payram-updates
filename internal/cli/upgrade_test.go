package cli

import (
	"testing"
)

func TestParseUpgradeRequest(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		target     string
		wantMode   UpgradeMode
		wantTarget string
		wantErr    error
	}{
		{
			name:       "dashboard mode with version",
			mode:       "dashboard",
			target:     "v1.2.3",
			wantMode:   ModeDashboard,
			wantTarget: "v1.2.3",
			wantErr:    nil,
		},
		{
			name:       "manual mode with version",
			mode:       "manual",
			target:     "v1.2.3",
			wantMode:   ModeManual,
			wantTarget: "v1.2.3",
			wantErr:    nil,
		},
		{
			name:       "manual mode with latest",
			mode:       "manual",
			target:     "latest",
			wantMode:   ModeManual,
			wantTarget: "latest",
			wantErr:    nil,
		},
		{
			name:       "dashboard mode uppercase",
			mode:       "DASHBOARD",
			target:     "v1.0.0",
			wantMode:   ModeDashboard,
			wantTarget: "v1.0.0",
			wantErr:    nil,
		},
		{
			name:       "manual mode mixed case",
			mode:       "MaNuAl",
			target:     "v2.0.0",
			wantMode:   ModeManual,
			wantTarget: "v2.0.0",
			wantErr:    nil,
		},
		{
			name:       "mode with whitespace",
			mode:       "  dashboard  ",
			target:     "v1.0.0",
			wantMode:   ModeDashboard,
			wantTarget: "v1.0.0",
			wantErr:    nil,
		},
		{
			name:       "target with whitespace",
			mode:       "manual",
			target:     "  v1.0.0  ",
			wantMode:   ModeManual,
			wantTarget: "v1.0.0",
			wantErr:    nil,
		},
		{
			name:    "empty mode",
			mode:    "",
			target:  "v1.0.0",
			wantErr: ErrModeRequired,
		},
		{
			name:    "empty target",
			mode:    "dashboard",
			target:  "",
			wantErr: ErrTargetRequired,
		},
		{
			name:    "invalid mode",
			mode:    "auto",
			target:  "v1.0.0",
			wantErr: ErrInvalidMode,
		},
		{
			name:    "dashboard mode with latest (rejected)",
			mode:    "dashboard",
			target:  "latest",
			wantErr: ErrLatestNotAllowed,
		},
		{
			name:    "dashboard mode with LATEST uppercase (rejected)",
			mode:    "DASHBOARD",
			target:  "LATEST",
			wantErr: ErrLatestNotAllowed,
		},
		{
			name:       "manual mode with LATEST uppercase (allowed)",
			mode:       "manual",
			target:     "LATEST",
			wantMode:   ModeManual,
			wantTarget: "LATEST",
			wantErr:    nil,
		},
		{
			name:    "both empty",
			mode:    "",
			target:  "",
			wantErr: ErrModeRequired,
		},
		{
			name:    "whitespace only mode",
			mode:    "   ",
			target:  "v1.0.0",
			wantErr: ErrInvalidMode,
		},
		{
			name:    "whitespace only target",
			mode:    "dashboard",
			target:  "   ",
			wantErr: ErrTargetRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := ParseUpgradeRequest(tt.mode, tt.target)

			if tt.wantErr != nil {
				if err == nil {
					t.Errorf("ParseUpgradeRequest() expected error %v, got nil", tt.wantErr)
					return
				}
				if err != tt.wantErr {
					t.Errorf("ParseUpgradeRequest() error = %v, want %v", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("ParseUpgradeRequest() unexpected error: %v", err)
				return
			}

			if req.Mode != tt.wantMode {
				t.Errorf("ParseUpgradeRequest() mode = %v, want %v", req.Mode, tt.wantMode)
			}
			if req.RequestedTarget != tt.wantTarget {
				t.Errorf("ParseUpgradeRequest() target = %v, want %v", req.RequestedTarget, tt.wantTarget)
			}
		})
	}
}

func TestUpgradeModeValues(t *testing.T) {
	// Verify the constant values match what's expected
	if ModeDashboard != "DASHBOARD" {
		t.Errorf("ModeDashboard = %q, want %q", ModeDashboard, "DASHBOARD")
	}
	if ModeManual != "MANUAL" {
		t.Errorf("ModeManual = %q, want %q", ModeManual, "MANUAL")
	}
}

func TestUpgradeRequestStruct(t *testing.T) {
	req := &UpgradeRequest{
		Mode:            ModeDashboard,
		RequestedTarget: "v1.0.0",
	}

	if req.Mode != ModeDashboard {
		t.Errorf("Mode = %v, want %v", req.Mode, ModeDashboard)
	}
	if req.RequestedTarget != "v1.0.0" {
		t.Errorf("RequestedTarget = %v, want v1.0.0", req.RequestedTarget)
	}
}
