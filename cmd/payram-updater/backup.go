package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
)

// databaseReadyTimeout bounds the wait for the database after a container
// rollback, before the restore starts.
const databaseReadyTimeout = 5 * time.Minute

func runBackup() {
	// Parse backup subcommand
	if len(os.Args) < 3 {
		fmt.Println(`Usage: payram-updater backup <subcommand>

Subcommands:
  create    Create a new database backup
  list      List all available backups
  restore   Restore from a backup file

Examples:
  payram-updater backup create
  payram-updater backup list
  payram-updater backup restore --file /path/to/backup.dump
  payram-updater backup restore --file /path/to/backup.dump --mode db-only --yes`)
		os.Exit(1)
	}

	subcommand := os.Args[2]

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Create backup manager (works without daemon)
	// Backups are always enabled
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}
	backupCfg := backup.Config{
		Dir:                 cfg.Backup.Dir,
		Retention:           cfg.Backup.Retention,
		PGHost:              cfg.Backup.PGHost,
		PGPort:              cfg.Backup.PGPort,
		PGDB:                cfg.Backup.PGDB,
		PGUser:              cfg.Backup.PGUser,
		PGPassword:          cfg.Backup.PGPassword,
		ImagePattern:        imagePattern,
		TargetContainerName: cfg.TargetContainerName,
		StateDir:            cfg.StateDir,
		DockerBin:           cfg.DockerBin,
		SupervisorExclude:   cfg.SupervisorExclude,
	}
	mgr := backup.NewManager(backupCfg, &backup.RealExecutor{}, log.Default())

	switch subcommand {
	case "create":
		runBackupCreate(mgr, cfg)
	case "list":
		runBackupList(mgr)
	case "restore":
		runBackupRestore(mgr, cfg)
	default:
		fmt.Fprintf(os.Stderr, "Unknown backup subcommand: %s\n", subcommand)
		fmt.Println("Available subcommands: create, list, restore")
		os.Exit(1)
	}
}

func runBackupCreate(mgr *backup.Manager, cfg *config.Config) {
	// Backups are always enabled
	fmt.Fprintln(os.Stderr, "Creating database backup...")

	historyStore := history.NewStore(cfg.StateDir)

	ctx := context.Background()
	info, err := mgr.CreateBackup(ctx, backup.BackupMeta{
		FromVersion:   "manual",
		TargetVersion: "manual",
		JobID:         fmt.Sprintf("manual-%d", time.Now().Unix()),
	})
	if err != nil {
		_ = historyStore.Append(history.Event{
			Type:    "backup",
			Status:  "failed",
			Message: err.Error(),
			Data: map[string]string{
				"fromVersion":   "manual",
				"targetVersion": "manual",
			},
		})
		printJSONError(err.Error())
		os.Exit(1)
	}

	_ = historyStore.Append(history.Event{
		Type:    "backup",
		Status:  "succeeded",
		Message: "Backup completed",
		Data: map[string]string{
			"fromVersion":   "manual",
			"targetVersion": "manual",
			"backupPath":    info.Path,
			"sizeBytes":     fmt.Sprintf("%d", info.Size),
		},
	})

	// Prune old backups (never the recovery point of a failed update)
	pruned, _ := mgr.PruneBackups(mgr.Config.Retention)

	response := map[string]interface{}{
		"success": true,
		"backup":  info,
	}
	if len(pruned) > 0 {
		response["pruned_count"] = len(pruned)
	}

	jsonOut, _ := json.MarshalIndent(response, "", "  ")
	fmt.Println(string(jsonOut))
}

func runBackupList(mgr *backup.Manager) {
	backups, err := mgr.ListBackups()
	if err != nil {
		printJSONError(err.Error())
		os.Exit(1)
	}

	// Return JSON matching spec
	response := map[string]interface{}{
		"backups": backups,
		"count":   len(backups),
		"success": true,
	}

	jsonOut, _ := json.MarshalIndent(response, "", "  ")
	fmt.Println(string(jsonOut))
}

// printJSONError prints the CLI's JSON failure response on stdout.
func printJSONError(message string) {
	jsonOut, _ := json.MarshalIndent(map[string]interface{}{
		"success": false,
		"error":   message,
	}, "", "  ")
	fmt.Println(string(jsonOut))
}

