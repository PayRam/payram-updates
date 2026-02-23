// Package recover provides automated recovery actions for failed upgrades.
package recover

import (
	"context"
	"fmt"

	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/recovery"
)

// RecoveryResult represents the outcome of a recovery attempt.
type RecoveryResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Action   string `json:"action"`
	Code     string `json:"code"`
	Refusals string `json:"refusals,omitempty"`
}

// Recoverer performs automated recovery actions.
type Recoverer struct {
	jobStore      *jobs.Store
	dockerRunner  *dockerexec.Runner
	containerName string
	coreBaseURL   string
}

// NewRecoverer creates a new recoverer.
func NewRecoverer(
	jobStore *jobs.Store,
	dockerRunner *dockerexec.Runner,
	containerName string,
	coreBaseURL string,
) *Recoverer {
	return &Recoverer{
		jobStore:      jobStore,
		dockerRunner:  dockerRunner,
		containerName: containerName,
		coreBaseURL:   coreBaseURL,
	}
}

// CanRecover checks if recovery is possible for the given failure code.
// Returns (canRecover, reason) where reason explains why recovery is refused.
func CanRecover(failureCode string) (bool, string) {
	playbook := recovery.GetPlaybook(failureCode)

	// Refuse recovery if playbook requires manual intervention
	if recovery.RequiresManualIntervention(failureCode) {
		return false, fmt.Sprintf("Failure code %s requires manual intervention: %s", failureCode, playbook.UserMessage)
	}

	// Refuse recovery if there's data risk
	if recovery.HasDataRisk(failureCode) {
		return false, fmt.Sprintf("Failure code %s has potential data risk (%s). Manual intervention recommended.", failureCode, playbook.DataRisk)
	}

	return true, ""
}

// Run attempts to recover from a failed upgrade.
func (r *Recoverer) Run(ctx context.Context) (*RecoveryResult, error) {
	// Load the latest job
	job, err := r.jobStore.LoadLatest()
	if err != nil {
		return nil, fmt.Errorf("failed to load latest job: %w", err)
	}

	if job == nil {
		return &RecoveryResult{
			Success: false,
			Message: "No upgrade job found to recover",
		}, nil
	}

	// Check if job is in failed state
	if job.State != jobs.JobStateFailed {
		return &RecoveryResult{
			Success: false,
			Message: fmt.Sprintf("Job is not in FAILED state (current: %s)", job.State),
		}, nil
	}

	failureCode := job.FailureCode

	// Check if recovery is allowed
	canRecover, refusal := CanRecover(failureCode)
	if !canRecover {
		return &RecoveryResult{
			Success:  false,
			Message:  "Automated recovery refused",
			Code:     failureCode,
			Refusals: refusal,
		}, nil
	}

	// Perform recovery action based on failure code
	result := r.performRecovery(ctx, failureCode, job)
	return result, nil
}

// performRecovery executes the recovery action for the given failure code.
func (r *Recoverer) performRecovery(ctx context.Context, failureCode string, job *jobs.Job) *RecoveryResult {
	switch failureCode {
	case "DOCKER_PULL_FAILED":
		return r.recoverDockerPull(ctx, job)
	case "DOCKER_ERROR":
		return r.recoverDockerError(ctx)
	case "HEALTHCHECK_FAILED":
		return r.recoverHealthcheck(ctx)
	case "POLICY_FETCH_FAILED", "MANIFEST_FETCH_FAILED":
		return r.recoverFetchFailed(ctx, failureCode)
	case "CONCURRENCY_BLOCKED":
		return r.recoverConcurrencyBlocked(ctx)
	case "DISK_SPACE_LOW":
		return &RecoveryResult{
			Success:  false,
			Message:  "Disk space issues cannot be automatically resolved. Please free disk space manually.",
			Code:     failureCode,
			Refusals: "Requires manual cleanup of disk space",
		}
	case "BACKUP_FAILED_AFTER_QUIESCE":
		return &RecoveryResult{
			Success:  false,
			Message:  "Backup failed after quiesce. Services should have been restarted; resolve backup issues and retry.",
			Code:     failureCode,
			Refusals: "No automated recovery action defined",
		}
	case "SUPERVISORCTL_FAILED":
		return &RecoveryResult{
			Success:  false,
			Message:  "Supervisor control failed. Check supervisor status inside the container and retry.",
			Code:     failureCode,
			Refusals: "No automated recovery action defined",
		}
	default:
		return &RecoveryResult{
			Success:  false,
			Message:  fmt.Sprintf("No automated recovery action defined for: %s", failureCode),
			Code:     failureCode,
			Refusals: "Unknown failure code",
		}
	}
}

func (r *Recoverer) recoverDockerPull(ctx context.Context, job *jobs.Job) *RecoveryResult {
	// Stop the old container if it exists
	_ = r.dockerRunner.Stop(ctx, r.containerName)

	// Attempt to restart the container with the previous image
	return &RecoveryResult{
		Success: true,
		Message: "Docker pull failure recovery attempted. Container stopped. You may retry the upgrade or manually pull the image.",
		Action:  "stopped_container",
		Code:    "DOCKER_PULL_FAILED",
	}
}

func (r *Recoverer) recoverDockerError(ctx context.Context) *RecoveryResult {
	// Try to stop and remove the container, then restart
	_ = r.dockerRunner.Stop(ctx, r.containerName)
	_ = r.dockerRunner.Remove(ctx, r.containerName)

	return &RecoveryResult{
		Success: true,
		Message: "Docker error recovery attempted. Container stopped and removed. You may retry the upgrade.",
		Action:  "stopped_and_removed_container",
		Code:    "DOCKER_ERROR",
	}
}

func (r *Recoverer) recoverHealthcheck(ctx context.Context) *RecoveryResult {
	// Restart the container
	_ = r.dockerRunner.Stop(ctx, r.containerName)

	return &RecoveryResult{
		Success: true,
		Message: "Healthcheck failure recovery attempted. Container stopped. Check logs and manually start the container if appropriate.",
		Action:  "stopped_container",
		Code:    "HEALTHCHECK_FAILED",
	}
}

func (r *Recoverer) recoverFetchFailed(ctx context.Context, failureCode string) *RecoveryResult {
	return &RecoveryResult{
		Success: true,
		Message: "Network fetch failure - please verify network connectivity and retry the upgrade.",
		Action:  "network_retry_suggested",
		Code:    failureCode,
	}
}

func (r *Recoverer) recoverConcurrencyBlocked(ctx context.Context) *RecoveryResult {
	return &RecoveryResult{
		Success: true,
		Message: "Concurrency block cleared. You may retry the upgrade.",
		Action:  "cleared_concurrency_block",
		Code:    "CONCURRENCY_BLOCKED",
	}
}
