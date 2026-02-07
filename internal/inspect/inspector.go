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
	CurrentVersion        string          `json:"current_version"`
	LatestVersion         string          `json:"latest_version"`
	UpdateAvailable       bool            `json:"update_available"`
	CanUpdateViaDashboard bool            `json:"can_update_via_dashboard"`
	MaxDashboardVersion   string          `json:"max_dashboard_version,omitempty"`
	NextBreakpoint        *BreakpointInfo `json:"next_breakpoint,omitempty"`
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
	OverallState     OverallState           `json:"overall_state"`
	Issues           []Issue                `json:"issues"`
	Recommendations  []Recommendation       `json:"recommendations"`
	LastJob          *jobs.Job              `json:"last_job,omitempty"`
	RecoveryPlaybook *recovery.Playbook     `json:"recovery_playbook,omitempty"`
	UpdateInfo       *UpdateInfo            `json:"update_info,omitempty"`
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
	i.checkContainer(ctx, result)

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
	i.generateRecommendations(result)

	return result
}

func (i *Inspector) checkLastJob(result *InspectResult) {
	job, err := i.jobStore.LoadLatest()
	if err != nil {
		result.Checks["last_job"] = CheckResult{
			Status:  "UNKNOWN",
			Message: fmt.Sprintf("Failed to load job: %v", err),
		}
		return
	}

	if job == nil {
		result.Checks["last_job"] = CheckResult{
			Status:  "OK",
			Message: "No previous upgrade job",
		}
		return
	}

	result.LastJob = job

	switch job.State {
	case jobs.JobStateReady:
		result.Checks["last_job"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Last upgrade completed successfully (target: %s)", job.ResolvedTarget),
		}
	case jobs.JobStateFailed:
		result.Checks["last_job"] = CheckResult{
			Status:  "FAILED",
			Message: fmt.Sprintf("Last upgrade failed: %s - %s", job.FailureCode, job.Message),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "upgrade",
			Description: fmt.Sprintf("Last upgrade failed with code %s: %s", job.FailureCode, job.Message),
			Severity:    "CRITICAL",
		})
		result.OverallState = StateBroken

		// Attach recovery playbook (rendered with runtime context if available)
		ctx := i.buildPlaybookContext(job.BackupPath)
		playbook := recovery.RenderPlaybook(job.FailureCode, ctx)
		result.RecoveryPlaybook = &playbook
	case jobs.JobStateBackingUp, jobs.JobStateExecuting, jobs.JobStateVerifying:
		result.Checks["last_job"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Upgrade in progress: %s", job.State),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "upgrade",
			Description: fmt.Sprintf("Upgrade is still in state: %s", job.State),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
	default:
		result.Checks["last_job"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Job state: %s", job.State),
		}
	}
}

