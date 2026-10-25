// Package recover explains what, if anything, needs recovering after a failed
// upgrade. It never stops, removes, restarts or starts a container: the
// updater already puts PayRam back by itself, and when it could not, a person
// follows the recovery playbook, which starts with read-only checks.
package recover

import (
	"context"
	"fmt"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/recovery"
)

// RecoveryResult represents the outcome of a recovery attempt.
type RecoveryResult struct {
	Success  bool               `json:"success"`
	Message  string             `json:"message"`
	Action   string             `json:"action"`
	Code     string             `json:"code"`
	Refusals string             `json:"refusals,omitempty"`
	Playbook *recovery.Playbook `json:"playbook,omitempty"`
}

// Actions reported in RecoveryResult.Action.
const (
	ActionNone             = "none"               // nothing to recover; PayRam is running
	ActionStartContainer   = "start_container"    // the merchant should start PayRam's container
	ActionWaitForUpdate    = "wait_for_update"    // an update is running right now
	ActionRestartUpdater   = "restart_updater"    // the running update stopped responding
	ActionFollowPlaybook   = "follow_playbook"    // a person has to check, following the playbook
	ActionCheckDockerState = "check_docker_state" // Docker could not say whether PayRam runs
)

// ContainerStatusReader is the only Docker access recover has: it reads
// whether a container runs and cannot change one. *dockerexec.Runner
// satisfies it.
type ContainerStatusReader interface {
	InspectRunning(ctx context.Context, container string) (bool, error)
}

// Recoverer explains the state a failed upgrade left behind.
type Recoverer struct {
	jobStore      *jobs.Store
	docker        ContainerStatusReader
	containerName string
	coreBaseURL   string
	now           func() time.Time
}

// NewRecoverer creates a new recoverer.
func NewRecoverer(
	jobStore *jobs.Store,
	docker ContainerStatusReader,
	containerName string,
	coreBaseURL string,
) *Recoverer {
	return &Recoverer{
		jobStore:      jobStore,
		docker:        docker,
		containerName: containerName,
		coreBaseURL:   coreBaseURL,
		now:           time.Now,
	}
}

// Run reads the latest job and reports what, if anything, the merchant has
// to do. It only reads: no container is stopped, removed, restarted or started.
func (r *Recoverer) Run(ctx context.Context) (*RecoveryResult, error) {
	job, err := r.jobStore.LoadLatest()
	if err != nil {
		return nil, fmt.Errorf("failed to load latest job: %w", err)
	}

	if job == nil {
		return &RecoveryResult{
			Success: true,
			Action:  ActionNone,
			Message: "No update has run yet. Nothing to recover.",
		}, nil
	}

	if job.IsActive() {
		return r.activeJobResult(job), nil
	}

	if job.State != jobs.JobStateFailed {
		return &RecoveryResult{
			Success: true,
			Action:  ActionNone,
			Message: fmt.Sprintf("The last update did not fail (state: %s). Nothing to recover.", job.State),
		}, nil
	}

	playbook := recovery.RenderJobPlaybook(job, r.playbookContext())
	result := &RecoveryResult{Code: job.FailureCode, Playbook: &playbook}

	switch playbook.PayRamState {
	case recovery.PayRamUnchanged:
		r.reportRunning(ctx, job, result, "Nothing to recover: PayRam was not changed and is running (container %s). Fix the cause shown in the steps below, then retry the update.")
	case recovery.PayRamRestored:
		r.reportRunning(ctx, job, result, "Nothing to recover: the updater already put PayRam back on the version it had before the update, and it is running (container %s). Read the steps below before retrying the update.")
	default:
		result.Action = ActionFollowPlaybook
		result.Message = fmt.Sprintf("recover does not change containers after %s. %s", job.FailureCode, playbook.UserMessage)
		result.Refusals = "Follow the recovery steps below. They start with read-only checks; do not restore a backup unless PayRam support asks you to."
	}
	return result, nil
}

// activeJobResult explains why recover does nothing while an update is active.
func (r *Recoverer) activeJobResult(job *jobs.Job) *RecoveryResult {
	if recovery.IsStale(job, r.now()) {
		code := recovery.StalePlaybookCode(job.State)
		playbook := recovery.RenderPlaybook(code, r.playbookContext().WithJob(job))
		return &RecoveryResult{
			Code:     code,
			Action:   ActionRestartUpdater,
			Message:  fmt.Sprintf("The update in progress (state %s) has made no progress for more than %d minutes. Restart the updater: sudo systemctl restart payram-updater. It puts PayRam back on its own.", job.State, int(recovery.HeartbeatStaleAfter.Minutes())),
			Refusals: "recover does not change containers while an update is recorded as running.",
			Playbook: &playbook,
		}
	}
	return &RecoveryResult{
		Action:   ActionWaitForUpdate,
		Message:  fmt.Sprintf("An update is running right now (state %s). Wait for it to finish; do not restart PayRam or the updater meanwhile.", job.State),
		Refusals: "recover does nothing while an update is running.",
	}
}

// reportRunning confirms PayRam's container is running and says so with
// runningFormat; when it is not running it tells the merchant how to start it.
func (r *Recoverer) reportRunning(ctx context.Context, job *jobs.Job, result *RecoveryResult, runningFormat string) {
	name := r.targetContainer(job)
	running, err := r.docker.InspectRunning(ctx, name)
	if err != nil {
		result.Action = ActionCheckDockerState
		result.Message = fmt.Sprintf("Could not check whether PayRam's container %s is running: %v. Check that Docker runs (sudo systemctl status docker) and look at the container: docker ps -a --filter name=%s", name, err, name)
		return
	}
	if !running {
		result.Action = ActionStartContainer
		result.Message = fmt.Sprintf("PayRam's data is safe, but its container %s is not running. See it with: docker ps -a --filter name=%s. If it is listed as Exited, start it: docker start %s. If only %s-previous is listed, follow the recovery steps below to put it back.", name, name, name, name)
		return
	}
	result.Success = true
	result.Action = ActionNone
	result.Message = fmt.Sprintf(runningFormat, name)
}

// targetContainer prefers the container the job recorded: it is the one the
// update actually worked on.
func (r *Recoverer) targetContainer(job *jobs.Job) string {
	if job.ContainerName != "" {
		return job.ContainerName
	}
	if r.containerName != "" {
		return r.containerName
	}
	return recovery.DefaultContainerName
}

func (r *Recoverer) playbookContext() recovery.PlaybookContext {
	return recovery.PlaybookContext{ContainerName: r.containerName, BaseURL: r.coreBaseURL}
}
