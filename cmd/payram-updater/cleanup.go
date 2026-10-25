package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/jobs"
)

func runCleanup() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: payram-updater cleanup <state|backups> [--yes] [--force]")
		os.Exit(1)
	}

	subcommand := os.Args[2]
	confirmYes := false
	force := false
	for _, arg := range os.Args[3:] {
		switch arg {
		case "--yes":
			confirmYes = true
		case "--force":
			force = true
		}
	}

	if subcommand != "state" && subcommand != "backups" {
		fmt.Fprintln(os.Stderr, "Invalid cleanup target. Use 'state' or 'backups'.")
		os.Exit(1)
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Block cleanup if a job is active
	jobStore := jobs.NewStore(cfg.StateDir)
	if job, err := jobStore.LoadLatest(); err == nil && job != nil && job.IsActive() {
		fmt.Fprintln(os.Stderr, "Active job in progress. Cleanup is blocked.")
		os.Exit(1)
	}

	targetDir := cfg.StateDir
	if subcommand == "backups" {
		targetDir = cfg.Backup.Dir
	}
	if err := checkCleanupTarget(targetDir, targetDir); err != nil {
		fmt.Fprintf(os.Stderr, "Refusing to clean up %s: %v\n", subcommand, err)
		os.Exit(1)
	}

	if !force {
		if reason := cleanupBlockedByFailedUpdate(cfg.StateDir, cfg.Backup.Dir, subcommand); reason != "" {
			fmt.Fprintln(os.Stderr, reason)
			os.Exit(1)
		}
	}

	// Nothing may be deleted while an update or restore is writing there.
	release, err := jobs.AcquireContainerLock(cfg.StateDir)
	if errors.Is(err, jobs.ErrUpgradeLocked) {
		fmt.Fprintln(os.Stderr, backup.MessageRestoreLocked)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not take the update lock: %v\n", err)
		os.Exit(1)
	}
	defer release()

	// Require confirmation unless --yes was provided
	if !confirmYes {
		what := fmt.Sprintf("the updater's job status, logs and history in %s", targetDir)
		if subcommand == "backups" {
			what = fmt.Sprintf("every PayRam backup file (payram-backup-*) in %s", targetDir)
		}
		fmt.Printf("WARNING: This will delete %s. Type \"yes\" to continue: ", what)
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(input)) != "yes" {
			fmt.Fprintln(os.Stderr, "Cleanup cancelled.")
			release()
			os.Exit(1)
		}
	}

	if subcommand == "backups" {
		removed, err := removeBackupFiles(targetDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to remove backups: %v\n", err)
			release()
			os.Exit(1)
		}
		fmt.Printf("Cleanup complete: backups (%d files removed from %s)\n", removed, targetDir)
		return
	}

	if err := removeUpdaterState(targetDir); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to remove state: %v\n", err)
		release()
		os.Exit(1)
	}
	fmt.Printf("Cleanup complete: %s\n", subcommand)
}

// checkCleanupTarget refuses a directory whose cleanup could destroy more
// than the updater's own files: an empty path, "/", the home directory or
// a directory above it (also when reached through a symlink), and anything
// other than the configured directory.
func checkCleanupTarget(target, configured string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("the directory is not configured")
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %v", target, err)
	}
	configuredAbs, err := filepath.Abs(configured)
	if err != nil || strings.TrimSpace(configured) == "" || abs != configuredAbs {
		return fmt.Errorf("%s is not the configured directory %q", abs, configured)
	}
	paths := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		paths = append(paths, resolved)
	}
	home, _ := os.UserHomeDir()
	for _, p := range paths {
		if p == string(filepath.Separator) {
			return fmt.Errorf("%s is the root directory", target)
		}
		if home != "" && (p == home || isInsideDir(home, p)) {
			return fmt.Errorf("%s is (or contains) the home directory %s", target, home)
		}
	}
	return nil
}

// isInsideDir reports whether path lies strictly inside dir.
func isInsideDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// cleanupBlockedByFailedUpdate explains why a cleanup must not run while the
// latest update FAILED and its backup is the recovery point; empty when it
// may run. Cleaning backups would delete that backup; cleaning state would
// forget which backup it is.
func cleanupBlockedByFailedUpdate(stateDir, backupDir, subcommand string) string {
	failed := backup.FindFailedUpdateBackup(stateDir)
	if failed == "" {
		return ""
	}
	if subcommand == "backups" {
		backupAbs, err1 := filepath.Abs(backupDir)
		failedAbs, err2 := filepath.Abs(failed)
		if err1 != nil || err2 != nil || filepath.Dir(failedAbs) != backupAbs {
			return "" // that backup lives elsewhere and is not touched
		}
	}
	return fmt.Sprintf("The last update failed and its backup %s is the recovery point. "+
		"Recover first (payram-updater recover, or payram-updater backup restore --file %s), "+
		"or re-run with --force to clean up anyway.", failed, failed)
}

// removeBackupFiles deletes the PayRam backup files (finished and partial)
// in dir, and nothing else; the directory stays, restricted to its owner.
func removeBackupFiles(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "payram-backup-") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return removed, err
	}
	return removed, nil
}

// removeUpdaterState deletes the job status, job logs and history the
// updater keeps in stateDir, and nothing else: the directory may be shared
// (STATE_DIR=/var/lib/payram also holds backups and PayRam's data), and it
// keeps the updater's own settings.
func removeUpdaterState(stateDir string) error {
	if err := os.RemoveAll(filepath.Join(stateDir, "jobs")); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(stateDir, "history.jsonl")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
