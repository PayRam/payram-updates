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
	// back to the previous version and is running it, on the database as it
	// is: the new version never started, or it did not change the database's
	// structure (no migration, no schema change). CauseCode says why.
	FailureUpgradeRolledBack = "UPGRADE_ROLLED_BACK"
	// FailureUpgradeRolledBackRestored: the new version failed after it had
	// changed the database (migrations), and had moved no money (see
	// FailureUpgradeMoneyMoved), so the pre-update backup was restored and PayRam was switched back to the
	// previous version, which is running. What PayRam recorded after the
	// backup (BackupTakenAt) was removed; the database as the new version
	// left it is kept in SafetyBackupPath.
	FailureUpgradeRolledBackRestored = "UPGRADE_ROLLED_BACK_RESTORED"
	// FailureUpgradeMoneyMoved: the new version failed after it had changed
	// the database AND, since the backup, requested, signed or sent a payout,
	// started a sweep, sent gas, deployed a deposit wallet or handed out a
	// payment address. Restoring the backup would forget that (a payout could
	// be sent twice, a customer's payment go unseen), and the previous version
	// cannot run on the changed database, so neither was done: PayRam was
	// left on the new version and support must look.
	FailureUpgradeMoneyMoved = "UPGRADE_MONEY_MOVED_NOT_RESTORED"
	// FailureUpgradeRestoreFailed: the backup had to be restored before
	// switching back, and the restore failed. PayRam was left on the new
	// version; support must look.
	FailureUpgradeRestoreFailed = "UPGRADE_RESTORE_FAILED"
	// FailureUpgradeRestoreDeclined: the person running the update answered
	// "no" when asked to restore the backup and switch back. Both versions
	// were left stopped.
	FailureUpgradeRestoreDeclined = "UPGRADE_RESTORE_DECLINED"
	// FailureUpgradeDatabaseUnchecked: the new version started and failed,
	// and the updater could not read the database to tell whether it changed
	// it or sent money. It neither restored nor switched back: PayRam was left
	// on the new version; support must look.
	FailureUpgradeDatabaseUnchecked = "UPGRADE_DATABASE_UNCHECKED"
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
	// FailureContainerNotReplaceable: PayRam's container is set up so that
	// the updater could not switch back to it (started with --rm). Refused
	// before anything was touched.
	FailureContainerNotReplaceable = "CONTAINER_NOT_REPLACEABLE"
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

// DatabaseState is what an update records about PayRam's database before it
// switches versions, and reads again when the new version fails, to decide
// whether switching back is safe as it is, needs the backup restored first,
// or must not happen.
type DatabaseState struct {
	// ReadAt is when the database was read.
	ReadAt time.Time `json:"readAt"`
	// MigrationIDs are the IDs in PayRam's migrations table, sorted; empty
	// when the database has no migrations table.
	MigrationIDs []string `json:"migrationIds"`
	// SeedNames are the names in PayRam's seeder_logs table (the reference
	// data a version seeds when it starts), sorted.
	SeedNames []string `json:"seedNames"`
	// SchemaDigest is a digest of every table and column (with its type) in
	// the public schema: a migration that failed half way changes it without
	// adding its ID.
	SchemaDigest string `json:"schemaDigest"`
	// MoneyRecords holds, per table, a digest of PayRam's records of money it
	// moved or may move: payouts requested, signed or sent, sweeps started,
	// gas sent, deposit wallets deployed, payment addresses handed out (see
	// backup.ReadDatabaseState). A table missing from the map has no rows.
	MoneyRecords map[string]string `json:"moneyRecords"`
}

// DatabaseCheck says what switching back found about the database, saved
// before acting on it so a restarted updater carries on from there.
type DatabaseCheck string

const (
	// DatabaseCheckNone: not checked yet.
	DatabaseCheckNone DatabaseCheck = ""
	// DatabaseCheckUnchanged: the new version did not change the database's
	// structure (or never started); the previous version runs on it as it is.
	DatabaseCheckUnchanged DatabaseCheck = "UNCHANGED"
	// DatabaseCheckRestoring: the pre-update backup is being restored.
	DatabaseCheckRestoring DatabaseCheck = "RESTORING"
	// DatabaseCheckRestored: the pre-update backup was restored.
	DatabaseCheckRestored DatabaseCheck = "RESTORED"
)

// Question is asked of the person following a manual update at root's
// command line, who answers before Deadline or gets the default.
type Question struct {
	// ID is unique per question; the answer names it.
	ID string `json:"id"`
	// Text says what will happen and asks yes or no.
	Text string `json:"text"`
	// Deadline is when the default (yes) is taken without an answer.
	Deadline time.Time `json:"deadline"`
}

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

	// PreviousPublishesDatabase: the previous container publishes the
	// bundled database's port (5432) on the host; a switch back keeps that.
	PreviousPublishesDatabase bool `json:"previousPublishesDatabase,omitempty"`

	// Database bookkeeping for switching back (see DatabaseState).
	// BackupTakenAt is when the pre-update backup (BackupPath) was started.
	BackupTakenAt time.Time `json:"backupTakenAt,omitempty"`
	// DatabaseAtBackup is the database as the backup holds it.
	DatabaseAtBackup *DatabaseState `json:"databaseAtBackup,omitempty"`
	// DatabaseBeforeSwitch is the database right before this hop's switch:
	// what the container a switch back starts again was running on.
	DatabaseBeforeSwitch *DatabaseState `json:"databaseBeforeSwitch,omitempty"`
	// DatabaseCheck is what switching back found (see DatabaseCheck).
	DatabaseCheck DatabaseCheck `json:"databaseCheck,omitempty"`
	// ProgramsStoppedForCheck are the new version's programs stopped so that
	// nothing writes while its database is checked and restored.
	ProgramsStoppedForCheck []string `json:"programsStoppedForCheck,omitempty"`
	// SafetyBackupPath is the database as the new version left it, dumped
	// right before the pre-update backup was restored.
	SafetyBackupPath string `json:"safetyBackupPath,omitempty"`

	// Attended says a person at root's command line follows this update and
	// can be asked before the backup is restored (payram-updater run on a
	// terminal). Everything else is unattended: no one can be asked while
	// PayRam is down, so the default answer is taken.
	Attended bool `json:"attended,omitempty"`
	// Question is waiting for that person's answer.
	Question *Question `json:"question,omitempty"`
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
