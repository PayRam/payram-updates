// Package inspect provides read-only diagnostics for Payram upgrade state.
package inspect

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/coreclient"
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

// InspectResult contains the full inspection output.
type InspectResult struct {
	OverallState     OverallState           `json:"overall_state"`
	Issues           []Issue                `json:"issues"`
	Recommendations  []Recommendation       `json:"recommendations"`
	LastJob          *jobs.Job              `json:"last_job,omitempty"`
	RecoveryPlaybook *recovery.Playbook     `json:"recovery_playbook,omitempty"`
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
	manifestPorts []int
}

// NewInspector creates a new inspector with the given configuration.
func NewInspector(
	jobStore *jobs.Store,
	dockerBin string,
	containerName string,
	coreBaseURL string,
	policyURL string,
	manifestURL string,
	manifestPorts []int,
) *Inspector {
	return &Inspector{
		jobStore:      jobStore,
		dockerBin:     dockerBin,
		containerName: containerName,
		coreBaseURL:   coreBaseURL,
		policyURL:     policyURL,
		manifestURL:   manifestURL,
		manifestPorts: manifestPorts,
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

	// Check 4: Port availability
	i.checkPorts(result)

	// Check 5: Policy readability
	i.checkPolicy(ctx, result)

	// Check 6: Manifest readability
	i.checkManifest(ctx, result)

	// Check 7: Health endpoint (if container running)
	i.checkHealth(ctx, result)

	// Check 8: Running version (if container running)
	i.checkVersion(ctx, result)

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

func (i *Inspector) checkPorts(result *InspectResult) {
	for _, port := range i.manifestPorts {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			result.Checks[fmt.Sprintf("port_%d", port)] = CheckResult{
				Status:  "OK",
				Message: fmt.Sprintf("Port %d is available", port),
			}
		} else {
			conn.Close()
			result.Checks[fmt.Sprintf("port_%d", port)] = CheckResult{
				Status:  "OK",
				Message: fmt.Sprintf("Port %d is in use (expected if container running)", port),
			}
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

	client := coreclient.NewClient(i.coreBaseURL)
	healthCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	healthResp, err := client.Health(healthCtx)
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

	if healthResp.Status == "ok" && healthResp.DB == "ok" {
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

	client := coreclient.NewClient(i.coreBaseURL)
	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	versionResp, err := client.Version(versionCtx)
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
		Message: fmt.Sprintf("Running version: %s", versionResp.Version),
	}

	// Check for version mismatch with last successful job
	if result.LastJob != nil && result.LastJob.State == jobs.JobStateReady {
		if versionResp.Version != result.LastJob.ResolvedTarget {
			cmp := compareVersions(versionResp.Version, result.LastJob.ResolvedTarget)

			if cmp < 0 {
				// Running version is LOWER than expected - downgrade detected
				result.Checks["version"] = CheckResult{
					Status:  "FAILED",
					Message: fmt.Sprintf("Downgrade detected: running %s (expected %s)", versionResp.Version, result.LastJob.ResolvedTarget),
				}
				result.Issues = append(result.Issues, Issue{
					Component:   "version",
					Description: fmt.Sprintf("Downgrade detected: running %s but last upgrade targeted %s", versionResp.Version, result.LastJob.ResolvedTarget),
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
						Message: fmt.Sprintf("External upgrade detected: running %s (expected %s)", versionResp.Version, result.LastJob.ResolvedTarget),
					}
					result.Issues = append(result.Issues, Issue{
						Component:   "version",
						Description: fmt.Sprintf("External upgrade detected: running %s but last upgrade targeted %s. System is healthy.", versionResp.Version, result.LastJob.ResolvedTarget),
						Severity:    "INFO",
					})
					// Don't degrade state if health is OK - this is recoverable
				} else {
					result.Checks["version"] = CheckResult{
						Status:  "WARNING",
						Message: fmt.Sprintf("External upgrade detected: running %s (expected %s), health not confirmed", versionResp.Version, result.LastJob.ResolvedTarget),
					}
					result.Issues = append(result.Issues, Issue{
						Component:   "version",
						Description: fmt.Sprintf("External upgrade detected: running %s but last upgrade targeted %s. Health status unclear.", versionResp.Version, result.LastJob.ResolvedTarget),
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
					Message: fmt.Sprintf("Running version: %s (expected %s from last upgrade)", versionResp.Version, result.LastJob.ResolvedTarget),
				}
				result.Issues = append(result.Issues, Issue{
					Component:   "version",
					Description: fmt.Sprintf("Version mismatch: running %s but last upgrade targeted %s", versionResp.Version, result.LastJob.ResolvedTarget),
					Severity:    "WARNING",
				})
				if result.OverallState == StateOK {
					result.OverallState = StateDegraded
				}
			}
		}
	}
}

// compareVersions compares two semver strings.
// Returns: -1 if v1 < v2, 0 if v1 == v2, 1 if v1 > v2
func compareVersions(v1, v2 string) int {
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
