package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/cli"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/dockerexec"
	internalhttp "github.com/payram/payram-updater/internal/http"
	"github.com/payram/payram-updater/internal/inspect"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/recover"
	"github.com/payram/payram-updater/internal/recovery"
)

func main() {
	if len(os.Args) < 2 {
		// Default command is "serve"
		runServe()
		return
	}

	command := os.Args[1]
	// Handle help flags
	if command == "-h" || command == "--help" || command == "help" {
		printHelp()
		return
	}

	switch command {
	case "serve":
		runServe()
	case "status":
		runStatus()
	case "logs":
		runLogs()
	case "dry-run":
		runDryRun()
	case "run":
		runRun()
	case "inspect":
		runInspect()
	case "recover":
		runRecover()
	case "backup":
		runBackup()
	case "sync":
		runSync()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Print(`payram-updater - Payram runtime upgrade manager

USAGE:
  payram-updater [COMMAND]

COMMANDS:
  serve            Start the upgrade daemon (default)
  status           Get current upgrade status
  logs             Get upgrade logs
  dry-run          Validate upgrade (read-only, no changes)
  run              Execute an upgrade via the daemon
  inspect          Read-only system diagnostics
  recover          Attempt automated recovery from a failed upgrade
  sync             Sync internal state after external upgrade
  backup           Manage database backups (create, list, restore)
  help             Show this help message

DRY-RUN FLAGS:
  --mode string    Upgrade mode: 'dashboard' or 'manual' (required)
  --to string      Target version (required)

RUN FLAGS:
  --mode string    Upgrade mode: 'dashboard' or 'manual' (default: manual)
  --to string      Target version (required)
  --yes            Skip confirmation prompt (default: false)

BACKUP SUBCOMMANDS:
  backup create           Create a new database backup manually
  backup list             List all available backups
  backup restore --file   Restore from a backup (requires --yes to confirm)

BACKUP FLAGS:
  --file string    Path to backup file (for restore)
  --yes            Skip confirmation prompt (for restore)

EXAMPLES:
  payram-updater serve
  payram-updater status
  payram-updater logs
  payram-updater dry-run --mode dashboard --to v1.7.0
  payram-updater dry-run --mode manual --to v1.2.3
  payram-updater run --mode dashboard --to v1.7.0
  payram-updater run --mode manual --to v1.2.3 --yes
  payram-updater run --mode manual --to latest
  payram-updater inspect
  payram-updater recover
  payram-updater sync
  payram-updater backup create
  payram-updater backup list
  payram-updater backup restore --file /path/to/backup.dump --yes

CONFIG:
  Configuration is loaded from environment variables first,
  then from /etc/payram/updater.env if it exists.

`)
}

// discoverCoreBaseURLOrDefault discovers the Payram Core base URL dynamically.
// If discovery fails, returns the default http://127.0.0.1:8080.
// ALWAYS performs discovery regardless of CORE_BASE_URL config value.
// Uses a quiet logger (discards output) to avoid cluttering CLI output.
func discoverCoreBaseURLOrDefault(ctx context.Context, cfg *config.Config) string {
	// Use imagePattern for discovery (default to payramapp/payram if not overridden)
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	// Use a null logger to suppress discovery logs for CLI commands
	nullLogger := log.New(io.Discard, "", 0)

	// Discover container
	discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, nullLogger)
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to discover Payram container: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	// Extract runtime state to get ports
	inspector := container.NewInspector(cfg.DockerBin, nullLogger)
	runtimeState, err := inspector.ExtractRuntimeState(ctx, discovered.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to extract runtime state: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	// Identify which port serves Payram Core
	identifier := container.NewPortIdentifier(nullLogger)
	identifiedPort, err := identifier.IdentifyPayramCorePort(ctx, runtimeState)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to identify Payram Core port: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	coreBaseURL := fmt.Sprintf("http://127.0.0.1:%s", identifiedPort.HostPort)
	return coreBaseURL
}

func runServe() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	log.Printf("payram-updater starting with config:")
	log.Printf("  Port: %d", cfg.Port)
	log.Printf("  PolicyURL: %s", cfg.PolicyURL)
	log.Printf("  RuntimeManifestURL: %s", cfg.RuntimeManifestURL)
	log.Printf("  FetchTimeout: %d seconds", cfg.FetchTimeoutSeconds)
	log.Printf("  StateDir: %s", cfg.StateDir)
	log.Printf("  LogDir: %s", cfg.LogDir)
	log.Printf("  CoreBaseURL: %s", cfg.CoreBaseURL)
	log.Printf("  ExecutionMode: %s", cfg.ExecutionMode)
	log.Printf("  DockerBin: %s", cfg.DockerBin)

	// Create job store
	jobStore := jobs.NewStore(cfg.StateDir)

	// Create and start the HTTP server
	server := internalhttp.New(cfg, jobStore)
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

func runStatus() {
	port := getPort()
	url := fmt.Sprintf("http://127.0.0.1:%d/upgrade/status", port)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is the payram-updater daemon running?\n")
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	// Parse response to check for recovery playbook
	var statusResp struct {
		State            string             `json:"state"`
		FailureCode      string             `json:"failure_code"`
		Message          string             `json:"message"`
		RecoveryPlaybook *recovery.Playbook `json:"recovery_playbook,omitempty"`
	}

	if err := json.Unmarshal(body, &statusResp); err == nil && statusResp.RecoveryPlaybook != nil {
		// Format with human-readable playbook
		printStatusWithPlaybook(body, statusResp.RecoveryPlaybook)
		return
	}

	// Pretty-print JSON (no playbook or parsing failed)
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to format JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(prettyJSON.String())
}

// printStatusWithPlaybook formats status output with human-readable playbook
func printStatusWithPlaybook(body []byte, playbook *recovery.Playbook) {
	// First print the JSON status
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		fmt.Println(string(body))
	} else {
		fmt.Println(prettyJSON.String())
	}

	// Then print formatted recovery instructions
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Printf("⚠️  RECOVERY: %s\n", playbook.Title)
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nSeverity: %s\n", playbook.Severity)
	fmt.Printf("Data Risk: %s\n", playbook.DataRisk)
	fmt.Printf("\n%s\n", playbook.UserMessage)
	fmt.Println("\n--- Recovery Steps (SSH) ---")
	for _, step := range playbook.SSHSteps {
		fmt.Printf("  %s\n", step)
	}
	if playbook.DocsURL != "" {
		fmt.Printf("\nDocumentation: %s\n", playbook.DocsURL)
	}
	fmt.Println(strings.Repeat("=", 60))
}

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

	resolver := container.NewResolver(cfg.TargetContainerName, cfg.DockerBin, log.Default())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve target container: %v\n", err)
		fmt.Fprintf(os.Stderr, "Set TARGET_CONTAINER_NAME environment variable or ensure manifest has container_name\n")
		os.Exit(1)
	}
	containerName := resolved.Name
	fmt.Printf("Target container resolved as: %s\n\n", containerName)

	// Determine CoreBaseURL: if not provided, discover it dynamically
	coreBaseURL := discoverCoreBaseURLOrDefault(ctx, cfg)

	// Default ports
	defaultPorts := []int{8080, 443}

	inspector := inspect.NewInspector(
		jobStore,
		cfg.DockerBin,
		containerName,
		coreBaseURL, // Use resolved CoreBaseURL
		cfg.PolicyURL,
		cfg.RuntimeManifestURL,
		defaultPorts,
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

	// If recovery failed due to MIGRATION_FAILED, provide additional guidance
	if result.Code == "MIGRATION_FAILED" {
		playbook := recovery.GetPlaybook("MIGRATION_FAILED")
		fmt.Println("\n--- Manual Recovery Required ---")
		fmt.Printf("Title: %s\n", playbook.Title)
		fmt.Printf("Data Risk: %s\n", playbook.DataRisk)
		fmt.Println("\nSSH Recovery Steps:")
		for _, step := range playbook.SSHSteps {
			fmt.Printf("  %s\n", step)
		}
		if playbook.DocsURL != "" {
			fmt.Printf("\nDocumentation: %s\n", playbook.DocsURL)
		}
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

	// Get current running version from /version endpoint
	coreClient := coreclient.NewClient(coreBaseURL)
	versionResp, err := coreClient.Version(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get running version: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is the container running and healthy?\n")
		os.Exit(1)
	}

	// Verify health
	healthResp, err := coreClient.Health(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to verify health: %v\n", err)
		fmt.Fprintf(os.Stderr, "Cannot sync state when health check fails.\n")
		os.Exit(1)
	}

	if healthResp.Status != "ok" || healthResp.DB != "ok" {
		fmt.Fprintf(os.Stderr, "Health check not OK (status=%s, db=%s)\n", healthResp.Status, healthResp.DB)
		fmt.Fprintf(os.Stderr, "Cannot sync state when system is unhealthy.\n")
		os.Exit(1)
	}

	// Load existing job to check if sync is needed
	jobStore := jobs.NewStore(cfg.StateDir)
	existingJob, _ := jobStore.LoadLatest()

	if existingJob != nil && existingJob.State == jobs.JobStateReady && existingJob.ResolvedTarget == versionResp.Version {
		fmt.Printf("Internal state already matches running version (%s). No sync needed.\n", versionResp.Version)
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
	syncJob := jobs.NewJob(jobID, jobs.JobModeManual, versionResp.Version)
	syncJob.ResolvedTarget = versionResp.Version
	syncJob.State = jobs.JobStateReady
	syncJob.Message = fmt.Sprintf("Synced from external upgrade (was %s, now %s)", previousVersion, versionResp.Version)

	// Save the synthetic job
	if err := jobStore.Save(syncJob); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save sync job: %v\n", err)
		os.Exit(1)
	}

	// Log the sync
	logMsg := fmt.Sprintf("SYNC: External upgrade detected and synced. Running version: %s (was: %s)", versionResp.Version, previousVersion)
	if err := jobStore.AppendLog(logMsg); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to write log: %v\n", err)
	}

	// Output success
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("✅ SYNC SUCCESSFUL")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nPrevious tracked version: %s\n", previousVersion)
	fmt.Printf("Current running version:  %s\n", versionResp.Version)
	fmt.Printf("Health status:            OK (status=%s, db=%s)\n", healthResp.Status, healthResp.DB)
	fmt.Println("\nInternal state has been updated to match the running version.")
	fmt.Println("Run 'payram-updater inspect' to verify.")
	fmt.Println(strings.Repeat("=", 60))
}