func (i *Inspector) checkDockerDaemon(ctx context.Context, result *InspectResult) {
	cmd := exec.CommandContext(ctx, i.dockerBin, "info", "--format", "{{.ServerVersion}}")
	output, err := cmd.Output()
	if err != nil {
		result.Checks["docker_daemon"] = CheckResult{
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

	result.Checks["docker_daemon"] = CheckResult{
		Status:  "OK",
		Message: fmt.Sprintf("Docker daemon running (version: %s)", strings.TrimSpace(string(output))),
	}
}

func (i *Inspector) checkContainer(ctx context.Context, result *InspectResult) {
	// Check if container exists
	cmd := exec.CommandContext(ctx, i.dockerBin, "inspect", "--format", "{{.State.Status}}", i.containerName)
	output, err := cmd.Output()
	if err != nil {
		result.Checks["container"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Container '%s' does not exist", i.containerName),
		}
		result.Issues = append(result.Issues, Issue{
			Component:   "container",
			Description: fmt.Sprintf("Container '%s' is missing", i.containerName),
			Severity:    "WARNING",
		})
		if result.OverallState == StateOK {
			result.OverallState = StateDegraded
		}
		return
	}

	status := strings.TrimSpace(string(output))
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
		result.Checks["update_check"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Policy URL not configured",
		}
		return
	}

	// Get current version from version check
	versionCheck, versionExists := result.Checks["version"]
	if !versionExists || versionCheck.Status != "OK" {
		result.Checks["update_check"] = CheckResult{
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
		result.Checks["update_check"] = CheckResult{
			Status:  "UNKNOWN",
			Message: "Cannot parse current version",
		}
		return
	}

	// Fetch policy
	policyClient := policy.NewClient(5 * time.Second)
	policyData, err := policyClient.Fetch(ctx, i.policyURL)
	if err != nil {
		result.Checks["update_check"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Failed to fetch policy: %v", err),
		}
		return
	}

	latestVersion := strings.TrimSpace(policyData.Latest)
	if latestVersion == "" {
		result.Checks["update_check"] = CheckResult{
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
		result.Checks["update_check"] = CheckResult{
			Status:  "OK",
			Message: fmt.Sprintf("Running latest version %s", currentVersion),
		}
	} else if cmp > 0 {
		// Running version is ahead of policy latest (unusual)
		updateInfo.CanUpdateViaDashboard = false
		updateInfo.Message = "Running version is ahead of policy latest"
		result.Checks["update_check"] = CheckResult{
			Status:  "WARNING",
			Message: fmt.Sprintf("Running version %s is ahead of latest %s", currentVersion, latestVersion),
		}
	} else {
		// Update available
		// Check if there's a breakpoint between current and latest
		hasBreakpoint := false
		var nextBreakpoint *policy.Breakpoint

		for _, bp := range policyData.Breakpoints {
			bpNorm := corecompat.NormalizeVersion(bp.Version)
			// Check if breakpoint is between current and latest (or equal to latest)
			cmpCurrent := i.compareVersions(bpNorm, currentNorm)
			cmpLatest := i.compareVersions(bpNorm, latestNorm)

			if cmpCurrent > 0 && cmpLatest <= 0 {
				// Breakpoint is after current and at or before latest
				hasBreakpoint = true
				if nextBreakpoint == nil || i.compareVersions(bpNorm, corecompat.NormalizeVersion(nextBreakpoint.Version)) < 0 {
					nextBreakpoint = &bp
				}
			}
		}

		if hasBreakpoint && nextBreakpoint != nil {
			// Breakpoint in the way of latest - may still allow dashboard update up to max version
			updateInfo.CanUpdateViaDashboard = false
			updateInfo.NextBreakpoint = &BreakpointInfo{
				Version: nextBreakpoint.Version,
				Reason:  nextBreakpoint.Reason,
				Docs:    nextBreakpoint.Docs,
			}

			// Find max dashboard version (highest release before breakpoint)
			maxDashboardVer := ""
			breakpointNorm := corecompat.NormalizeVersion(nextBreakpoint.Version)
			for _, release := range policyData.Releases {
				releaseNorm := corecompat.NormalizeVersion(release)
				// Release must be after current and before breakpoint
				if i.compareVersions(releaseNorm, currentNorm) > 0 && i.compareVersions(releaseNorm, breakpointNorm) < 0 {
					if maxDashboardVer == "" || i.compareVersions(releaseNorm, corecompat.NormalizeVersion(maxDashboardVer)) > 0 {
						maxDashboardVer = release
					}
				}
			}
			if maxDashboardVer != "" {
				updateInfo.MaxDashboardVersion = maxDashboardVer
			}

			if maxDashboardVer != "" {
				updateInfo.CanUpdateViaDashboard = true
				updateInfo.Message = fmt.Sprintf("Update to %s available via dashboard; latest %s requires manual CLI upgrade (breakpoint at %s)", maxDashboardVer, latestVersion, nextBreakpoint.Version)
				result.Checks["update_check"] = CheckResult{
					Status:  "WARNING",
					Message: fmt.Sprintf("Update available up to %s via dashboard; latest %s blocked by breakpoint at %s.", maxDashboardVer, latestVersion, nextBreakpoint.Version),
				}
			} else {
				updateInfo.Message = fmt.Sprintf("Update to %s available, but requires manual CLI upgrade (breakpoint at %s)", latestVersion, nextBreakpoint.Version)
				result.Checks["update_check"] = CheckResult{
					Status:  "WARNING",
					Message: fmt.Sprintf("Update available but blocked by breakpoint at %s. Manual upgrade required.", nextBreakpoint.Version),
				}
			}
		} else {
			// Can update via dashboard
			updateInfo.CanUpdateViaDashboard = true
			updateInfo.MaxDashboardVersion = latestVersion
			updateInfo.Message = fmt.Sprintf("Update to %s available via dashboard", latestVersion)
			result.Checks["update_check"] = CheckResult{
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

func (i *Inspector) generateRecommendations(result *InspectResult) {
	// Priority-based recommendations
	priority := 1

	// If last job failed with MIGRATION_FAILED
	if result.LastJob != nil && result.LastJob.State == jobs.JobStateFailed {
		switch result.LastJob.FailureCode {
		case "MIGRATION_FAILED":
			result.Recommendations = append(result.Recommendations, Recommendation{
				Action:      "restore_db",
				Description: "CRITICAL: Restore database from backup before any further action",
				Priority:    priority,
			})
			priority++
			result.Recommendations = append(result.Recommendations, Recommendation{
				Action:      "manual_rollback",
				Description: "After DB restore, manually run the previous known-good image version",
				Priority:    priority,
			})
			priority++
		case "DOCKER_ERROR", "HEALTHCHECK_FAILED":
			result.Recommendations = append(result.Recommendations, Recommendation{
				Action:      "recover",
				Description: "Run 'payram-updater recover' to attempt automatic recovery",
				Priority:    priority,
			})
			priority++
		case "POLICY_FETCH_FAILED", "MANIFEST_FETCH_FAILED", "DOCKER_PULL_FAILED", "CONCURRENCY_BLOCKED":
			result.Recommendations = append(result.Recommendations, Recommendation{
				Action:      "retry",
				Description: "This failure is likely temporary. Retry the upgrade.",
				Priority:    priority,
			})
			priority++
		default:
			result.Recommendations = append(result.Recommendations, Recommendation{
				Action:      "wait",
				Description: "Investigate the failure before taking action",
				Priority:    priority,
			})
			priority++
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
				result.Recommendations = append(result.Recommendations, Recommendation{
					Action:      "sync",
					Description: "Run 'payram-updater sync' to update internal state to match running version.",
					Priority:    priority,
				})
				priority++
			}
		}
	}

	// If container is missing
	containerCheck, ok := result.Checks["container"]
	if ok && containerCheck.Status == "WARNING" && strings.Contains(containerCheck.Message, "missing") {
		result.Recommendations = append(result.Recommendations, Recommendation{
			Action:      "recover",
			Description: "Run 'payram-updater recover' to restart the container",
			Priority:    priority,
		})
		priority++
	}

	// If docker daemon is down
	dockerCheck, ok := result.Checks["docker_daemon"]
	if ok && dockerCheck.Status == "FAILED" {
		result.Recommendations = append(result.Recommendations, Recommendation{
			Action:      "reinstall",
			Description: "Docker daemon is not running. Start Docker service or reinstall.",
			Priority:    1, // Highest priority
		})
	}

	// If everything is OK
	if result.OverallState == StateOK && len(result.Recommendations) == 0 {
		result.Recommendations = append(result.Recommendations, Recommendation{
			Action:      "none",
			Description: "System is healthy. No action required.",
			Priority:    priority,
		})
	}
}

// buildPlaybookContext constructs a PlaybookContext from available inspector state.
// Uses containerName and coreBaseURL from inspector configuration.
func (i *Inspector) buildPlaybookContext(backupPath string) recovery.PlaybookContext {
	ctx := recovery.PlaybookContext{
		BackupPath:    backupPath,
		ContainerName: i.containerName,
		BaseURL:       i.coreBaseURL,
		ImageRepo:     "payramapp/payram", // default, could be made configurable
	}

	// Try to extract HTTP port from coreBaseURL (e.g., "http://127.0.0.1:8080")
	// This is a best-effort extraction
	if len(i.coreBaseURL) > 0 {
		// Simple parsing: look for the port after the last colon
		for j := len(i.coreBaseURL) - 1; j >= 0; j-- {
			if i.coreBaseURL[j] == ':' {
				ctx.HTTPPort = i.coreBaseURL[j+1:]
				break
			}
		}
	}

	return ctx
}
