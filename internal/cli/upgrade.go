// Package cli provides shared helpers for CLI commands.
package cli

import (
	"errors"
	"strings"
)

// UpgradeMode represents the upgrade mode.
type UpgradeMode string

const (
	// ModeDashboard is controlled by the central dashboard.
	ModeDashboard UpgradeMode = "DASHBOARD"
	// ModeManual is initiated by an operator.
	ModeManual UpgradeMode = "MANUAL"
)

// UpgradeRequest represents a validated upgrade request.
type UpgradeRequest struct {
	Mode            UpgradeMode
	RequestedTarget string
}

// Validation errors.
var (
	ErrModeRequired     = errors.New("--mode flag is required (dashboard or manual)")
	ErrTargetRequired   = errors.New("--to flag is required")
	ErrInvalidMode      = errors.New("--mode must be 'dashboard' or 'manual'")
	ErrLatestNotAllowed = errors.New("'latest' is not allowed in dashboard mode; specify an exact version")
)

// ParseUpgradeRequest validates and parses mode and target into an UpgradeRequest.
// It enforces:
// - mode must be "dashboard" or "manual" (case-insensitive)
// - target must not be empty
// - "latest" is only allowed in manual mode
func ParseUpgradeRequest(mode, target string) (*UpgradeRequest, error) {
	// Validate mode is present
	if mode == "" {
		return nil, ErrModeRequired
	}

	// Normalize target first to check for whitespace-only
	normalizedTarget := strings.TrimSpace(target)

	// Validate target is present
	if normalizedTarget == "" {
		return nil, ErrTargetRequired
	}

	// Normalize and validate mode
	upperMode := strings.ToUpper(strings.TrimSpace(mode))
	var parsedMode UpgradeMode
	switch upperMode {
	case "DASHBOARD":
		parsedMode = ModeDashboard
	case "MANUAL":
		parsedMode = ModeManual
	default:
		return nil, ErrInvalidMode
	}

	// Dashboard mode cannot use 'latest'
	if parsedMode == ModeDashboard && strings.ToLower(normalizedTarget) == "latest" {
		return nil, ErrLatestNotAllowed
	}

	return &UpgradeRequest{
		Mode:            parsedMode,
		RequestedTarget: normalizedTarget,
	}, nil
}
