// Package inspect provides read-only diagnostics for Payram upgrade state.
package inspect

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/corecompat"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/policy"
	"github.com/payram/payram-updater/internal/recovery"
)

// OverallState represents the overall system health state.
type OverallState string

const (
	StateOK       OverallState = "OK"
	StateDegraded OverallState = "DEGRADED"
	StateBroken   OverallState = "BROKEN"
)

// Issue represents a detected problem.
type Issue struct {
	Component   string `json:"component"`
	Description string `json:"description"`
	Severity    string `json:"severity"` // INFO, WARNING, CRITICAL
}

// Recommendation represents a suggested action.
type Recommendation struct {
	Action      string `json:"action"`
	Description string `json:"description"`
	Priority    int    `json:"priority"` // 1 = highest
}

// UpdateInfo contains information about available updates.
type UpdateInfo struct {
	CurrentVersion        string          `json:"currentVersion"`
	LatestVersion         string          `json:"latestVersion"`
	UpdateAvailable       bool            `json:"updateAvailable"`
	CanUpdateViaDashboard bool            `json:"canUpdateViaDashboard"`
	MaxDashboardVersion   string          `json:"maxDashboardVersion,omitempty"`
	NextBreakpoint        *BreakpointInfo `json:"nextBreakpoint,omitempty"`
	Message               string          `json:"message"`
}

// BreakpointInfo contains information about the next version breakpoint.
type BreakpointInfo struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
	Docs    string `json:"docs"`
}

// InspectResult contains the full inspection output.
type InspectResult struct {
	OverallState     OverallState           `json:"overallState"`
	Issues           []Issue                `json:"issues"`
	Recommendations  []Recommendation       `json:"recommendations"`
	LastJob          *jobs.Job              `json:"lastJob,omitempty"`
	RecoveryPlaybook *recovery.Playbook     `json:"recoveryPlaybook,omitempty"`
	UpdateInfo       *UpdateInfo            `json:"updateInfo,omitempty"`
	Checks           map[string]CheckResult `json:"checks"`
}

// CheckResult represents the result of a single check.
type CheckResult struct {
	Status  string `json:"status"` // OK, WARNING, FAILED, UNKNOWN
	Message string `json:"message"`
}

// Inspector performs read-only system inspection.
type Inspector struct {
	jobStore      *jobs.Store
	dockerBin     string
	containerName string
	coreBaseURL   string
	policyURL     string
	manifestURL   string
	policyInitVer string
	policyInitSet bool
	debugMode     bool
	releaseOrder  []string // For debug mode version ordering
	now           func() time.Time
}

// containerFacts is what checkContainer learned, for the recommendations.
type containerFacts struct {
	checked        bool   // Docker answered the inspect
	exists         bool   // PayRam's container exists
	status         string // its Docker status: running, exited, created, ...
	previousName   string // the old container the switch renames aside
	previousExists bool
	previousStatus string
}

// NewInspector creates a new inspector with the given configuration.
func NewInspector(
	jobStore *jobs.Store,
	dockerBin string,
	containerName string,
	coreBaseURL string,
	policyURL string,
	manifestURL string,
	debugMode bool,
) *Inspector {
	return &Inspector{
		jobStore:      jobStore,
		dockerBin:     dockerBin,
		containerName: containerName,
		coreBaseURL:   coreBaseURL,
		policyURL:     policyURL,
		manifestURL:   manifestURL,
		debugMode:     debugMode,
		now:           time.Now,
	}
}

// Run performs all inspection checks and returns the result.
func (i *Inspector) Run(ctx context.Context) *InspectResult {
	result := &InspectResult{
		OverallState:    StateOK,
		Issues:          []Issue{},
		Recommendations: []Recommendation{},
		Checks:          make(map[string]CheckResult),
	}

	// Check 1: Last upgrade job state
	i.checkLastJob(result)

	// Check 2: Docker daemon availability
	i.checkDockerDaemon(ctx, result)

	// Check 3: Container existence and running state
	facts := i.checkContainer(ctx, result)

	// Check 4: Policy readability
	i.checkPolicy(ctx, result)

	// Check 5: Manifest readability
	i.checkManifest(ctx, result)

	// Check 6: Health endpoint (if container running)
	i.checkHealth(ctx, result)

	// Check 7: Running version (if container running)
	i.checkVersion(ctx, result)

	// Check 8: Update availability
	i.checkUpdateAvailability(ctx, result)

	// Generate recommendations based on state
	i.generateRecommendations(result, facts)

	return result
}