// performContainerRollback rolls back the Payram container to a previous version.
// This function:
// 1. Discovers the current running container
// 2. Extracts its runtime state
// 3. Stops and removes the current container
// 4. Runs a new container with the previous version using the existing docker run builder
// 5. Verifies the container is running
func performContainerRollback(ctx context.Context, targetVersion string) error {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Discover running container. Prefer explicit name (handles non-semver tags).
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	var containerName string
	if cfg.TargetContainerName != "" {
		containerName = cfg.TargetContainerName
		log.Printf("Using explicit container name: %s", containerName)
	} else {
		discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, log.Default())
		discovered, err := discoverer.DiscoverPayramContainer(ctx)
		if err != nil {
			return fmt.Errorf("failed to discover running container: %w", err)
		}
		containerName = discovered.Name
		log.Printf("Discovered container: %s (current version: %s)", containerName, discovered.ImageTag)
	}

	// Extract runtime state from current container
	inspector := container.NewInspector(cfg.DockerBin, log.Default())
	runtimeState, err := inspector.ExtractRuntimeState(ctx, containerName)
	if err != nil {
		return fmt.Errorf("failed to extract runtime state: %w", err)
	}
	log.Printf("Extracted runtime state: %d ports, %d mounts, %d env vars",
		len(runtimeState.Ports), len(runtimeState.Mounts), len(runtimeState.Env))

	// Create a minimal manifest for rollback
	// The manifest is used only to specify the target version
	// The runtime state preserves all actual configuration
	manifestData := &manifest.Manifest{
		Image: manifest.Image{
			Repo: strings.Split(runtimeState.Image, ":")[0],
		},
		Defaults: manifest.Defaults{
			ContainerName: runtimeState.Name,
			RestartPolicy: runtimeState.RestartPolicy.Name,
		},
	}

	// Build docker run arguments using the container builder
	builder := container.NewDockerRunBuilder(log.Default())
	dockerArgs, err := builder.BuildUpgradeArgs(runtimeState, manifestData, targetVersion)
	if err != nil {
		return fmt.Errorf("failed to build docker run args: %w", err)
	}

	// Stop and remove current container
	log.Printf("Stopping container: %s", containerName)
	runner := &dockerexec.Runner{DockerBin: cfg.DockerBin, Logger: log.Default()}
	if err := runner.Stop(ctx, containerName); err != nil {
		return fmt.Errorf("failed to stop container: %w", err)
	}

	log.Printf("Removing container: %s", containerName)
	if err := runner.Remove(ctx, containerName); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	// Run new container with previous version
	log.Printf("Starting container with rollback version: %s", targetVersion)
	if err := runner.Run(ctx, dockerArgs); err != nil {
		return fmt.Errorf("failed to run container: %w", err)
	}

	// Wait for container to start
	time.Sleep(5 * time.Second)

	// Verify the container is running
	isRunning, err := runner.InspectRunning(ctx, containerName)
	if err != nil {
		return fmt.Errorf("failed to check container status: %w", err)
	}
	if !isRunning {
		return fmt.Errorf("container is not running after rollback")
	}

	log.Printf("Container rollback completed successfully")
	return nil
}

// restoreMode is what a restore replaces.
type restoreMode string

const (
	// restoreModeDBOnly restores the database and leaves the container as it is.
	restoreModeDBOnly restoreMode = "db-only"
	// restoreModeFull rolls the container back to the backup's version, then
	// restores the database into it.
	restoreModeFull restoreMode = "full"
)

// parseRestoreMode reads --mode and its alias --full-recovery. An empty
// mode means the operator did not choose one.
func parseRestoreMode(modeFlag string, fullRecovery bool) (restoreMode, error) {
	mode := restoreMode(strings.TrimSpace(strings.ToLower(modeFlag)))
	switch mode {
	case "", restoreModeDBOnly, restoreModeFull:
	default:
		return "", fmt.Errorf("unknown --mode %q: use --mode db-only (restore only the database) or --mode full (roll PayRam back to the backup's version too)", modeFlag)
	}
	if fullRecovery {
		if mode == restoreModeDBOnly {
			return "", fmt.Errorf("--full-recovery and --mode db-only contradict each other; pick one")
		}
		mode = restoreModeFull
	}
	return mode, nil
}

// restoreDecision is the outcome of asking the operator what to restore.
type restoreDecision struct {
	Mode     restoreMode
	Proceed  bool
	Message  string // shown when not proceeding
	ExitCode int
}

// restorePrompt holds what decideRestore needs to ask the operator.
type restorePrompt struct {
	Backup       backup.BackupListItem
	Mode         restoreMode // from flags; empty when not given
	Yes          bool        // --yes
	Interactive  bool        // stdin is a terminal
	In           *bufio.Reader
	Out          io.Writer
	SafetyBackup bool // a safety backup will be taken first
}

