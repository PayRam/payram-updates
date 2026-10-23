package jobs

import (
	"time"
)

// JobMode represents how a job was initiated.
type JobMode string

const (
	JobModeDashboard JobMode = "DASHBOARD"
	JobModeManual    JobMode = "MANUAL"
)

// JobState represents the current state of a job.
type JobState string

const (
	JobStateIdle             JobState = "IDLE"
	JobStatePolicyFetching   JobState = "POLICY_FETCHING"
	JobStateManifestFetching JobState = "MANIFEST_FETCHING"
	JobStateReady            JobState = "READY"
	JobStateBackingUp        JobState = "BACKING_UP"
	JobStateExecuting        JobState = "EXECUTING"
	JobStateVerifying        JobState = "VERIFYING"
	JobStateFailed           JobState = "FAILED"
)

// Failure codes that say where PayRam ended up after the container switch.
// The step codes (DOCKER_PULL_FAILED, BACKUP_FAILED, HEALTHCHECK_FAILED, ...)
// stay in use for failures before anything changed, and as CauseCode.
const (
	// FailureUpgradeRolledBack: the new version failed, PayRam was switched
	// back to the previous version and is running it. CauseCode says why.
	FailureUpgradeRolledBack = "UPGRADE_ROLLED_BACK"
	// FailureRollbackFailed: the new version failed and switching back failed
	// too. PayRam may be down; a person has to look.
	FailureRollbackFailed = "ROLLBACK_FAILED"
	// FailureUpgradeInterrupted: the updater stopped in the middle of an
	// update (restart, crash, reboot) and put PayRam back when it came up.
	FailureUpgradeInterrupted = "UPGRADE_INTERRUPTED"
	// FailureDockerCreateFailed: the new container could not be created
	// (image, mount or network problem). Nothing was changed.
	FailureDockerCreateFailed = "DOCKER_CREATE_FAILED"
	// FailureTargetNotAllowed: the requested version or image is not one the
	// release policy allows (unknown version, downgrade, foreign image).
	// Refused before anything was touched.
	FailureTargetNotAllowed = "TARGET_NOT_ALLOWED"
)

// SwitchStage records how far the container switch got, so a restarted
// updater can put PayRam back. Stages only move forward within one hop.
type SwitchStage string

const (
	SwitchStageNone      SwitchStage = ""
	SwitchStagePrepared  SwitchStage = "PREPARED"  // new container created alongside the running one
	SwitchStageQuiesced  SwitchStage = "QUIESCED"  // PayRam's programs stopped for the backup
	SwitchStageStopped   SwitchStage = "STOPPED"   // old container stopped, not yet renamed
	SwitchStageSwitched  SwitchStage = "SWITCHED"  // old container renamed aside, new one started
	SwitchStageVerified  SwitchStage = "VERIFIED"  // new version healthy; old container can go
	SwitchStageReverting SwitchStage = "REVERTING" // switching back to the old container
)

// Job represents an update job with its current state.
type Job struct {
	JobID           string    `json:"jobId"`
	Mode            JobMode   `json:"mode"`
	RequestedTarget string    `json:"requestedTarget"`
	ResolvedTarget  string    `json:"resolvedTarget"`
	State           JobState  `json:"state"`
	FailureCode     string    `json:"failureCode"`
	Message         string    `json:"message"`
	BackupPath      string    `json:"backupPath,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`

	// CauseCode is the failing step's code when FailureCode only says where
	// PayRam ended up (UPGRADE_ROLLED_BACK, ROLLBACK_FAILED).
	CauseCode string `json:"causeCode,omitempty"`

	// Switch bookkeeping, saved before every step that changes a container
	// so an interrupted update can be put back (see SwitchStage).
	ContainerName         string      `json:"containerName,omitempty"`
	CoreBaseURL           string      `json:"coreBaseUrl,omitempty"`
	FromVersion           string      `json:"fromVersion,omitempty"`
	SwitchStage           SwitchStage `json:"switchStage,omitempty"`
	NextContainer         string      `json:"nextContainer,omitempty"`
	PreviousContainer     string      `json:"previousContainer,omitempty"`
	PreviousImage         string      `json:"previousImage,omitempty"`
	PreviousRestartPolicy string      `json:"previousRestartPolicy,omitempty"`
	QuiescedPrograms      []string    `json:"quiescedPrograms,omitempty"`
	// SwitchedAt is when the new container was started; the health check's
	// window runs from here, also for an updater restarted mid-check.
	SwitchedAt time.Time `json:"switchedAt,omitempty"`
}

// NewJob creates a new job with the given mode and requested target.
func NewJob(jobID string, mode JobMode, requestedTarget string) *Job {
	now := time.Now().UTC()
	return &Job{
		JobID:           jobID,
		Mode:            mode,
		RequestedTarget: requestedTarget,
		State:           JobStateIdle,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// IsActive reports whether the job is still running. It is the one
// definition the daemon, the auto-updater and the CLI share.
func (j *Job) IsActive() bool {
	switch j.State {
	case JobStatePolicyFetching, JobStateManifestFetching, JobStateBackingUp,
		JobStateExecuting, JobStateVerifying:
		return true
	}
	return false
}