func (i *Inspector) checkLastJob(result *InspectResult) {
	job, err := i.jobStore.LoadLatest()
	if err != nil {
		result.Checks["lastJob"] = CheckResult{
			Status:  "UNKNOWN",
			Message: fmt.Sprintf("Failed to load job: %v", err),
		}
		return
	}

	if job == nil {
		result.Checks["lastJob"] = CheckResult{
			Status:  "OK",
			Message: "No previous upgrade job",
		}
		return
	}

	result.LastJob = job

	switch job.State {
	case jobs.JobStateReady:
		result.Checks["lastJob"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Last upgrade completed successfully (target: %s)", job.ResolvedTarget),
		}
	case jobs.JobStateFailed:
		result.Checks["lastJob"] = CheckResult{
			Status:  "FAILED",
			Message: fmt.Sprintf("Last upgrade failed: %s - %s", job.FailureCode, job.Message),
		}

		// Attach recovery playbook (rendered with runtime context if available)
		playbook := recovery.RenderJobPlaybook(job, i.buildPlaybookContext())
		result.RecoveryPlaybook = &playbook

		// A failure that left PayRam unchanged or put it back is not a broken
		// system: PayRam runs. Only an uncertain outcome is.
		if playbook.PayRamState == recovery.PayRamUncertain {
			result.Issues = append(result.Issues, Issue{
				Component:   "upgrade",
				Description: fmt.Sprintf("Last upgrade failed with code %s: %s", job.FailureCode, job.Message),
				Severity:    "CRITICAL",
			})
			result.OverallState = StateBroken
		} else {
			result.Issues = append(result.Issues, Issue{
				Component:   "upgrade",
				Description: fmt.Sprintf("Last upgrade failed with code %s (PayRam was left running): %s", job.FailureCode, job.Message),
				Severity:    "WARNING",
			})
			if result.OverallState == StateOK {
				result.OverallState = StateDegraded
			}
		}
	case jobs.JobStatePolicyFetching, jobs.JobStateManifestFetching,
		jobs.JobStateBackingUp, jobs.JobStateExecuting, jobs.JobStateVerifying:
		silentFor := i.now().Sub(job.UpdatedAt).Round(time.Second)
		if !recovery.IsStale(job, i.now()) {
			// A running update saves progress at least every 30 seconds.
			result.Checks["lastJob"] = CheckResult{
				Status:  "WARNING",
				Message: fmt.Sprintf("Upgrade in progress: %s (last progress %s ago)", job.State, silentFor),
			}
			result.Issues = append(result.Issues, Issue{
				Component:   "upgrade",
				Description: fmt.Sprintf("An upgrade is running (state: %s)", job.State),
				Severity:    "INFO",
			})
			if result.OverallState == StateOK {
				result.OverallState = StateDegraded
			}
			return
		}

		result.Checks["lastJob"] = CheckResult{
			Status:  "FAILED",
			Message: fmt.Sprintf("Upgrade stopped responding in state %s (no progress for %s)", job.State, silentFor),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "upgrade",
			Description: fmt.Sprintf("The upgrade in state %s has made no progress for %s; the updater has stopped working on it", job.State, silentFor),
			Severity:    "CRITICAL",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}

		playbook := recovery.RenderPlaybook(recovery.StalePlaybookCode(job.State), i.buildPlaybookContext().WithJob(job))
		result.RecoveryPlaybook = &playbook
	default:
		result.Checks["lastJob"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Job state: %s", job.State),
		}
	}
}

func (i *Inspector) checkDockerDaemon(ctx context.Context, result *InspectResult) {
	cmd := exec.CommandContext(ctx, i.dockerBin, "info", "--format", "{{.ServerVersion}}")
	output, err := cmd.Output()
	if err != nil {
		result.Checks["dockerDaemon"] = CheckResult{
			Status:  "FAILED",
			Message: fmt.Sprintf("Docker daemon not accessible: %v", err),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "docker",
			Description: "Docker daemon is not running or not accessible",
			Severity:    "CRITICAL",
		})
		result.OverallState = StateBroken
		return
	}

	result.Checks["dockerDaemon"] = CheckResult{
		Status:  "OK",
		Message: fmt.Sprintf("Docker daemon running (version: %s)", strings.TrimSpace(string(output))),
	}
}

