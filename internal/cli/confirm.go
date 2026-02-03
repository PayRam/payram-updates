// Package cli provides shared helpers for CLI commands.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ConfirmResult represents the result of a confirmation prompt.
type ConfirmResult int

const (
	// ConfirmYes means the user confirmed.
	ConfirmYes ConfirmResult = iota
	// ConfirmNo means the user declined.
	ConfirmNo
	// ConfirmNonInteractive means stdin is not a TTY and --yes was not set.
	ConfirmNonInteractive
)

// UpgradeSummary contains the information to display in the confirmation prompt.
type UpgradeSummary struct {
	Mode            string
	RequestedTarget string
	ResolvedTarget  string
	ImageRepo       string
	ContainerName   string
}

// Confirmer handles interactive confirmation prompts.
type Confirmer struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// IsTTY is a function that returns true if stdin is a TTY.
	// This allows for testing by injecting a mock function.
	IsTTY func() bool
}

// NewConfirmer creates a new Confirmer with default stdin/stdout/stderr.
func NewConfirmer() *Confirmer {
	return &Confirmer{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		IsTTY:  defaultIsTTY,
	}
}

// defaultIsTTY checks if stdin is a TTY.
func defaultIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// Confirm prompts the user for confirmation before running an upgrade.
// Returns ConfirmYes if confirmed, ConfirmNo if declined, or ConfirmNonInteractive
// if stdin is not a TTY and yesFlag is false.
func (c *Confirmer) Confirm(summary *UpgradeSummary, yesFlag bool) ConfirmResult {
	// If --yes flag is set, skip prompt
	if yesFlag {
		return ConfirmYes
	}

	// Check if stdin is a TTY
	if !c.IsTTY() {
		return ConfirmNonInteractive
	}

	// Print summary
	c.printSummary(summary)

	// Prompt for confirmation
	fmt.Fprint(c.Stdout, "Proceed? (y/N): ")

	reader := bufio.NewReader(c.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		// EOF or error - treat as "no"
		fmt.Fprintln(c.Stdout)
		return ConfirmNo
	}

	input = strings.TrimSpace(strings.ToLower(input))
	if input == "y" || input == "yes" {
		return ConfirmYes
	}

	return ConfirmNo
}

// printSummary prints the upgrade summary to stdout.
func (c *Confirmer) printSummary(summary *UpgradeSummary) {
	fmt.Fprintln(c.Stdout)
	fmt.Fprintln(c.Stdout, "╔══════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(c.Stdout, "║                     UPGRADE SUMMARY                          ║")
	fmt.Fprintln(c.Stdout, "╠══════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(c.Stdout, "║  Mode:             %-40s  ║\n", summary.Mode)
	fmt.Fprintf(c.Stdout, "║  Requested Target: %-40s  ║\n", summary.RequestedTarget)
	if summary.ResolvedTarget != "" && summary.ResolvedTarget != summary.RequestedTarget {
		fmt.Fprintf(c.Stdout, "║  Resolved Target:  %-40s  ║\n", summary.ResolvedTarget)
	}
	if summary.ImageRepo != "" {
		fmt.Fprintf(c.Stdout, "║  Image:            %-40s  ║\n", summary.ImageRepo)
	}
	if summary.ContainerName != "" {
		fmt.Fprintf(c.Stdout, "║  Container:        %-40s  ║\n", summary.ContainerName)
	}
	fmt.Fprintln(c.Stdout, "╠══════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(c.Stdout, "║  ⚠️  This will stop and replace the container.               ║")
	fmt.Fprintln(c.Stdout, "║     Brief downtime expected.                                 ║")
	if summary.Mode == "DASHBOARD" {
		fmt.Fprintln(c.Stdout, "║                                                              ║")
		fmt.Fprintln(c.Stdout, "║  ℹ️  Dashboard upgrades may be blocked by policy breakpoints.║")
	}
	fmt.Fprintln(c.Stdout, "╚══════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(c.Stdout)
}

// ConfirmOrExit is a convenience function that handles the confirmation result
// and exits appropriately. It returns true if the user confirmed.
// If the user declines, it prints "Aborted by user." and exits with code 0.
// If non-interactive without --yes, it prints an error and exits with code 2.
func (c *Confirmer) ConfirmOrExit(summary *UpgradeSummary, yesFlag bool) bool {
	result := c.Confirm(summary, yesFlag)

	switch result {
	case ConfirmYes:
		return true
	case ConfirmNo:
		fmt.Fprintln(c.Stdout, "Aborted by user.")
		os.Exit(0)
	case ConfirmNonInteractive:
		fmt.Fprintln(c.Stderr, "ERROR: refusing to run without confirmation in non-interactive mode. Re-run with --yes.")
		os.Exit(2)
	}

	return false // unreachable
}