// decideRestore picks the restore mode and gets confirmation. Every answer
// that is not an explicit choice cancels: Enter, end of input, n, q or
// anything unrecognised, and so does a missing terminal. A restore runs
// without the typed "yes" only when both --yes and an explicit mode were
// passed.
func decideRestore(p restorePrompt) restoreDecision {
	cancel := func(message string, code int) restoreDecision {
		return restoreDecision{Message: message, ExitCode: code}
	}
	if p.Mode == restoreModeFull && !p.Backup.HasUpgradeVersions() {
		return cancel(fmt.Sprintf("This backup does not record which PayRam version took it (from %q to %q: a manual or safety backup), so PayRam cannot be rolled back with it. Nothing was changed. Re-run with --mode db-only to restore only the database.",
			p.Backup.FromVersion, p.Backup.ToVersion), 1)
	}
	if p.Yes && p.Mode != "" {
		return restoreDecision{Mode: p.Mode, Proceed: true}
	}
	if !p.Interactive {
		return cancel("Restore cancelled; nothing was changed. There is no terminal to confirm on: to restore without prompts pass --yes together with --mode db-only (restore only the database) or --mode full (also roll PayRam back to the backup's version).", 2)
	}

	mode := p.Mode
	if mode == "" {
		if p.Backup.HasUpgradeVersions() {
			fmt.Fprintf(p.Out, "\nChoose what to restore:\n")
			fmt.Fprintf(p.Out, "  [1] Restore the database only (PayRam stays on its current version)\n")
			fmt.Fprintf(p.Out, "  [2] Roll PayRam back to %s AND restore the database\n", p.Backup.FromVersion)
			fmt.Fprintf(p.Out, "  [c] Cancel (default)\n")
			fmt.Fprintf(p.Out, "\nEnter choice [1/2/C]: ")
			answer, ok := readAnswer(p.In)
			switch {
			case ok && answer == "1":
				mode = restoreModeDBOnly
			case ok && answer == "2":
				mode = restoreModeFull
			default:
				return cancel("Restore cancelled; nothing was changed.", 0)
			}
		} else {
			fmt.Fprintf(p.Out, "\nThis backup does not record PayRam versions, so only the database can be restored; PayRam stays on its current version.\n")
			mode = restoreModeDBOnly
		}
	}

	if mode == restoreModeFull {
		fmt.Fprintf(p.Out, "\nPayRam will be rolled back to %s, then its database replaced with this backup.\n", p.Backup.FromVersion)
	} else {
		fmt.Fprintf(p.Out, "\nThe database will be replaced with this backup; PayRam stays on its current version.\n")
	}
	if p.SafetyBackup {
		fmt.Fprintf(p.Out, "The current database is backed up first, so this can be undone.\n")
	} else {
		fmt.Fprintf(p.Out, "--no-safety-backup: the current database is NOT backed up first; this cannot be undone.\n")
	}
	fmt.Fprintf(p.Out, "Type 'yes' to continue (anything else cancels): ")
	answer, ok := readAnswer(p.In)
	if !ok || answer != "yes" {
		return cancel("Restore cancelled; nothing was changed.", 0)
	}
	return restoreDecision{Mode: mode, Proceed: true}
}

// readAnswer reads one line, lowercased and trimmed. ok is false at end of
// input with nothing typed, or on a read error: that is never a yes.
func readAnswer(in *bufio.Reader) (string, bool) {
	line, err := in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(line)), true
}