func (i *Inspector) checkContainer(ctx context.Context, result *InspectResult) containerFacts {
	facts := containerFacts{previousName: i.containerName + "-previous"}
	if result.LastJob != nil && result.LastJob.PreviousContainer != "" {
		facts.previousName = result.LastJob.PreviousContainer
	}

	status, exists, err := i.containerStatus(ctx, i.containerName)
	if err != nil {
		result.Checks["container"] = CheckResult{
			Status:  "UNKNOWN",
			Message: fmt.Sprintf("Could not check container '%s': %v", i.containerName, err),
		}
		return facts
	}
	facts.checked = true
	facts.exists = exists
	facts.status = status
	if !exists || status != "running" {
		facts.previousStatus, facts.previousExists, _ = i.containerStatus(ctx, facts.previousName)
	}

	if !exists {
		result.Checks["container"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Container '%s' does not exist", i.containerName),
		}
		description := fmt.Sprintf("Container '%s' is missing", i.containerName)
		if facts.previousExists {
			description += fmt.Sprintf("; '%s' (the version before the last update) exists and is %s", facts.previousName, facts.previousStatus)
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "container",
			Description: description,
			Severity:    "CRITICAL",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
		return facts
	}

	if status == "running" {
		// Get image info
		imgCmd := exec.CommandContext(ctx, i.dockerBin, "inspect", "--format", "{{.Config.Image}}", i.containerName)
		imgOutput, _ := imgCmd.Output()
		image := strings.TrimSpace(string(imgOutput))

		result.Checks["container"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Container '%s' is running (image: %s)", i.containerName, image),
		}
	} else {
		result.Checks["container"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Container '%s' exists but is %s", i.containerName, status),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "container",
			Description: fmt.Sprintf("Container is not running (status: %s)", status),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
	}
	return facts
}

// containerStatus returns a container's Docker status ("running", "exited",
// ...) and whether it exists. err is set only when Docker itself could not
// answer, so a missing container is never confused with a Docker outage.
func (i *Inspector) containerStatus(ctx context.Context, name string) (string, bool, error) {
	output, err := exec.CommandContext(ctx, i.dockerBin, "inspect", "--format", "{{.State.Status}}", name).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(output))
		if strings.Contains(text, "No such object") || strings.Contains(text, "No such container") {
			return "", false, nil
		}
		if text == "" {
			return "", false, err
		}
		return "", false, fmt.Errorf("%w: %s", err, text)
	}
	return strings.TrimSpace(string(output)), true, nil
}