func runLogs() {
	port := getPort()
	url := fmt.Sprintf("http://127.0.0.1:%d/upgrade/logs", port)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is the payram-updater daemon running?\n")
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	// Print plain text logs directly
	fmt.Print(string(body))
}

func runDryRun() {
	// Parse flags for dry-run command
	dryRunCmd := flag.NewFlagSet("dry-run", flag.ExitOnError)
	mode := dryRunCmd.String("mode", "", "Upgrade mode (dashboard or manual)")
	to := dryRunCmd.String("to", "", "Target version")

	// Parse arguments after "dry-run"
	dryRunCmd.Parse(os.Args[2:])

	// Use shared validation
	req, err := cli.ParseUpgradeRequest(*mode, *to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	port := getPort()
	url := fmt.Sprintf("http://127.0.0.1:%d/upgrade/plan", port)

	// Create request payload
	payload := map[string]string{
		"mode":             string(req.Mode),
		"requested_target": req.RequestedTarget,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create request: %v\n", err)
		os.Exit(1)
	}

	// Send POST request
	resp, err := http.Post(url, "application/json", bytes.NewReader(payloadBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is the payram-updater daemon running?\n")
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	// Pretty-print JSON
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		// If JSON formatting fails, just print raw response
		fmt.Println(string(body))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	fmt.Println(prettyJSON.String())

	// Check if plan failed
	var planResp struct {
		State       string `json:"state"`
		FailureCode string `json:"failure_code"`
	}
	if err := json.Unmarshal(body, &planResp); err == nil {
		if planResp.State == "FAILED" {
			os.Exit(1)
		}
	}

	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}

func runRun() {
	// Parse flags for run command
	runCmd := flag.NewFlagSet("run", flag.ExitOnError)
	mode := runCmd.String("mode", "manual", "Upgrade mode (dashboard or manual)")
	to := runCmd.String("to", "", "Target version")
	yes := runCmd.Bool("yes", false, "Skip confirmation prompt")

	// Parse arguments after "run"
	runCmd.Parse(os.Args[2:])

	// Use shared validation
	req, err := cli.ParseUpgradeRequest(*mode, *to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	port := getPort()

	// Step 1: Call /upgrade/plan to validate and get resolved values
	planURL := fmt.Sprintf("http://127.0.0.1:%d/upgrade/plan", port)
	planPayload := map[string]string{
		"mode":             string(req.Mode),
		"requested_target": req.RequestedTarget,
	}
	planPayloadBytes, err := json.Marshal(planPayload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create request: %v\n", err)
		os.Exit(1)
	}

	planResp, err := http.Post(planURL, "application/json", bytes.NewReader(planPayloadBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is the payram-updater daemon running?\n")
		os.Exit(1)
	}
	defer planResp.Body.Close()

	planBody, err := io.ReadAll(planResp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	// Parse plan response
	var plan struct {
		State           string `json:"state"`
		Mode            string `json:"mode"`
		RequestedTarget string `json:"requested_target"`
		ResolvedTarget  string `json:"resolved_target"`
		FailureCode     string `json:"failure_code"`
		Message         string `json:"message"`
		ImageRepo       string `json:"image_repo"`
		ContainerName   string `json:"container_name"`
	}
	if err := json.Unmarshal(planBody, &plan); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse plan response: %v\n", err)
		os.Exit(1)
	}

	// Step 2: If planning failed, show the error and exit (no prompt)
	if plan.State == "FAILED" {
		fmt.Fprintf(os.Stderr, "Upgrade validation failed:\n")
		fmt.Fprintf(os.Stderr, "  Code: %s\n", plan.FailureCode)
		fmt.Fprintf(os.Stderr, "  Message: %s\n", plan.Message)
		os.Exit(1)
	}

	// Step 3: Planning succeeded - prompt for confirmation
	summary := &cli.UpgradeSummary{
		Mode:            plan.Mode,
		RequestedTarget: plan.RequestedTarget,
		ResolvedTarget:  plan.ResolvedTarget,
		ImageRepo:       plan.ImageRepo,
		ContainerName:   plan.ContainerName,
	}

	confirmer := cli.NewConfirmer()
	confirmer.ConfirmOrExit(summary, *yes)

	// Step 4: User confirmed - call /upgrade/run to start the job
	runURL := fmt.Sprintf("http://127.0.0.1:%d/upgrade/run", port)
	runPayload := map[string]string{
		"mode":             string(req.Mode),
		"requested_target": req.RequestedTarget,
		"source":           "CLI",
	}
	runPayloadBytes, err := json.Marshal(runPayload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create request: %v\n", err)
		os.Exit(1)
	}

	runResp, err := http.Post(runURL, "application/json", bytes.NewReader(runPayloadBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
		os.Exit(1)
	}
	defer runResp.Body.Close()

	runBody, err := io.ReadAll(runResp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}

	// Handle conflict (409)
	if runResp.StatusCode == http.StatusConflict {
		var conflictResp struct {
			Error string `json:"error"`
			JobID string `json:"job_id"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(runBody, &conflictResp); err == nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", conflictResp.Error)
			fmt.Fprintf(os.Stderr, "Active job: %s (state=%s)\n", conflictResp.JobID, conflictResp.State)
			fmt.Fprintf(os.Stderr, "Use 'payram-updater status' to check the current job.\n")
		} else {
			fmt.Fprintf(os.Stderr, "An upgrade job is already running.\n")
		}
		os.Exit(1)
	}

	// Parse run response
	var runResult struct {
		JobID           string `json:"job_id"`
		State           string `json:"state"`
		Mode            string `json:"mode"`
		RequestedTarget string `json:"requested_target"`
		ResolvedTarget  string `json:"resolved_target"`
		FailureCode     string `json:"failure_code"`
		Message         string `json:"message"`
	}
	if err := json.Unmarshal(runBody, &runResult); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse run response: %v\n", err)
		os.Exit(1)
	}

	// Check if run failed immediately (e.g., policy fetch failed after plan)
	if runResult.State == "FAILED" {
		fmt.Fprintf(os.Stderr, "Upgrade failed to start:\n")
		fmt.Fprintf(os.Stderr, "  Code: %s\n", runResult.FailureCode)
		fmt.Fprintf(os.Stderr, "  Message: %s\n", runResult.Message)
		os.Exit(1)
	}

	// Success - print job info
	fmt.Printf("Started upgrade job %s (state=%s).\n", runResult.JobID, runResult.State)
	fmt.Println("Use 'payram-updater status' to check progress and 'payram-updater logs' for details.")
}

func getPort() int {
	// Load config the same way as daemon (env vars first, then /etc/payram/updater.env)
	cfg, err := config.Load()
	if err != nil {
		// If config loading fails, fall back to reading UPDATER_PORT directly
		if portStr := os.Getenv("UPDATER_PORT"); portStr != "" {
			var port int
			if _, err := fmt.Sscanf(portStr, "%d", &port); err == nil {
				return port
			}
		}
		// Default port
		return 2359
	}
	return cfg.Port
}

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

	ctx := context.Background()
	info, err := mgr.CreateBackup(ctx, backup.BackupMeta{
		FromVersion:   "manual",
		TargetVersion: "manual",
		JobID:         fmt.Sprintf("manual-%d", time.Now().Unix()),
	})
	if err != nil {
		errResp := map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		}
		jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
		fmt.Println(string(jsonOut))
		os.Exit(1)
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
		errResp := map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		}
		jsonOut, _ := json.MarshalIndent(errResp, "", "  ")
		fmt.Println(string(jsonOut))
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "\n✅ Database restored successfully.")

	if doFullRecovery && needsRecovery {
		fmt.Fprintf(os.Stderr, "\n✅ Full recovery completed successfully.\n")
		fmt.Fprintf(os.Stderr, "Service restored to version %s with database from backup.\n", metadata.FromVersion)
	}

	response := map[string]interface{}{
		"success":       true,
		"message":       "Database restored successfully",
		"backup_file":   *filePath,
		"from_version":  result.FromVersion,
		"to_version":    result.ToVersion,
		"full_recovery": doFullRecovery,
	}
	jsonOut, _ := json.MarshalIndent(response, "", "  ")
	fmt.Println(string(jsonOut))
}