// formatAge says roughly how long ago something happened.
func formatAge(d time.Duration) string {
	switch {
	case d < 0:
		return "in the future; check the server clock"
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// printRestoreSummary tells the operator exactly what they are about to
// restore and what it removes.
func printRestoreSummary(out io.Writer, item backup.BackupListItem, now time.Time) {
	fmt.Fprintf(out, "\nBackup file: %s\n", item.File)
	if taken := item.TakenAt(); !taken.IsZero() {
		fmt.Fprintf(out, "Taken:       %s (%s)\n", taken.UTC().Format("2006-01-02 15:04:05 UTC"), formatAge(now.Sub(taken)))
	}
	if item.HasUpgradeVersions() {
		fmt.Fprintf(out, "Versions:    taken before upgrading from %s to %s\n", item.FromVersion, item.ToVersion)
	} else {
		fmt.Fprintf(out, "Versions:    not recorded (from %q to %q)\n", item.FromVersion, item.ToVersion)
	}
	fmt.Fprintf(out, "\nWARNING: every payment, deposit and payout recorded after this backup was taken\n")
	fmt.Fprintf(out, "will be removed from the database.\n")
}

func runBackupRestore(mgr *backup.Manager, cfg *config.Config) {
	restoreFlags := flag.NewFlagSet("restore", flag.ExitOnError)
	filePath := restoreFlags.String("file", "", "Path to backup file (required)")
	yes := restoreFlags.Bool("yes", false, "Do not ask for confirmation; requires --mode")
	modeFlag := restoreFlags.String("mode", "", "db-only: restore only the database; full: roll PayRam back to the backup's version, then restore")
	fullRecoveryFlag := restoreFlags.Bool("full-recovery", false, "Same as --mode full")
	noSafetyBackup := restoreFlags.Bool("no-safety-backup", false, "Do not back up the current database first (the restore cannot be undone)")

	if err := restoreFlags.Parse(os.Args[3:]); err != nil {
		os.Exit(1)
	}

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "Error: --file is required")
		fmt.Fprintln(os.Stderr, "Usage: payram-updater backup restore --file /path/to/backup.dump [--mode db-only|full] [--yes] [--no-safety-backup]")
		os.Exit(1)
	}
	mode, err := parseRestoreMode(*modeFlag, *fullRecoveryFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if err := mgr.VerifyBackupFile(*filePath); err != nil {
		printJSONError(err.Error())
		os.Exit(1)
	}
	item, err := backup.DescribeBackupFile(*filePath)
	if err != nil {
		printJSONError(err.Error())
		os.Exit(1)
	}

	// Ctrl-C, a closed SSH session or a stop signal end the restore cleanly:
	// the transaction rolls back and PayRam's programs are started again.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	historyStore := history.NewStore(cfg.StateDir)
	latestJob, _ := jobs.NewStore(cfg.StateDir).LoadLatest()

	printRestoreSummary(os.Stderr, item, time.Now())

	// Check the file is complete before asking anything.
	if err := mgr.CheckBackupFile(ctx, item.File, ""); err != nil {
		printJSONError(fmt.Sprintf("The backup file cannot be restored and nothing was changed: %v", err))
		os.Exit(1)
	}

	decision := decideRestore(restorePrompt{
		Backup:       item,
		Mode:         mode,
		Yes:          *yes,
		Interactive:  term.IsTerminal(int(os.Stdin.Fd())),
		In:           bufio.NewReader(os.Stdin),
		Out:          os.Stderr,
		SafetyBackup: !*noSafetyBackup,
	})
	if !decision.Proceed {
		fmt.Fprintln(os.Stderr, decision.Message)
		os.Exit(decision.ExitCode)
	}
	fullRecovery := decision.Mode == restoreModeFull

	if fullRecovery && isSuccessfulUpgradeJob(latestJob) {
		printJSONError("Rollback is blocked because the latest upgrade completed successfully. Re-run restore with --mode db-only.")
		os.Exit(1)
	}

	// One restore or update at a time, for the whole restore (rollback included).
	release, err := jobs.AcquireContainerLock(cfg.StateDir)
	if errors.Is(err, jobs.ErrUpgradeLocked) {
		printJSONError(backup.MessageRestoreLocked)
		os.Exit(1)
	}
	if err != nil {
		printJSONError(fmt.Sprintf("Could not take the update lock: %v", err))
		os.Exit(1)
	}
	defer release()

	fail := func(err error) {
		_ = historyStore.Append(history.Event{
			Type:    "restore",
			Status:  "failed",
			Message: err.Error(),
			Data: map[string]string{
				"backupFile":   item.File,
				"fromVersion":  item.FromVersion,
				"toVersion":    item.ToVersion,
				"fullRecovery": fmt.Sprintf("%t", fullRecovery),
			},
		})
		release()
		printJSONError(err.Error())
		os.Exit(1)
	}

	opts := backup.RestoreOptions{
		Confirmed:         true,
		FullRecovery:      fullRecovery,
		ContainerLockHeld: true,
		SkipSafetyBackup:  *noSafetyBackup,
	}

	var safetyBackupPath string
	if fullRecovery {
		// The safety backup comes first: once the container is rolled back the
		// database still holds the newer version's data, and this is its copy.
		if !*noSafetyBackup {
			fmt.Fprintln(os.Stderr, "\nBacking up the current database before changing anything...")
			path, err := mgr.CreateSafetyBackup(ctx, "")
			if err != nil {
				fail(fmt.Errorf("could not back up the current database, so nothing was changed. Fix the cause (often disk space) and retry, or pass --no-safety-backup: %w", err))
			}
			safetyBackupPath = path
			fmt.Fprintf(os.Stderr, "Safety backup: %s\n", path)
		}
		opts.SkipSafetyBackup = true

		fmt.Fprintf(os.Stderr, "\nRolling PayRam back to %s before restoring the database...\n", item.FromVersion)
		if err := performContainerRollback(ctx, item.FromVersion); err != nil {
			fail(fmt.Errorf("container rollback failed: %v\nDatabase NOT restored", err))
		}
		fmt.Fprintf(os.Stderr, "Container rolled back to version %s\n", item.FromVersion)

		rollbackContainer, err := discoverPayramContainer(ctx, cfg)
		if err != nil {
			fail(fmt.Errorf("failed to discover the rolled-back container: %v. PayRam was rolled back to %s but the database was NOT restored", err, item.FromVersion))
		}
		opts.ContainerName = rollbackContainer

		fmt.Fprintf(os.Stderr, "Waiting for the database in %s to accept connections (up to %s)...\n", rollbackContainer, databaseReadyTimeout)
		if err := mgr.WaitForDatabase(ctx, rollbackContainer, databaseReadyTimeout); err != nil {
			fail(fmt.Errorf("the database did not become ready: %v. PayRam was rolled back to %s but the database was NOT restored", err, item.FromVersion))
		}
	}

	fmt.Fprintln(os.Stderr, "\nRestoring database from backup (one transaction: any error leaves the database unchanged)...")
	result, err := mgr.RestoreBackup(ctx, item.File, opts)
	if err != nil {
		if fullRecovery {
			err = fmt.Errorf("%w\nPayRam was rolled back to %s but its database was NOT restored", err, item.FromVersion)
		}
		if safetyBackupPath != "" {
			err = fmt.Errorf("%w\nThe database as it was before this attempt is saved in %s", err, safetyBackupPath)
		}
		fail(err)
	}
	if result.SafetyBackupPath != "" {
		safetyBackupPath = result.SafetyBackupPath
	}

	_ = historyStore.Append(history.Event{
		Type:    "restore",
		Status:  "succeeded",
		Message: "Database restored",
		Data: map[string]string{
			"backupFile":   item.File,
			"fromVersion":  result.FromVersion,
			"toVersion":    result.ToVersion,
			"fullRecovery": fmt.Sprintf("%t", fullRecovery),
			"safetyBackup": safetyBackupPath,
		},
	})

	fmt.Fprintln(os.Stderr, "\nDatabase restored successfully.")
	if result.Database.HasMigrations {
		fmt.Fprintf(os.Stderr, "It now holds %d tables and %d applied migrations.\n", result.Database.Tables, result.Database.MigrationRows)
	} else if result.Database.Tables > 0 {
		fmt.Fprintf(os.Stderr, "It now holds %d tables.\n", result.Database.Tables)
	}
	if fullRecovery {
		fmt.Fprintf(os.Stderr, "PayRam runs version %s with the database from the backup.\n", item.FromVersion)
	}
	if safetyBackupPath != "" {
		fmt.Fprintf(os.Stderr, "To undo this restore: payram-updater backup restore --file %s --mode db-only\n", safetyBackupPath)
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", warning)
	}

	response := map[string]interface{}{
		"success":       true,
		"message":       "Database restored successfully",
		"backupFile":    item.File,
		"fromVersion":   result.FromVersion,
		"toVersion":     result.ToVersion,
		"fullRecovery":  fullRecovery,
		"safetyBackup":  safetyBackupPath,
		"tables":        result.Database.Tables,
		"migrationRows": result.Database.MigrationRows,
	}
	if len(result.Warnings) > 0 {
		response["warnings"] = result.Warnings
	}
	jsonOut, _ := json.MarshalIndent(response, "", "  ")
	fmt.Println(string(jsonOut))
}

// discoverPayramContainer names the PayRam container: the configured name,
// else the running container of the PayRam image.
func discoverPayramContainer(ctx context.Context, cfg *config.Config) (string, error) {
	if cfg.TargetContainerName != "" {
		return cfg.TargetContainerName, nil
	}
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}
	discovered, err := container.NewDiscoverer(cfg.DockerBin, imagePattern, log.Default()).DiscoverPayramContainer(ctx)
	if err != nil {
		return "", err
	}
	return discovered.Name, nil
}

func isSuccessfulUpgradeJob(job *jobs.Job) bool {
	if job == nil {
		return false
	}
	return job.State == jobs.JobStateReady && strings.TrimSpace(job.Message) == "Upgrade completed successfully"
}