func (i *Inspector) checkPolicy(ctx context.Context, result *InspectResult) {
	if i.policyURL == "" {
		result.Checks["policy"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Policy URL not configured",
		}
		return
	}

	client := policy.NewClient(5 * time.Second)
	_, err := client.Fetch(ctx, i.policyURL)
	if err != nil {
		result.Checks["policy"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Failed to fetch policy: %v", err),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "policy",
			Description: fmt.Sprintf("Cannot fetch upgrade policy: %v", err),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
		return
	}

	result.Checks["policy"] = CheckResult{
		Status:  "OK",
		Message: "Policy is readable",
	}
}

func (i *Inspector) checkManifest(ctx context.Context, result *InspectResult) {
	if i.manifestURL == "" {
		result.Checks["manifest"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Manifest URL not configured",
		}
		return
	}

	client := manifest.NewClient(5 * time.Second)
	_, err := client.Fetch(ctx, i.manifestURL)
	if err != nil {
		result.Checks["manifest"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Failed to fetch manifest: %v", err),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "manifest",
			Description: fmt.Sprintf("Cannot fetch runtime manifest: %v", err),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
		return
	}

	result.Checks["manifest"] = CheckResult{
		Status:  "OK",
		Message: "Manifest is readable",
	}
}

func (i *Inspector) checkHealth(ctx context.Context, result *InspectResult) {
	// Only check health if container appears to be running
	containerCheck, ok := result.Checks["container"]
	if !ok || containerCheck.Status != "OK" || !strings.Contains(containerCheck.Message, "running") {
		result.Checks["health"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Skipped (container not running)",
		}
		return
	}

	initVersion := i.getPolicyInitVersion(ctx)
	_, useLegacy, err := i.resolveCoreVersion(ctx, initVersion)
	if err != nil {
		result.Checks["health"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Health check failed: %v", err),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "health",
			Description: fmt.Sprintf("Health endpoint not responding: %v", err),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
		return
	}

	client := coreclient.NewClient(i.coreBaseURL)
	healthCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var healthResp *coreclient.HealthResponse
	if useLegacy {
		err = corecompat.LegacyHealth(healthCtx, i.coreBaseURL)
		if err == nil {
			healthResp = &coreclient.HealthResponse{Status: "ok"}
		}
	} else {
		healthResp, err = client.Health(healthCtx)
	}
	if err != nil {
		result.Checks["health"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Health check failed: %v", err),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "health",
			Description: fmt.Sprintf("Health endpoint not responding: %v", err),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
		return
	}

	if healthResp.Status == "ok" && (healthResp.DB == "ok" || healthResp.DB == "") {
		result.Checks["health"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Health OK (status=%s, db=%s)", healthResp.Status, healthResp.DB),
		}
	} else {
		result.Checks["health"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Health degraded (status=%s, db=%s)", healthResp.Status, healthResp.DB),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "health",
			Description: fmt.Sprintf("Health check returned degraded status (status=%s, db=%s)", healthResp.Status, healthResp.DB),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
	}
}

func (i *Inspector) checkVersion(ctx context.Context, result *InspectResult) {
	// Only check version if container appears to be running
	containerCheck, ok := result.Checks["container"]
	if !ok || containerCheck.Status != "OK" || !strings.Contains(containerCheck.Message, "running") {
		result.Checks["version"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Skipped (container not running)",
		}
		return
	}

	initVersion := i.getPolicyInitVersion(ctx)
	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	versionValue, _, err := i.resolveCoreVersion(versionCtx, initVersion)
	if err != nil {
		result.Checks["version"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Version check failed: %v", err),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "version",
			Description: fmt.Sprintf("Version endpoint not responding: %v", err),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
		return
	}

	result.Checks["version"] = CheckResult{
		Status:  "OK",
		Message: fmt.Sprintf("Running version: %s", versionValue),
	}

	// Check for version mismatch with last successful job
	if result.LastJob != nil && result.LastJob.State == jobs.JobStateReady {
		if versionValue != result.LastJob.ResolvedTarget {
			cmp := i.compareVersions(versionValue, result.LastJob.ResolvedTarget)

			if cmp < 0 {
				// Running version is LOWER than expected - downgrade detected
				result.Checks["version"] = CheckResult{
					Status:  "FAILED",
					Message: fmt.Sprintf("Downgrade detected: running %s (expected %s)", versionValue, result.LastJob.ResolvedTarget),
				}
				result.Issues = append(result.Issues, Issue{
					Component:   "version",
					Description: fmt.Sprintf("Downgrade detected: running %s but last upgrade targeted %s", versionValue, result.LastJob.ResolvedTarget),
					Severity:    "CRITICAL",
				})
				result.OverallState = StateBroken
			} else if cmp > 0 {
				// Running version is HIGHER than expected - external upgrade detected
				// Check if health is OK
				healthCheck, healthOK := result.Checks["health"]
				if healthOK && healthCheck.Status == "OK" {
					result.Checks["version"] = CheckResult{
						Status:  "WARNING",
						Message: fmt.Sprintf("External upgrade detected: running %s (expected %s)", versionValue, result.LastJob.ResolvedTarget),
					}
					result.Issues = append(result.Issues, Issue{
						Component:   "version",
						Description: fmt.Sprintf("External upgrade detected: running %s but last upgrade targeted %s. System is healthy.", versionValue, result.LastJob.ResolvedTarget),
						Severity:    "INFO",
					})
					// Don't degrade state if health is OK - this is recoverable
				} else {
					result.Checks["version"] = CheckResult{
						Status:  "WARNING",
						Message: fmt.Sprintf("External upgrade detected: running %s (expected %s), health not confirmed", versionValue, result.LastJob.ResolvedTarget),
					}
					result.Issues = append(result.Issues, Issue{
						Component:   "version",
						Description: fmt.Sprintf("External upgrade detected: running %s but last upgrade targeted %s. Health status unclear.", versionValue, result.LastJob.ResolvedTarget),
						Severity:    "WARNING",
					})
					if result.OverallState == StateOK {
						result.OverallState = StateDegraded
					}
				}
			} else {
				// Versions are same but strings differ (shouldn't happen often)
				result.Checks["version"] = CheckResult{
					Status:  "WARNING",
					Message: fmt.Sprintf("Running version: %s (expected %s from last upgrade)", versionValue, result.LastJob.ResolvedTarget),
				}
				result.Issues = append(result.Issues, Issue{
					Component:   "version",
					Description: fmt.Sprintf("Version mismatch: running %s but last upgrade targeted %s", versionValue, result.LastJob.ResolvedTarget),
					Severity:    "WARNING",
				})
				if result.OverallState == StateOK {
					result.OverallState = StateDegraded
				}
			}
		}
	}
}

func (i *Inspector) getPolicyInitVersion(ctx context.Context) string {
	if i.policyInitSet {
		return i.policyInitVer
	}
	if i.policyURL == "" {
		i.policyInitSet = true
		return ""
	}

	client := policy.NewClient(5 * time.Second)
	policyData, err := client.Fetch(ctx, i.policyURL)
	if err != nil {
		i.policyInitSet = true
		return ""
	}

	i.policyInitVer = strings.TrimSpace(policyData.UpdaterAPIInitVersion)
	i.policyInitSet = true
	return i.policyInitVer
}

func (i *Inspector) resolveCoreVersion(ctx context.Context, initVersion string) (string, bool, error) {
	client := coreclient.NewClient(i.coreBaseURL)
	versionResp, err := client.Version(ctx)
	if err == nil && versionResp != nil && versionResp.Version != "" {
		legacy, legacyErr := corecompat.IsBeforeInit(versionResp.Version, initVersion)
		if legacyErr != nil {
			return versionResp.Version, false, nil
		}
		return versionResp.Version, legacy, nil
	}

	labelVersion, err := corecompat.VersionFromLabels(ctx, i.dockerBin, i.containerName)
	if err != nil {
		return "", false, err
	}

	legacy, legacyErr := corecompat.IsBeforeInit(labelVersion, initVersion)
	if legacyErr != nil {
		return labelVersion, false, nil
	}

	return labelVersion, legacy, nil
}

func (i *Inspector) checkUpdateAvailability(ctx context.Context, result *InspectResult) {
	if i.policyURL == "" {
		result.Checks["updateCheck"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Policy URL not configured",
		}
		return
	}

	// Get current version from version check
	versionCheck, versionExists := result.Checks["version"]
	if !versionExists || versionCheck.Status != "OK" {
		result.Checks["updateCheck"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Cannot check updates - current version unknown",
		}
		return
	}

	// Extract current version from version check message
	currentVersion := ""
	if strings.Contains(versionCheck.Message, "Running version: ") {
		currentVersion = strings.TrimSpace(strings.TrimPrefix(versionCheck.Message, "Running version: "))
	}
	if currentVersion == "" {
		result.Checks["updateCheck"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Cannot parse current version",
		}
		return
	}

	// Fetch policy
	policyClient := policy.NewClient(5 * time.Second)
	policyData, err := policyClient.Fetch(ctx, i.policyURL)
	if err != nil {
		result.Checks["updateCheck"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Failed to fetch policy: %v", err),
		}
		return
	}

	latestVersion := strings.TrimSpace(policyData.Latest)
	if latestVersion == "" {
		result.Checks["updateCheck"] = CheckResult{
			Status:  "WARNING",
			Message: "Policy does not specify latest version",
		}
		return
	}

	// Store release order for debug mode
	i.releaseOrder = policyData.Releases

	// Normalize versions for comparison
	currentNorm := corecompat.NormalizeVersion(currentVersion)
	latestNorm := corecompat.NormalizeVersion(latestVersion)

	// Compare versions
	cmp := i.compareVersions(currentNorm, latestNorm)

	updateInfo := &UpdateInfo{
		CurrentVersion:  currentVersion,
		LatestVersion:   latestVersion,
		UpdateAvailable: cmp < 0,
	}

	if cmp == 0 {
		// Already on latest
		updateInfo.CanUpdateViaDashboard = false
		updateInfo.Message = "Already on latest version"
		result.Checks["updateCheck"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Running latest version %s", currentVersion),
		}
	} else if cmp > 0 {
		// Running version is ahead of policy latest (unusual)
		updateInfo.CanUpdateViaDashboard = false
		updateInfo.Message = "Running version is ahead of policy latest"
		result.Checks["updateCheck"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Running version %s is ahead of latest %s", currentVersion, latestVersion),
		}
	} else {
		// Update available
		// Find the lowest gate (breakpoint or stop point) between current and latest.
		// Both are surfaced via nextBreakpoint for backward compatibility with payram-core.
		hasBreakpoint := false
		var nextBreakpoint *policy.Breakpoint
		nextBreakpointIsStopPoint := false

		for _, bp := range policyData.Breakpoints {
			bpNorm := corecompat.NormalizeVersion(bp.Version)
			// Check if gate is between current and latest (or equal to latest)
			cmpCurrent := i.compareVersions(bpNorm, currentNorm)
			cmpLatest := i.compareVersions(bpNorm, latestNorm)

			if cmpCurrent > 0 && cmpLatest <= 0 {
				hasBreakpoint = true
				if nextBreakpoint == nil || i.compareVersions(bpNorm, corecompat.NormalizeVersion(nextBreakpoint.Version)) < 0 {
					nextBreakpoint = &bp
					nextBreakpointIsStopPoint = false
				}
			}
		}
		for _, sp := range policyData.StopPoints {
			spNorm := corecompat.NormalizeVersion(sp.Version)
			cmpCurrent := i.compareVersions(spNorm, currentNorm)
			cmpLatest := i.compareVersions(spNorm, latestNorm)

			if cmpCurrent > 0 && cmpLatest <= 0 {
				hasBreakpoint = true
				if nextBreakpoint == nil || i.compareVersions(spNorm, corecompat.NormalizeVersion(nextBreakpoint.Version)) < 0 {
					// Map stop point into a Breakpoint so the rest of the logic is identical.
					nextBreakpoint = &policy.Breakpoint{Version: sp.Version, Reason: sp.Reason, Docs: sp.Docs}
					nextBreakpointIsStopPoint = true
				}
			}
		}
		_ = nextBreakpointIsStopPoint // used in canUpdateViaDashboard logic below

		if hasBreakpoint && nextBreakpoint != nil {
			// Breakpoint in the way of latest - may still allow dashboard update up to max version
			updateInfo.NextBreakpoint = &BreakpointInfo{
				Version: nextBreakpoint.Version,
				Reason:  nextBreakpoint.Reason,
				Docs:    nextBreakpoint.Docs,
			}

			// Find max dashboard version (highest release before the gate)
			maxDashboardVer := ""
			breakpointNorm := corecompat.NormalizeVersion(nextBreakpoint.Version)
			for _, release := range policyData.Releases {
				releaseNorm := corecompat.NormalizeVersion(release)
				// Release must be after current and before gate
				if i.compareVersions(releaseNorm, currentNorm) > 0 && i.compareVersions(releaseNorm, breakpointNorm) < 0 {
					if maxDashboardVer == "" || i.compareVersions(releaseNorm, corecompat.NormalizeVersion(maxDashboardVer)) > 0 {
						maxDashboardVer = release
					}
				}
			}
			if maxDashboardVer != "" {
				updateInfo.MaxDashboardVersion = maxDashboardVer
			}

			if !nextBreakpointIsStopPoint {
				// Breakpoint: dashboard chains through the stepping stone transparently in one job.
				// MaxDashboardVersion is the breakpoint version itself — the frontend sends this as
				// requestedTarget, the updater handles the stepping-stone hop silently inside the job.
				updateInfo.CanUpdateViaDashboard = true
				updateInfo.MaxDashboardVersion = nextBreakpoint.Version
				updateInfo.Message = fmt.Sprintf("Update to %s available via dashboard (breakpoint at %s handled automatically)", nextBreakpoint.Version, nextBreakpoint.Version)
				result.Checks["updateCheck"] = CheckResult{
					Status:  "OK",
					Message: fmt.Sprintf("Update available: %s → %s (dashboard upgrade, breakpoint handled automatically)", currentVersion, nextBreakpoint.Version),
				}
			} else if maxDashboardVer != "" {
				// Stop point: dashboard can upgrade to the stepping stone; SSH needed after.
				updateInfo.CanUpdateViaDashboard = true
				updateInfo.Message = fmt.Sprintf("Update to %s available via dashboard; upgrade to %s requires SSH (stop point at %s)", maxDashboardVer, latestVersion, nextBreakpoint.Version)
				result.Checks["updateCheck"] = CheckResult{
					Status:  "WARNING",
					Message: fmt.Sprintf("Update available up to %s via dashboard; %s requires SSH at stop point %s.", maxDashboardVer, latestVersion, nextBreakpoint.Version),
				}
			} else {
				// Stop point and no stepping stone below it — SSH required immediately.
				updateInfo.CanUpdateViaDashboard = false
				updateInfo.Message = fmt.Sprintf("Upgrade to %s requires SSH (stop point at %s)", latestVersion, nextBreakpoint.Version)
				result.Checks["updateCheck"] = CheckResult{
					Status:  "WARNING",
					Message: fmt.Sprintf("Update available but blocked by stop point at %s. SSH required.", nextBreakpoint.Version),
				}
			}
		} else {
			// Can update via dashboard
			updateInfo.CanUpdateViaDashboard = true
			updateInfo.MaxDashboardVersion = latestVersion
			updateInfo.Message = fmt.Sprintf("Update to %s available via dashboard", latestVersion)
			result.Checks["updateCheck"] = CheckResult{
				Status:  "OK",
				Message: fmt.Sprintf("Update available: %s → %s (dashboard upgrade)", currentVersion, latestVersion),
			}
		}
	}

	result.UpdateInfo = updateInfo
}

// compareVersions compares two version strings.
// In debug mode, uses release list ordering. Otherwise uses semver parsing.
// Returns: -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2
func (i *Inspector) compareVersions(v1, v2 string) int {
	// In debug mode, use release list ordering
	if i.debugMode && len(i.releaseOrder) > 0 {
		idx1 := -1
		idx2 := -1
		for idx, release := range i.releaseOrder {
			if corecompat.NormalizeVersion(release) == v1 {
				idx1 = idx
			}
			if corecompat.NormalizeVersion(release) == v2 {
				idx2 = idx
			}
		}
		// If both versions found in release list, compare by position
		// Earlier index means newer version (list order is authoritative)
		if idx1 != -1 && idx2 != -1 {
			if idx1 < idx2 {
				return 1
			} else if idx1 > idx2 {
				return -1
			}
			return 0
		}
		// Fall through to semver comparison if not found in list
	}

	// Standard semver comparison
	// Strip leading 'v' if present
	v1 = strings.TrimPrefix(v1, "v")
	v2 = strings.TrimPrefix(v2, "v")

	// Check if either version is non-semver (doesn't start with a digit)
	v1IsSemver := len(v1) > 0 && v1[0] >= '0' && v1[0] <= '9'
	v2IsSemver := len(v2) > 0 && v2[0] >= '0' && v2[0] <= '9'

	// If either is non-semver, do string comparison (only equal if identical)
	if !v1IsSemver || !v2IsSemver {
		if v1 == v2 {
			return 0
		}
		// Non-semver versions can't be ordered, but they're definitely different
		// Report as "update available" (-1) when current is non-semver and latest differs
		return -1
	}

	parts1 := strings.Split(v1, ".")
	parts2 := strings.Split(v2, ".")

	// Compare each part
	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(parts1) {
			// Extract numeric part (ignore pre-release suffixes like -beta)
			numStr := strings.Split(parts1[i], "-")[0]
			fmt.Sscanf(numStr, "%d", &n1)
		}
		if i < len(parts2) {
			numStr := strings.Split(parts2[i], "-")[0]
			fmt.Sscanf(numStr, "%d", &n2)
		}

		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
	}

	return 0
}

func (i *Inspector) generateRecommendations(result *InspectResult, facts containerFacts) {
	// Priority-based recommendations
	priority := 1
	add := func(action, description string) {
		result.Recommendations = append(result.Recommendations, Recommendation{
			Action:      action,
			Description: description,
			Priority:    priority,
		})
		priority++
	}

	job := result.LastJob
	stale := recovery.IsStale(job, i.now())
	updateRunning := job != nil && job.IsActive() && !stale

	switch {
	case updateRunning:
		add("wait", fmt.Sprintf("An update is running (state %s). Wait for it to finish; do not restart PayRam or the updater meanwhile.", job.State))
	case stale:
		add("restart_updater", fmt.Sprintf("The update in state %s has made no progress for more than %d minutes. Restart the updater: sudo systemctl restart payram-updater. It puts PayRam back on its own.", job.State, int(recovery.HeartbeatStaleAfter.Minutes())))
	case job != nil && job.State == jobs.JobStateFailed && result.RecoveryPlaybook != nil:
		switch result.RecoveryPlaybook.PayRamState {
		case recovery.PayRamUnchanged:
			add("retry", "PayRam was not changed by the failed update. Fix the cause shown in the recovery playbook, then retry the update.")
		case recovery.PayRamRestored:
			add("send_logs", "The updater put PayRam back on the version it had before the update. Send the updater logs to PayRam support, then retry the update later.")
		default:
			add("check_health", "First check whether PayRam is healthy (step 1 of the recovery playbook), then follow the playbook. Do not restore a backup unless PayRam support asks you to.")
		}
	}

	// Check for version mismatch scenarios
	versionCheck, versionOK := result.Checks["version"]
	if versionOK {
		// Downgrade detected - contact Payram team
		if versionCheck.Status == "FAILED" && strings.Contains(versionCheck.Message, "Downgrade detected") {
			result.Recommendations = append(result.Recommendations, Recommendation{
				Action:      "contact_support",
				Description: "CRITICAL: Downgrade detected. Please contact Payram team for recovery assistance.",
				Priority:    1, // Highest priority
			})
		}

		// External upgrade detected with healthy system - offer sync
		if versionCheck.Status == "WARNING" && strings.Contains(versionCheck.Message, "External upgrade detected") {
			healthCheck, healthOK := result.Checks["health"]
			if healthOK && healthCheck.Status == "OK" {
				add("sync", "Run 'payram-updater sync' to update internal state to match running version.")
			}
		}
	}

	// A running update stops and renames containers on purpose; only advise
	// on the container when no update is working on it.
	if facts.checked && !updateRunning {
		name, previous := i.containerName, facts.previousName
		putPreviousBack := fmt.Sprintf("docker rename %s %s && docker update --restart unless-stopped %s && docker start %s", previous, name, name, name)
		lastUpdateFailed := stale || (job != nil && job.State == jobs.JobStateFailed)
		afterRestart := ""
		if stale {
			// The restarted updater repairs the switch itself; doing it by
			// hand at the same time would race it.
			afterRestart = "If PayRam is still not running a minute after you restart the updater: "
		}
		switch {
		case !facts.exists && facts.previousExists:
			add("put_previous_back", fmt.Sprintf("PayRam's container '%s' is missing but '%s' (the version that ran before the update) exists: an update was interrupted while switching versions. Restart the updater (sudo systemctl restart payram-updater); it repairs an interrupted update on its own. If PayRam is still not running a minute later, put the previous version back (this does not touch your data): %s", name, previous, putPreviousBack))
		case !facts.exists:
			add("find_container", fmt.Sprintf("PayRam's container '%s' does not exist. List every container with: docker ps -a. If PayRam runs under another name, set TARGET_CONTAINER_NAME in /etc/payram/updater.env; otherwise contact PayRam support. Do not delete PayRam's data folders or Docker volumes.", name))
		case facts.status != "running" && facts.previousExists && lastUpdateFailed:
			add("put_previous_back", fmt.Sprintf("%sPayRam's container '%s' is %s and '%s' (the version that ran before the failed update) exists. Put the previous version back (this does not touch your data): docker rename %s %s-failed-manual && %s", afterRestart, name, facts.status, previous, name, name, putPreviousBack))
		case facts.status != "running":
			add("start_container", fmt.Sprintf("PayRam's container '%s' is %s. Start it: docker start %s", name, facts.status, name))
		}
	}

	// If docker daemon is down
	dockerCheck, ok := result.Checks["dockerDaemon"]
	if ok && dockerCheck.Status == "FAILED" {
		result.Recommendations = append(result.Recommendations, Recommendation{
			Action:      "start_docker",
			Description: "Docker is not running, so PayRam is not running either. Start it: sudo systemctl start docker",
			Priority:    1, // Highest priority
		})
	}

	// If everything is OK
	if result.OverallState == StateOK && len(result.Recommendations) == 0 {
		add("none", "System is healthy. No action required.")
	}
}

// buildPlaybookContext constructs a PlaybookContext from the inspector's
// configuration. Callers add the job with WithJob, whose recorded container
// and URL win over the inspector's guesses.
func (i *Inspector) buildPlaybookContext() recovery.PlaybookContext {
	return recovery.PlaybookContext{
		ContainerName: i.containerName,
		BaseURL:       i.coreBaseURL,
	}
}
