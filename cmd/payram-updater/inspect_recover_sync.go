package main

import (
	"context"
	"encoding/json"
	"errors"
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

// resolveTargetContainer names PayRam's container: TARGET_CONTAINER_NAME or
// the manifest's container_name, else the container the last job worked on,
// else the running PayRam container found by its image.
func resolveTargetContainer(ctx context.Context, cfg *config.Config, job *jobs.Job) (string, error) {
	manifestClient := manifest.NewClient(time.Duration(cfg.FetchTimeoutSeconds) * time.Second)
	manifestData, _ := manifestClient.Fetch(ctx, cfg.RuntimeManifestURL)

	resolved, err := container.NewResolver(cfg.TargetContainerName, cfg.DockerBin, log.Default()).Resolve(manifestData)
	if err == nil {
		return resolved.Name, nil
	}
	if resErr, ok := err.(*container.ResolutionError); !ok || resErr.GetFailureCode() != "CONTAINER_NAME_UNRESOLVED" {
		return "", err
	}
	if job != nil && job.ContainerName != "" {
		return job.ContainerName, nil
	}

	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}
	discovered, discoverErr := container.NewDiscoverer(cfg.DockerBin, imagePattern, log.Default()).DiscoverPayramContainer(ctx)
	if discoverErr != nil {
		return "", err
	}
	return discovered.Name, nil
}

// resolveTargetContainerOrDefault is resolveTargetContainer for the read-only
// commands: when PayRam's container cannot be named they go on with the
// standard name, so the merchant still gets a diagnosis.
func resolveTargetContainerOrDefault(ctx context.Context, cfg *config.Config, job *jobs.Job) string {
	name, err := resolveTargetContainer(ctx, cfg, job)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not resolve the target container (%v); using %q.\n", err, recovery.DefaultContainerName)
		fmt.Fprintf(os.Stderr, "Set TARGET_CONTAINER_NAME in /etc/payram/updater.env if PayRam's container has another name.\n")
		name = recovery.DefaultContainerName
	}
	fmt.Printf("Target container resolved as: %s\n\n", name)
	return name
}

// resolveCoreBaseURL picks the address to reach PayRam on. CORE_BASE_URL wins
// when set. Otherwise it takes the first of these that answers: the URL the
// last job used, the discovered one, and PayRam's standard HTTPS and HTTP
// addresses, so a wrong guess (such as the unpublished port 8080) never makes
// a healthy PayRam look down.
func resolveCoreBaseURL(ctx context.Context, cfg *config.Config, containerName string, job *jobs.Job) string {
	if cfg.CoreBaseURL != "" {
		return cfg.CoreBaseURL
	}

	candidates := []func() string{
		func() string {
			if job == nil {
				return ""
			}
			return job.CoreBaseURL
		},
		func() string { return discoverCoreBaseURLWithContainer(ctx, cfg, containerName) },
		func() string { return "https://127.0.0.1" },
		func() string { return "http://127.0.0.1" },
	}
	first := ""
	tried := map[string]bool{}
	for _, candidate := range candidates {
		baseURL := candidate()
		if baseURL == "" || tried[baseURL] {
			continue
		}
		tried[baseURL] = true
		if first == "" {
			first = baseURL
		}
		if coreAnswers(ctx, baseURL) {
			return baseURL
		}
	}
	return first
}

// coreAnswers reports whether PayRam replies to a health check at baseURL.
func coreAnswers(ctx context.Context, baseURL string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := coreclient.NewClient(baseURL).Health(probeCtx); err == nil {
		return true
	}
	return corecompat.LegacyHealth(probeCtx, baseURL) == nil
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
	latestJob, _ := jobStore.LoadLatest()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	containerName := resolveTargetContainerOrDefault(ctx, cfg, latestJob)
	coreBaseURL := resolveCoreBaseURL(ctx, cfg, containerName, latestJob)

	inspector := inspect.NewInspector(
		jobStore,
		cfg.DockerBin,
		containerName,
		coreBaseURL,
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
		printPlaybookSteps("RECOVERY PLAYBOOK", result.RecoveryPlaybook)
	}

	fmt.Println(strings.Repeat("=", 60))

	// Exit with non-zero if BROKEN
	if result.OverallState == inspect.StateBroken {
		os.Exit(1)
	}
}

// printPlaybookSteps prints a playbook's summary and its numbered steps.
func printPlaybookSteps(heading string, playbook *recovery.Playbook) {
	fmt.Printf("\n%s:\n", heading)
	fmt.Printf("  Code: %s\n", playbook.Code)
	if playbook.CauseCode != "" {
		fmt.Printf("  Cause: %s\n", playbook.CauseCode)
	}
	fmt.Printf("  Title: %s\n", playbook.Title)
	fmt.Printf("  Severity: %s\n", playbook.Severity)
	if playbook.DataRisk != recovery.DataRiskNone {
		fmt.Printf("  Data Risk: %s\n", playbook.DataRisk)
	}
	fmt.Printf("\n  %s\n", playbook.UserMessage)
	fmt.Println("\n  Steps:")
	for _, step := range playbook.SSHSteps {
		fmt.Printf("    %s\n", step)
	}
}

