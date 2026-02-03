package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfirm_YesFlag(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, true)

	if result != ConfirmYes {
		t.Errorf("expected ConfirmYes when --yes flag is set, got %v", result)
	}
}

func TestConfirm_YesFlagSkipsPrompt(t *testing.T) {
	stdout := &bytes.Buffer{}
	c := &Confirmer{
		Stdin:  strings.NewReader(""),
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	c.Confirm(summary, true)

	// With --yes, nothing should be printed
	if stdout.Len() != 0 {
		t.Errorf("expected no output with --yes flag, got %q", stdout.String())
	}
}

func TestConfirm_TTY_UserConfirmsY(t *testing.T) {
	stdout := &bytes.Buffer{}
	c := &Confirmer{
		Stdin:  strings.NewReader("y\n"),
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "MANUAL",
		RequestedTarget: "v1.5.0",
		ResolvedTarget:  "v1.5.0",
		ImageRepo:       "ghcr.io/payram/runtime",
		ContainerName:   "payram-core",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmYes {
		t.Errorf("expected ConfirmYes when user enters 'y', got %v", result)
	}

	// Verify summary was printed
	output := stdout.String()
	if !strings.Contains(output, "UPGRADE SUMMARY") {
		t.Error("expected summary to be printed")
	}
	if !strings.Contains(output, "MANUAL") {
		t.Error("expected mode to be in summary")
	}
	if !strings.Contains(output, "v1.5.0") {
		t.Error("expected target to be in summary")
	}
	if !strings.Contains(output, "Proceed? (y/N):") {
		t.Error("expected prompt to be shown")
	}
}

func TestConfirm_TTY_UserConfirmsYes(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader("yes\n"),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmYes {
		t.Errorf("expected ConfirmYes when user enters 'yes', got %v", result)
	}
}

func TestConfirm_TTY_UserConfirmsUpperY(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader("Y\n"),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmYes {
		t.Errorf("expected ConfirmYes when user enters 'Y', got %v", result)
	}
}

func TestConfirm_TTY_UserDeclinesN(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader("n\n"),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmNo {
		t.Errorf("expected ConfirmNo when user enters 'n', got %v", result)
	}
}

func TestConfirm_TTY_UserDeclinesEmpty(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader("\n"),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmNo {
		t.Errorf("expected ConfirmNo when user presses enter, got %v", result)
	}
}

func TestConfirm_TTY_UserDeclinesAnything(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader("maybe\n"),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmNo {
		t.Errorf("expected ConfirmNo for any input other than y/yes, got %v", result)
	}
}

func TestConfirm_NonTTY_NoYesFlag(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader("y\n"), // Even if input is y, should fail
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return false }, // Not a TTY
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmNonInteractive {
		t.Errorf("expected ConfirmNonInteractive when stdin is not TTY and --yes is false, got %v", result)
	}
}

func TestConfirm_NonTTY_WithYesFlag(t *testing.T) {
	c := &Confirmer{
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return false }, // Not a TTY
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, true) // --yes is set

	if result != ConfirmYes {
		t.Errorf("expected ConfirmYes when --yes flag is set even without TTY, got %v", result)
	}
}

func TestConfirm_TTY_EOF(t *testing.T) {
	// Simulate EOF (e.g., Ctrl+D)
	c := &Confirmer{
		Stdin:  strings.NewReader(""), // No newline = EOF on ReadString
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
	}

	result := c.Confirm(summary, false)

	if result != ConfirmNo {
		t.Errorf("expected ConfirmNo on EOF, got %v", result)
	}
}

func TestPrintSummary_DashboardMode(t *testing.T) {
	stdout := &bytes.Buffer{}
	c := &Confirmer{
		Stdin:  strings.NewReader("n\n"),
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "DASHBOARD",
		RequestedTarget: "v1.7.0",
		ResolvedTarget:  "v1.7.0",
		ImageRepo:       "ghcr.io/payram/runtime",
		ContainerName:   "payram-core",
	}

	c.Confirm(summary, false)

	output := stdout.String()

	// Dashboard mode should show policy breakpoint warning
	if !strings.Contains(output, "Dashboard upgrades may be blocked by policy breakpoints") {
		t.Error("expected dashboard mode to show policy breakpoint warning")
	}
	if !strings.Contains(output, "stop and replace the container") {
		t.Error("expected downtime warning")
	}
}

func TestPrintSummary_ManualMode(t *testing.T) {
	stdout := &bytes.Buffer{}
	c := &Confirmer{
		Stdin:  strings.NewReader("n\n"),
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "MANUAL",
		RequestedTarget: "v1.5.0",
		ResolvedTarget:  "v1.5.0",
	}

	c.Confirm(summary, false)

	output := stdout.String()

	// Manual mode should NOT show policy breakpoint warning
	if strings.Contains(output, "Dashboard upgrades may be blocked by policy breakpoints") {
		t.Error("manual mode should NOT show policy breakpoint warning")
	}
	// But should still show downtime warning
	if !strings.Contains(output, "stop and replace the container") {
		t.Error("expected downtime warning")
	}
}

func TestPrintSummary_DifferentResolvedTarget(t *testing.T) {
	stdout := &bytes.Buffer{}
	c := &Confirmer{
		Stdin:  strings.NewReader("n\n"),
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "MANUAL",
		RequestedTarget: "latest",
		ResolvedTarget:  "v1.8.0",
	}

	c.Confirm(summary, false)

	output := stdout.String()

	// Should show both requested and resolved targets
	if !strings.Contains(output, "latest") {
		t.Error("expected requested target 'latest' in output")
	}
	if !strings.Contains(output, "v1.8.0") {
		t.Error("expected resolved target 'v1.8.0' in output")
	}
	if !strings.Contains(output, "Resolved Target:") {
		t.Error("expected 'Resolved Target:' label when different from requested")
	}
}

func TestPrintSummary_SameResolvedTarget(t *testing.T) {
	stdout := &bytes.Buffer{}
	c := &Confirmer{
		Stdin:  strings.NewReader("n\n"),
		Stdout: stdout,
		Stderr: &bytes.Buffer{},
		IsTTY:  func() bool { return true },
	}

	summary := &UpgradeSummary{
		Mode:            "MANUAL",
		RequestedTarget: "v1.5.0",
		ResolvedTarget:  "v1.5.0", // Same as requested
	}

	c.Confirm(summary, false)

	output := stdout.String()

	// Should NOT show "Resolved Target:" when same as requested
	if strings.Contains(output, "Resolved Target:") {
		t.Error("should NOT show 'Resolved Target:' when same as requested")
	}
}

func TestConfirmResultValues(t *testing.T) {
	// Ensure the constants have expected values
	if ConfirmYes != 0 {
		t.Errorf("expected ConfirmYes to be 0, got %d", ConfirmYes)
	}
	if ConfirmNo != 1 {
		t.Errorf("expected ConfirmNo to be 1, got %d", ConfirmNo)
	}
	if ConfirmNonInteractive != 2 {
		t.Errorf("expected ConfirmNonInteractive to be 2, got %d", ConfirmNonInteractive)
	}
}
