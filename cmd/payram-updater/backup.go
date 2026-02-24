package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
)

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
  payram-updater backup restore --file /path/to/backup.dump --yes`)
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
		Dir:          cfg.Backup.Dir,
		Retention:    cfg.Backup.Retention,
		PGHost:       cfg.Backup.PGHost,
		PGPort:       cfg.Backup.PGPort,
		PGDB:         cfg.Backup.PGDB,
		PGUser:       cfg.Backup.PGUser,
		PGPassword:   cfg.Backup.PGPassword,
		ImagePattern: imagePattern,
	}
	mgr := backup.NewManager(backupCfg, &backup.RealExecutor{}, log.Default())

	switch subcommand {
	case "create":
		runBackupCreate(mgr)
	case "list":
		runBackupList(mgr)
	case "restore":
		runBackupRestore(mgr)
	default:
		fmt.Fprintf(os.Stderr, "Unknown backup subcommand: %s\n", subcommand)
		fmt.Println("Available subcommands: create, list, restore")
		os.Exit(1)
	}
}

func runBackupCreate(mgr *backup.Manager) {
	// Backups are always enabled
	fmt.Fprintln(os.Stderr, "Creating database backup...")

	var historyStore *history.Store
	if cfg, err := config.Load(); err == nil {
		historyStore = history.NewStore(cfg.StateDir)
	}

	ctx := context.Background()
	info, err := mgr.CreateBackup(ctx, backup.BackupMeta{
		FromVersion:   "manual",
		TargetVersion: "manual",
		JobID:         fmt.Sprintf("manual-%d", time.Now().Unix()),
	})
	if err != nil {
		if historyStore != nil {
			_ = historyStore.Append(history.Event{
				Type:    "backup",
				Status:  "failed",
				Message: err.Error(),
				Data: map[string]string{
					"fromVersion":   "manual",
					"targetVersion": "manual",
				},
			})
		}
		errResp := map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		}
		jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
		fmt.Println(string(jsonOut))
		os.Exit(1)
	}

	if historyStore != nil {
		data := map[string]string{
			"fromVersion":   "manual",
			"targetVersion": "manual",
			"backupPath":    info.Path,
			"sizeBytes":     fmt.Sprintf("%d", info.Size),
		}
		_ = historyStore.Append(history.Event{
			Type:    "backup",
			Status:  "succeeded",
			Message: "Backup completed",
			Data:    data,
		})
	}

	// Prune old backups
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
		errResp := map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		}
		jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
		fmt.Println(string(jsonOut))
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

// parseBackupFilename extracts version metadata from a backup filename.
// Expected format: payram-backup-YYYYMMDD-HHMMSS-fromVer-to-toVer.(sql|dump)
func parseBackupFilename(filename string) struct {
	FromVersion string
	ToVersion   string
} {
	result := struct {
		FromVersion string
		ToVersion   string
	}{
		FromVersion: "unknown",
		ToVersion:   "unknown",
	}

	// Strip prefix and extension
	name := strings.TrimPrefix(filename, "payram-backup-")
	name = strings.TrimSuffix(name, ".sql")
	name = strings.TrimSuffix(name, ".dump")

	// Split by '-'
	parts := strings.Split(name, "-")
	if len(parts) < 4 {
		return result
	}

	// Parse versions: YYYYMMDD-HHMMSS-fromVer-to-toVer
	// Find "to" separator
	for i := 2; i < len(parts)-1; i++ {
		if parts[i] == "to" {
			// Everything before "to" is fromVersion
			result.FromVersion = strings.Join(parts[2:i], "-")
			// Everything after "to" is toVersion
			result.ToVersion = strings.Join(parts[i+1:], "-")
			break
		}
	}

	return result
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

	// Discover running container
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, log.Default())
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover running container: %w", err)
	}

	containerName := discovered.Name
	log.Printf("Discovered container: %s (current version: %s)", containerName, discovered.ImageTag)

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

func runBackupRestore(mgr *backup.Manager) {
	// Parse restore flags
	restoreFlags := flag.NewFlagSet("restore", flag.ExitOnError)
	filePath := restoreFlags.String("file", "", "Path to backup file (required)")
	confirmed := restoreFlags.Bool("yes", false, "Skip confirmation prompt")
	fullRecovery := restoreFlags.Bool("full-recovery", false, "Perform full recovery (DB restore + container rollback) without prompt")

	if err := restoreFlags.Parse(os.Args[3:]); err != nil {
		os.Exit(1)
	}

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "Error: --file is required")
		fmt.Fprintln(os.Stderr, "Usage: payram-updater backup restore --file /path/to/backup.dump [--yes] [--full-recovery]")
		os.Exit(1)
	}

	// Verify the file exists
	if err := mgr.VerifyBackupFile(*filePath); err != nil {
		errResp := map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		}
		jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
		fmt.Println(string(jsonOut))
		os.Exit(1)
	}

	// Parse backup metadata to determine if recovery is needed
	ctx := context.Background()
	filename := filepath.Base(*filePath)
	metadata := parseBackupFilename(filename)
	needsRecovery := metadata.FromVersion != "unknown" && metadata.ToVersion != "unknown"

	var historyStore *history.Store
	var latestJob *jobs.Job
	if cfg, err := config.Load(); err == nil {
		historyStore = history.NewStore(cfg.StateDir)
		if job, loadErr := jobs.NewStore(cfg.StateDir).LoadLatest(); loadErr == nil {
			latestJob = job
		}
	}

	// Determine if full recovery will be performed
	doFullRecovery := *fullRecovery
	var rollbackContainerName string

	// If recovery is needed and not auto-confirmed, ask user BEFORE restoring
	if needsRecovery && !*fullRecovery {
		fmt.Fprintf(os.Stderr, "\nThis backup was created before upgrading:\n")
		fmt.Fprintf(os.Stderr, "  FROM version: %s\n", metadata.FromVersion)
		fmt.Fprintf(os.Stderr, "  TO version:   %s\n", metadata.ToVersion)
		fmt.Fprintf(os.Stderr, "\nChoose recovery mode:\n")
		fmt.Fprintf(os.Stderr, "  [1] Restore database only (leave container as-is)\n")
		fmt.Fprintf(os.Stderr, "  [2] Restore database AND roll back service to %s (recommended)\n", metadata.FromVersion)
		fmt.Fprintf(os.Stderr, "\nEnter choice [1/2]: ")

		var choice string
		fmt.Scanln(&choice)
		choice = strings.TrimSpace(choice)

		if choice == "2" || choice == "" {
			// Default to option 2
			doFullRecovery = true
			// User has explicitly chosen full recovery - this counts as confirmation
			// for the subsequent database restore (no redundant prompt needed)
			*confirmed = true
			fmt.Fprintln(os.Stderr, "\n✓ Full recovery mode selected - container rollback + database restore")
		}
	}

	// If using --full-recovery flag, treat it as implicit confirmation
	if *fullRecovery && needsRecovery {
		*confirmed = true
	}

	// CRITICAL SEQUENCING FIX: If full recovery is requested, roll back container FIRST
	// This ensures database restore happens inside the rollback container, not the failed one
	if doFullRecovery && needsRecovery {
		if isSuccessfulUpgradeJob(latestJob) {
			errResp := map[string]interface{}{
				"success": false,
				"error":   "Rollback is blocked because the latest upgrade completed successfully. Re-run restore in database-only mode.",
			}
			jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
			fmt.Println(string(jsonOut))
			os.Exit(1)
		}

		fmt.Fprintln(os.Stderr, "\n⚠️  Full recovery mode: Rolling back container BEFORE database restore...")
		fmt.Fprintf(os.Stderr, "This ensures database restore happens inside the rollback container (version %s)\n\n", metadata.FromVersion)

		if err := performContainerRollback(ctx, metadata.FromVersion); err != nil {
			errResp := map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("❌ Container rollback failed: %v\nDatabase NOT restored.", err),
			}
			jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
			fmt.Println(string(jsonOut))
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, "✅ Container rolled back to version %s\n", metadata.FromVersion)
		fmt.Fprintln(os.Stderr, "Waiting for database readiness...")
		time.Sleep(5 * time.Second)

		// Get the container name for restore
		cfg, err := config.Load()
		if err != nil {
			errResp := map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Failed to load config: %v", err),
			}
			jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
			fmt.Println(string(jsonOut))
			os.Exit(1)
		}

		imagePattern := "payramapp/payram:"
		if cfg.ImageRepoOverride != "" {
			imagePattern = cfg.ImageRepoOverride + ":"
		}

		discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, log.Default())
		discovered, err := discoverer.DiscoverPayramContainer(ctx)
		if err != nil {
			errResp := map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Failed to discover rollback container: %v", err),
			}
			jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
			fmt.Println(string(jsonOut))
			os.Exit(1)
		}
		rollbackContainerName = discovered.Name
		fmt.Fprintf(os.Stderr, "Rollback container ready: %s\n", rollbackContainerName)
	}

	// Interactive confirmation if --yes not provided
	// (Full recovery users already confirmed via recovery mode selection)
	if !*confirmed {
		fmt.Println("\nWARNING: This will restore the database from backup.")
		fmt.Println("All current data will be REPLACED with backup contents.")
		fmt.Printf("\nBackup file: %s\n", *filePath)
		if doFullRecovery && needsRecovery {
			fmt.Printf("Target: Rollback container (version %s)\n", metadata.FromVersion)
		} else {
			fmt.Printf("Target database: %s@%s:%d/%s\n",
				mgr.Config.PGUser, mgr.Config.PGHost, mgr.Config.PGPort, mgr.Config.PGDB)
		}
		fmt.Print("\nType 'yes' to confirm: ")

		var input string
		fmt.Scanln(&input)
		if strings.ToLower(strings.TrimSpace(input)) != "yes" {
			fmt.Println("Restore cancelled.")
			os.Exit(0)
		}
		*confirmed = true
	} else if doFullRecovery && needsRecovery {
		// Log why confirmation was skipped for full recovery
		fmt.Fprintln(os.Stderr, "✓ Skipping redundant confirmation (already confirmed via recovery mode selection)")
	}

	fmt.Fprintln(os.Stderr, "\nRestoring database from backup...")
	if doFullRecovery && needsRecovery {
		fmt.Fprintf(os.Stderr, "Executing restore inside rollback container (version %s)...\n", metadata.FromVersion)
	}

	result, err := mgr.RestoreBackup(ctx, *filePath, backup.RestoreOptions{
		Confirmed:     *confirmed,
		ContainerName: rollbackContainerName, // Use rollback container if full recovery
		FullRecovery:  doFullRecovery,
	})
	if err != nil {
		if historyStore != nil {
			_ = historyStore.Append(history.Event{
				Type:    "restore",
				Status:  "failed",
				Message: err.Error(),
				Data: map[string]string{
					"backupFile":   *filePath,
					"fromVersion":  metadata.FromVersion,
					"toVersion":    metadata.ToVersion,
					"fullRecovery": fmt.Sprintf("%t", doFullRecovery),
				},
			})
		}
		errResp := map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		}
		jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
		fmt.Println(string(jsonOut))
		os.Exit(1)
	}

	if historyStore != nil {
		_ = historyStore.Append(history.Event{
			Type:    "restore",
			Status:  "succeeded",
			Message: "Database restored",
			Data: map[string]string{
				"backupFile":   *filePath,
				"fromVersion":  result.FromVersion,
				"toVersion":    result.ToVersion,
				"fullRecovery": fmt.Sprintf("%t", doFullRecovery),
			},
		})
	}

	fmt.Fprintln(os.Stderr, "\n✅ Database restored successfully.")

	if doFullRecovery && needsRecovery {
		fmt.Fprintf(os.Stderr, "\n✅ Full recovery completed successfully.\n")
		fmt.Fprintf(os.Stderr, "Service restored to version %s with database from backup.\n", metadata.FromVersion)
	}

	response := map[string]interface{}{
		"success":      true,
		"message":      "Database restored successfully",
		"backupFile":   *filePath,
		"fromVersion":  result.FromVersion,
		"toVersion":    result.ToVersion,
		"fullRecovery": doFullRecovery,
	}
	jsonOut, _ := json.MarshalIndent(response, "", "  ")
	fmt.Println(string(jsonOut))
}

func isSuccessfulUpgradeJob(job *jobs.Job) bool {
	if job == nil {
		return false
	}
	return job.State == jobs.JobStateReady && strings.TrimSpace(job.Message) == "Upgrade completed successfully"
}
