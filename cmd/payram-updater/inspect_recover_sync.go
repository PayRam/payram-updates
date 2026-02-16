package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/corecompat"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/inspect"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/policy"
	"github.com/payram/payram-updater/internal/recover"
	"github.com/payram/payram-updater/internal/recovery"
)

func runInspect() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize job store (read-only)
	jobStore := jobs.NewStore(cfg.StateDir)

	// Resolve container name
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fetch manifest to get container name if not set in env
	manifestClient := manifest.NewClient(time.Duration(cfg.FetchTimeoutSeconds) * time.Second)
	manifestData, _ := manifestClient.Fetch(ctx, cfg.RuntimeManifestURL)

	// Use imagePattern for discovery (default to payramapp/payram if not overridden)
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	resolver := container.NewResolver(cfg.TargetContainerName, cfg.DockerBin, log.Default())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		if resErr, ok := err.(*container.ResolutionError); ok && resErr.GetFailureCode() == "CONTAINER_NAME_UNRESOLVED" {
			discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, log.Default())
			discovered, discoverErr := discoverer.DiscoverPayramContainer(ctx)
			if discoverErr != nil {
				fmt.Fprintf(os.Stderr, "Failed to resolve target container: %v\n", err)
				fmt.Fprintf(os.Stderr, "Set TARGET_CONTAINER_NAME environment variable or ensure manifest has container_name\n")
				os.Exit(1)
			}
			containerName := discovered.Name
			fmt.Printf("Target container discovered as: %s\n\n", containerName)
			resolved = &container.ResolvedContainer{Name: containerName}
		} else {
			fmt.Fprintf(os.Stderr, "Failed to resolve target container: %v\n", err)
			fmt.Fprintf(os.Stderr, "Set TARGET_CONTAINER_NAME environment variable or ensure manifest has container_name\n")
			os.Exit(1)
		}
	}
	containerName := resolved.Name
	if containerName != "" {
		fmt.Printf("Target container resolved as: %s\n\n", containerName)
	}

	// Determine CoreBaseURL: if not provided, discover it dynamically
	coreBaseURL := discoverCoreBaseURLOrDefault(ctx, cfg)

	inspector := inspect.NewInspector(
		jobStore,
		cfg.DockerBin,
		containerName,
		coreBaseURL, // Use resolved CoreBaseURL
		cfg.PolicyURL,
		cfg.RuntimeManifestURL,
		cfg.DebugVersionMode,
	)

	result := inspector.Run(ctx)

	// Output as JSON
	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to format output: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(output))

	// Print human-readable summary
	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("OVERALL STATE: %s\n", result.OverallState)
	fmt.Println(strings.Repeat("=", 60))

	if len(result.Issues) > 0 {
		fmt.Println("\nISSUES:")
		for _, issue := range result.Issues {
			fmt.Printf("  [%s] %s: %s\n", issue.Severity, issue.Component, issue.Description)
		}
	}

	if len(result.Recommendations) > 0 {
		fmt.Println("\nRECOMMENDATIONS:")
		for _, rec := range result.Recommendations {
			fmt.Printf("  %d. %s\n     %s\n", rec.Priority, rec.Action, rec.Description)
		}
	}

	if result.RecoveryPlaybook != nil {
		fmt.Println("\nRECOVERY PLAYBOOK:")
		fmt.Printf("  Code: %s\n", result.RecoveryPlaybook.Code)
		fmt.Printf("  Title: %s\n", result.RecoveryPlaybook.Title)
		fmt.Printf("  Severity: %s\n", result.RecoveryPlaybook.Severity)
		if result.RecoveryPlaybook.DataRisk != recovery.DataRiskNone {
			fmt.Printf("  Data Risk: %s\n", result.RecoveryPlaybook.DataRisk)
		}
		fmt.Println("\n  Steps:")
		for _, step := range result.RecoveryPlaybook.SSHSteps {
			fmt.Printf("    %s\n", step)
		}
	}

	fmt.Println(strings.Repeat("=", 60))

	// Exit with non-zero if BROKEN
	if result.OverallState == inspect.StateBroken {
		os.Exit(1)
	}
}

func runRecover() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Initialize job store
	jobStore := jobs.NewStore(cfg.StateDir)

	// Create docker runner
	runner := &dockerexec.Runner{DockerBin: cfg.DockerBin, Logger: log.Default()}

	// Resolve container name
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Fetch manifest to get container name if not set in env
	manifestClient := manifest.NewClient(time.Duration(cfg.FetchTimeoutSeconds) * time.Second)
	manifestData, _ := manifestClient.Fetch(ctx, cfg.RuntimeManifestURL)

	resolver := container.NewResolver(cfg.TargetContainerName, cfg.DockerBin, log.Default())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve target container: %v\n", err)
		fmt.Fprintf(os.Stderr, "Set TARGET_CONTAINER_NAME environment variable or ensure manifest has container_name\n")
		os.Exit(1)
	}
	containerName := resolved.Name
	fmt.Printf("Target container resolved as: %s\\n\\n", containerName)

	// Determine CoreBaseURL: if not provided, discover it dynamically
	coreBaseURL := discoverCoreBaseURLOrDefault(ctx, cfg)

	// Create recoverer
	recoverer := recover.NewRecoverer(
		jobStore,
		runner,
		containerName,
		coreBaseURL, // Use resolved CoreBaseURL
	)

	// Run recovery (reuse the context from container resolution)
	result, err := recoverer.Run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Recovery failed: %v\n", err)
		os.Exit(1)
	}

	// Output as JSON
	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to format output: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(output))

	// Print human-readable summary
	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))
	if result.Success {
		fmt.Println("✅ RECOVERY SUCCESSFUL")
	} else {
		fmt.Println("❌ RECOVERY REFUSED/FAILED")
	}
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nMessage: %s\n", result.Message)

	if result.Refusals != "" {
		fmt.Printf("\nReason: %s\n", result.Refusals)
	}

	if result.Action != "" {
		fmt.Printf("\nAction taken: %s\n", result.Action)
	}

	fmt.Println(strings.Repeat("=", 60))

	// Exit with non-zero if recovery failed
	if !result.Success {
		os.Exit(1)
	}
}