func runRecover() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	jobStore := jobs.NewStore(cfg.StateDir)
	latestJob, _ := jobStore.LoadLatest()

	// recover only reads container state; it never stops, removes or starts one.
	runner := &dockerexec.Runner{DockerBin: cfg.DockerBin, Logger: log.Default()}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	containerName := resolveTargetContainerOrDefault(ctx, cfg, latestJob)
	coreBaseURL := resolveCoreBaseURL(ctx, cfg, containerName, latestJob)

	result, err := recover.NewRecoverer(jobStore, runner, containerName, coreBaseURL).Run(ctx)
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
		fmt.Println("NOTHING TO RECOVER")
	} else {
		fmt.Println("ACTION NEEDED")
	}
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nMessage: %s\n", result.Message)

	if result.Refusals != "" {
		fmt.Printf("\nReason: %s\n", result.Refusals)
	}

	if result.Playbook != nil {
		printPlaybookSteps("RECOVERY STEPS", result.Playbook)
	}

	fmt.Println(strings.Repeat("=", 60))

	// Exit with non-zero if the merchant has something to do
	if !result.Success {
		os.Exit(1)
	}
}

// syncRefusal explains why sync must not overwrite the latest job, or returns
// "" when it may. Overwriting an active job would hide a running update (and
// let a second one start) or erase what an interrupted one needs to be put back.
func syncRefusal(job *jobs.Job, now time.Time) string {
	if job == nil || !job.IsActive() {
		return ""
	}
	silentFor := now.Sub(job.UpdatedAt).Round(time.Second)
	if recovery.IsStale(job, now) {
		return fmt.Sprintf("The last update (state %s) stopped making progress %s ago. sync will not write over an interrupted update.\n"+
			"Restart the updater instead: sudo systemctl restart payram-updater. It puts PayRam back on its own.", job.State, silentFor)
	}
	return fmt.Sprintf("An update is running right now (state %s, last progress %s ago). sync would overwrite it and let a second update start.\n"+
		"Wait for it to finish, then check with: payram-updater status", job.State, silentFor)
}

// syncedJob records that PayRam runs version, keeping the backup and switch
// bookkeeping of the job it replaces so nothing that job knew is lost.
func syncedJob(existing *jobs.Job, version, containerName, coreBaseURL string, now time.Time) *jobs.Job {
	previousVersion := "unknown"
	job := jobs.NewJob(fmt.Sprintf("sync-%d", now.UnixNano()), jobs.JobModeManual, version)
	if existing != nil {
		previousVersion = existing.ResolvedTarget
		job.BackupPath = existing.BackupPath
		job.ContainerName = existing.ContainerName
		job.FromVersion = existing.FromVersion
		job.SwitchStage = existing.SwitchStage
		job.NextContainer = existing.NextContainer
		job.PreviousContainer = existing.PreviousContainer
		job.PreviousImage = existing.PreviousImage
		job.PreviousRestartPolicy = existing.PreviousRestartPolicy
		job.QuiescedPrograms = existing.QuiescedPrograms
	}
	if job.ContainerName == "" {
		job.ContainerName = containerName
	}
	job.CoreBaseURL = coreBaseURL // just answered sync's health check
	job.ResolvedTarget = version
	job.State = jobs.JobStateReady
	job.Message = fmt.Sprintf("Synced from external upgrade (was %s, now %s)", previousVersion, version)
	return job
}

func runSync() {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Hold the container lock while deciding and writing, so no update can
	// start between the check below and the save.
	release, err := jobs.AcquireContainerLock(cfg.StateDir)
	if err != nil {
		if errors.Is(err, jobs.ErrUpgradeLocked) {
			fmt.Fprintln(os.Stderr, "An update or restore is changing PayRam right now. Wait for it to finish, then run sync again.")
		} else {
			fmt.Fprintf(os.Stderr, "Failed to take the updater lock: %v\n", err)
		}
		os.Exit(1)
	}
	defer release()

	// Never write over an update that is running or was interrupted.
	jobStore := jobs.NewStore(cfg.StateDir)
	existingJob, err := jobStore.LoadLatest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read the updater's state: %v\n", err)
		fmt.Fprintf(os.Stderr, "sync will not overwrite a state file it cannot read.\n")
		os.Exit(1)
	}
	if refusal := syncRefusal(existingJob, time.Now().UTC()); refusal != "" {
		fmt.Fprintln(os.Stderr, refusal)
		os.Exit(1)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	containerName, err := resolveTargetContainer(ctx, cfg, existingJob)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve target container: %v\n", err)
		fmt.Fprintf(os.Stderr, "Set TARGET_CONTAINER_NAME environment variable or ensure manifest has container_name\n")
		os.Exit(1)
	}
	fmt.Printf("Target container resolved as: %s\n\n", containerName)

	coreBaseURL := resolveCoreBaseURL(ctx, cfg, containerName, existingJob)
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

	if existingJob != nil && existingJob.State == jobs.JobStateReady && existingJob.ResolvedTarget == currentVersion {
		fmt.Printf("Internal state already matches running version (%s). No sync needed.\n", currentVersion)
		return
	}

	// Record the running version, keeping what the previous job knew.
	syncJob := syncedJob(existingJob, currentVersion, containerName, coreBaseURL, time.Now().UTC())
	previousVersion := "unknown"
	if existingJob != nil {
		previousVersion = existingJob.ResolvedTarget
	}

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
	fmt.Println("SYNC SUCCESSFUL")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nPrevious tracked version: %s\n", previousVersion)
	fmt.Printf("Current running version:  %s\n", currentVersion)
	fmt.Printf("Health status:            OK (status=%s, db=%s)\n", healthStatus, healthDB)
	fmt.Println("\nInternal state has been updated to match the running version.")
	fmt.Println("Run 'payram-updater inspect' to verify.")
	fmt.Println(strings.Repeat("=", 60))
}