func runSync() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fetch manifest to get container name if not set in env
	manifestClient := manifest.NewClient(time.Duration(cfg.FetchTimeoutSeconds) * time.Second)
	manifestData, _ := manifestClient.Fetch(ctx, cfg.RuntimeManifestURL)

	// Resolve container name
	resolver := container.NewResolver(cfg.TargetContainerName, cfg.DockerBin, log.Default())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve target container: %v\n", err)
		os.Exit(1)
	}
	containerName := resolved.Name
	fmt.Printf("Target container resolved as: %s\n\n", containerName)

	// Determine CoreBaseURL: if not provided, discover it dynamically
	coreBaseURL := discoverCoreBaseURLOrDefault(ctx, cfg)

	coreClient := coreclient.NewClient(coreBaseURL)

	// Fetch policy init point (if available)
	policyClient := policy.NewClient(time.Duration(cfg.FetchTimeoutSeconds) * time.Second)
	policyData, _ := policyClient.Fetch(ctx, cfg.PolicyURL)
	initVersion := ""
	if policyData != nil {
		initVersion = strings.TrimSpace(policyData.UpdaterAPIInitVersion)
	}

	// Get current running version (API or label fallback)
	var currentVersion string
	versionResp, err := coreClient.Version(ctx)
	if err == nil && versionResp != nil && versionResp.Version != "" {
		currentVersion = versionResp.Version
	} else {
		labelVersion, labelErr := corecompat.VersionFromLabels(ctx, cfg.DockerBin, containerName)
		if labelErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to get running version: %v\n", err)
			fmt.Fprintf(os.Stderr, "Is the container running and healthy?\n")
			os.Exit(1)
		}
		currentVersion = labelVersion
	}

	useLegacy := false
	if initVersion != "" {
		if before, compareErr := corecompat.IsBeforeInit(currentVersion, initVersion); compareErr == nil {
			useLegacy = before
		}
	}

	// Verify health
	healthStatus := ""
	healthDB := ""
	if useLegacy {
		if err := corecompat.LegacyHealth(ctx, coreBaseURL); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to verify health: %v\n", err)
			fmt.Fprintf(os.Stderr, "Cannot sync state when health check fails.\n")
			os.Exit(1)
		}
		healthStatus = "ok"
		healthDB = "unknown"
	} else {
		healthResp, err := coreClient.Health(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to verify health: %v\n", err)
			fmt.Fprintf(os.Stderr, "Cannot sync state when health check fails.\n")
			os.Exit(1)
		}

		if healthResp.Status != "ok" || (healthResp.DB != "" && healthResp.DB != "ok") {
			fmt.Fprintf(os.Stderr, "Health check not OK (status=%s, db=%s)\n", healthResp.Status, healthResp.DB)
			fmt.Fprintf(os.Stderr, "Cannot sync state when system is unhealthy.\n")
			os.Exit(1)
		}
		healthStatus = healthResp.Status
		healthDB = healthResp.DB
	}

	// Load existing job to check if sync is needed
	jobStore := jobs.NewStore(cfg.StateDir)
	existingJob, _ := jobStore.LoadLatest()

	if existingJob != nil && existingJob.State == jobs.JobStateReady && existingJob.ResolvedTarget == currentVersion {
		fmt.Printf("Internal state already matches running version (%s). No sync needed.\n", currentVersion)
		return
	}

	// Determine previous version for display
	previousVersion := "unknown"
	if existingJob != nil {
		previousVersion = existingJob.ResolvedTarget
	}

	// Create a synthetic job to reflect the external upgrade
	// Generate a unique job ID
	jobID := fmt.Sprintf("sync-%d", time.Now().UnixNano())
	syncJob := jobs.NewJob(jobID, jobs.JobModeManual, currentVersion)
	syncJob.ResolvedTarget = currentVersion
	syncJob.State = jobs.JobStateReady
	syncJob.Message = fmt.Sprintf("Synced from external upgrade (was %s, now %s)", previousVersion, versionResp.Version)

	// Save the synthetic job
	if err := jobStore.Save(syncJob); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save sync job: %v\n", err)
		os.Exit(1)
	}

	// Log the sync
	logMsg := fmt.Sprintf("SYNC: External upgrade detected and synced. Running version: %s (was: %s)", currentVersion, previousVersion)
	if err := jobStore.AppendLog(logMsg); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write log: %v\n", err)
	}

	// Output success
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("✅ SYNC SUCCESSFUL")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nPrevious tracked version: %s\n", previousVersion)
	fmt.Printf("Current running version:  %s\n", currentVersion)
	fmt.Printf("Health status:            OK (status=%s, db=%s)\n", healthStatus, healthDB)
	fmt.Println("\nInternal state has been updated to match the running version.")
	fmt.Println("Run 'payram-updater inspect' to verify.")
	fmt.Println(strings.Repeat("=", 60))
}
