// Package recovery provides recovery playbooks for upgrade failures.
// Each failure code maps to a playbook with severity, user messaging,
// SSH recovery steps, and documentation links.
//
// Merchants follow these steps literally over SSH, so every playbook:
//   - says first whether PayRam is running and whether data is at risk,
//   - starts with read-only checks,
//   - offers anything destructive (restoring a backup) only as a clearly
//     labelled last resort, with the data-loss warning,
//   - never stops, removes or prunes what PayRam needs to switch back.
package recovery

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
)

// PlaybookContext contains runtime context for rendering playbook templates.
// Empty fields fall back to the standard install layout (container "payram",
// backups in /var/lib/payram/backups), so a rendered step is always a command
// the merchant can paste.
type PlaybookContext struct {
	ContainerName     string // PayRam's container (job.ContainerName), default "payram"
	BaseURL           string // URL the updater reached PayRam on (job.CoreBaseURL), e.g. "https://127.0.0.1"
	ImageRepo         string // e.g. "payramapp/payram"
	BackupPath        string // backup taken before the update (job.BackupPath)
	BackupDir         string // BACKUP_DIR, default /var/lib/payram/backups
	TargetVersion     string // version the update was installing (job.ResolvedTarget)
	CauseCode         string // failing step behind UPGRADE_ROLLED_BACK / ROLLBACK_FAILED (job.CauseCode)
	PreviousContainer string // old container renamed aside during the switch (job.PreviousContainer), default "<name>-previous"
	NextContainer     string // new container created alongside the old one (job.NextContainer), default "<name>-next"

	// HTTPPort is no longer used by any playbook: PayRam's API port (8080)
	// lives inside the container and is normally not published.
	// Deprecated: kept so existing callers compile.
	HTTPPort string
}

// Standard install layout, used when the context does not say otherwise.
const (
	DefaultContainerName = "payram"
	DefaultImageRepo     = "payramapp/payram"
	DefaultBackupDir     = "/var/lib/payram/backups"
)

// WithJob returns the context with every field the job recorded filled in.
// The job's values win: they name the containers and URL the update really used.
func (c PlaybookContext) WithJob(job *jobs.Job) PlaybookContext {
	if job == nil {
		return c
	}
	setIfPresent := func(dst *string, value string) {
		if value != "" {
			*dst = value
		}
	}
	setIfPresent(&c.ContainerName, job.ContainerName)
	setIfPresent(&c.BaseURL, job.CoreBaseURL)
	setIfPresent(&c.BackupPath, job.BackupPath)
	setIfPresent(&c.TargetVersion, job.ResolvedTarget)
	setIfPresent(&c.CauseCode, job.CauseCode)
	setIfPresent(&c.PreviousContainer, job.PreviousContainer)
	setIfPresent(&c.NextContainer, job.NextContainer)
	return c
}

func (c PlaybookContext) withDefaults() PlaybookContext {
	if c.ContainerName == "" {
		c.ContainerName = DefaultContainerName
	}
	if c.PreviousContainer == "" {
		c.PreviousContainer = c.ContainerName + "-previous"
	}
	if c.NextContainer == "" {
		c.NextContainer = c.ContainerName + "-next"
	}
	if c.ImageRepo == "" {
		c.ImageRepo = DefaultImageRepo
	}
	if c.BackupDir == "" {
		c.BackupDir = DefaultBackupDir
	}
	return c
}

// Severity indicates how serious a failure is and what action is needed.
type Severity string

const (
	// SeverityInfo is for informational messages, no action required.
	SeverityInfo Severity = "INFO"
	// SeverityRetryable indicates the operation can be safely retried.
	SeverityRetryable Severity = "RETRYABLE"
	// SeverityManual indicates manual intervention is required.
	SeverityManual Severity = "MANUAL_REQUIRED"
)

// DataRisk indicates the potential for data loss or corruption.
type DataRisk string

const (
	DataRiskNone     DataRisk = "NONE"
	DataRiskPossible DataRisk = "POSSIBLE"
	DataRiskLikely   DataRisk = "LIKELY"
	DataRiskUnknown  DataRisk = "UNKNOWN"
)

// PayRamState says where a failed update left PayRam.
type PayRamState string

const (
	// PayRamUnchanged: the update stopped before PayRam was changed; the
	// version that was running before is still running.
	PayRamUnchanged PayRamState = "UNCHANGED"
	// PayRamRestored: the updater already put PayRam back on the version it
	// had before the update.
	PayRamRestored PayRamState = "RESTORED"
	// PayRamUncertain: PayRam may be down or on an unverified version; a
	// person has to check, starting with the read-only steps.
	PayRamUncertain PayRamState = "CHECK_REQUIRED"
)

// Playbook contains recovery instructions for a specific failure code.
type Playbook struct {
	Code        string      `json:"code"`
	Severity    Severity    `json:"severity"`
	Title       string      `json:"title"`
	UserMessage string      `json:"userMessage"` // short, dashboard-safe
	SSHSteps    []string    `json:"sshSteps"`    // exact commands or steps
	DocsURL     string      `json:"docsUrl,omitempty"`
	DataRisk    DataRisk    `json:"dataRisk"`
	PayRamState PayRamState `json:"payramState"`
	CauseCode   string      `json:"causeCode,omitempty"`  // the failing step, when the code only says where PayRam ended up
	BackupPath  string      `json:"backupPath,omitempty"` // populated when job has backup
}

// HeartbeatStaleAfter is how long an active job may go without saving
// progress before it counts as stale. The updater saves an active job at
// least every 30 seconds, so five minutes of silence means it has stopped
// working on the update.
const HeartbeatStaleAfter = 5 * time.Minute

// IsStale reports whether the job is active but has not saved progress for
// longer than HeartbeatStaleAfter.
func IsStale(job *jobs.Job, now time.Time) bool {
	return job != nil && job.IsActive() && now.Sub(job.UpdatedAt) > HeartbeatStaleAfter
}

// StalePlaybookCode returns the playbook code for a stale job in the given state.
func StalePlaybookCode(state jobs.JobState) string {
	switch state {
	case jobs.JobStateBackingUp:
		return "STALE_BACKING_UP"
	case jobs.JobStateExecuting:
		return "STALE_EXECUTING"
	case jobs.JobStateVerifying:
		return "STALE_VERIFYING"
	}
	return "STALE_PREPARING"
}

// Shared wording. Steps are numbered by numbered(), so these carry no number.
const (
	supportContact = "PayRam support (Telegram: https://t.me/PayRamChat, email: support@payram.com)"

	// dbUser and dbName read the database user and name from PayRam's own
	// container settings, so the commands work whatever the install called
	// them. The bundled database is never published on the host: every
	// database command runs inside the container with docker exec.
	dbUser = `"${POSTGRES_USER:-${POSTGRES_USERNAME:-payram}}"`
	dbName = `"${POSTGRES_DB:-${POSTGRES_DATABASE:-payram}}"`

	dataLossWarning = "Restoring removes every payment, deposit and payout recorded after the backup was taken."

	stepCheckHealth    = `Check whether PayRam answers (read-only): <health_check>. A reply containing "status":"ok" means PayRam is running.`
	stepListContainers = "See PayRam's containers and whether they are running (read-only): docker ps -a --filter name=<container_name>"
	stepReadReason     = "Read why the update stopped (read-only): payram-updater logs | tail -50"
	stepRetry          = "Retry the update from the dashboard."
	stepSendLogs       = "If it fails again, send the updater logs to " + supportContact + `: { payram-updater status; payram-updater logs; sudo journalctl -u payram-updater --since "6 hours ago" --no-pager; } > payram-update-report.txt 2>&1`
	stepContactSupport = "Contact " + supportContact + ` and attach the updater logs: { payram-updater status; payram-updater logs; sudo journalctl -u payram-updater --since "6 hours ago" --no-pager; docker ps -a; } > payram-update-report.txt 2>&1`
	stepRestartUpdater = "Restart the updater: sudo systemctl restart payram-updater. When it starts it finishes or undoes the interrupted update and puts PayRam back on its own."
	stepUpdaterWontRun = "If the updater does not start, see why (read-only): sudo systemctl status payram-updater --no-pager; sudo journalctl -u payram-updater -n 100 --no-pager"
	stepDBReady        = "Check the database answers (read-only): docker exec <container_name> sh -c 'pg_isready -U " + dbUser + " -d " + dbName + "'"
	stepPrograms       = "Check PayRam's programs are running (read-only): docker exec <container_name> supervisorctl status. Every line should say RUNNING."
	stepStartPrograms  = "If any program says STOPPED, start them again (safe; they run inside PayRam): docker exec <container_name> supervisorctl start all"
	stepExternalDB     = "If PayRam uses an external database (docker exec <container_name> printenv POSTGRES_HOST shows something other than localhost or 127.0.0.1), check that database server is up and reachable from this server."
	startPrevious      = "docker rename <previous_container> <container_name> && docker update --restart unless-stopped <container_name> && docker start <container_name>"
	stepStartPrevious  = "If <container_name> is not listed but <previous_container> is, <previous_container> is the version that ran before the update. Put it back as PayRam (this renames and starts a container; it does not touch your data): " + startPrevious
	switchBackByHand   = "if <previous_container> is listed, put it back as PayRam (this renames and starts containers; it does not touch your data): first, only if <container_name> is also listed (that is the new version), move it aside: docker stop <container_name> && docker rename <container_name> <container_name>-failed-manual. Then: " + startPrevious
	stepStartStopped   = "If <container_name> is listed as Exited, start it: docker update --restart unless-stopped <container_name> && docker start <container_name>. The first start after an update can take up to 10 minutes; follow it with docker logs -f --tail 100 <container_name> (Ctrl+C stops following)."
	stepKeepData       = "Do not delete PayRam's data folders or Docker volumes, and do not run docker system prune: PayRam's database lives there."
	stepNoRestore      = "Do not restore a backup: PayRam is running and has all your payments."
	stepLastResort     = "LAST RESORT, only if PayRam support asks you to: restore the database from the backup taken just before this update with payram-updater backup restore --file <backup_path> (payram-updater backup list shows every backup). WARNING: " + dataLossWarning

	msgUnchanged = "PayRam was not changed and is still running the version it had before; your data is safe. "
)

// numbered prefixes each step with its position ("1. ", "2. ", ...).
func numbered(steps ...string) []string {
	out := make([]string, len(steps))
	for i, step := range steps {
		out[i] = strconv.Itoa(i+1) + ". " + step
	}
	return out
}

// Steps shared by the codes that mean "PayRam's container could not be found".
var containerMissingSteps = numbered(
	stepCheckHealth,
	"List every container (read-only): docker ps -a --format 'table {{.Names}}\\t{{.Image}}\\t{{.Status}}'",
	stepStartStopped,
	stepStartPrevious,
	"If PayRam runs under another name (a container with image <image_repo>), tell the updater (replace NAME): echo 'TARGET_CONTAINER_NAME=NAME' | sudo tee -a /etc/payram/updater.env && sudo systemctl restart payram-updater",
	stepKeepData,
	stepContactSupport,
)

// playbooks maps failure codes to their recovery playbooks.
var playbooks = map[string]Playbook{
	// ---- Where PayRam ended up after the container switch ----

	jobs.FailureUpgradeRolledBack: {
		Code:        jobs.FailureUpgradeRolledBack,
		Severity:    SeverityInfo,
		Title:       "Update Rolled Back",
		UserMessage: "PayRam is running on the version it had before the update; your data is safe. The new version failed its checks, so the updater switched back to the previous version on its own. Nothing needs to be restored: the database was kept as it is, including every payment recorded during the update. Send the updater logs to PayRam support before retrying.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers+". <container_name> is PayRam; <container_name>-failed is the new version that failed, kept stopped so support can see why.",
			"See why the new version failed (read-only): docker logs --tail 300 <container_name>-failed",
			stepContactSupport,
			"Retry the update later, once PayRam support has confirmed the cause is fixed.",
			stepNoRestore,
		),
		DocsURL:     "https://docs.payram.com/recovery",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamRestored,
	},

	jobs.FailureRollbackFailed: {
		Code:        jobs.FailureRollbackFailed,
		Severity:    SeverityManual,
		Title:       "Update and Switch-Back Failed",
		UserMessage: "PayRam may be down. The new version failed, and the updater could not switch back to the previous version on its own. Your payments are still in the database: nothing has been deleted. Follow the steps to start the previous version; do not restore a backup unless PayRam support asks you to.",
		SSHSteps: numbered(
			stepCheckHealth+" If it replies ok, PayRam is fine: skip to the step that contacts support.",
			stepListContainers+". Look for <container_name>, <previous_container> (the version that ran before the update) and <container_name>-failed or <next_container> (the new version).",
			"If PayRam does not answer and "+switchBackByHand,
			"If <previous_container> is not listed, <container_name> already is the previous version. "+stepStartStopped,
			"Wait a few minutes and repeat the check from step 1.",
			stepContactSupport,
			stepLastResort,
		),
		DocsURL:     "https://docs.payram.com/recovery",
		DataRisk:    DataRiskPossible,
		PayRamState: PayRamUncertain,
	},

	jobs.FailureUpgradeInterrupted: {
		Code:        jobs.FailureUpgradeInterrupted,
		Severity:    SeverityInfo,
		Title:       "Update Interrupted",
		UserMessage: "PayRam should be running on the version it had before the update; your data is safe. The updater stopped in the middle of the update (it was restarted, crashed or the server rebooted), and when it came back it put PayRam back; the message says exactly what it did. You can retry the update.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers,
			stepStartStopped,
			stepStartPrevious,
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/recovery",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamRestored,
	},

	jobs.FailureDockerCreateFailed: {
		Code:        jobs.FailureDockerCreateFailed,
		Severity:    SeverityManual,
		Title:       "New Container Could Not Be Created",
		UserMessage: msgUnchanged + "The updater could not create the container for the new version (an image, folder mount or network problem), so it stopped before touching PayRam.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepReadReason,
			"Check Docker has room and is healthy (read-only): df -h /var/lib/docker; docker info --format '{{.ServerVersion}}'",
			"If docker ps -a lists <next_container>, it is the unfinished new version: it never ran and holds no data. Leave it in place and do not start it.",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/docker",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	jobs.FailureTargetNotAllowed: {
		Code:        jobs.FailureTargetNotAllowed,
		Severity:    SeverityManual,
		Title:       "Version Not Allowed",
		UserMessage: msgUnchanged + "The update was refused because the requested version is not one PayRam's release policy allows (an unknown version, a version older than the one running, or an image that is not PayRam's).",
		SSHSteps: numbered(
			stepCheckHealth,
			"See the version you run and the versions on offer (read-only): payram-updater inspect (look at updateInfo)",
			"Start the update from the dashboard: it only offers allowed versions. Over SSH, check first with: payram-updater dry-run --mode dashboard --to latest",
			"Going back to an older version is not done through the updater; ask PayRam support if you need it.",
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/upgrades",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	// ---- Failures before PayRam was changed ----

	"POLICY_FETCH_FAILED": {
		Code:        "POLICY_FETCH_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Policy Fetch Failed",
		UserMessage: msgUnchanged + "The updater could not download PayRam's release policy (the list of versions it may install). This is usually a temporary network problem.",
		SSHSteps: numbered(
			stepCheckHealth,
			"Check this server reaches the internet (read-only): curl -sI https://github.com | head -1",
			"Test the policy address the updater uses (read-only): grep POLICY_URL /etc/payram/updater.env, then curl -sI that address | head -1",
			"Make sure DNS and outbound HTTPS (port 443) are allowed by your firewall or hosting provider.",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/network",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"POLICY_INVALID_JSON": {
		Code:        "POLICY_INVALID_JSON",
		Severity:    SeverityRetryable,
		Title:       "Policy Could Not Be Read",
		UserMessage: msgUnchanged + "The updater downloaded PayRam's release policy but could not read it. A proxy or captive portal may be changing the reply, or the policy is being republished.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See what the policy address returns (read-only): grep POLICY_URL /etc/payram/updater.env, then curl -s that address | head -c 300",
			"If the reply is a web page instead of JSON, a proxy or firewall is in the way: allow the address.",
			"Retry the update in a few minutes.",
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/network",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"POLICY_REQUIRED": {
		Code:        "POLICY_REQUIRED",
		Severity:    SeverityRetryable,
		Title:       "Latest Version Unknown",
		UserMessage: msgUnchanged + "The update asked for the latest version, but the release policy that names it was not available.",
		SSHSteps: numbered(
			stepCheckHealth,
			"Test the policy address the updater uses (read-only): grep POLICY_URL /etc/payram/updater.env, then curl -s that address | head -c 300",
			"Retry the update from the dashboard once the policy downloads.",
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/network",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"MANUAL_UPGRADE_REQUIRED": {
		Code:        "MANUAL_UPGRADE_REQUIRED",
		Severity:    SeverityManual,
		Title:       "Manual Upgrade Required",
		UserMessage: msgUnchanged + "This version needs extra steps the dashboard cannot do on its own, so the update was not started.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See which version needs the extra steps and where its guide is (read-only): payram-updater inspect (look at updateInfo.nextBreakpoint)",
			"Read that version's release notes and upgrade guide before going further.",
			"Take a fresh database backup: payram-updater backup create",
			"Follow the guide, then run the update over SSH: payram-updater run --to VERSION (the version from the guide)",
			"If anything in the guide is unclear, contact "+supportContact+" before you start.",
		),
		DocsURL:     "https://docs.payram.com/upgrades/breaking-changes",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"MANIFEST_FETCH_FAILED": {
		Code:        "MANIFEST_FETCH_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Manifest Fetch Failed",
		UserMessage: msgUnchanged + "The updater could not download the runtime manifest (how the new version must be started). This is usually a temporary network problem.",
		SSHSteps: numbered(
			stepCheckHealth,
			"Check this server reaches the internet (read-only): curl -sI https://github.com | head -1",
			"Test the manifest address the updater uses (read-only): grep RUNTIME_MANIFEST_URL /etc/payram/updater.env, then curl -sI that address | head -1",
			"Make sure DNS and outbound HTTPS (port 443) are allowed by your firewall or hosting provider.",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/network",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"MANIFEST_INVALID_JSON": {
		Code:        "MANIFEST_INVALID_JSON",
		Severity:    SeverityRetryable,
		Title:       "Manifest Could Not Be Read",
		UserMessage: msgUnchanged + "The updater downloaded the runtime manifest but could not read it. A proxy or captive portal may be changing the reply, or the manifest is being republished.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See what the manifest address returns (read-only): grep RUNTIME_MANIFEST_URL /etc/payram/updater.env, then curl -s that address | head -c 300",
			"If the reply is a web page instead of JSON, a proxy or firewall is in the way: allow the address.",
			"Retry the update in a few minutes.",
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/network",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"DOCKER_PULL_FAILED": {
		Code:        "DOCKER_PULL_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Docker Pull Failed",
		UserMessage: msgUnchanged + "The new version's image could not be downloaded (network problem, Docker Hub limit or low disk space).",
		SSHSteps: numbered(
			stepCheckHealth,
			"Check free space for Docker (read-only): df -h /var/lib/docker",
			"Download the version you were updating to yourself, to see the exact error (it only downloads): docker pull <image_repo>:<target_version>",
			"If Docker Hub reports a rate limit (toomanyrequests), wait an hour or sign in with docker login.",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/docker",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"DISK_SPACE_LOW": {
		Code:        "DISK_SPACE_LOW",
		Severity:    SeverityManual,
		Title:       "Disk Space Low",
		UserMessage: msgUnchanged + "The update needs more free disk space than this server has: about 4 GB for Docker plus room for a database backup.",
		SSHSteps: numbered(
			"See free space (read-only): df -h / /var/lib/docker <backup_dir>",
			"See what uses it (read-only): docker system df; sudo du -sh <backup_dir> /var/log/journal",
			"Remove Docker's leftover build cache and untagged image layers (safe: every tagged image stays, including the <image_repo> images PayRam needs to switch back): docker image prune && docker builder prune",
			"Shrink the system journal if it is large: sudo journalctl --vacuum-size=500M",
			"Old updater backups in <backup_dir> can be large (payram-updater backup list shows them). Copy any you want to keep to another machine before deleting them, and always keep the newest one.",
			"Do not run docker system prune, docker container prune or any prune with -a (such as docker image prune -a): they delete stopped containers, including the previous PayRam version kept for switching back, or the <image_repo> images an update or switch-back needs.",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/disk",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"CONCURRENCY_BLOCKED": {
		Code:        "CONCURRENCY_BLOCKED",
		Severity:    SeverityRetryable,
		Title:       "Upgrade Already In Progress",
		UserMessage: "This request did not change PayRam; your data is safe. Another update is already running, so this one was not started.",
		SSHSteps: numbered(
			"See the running update (read-only): payram-updater status (look at state and updatedAt)",
			stepCheckHealth,
			"Wait for it to finish; most updates take under 15 minutes. Do not restart PayRam or start another update meanwhile.",
			"If updatedAt has not changed for more than 5 minutes, the updater has stopped working on it. "+stepRestartUpdater,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/concurrency",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"BACKUP_FAILED": {
		Code:        "BACKUP_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Database Backup Failed",
		UserMessage: msgUnchanged + "The safety backup of the database failed, so the updater stopped before touching PayRam.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepReadReason,
			"Check free space and permissions where backups are written (read-only): df -h <backup_dir>; ls -la <backup_dir>",
			stepDBReady,
			"Test a backup without writing any file (read-only; can take a few minutes on a large database): docker exec <container_name> sh -c 'pg_dump -U "+dbUser+" -d "+dbName+" > /dev/null && echo backup-ok'",
			stepExternalDB,
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/backup",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"BACKUP_FAILED_AFTER_QUIESCE": {
		Code:        "BACKUP_FAILED_AFTER_QUIESCE",
		Severity:    SeverityRetryable,
		Title:       "Database Backup Failed After Quiesce",
		UserMessage: msgUnchanged + "The updater paused PayRam's background programs for the safety backup, the backup failed, and the updater started the programs again.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepPrograms,
			stepStartPrograms,
			stepDBReady,
			"Check free space and permissions where backups are written (read-only): df -h <backup_dir>; ls -la <backup_dir>",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/backup",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"BACKUP_TIMEOUT": {
		Code:        "BACKUP_TIMEOUT",
		Severity:    SeverityRetryable,
		Title:       "Database Backup Timed Out",
		UserMessage: msgUnchanged + "The safety backup took longer than the updater allows, so it stopped before touching PayRam. This usually means a large database or a busy, slow disk.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See the database size (read-only): docker exec <container_name> sh -c 'psql -U "+dbUser+" -d "+dbName+" -tAc \"SELECT pg_size_pretty(pg_database_size(current_database()))\"'",
			"See how busy the database is (read-only): docker exec <container_name> sh -c 'psql -U "+dbUser+" -d "+dbName+" -c \"SELECT state, count(*) FROM pg_stat_activity GROUP BY state\"'",
			"Check memory and backup disk space (read-only): free -h; df -h <backup_dir>",
			"For a large database, give the backup more time: add BACKUP_TIMEOUT_SECONDS=1800 to /etc/payram/updater.env, then sudo systemctl restart payram-updater",
			"Retry the update at a quiet time.",
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/backup",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"SUPERVISORCTL_FAILED": {
		Code:        "SUPERVISORCTL_FAILED",
		Severity:    SeverityManual,
		Title:       "Supervisor Control Failed",
		UserMessage: msgUnchanged + "The updater could not pause or resume PayRam's background programs for the backup; it tried to start anything it had paused.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepPrograms,
			stepStartPrograms,
			"See PayRam's recent output (read-only): docker logs --tail 200 <container_name>",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/backup",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"DOCKER_DAEMON_DOWN": {
		Code:        "DOCKER_DAEMON_DOWN",
		Severity:    SeverityManual,
		Title:       "Docker Daemon Not Running",
		UserMessage: "The update did not change PayRam, but Docker is not running on this server, so PayRam is not running either. Your data is safe. Start Docker and PayRam starts with it.",
		SSHSteps: numbered(
			"Check Docker's status (read-only): sudo systemctl status docker --no-pager",
			"See why it stopped (read-only): sudo journalctl -u docker -n 50 --no-pager",
			"Start Docker: sudo systemctl start docker",
			"Check PayRam came back (read-only): docker ps -a --filter name=<container_name>. "+stepStartStopped,
			stepCheckHealth,
			"Retry the update once Docker and PayRam are running.",
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/docker",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"CONTAINER_NOT_FOUND": {
		Code:        "CONTAINER_NOT_FOUND",
		Severity:    SeverityManual,
		Title:       "Application Container Not Found",
		UserMessage: "The update did not change anything, but the updater could not find PayRam's container (<container_name>). Your data is safe. Check whether PayRam is running.",
		SSHSteps:    containerMissingSteps,
		DocsURL:     "https://docs.payram.com/troubleshooting/container",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"PAYRAM_CONTAINER_NOT_FOUND": {
		Code:        "PAYRAM_CONTAINER_NOT_FOUND",
		Severity:    SeverityManual,
		Title:       "No Running PayRam Container Found",
		UserMessage: "The update did not change anything, but the updater found no running container with PayRam's image. Your data is safe. Check whether PayRam is running.",
		SSHSteps:    containerMissingSteps,
		DocsURL:     "https://docs.payram.com/troubleshooting/container",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"CONTAINER_NAME_UNRESOLVED": {
		Code:        "CONTAINER_NAME_UNRESOLVED",
		Severity:    SeverityManual,
		Title:       "Target Container Name Not Specified",
		UserMessage: msgUnchanged + "The updater does not know which container is PayRam.",
		SSHSteps: numbered(
			stepCheckHealth,
			"Find PayRam's container (read-only): docker ps --format 'table {{.Names}}\\t{{.Image}}\\t{{.Status}}' (the one running <image_repo>)",
			"Tell the updater its name (replace NAME): echo 'TARGET_CONTAINER_NAME=NAME' | sudo tee -a /etc/payram/updater.env",
			"Restart the updater: sudo systemctl restart payram-updater",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/configuration",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"INVALID_DB_CONFIG": {
		Code:        "INVALID_DB_CONFIG",
		Severity:    SeverityManual,
		Title:       "Invalid Database Configuration",
		UserMessage: msgUnchanged + "The updater could not read the database settings from PayRam's container, so it could not take the safety backup and stopped.",
		SSHSteps: numbered(
			stepCheckHealth,
			"List the database settings PayRam has (names only; no passwords are shown): docker exec <container_name> sh -c 'env | grep ^POSTGRES_ | cut -d= -f1'",
			"The updater needs POSTGRES_HOST, POSTGRES_DB (or POSTGRES_DATABASE) and POSTGRES_USER (or POSTGRES_USERNAME). If one is missing, do not change the container yourself.",
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/database",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"RUNTIME_INSPECTION_FAILED": {
		Code:        "RUNTIME_INSPECTION_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Runtime State Inspection Failed",
		UserMessage: msgUnchanged + "The updater could not read the running container's settings, so it stopped before changing anything.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers,
			"Check Docker can read the container (read-only): docker inspect <container_name> > /dev/null && echo inspect-ok; docker info --format '{{.ServerVersion}}'",
			stepStartStopped,
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/docker",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"DOCKER_RUN_BUILD_FAILED": {
		Code:        "DOCKER_RUN_BUILD_FAILED",
		Severity:    SeverityManual,
		Title:       "Docker Run Arguments Build Failed",
		UserMessage: msgUnchanged + "The updater could not work out how to start the new version with your current container settings, so it stopped before changing anything.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepReadReason,
			"See the ports and folders PayRam uses now (read-only): docker inspect <container_name> --format '{{json .HostConfig.PortBindings}} {{json .Mounts}}'",
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/configuration",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"PORT_CONFLICT": {
		Code:        "PORT_CONFLICT",
		Severity:    SeverityManual,
		Title:       "Port Already In Use",
		UserMessage: msgUnchanged + "The new version needs a host port that is already taken, so the update stopped before changing anything.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepReadReason,
			"See which programs hold which ports (read-only): sudo ss -tlnp",
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/docker",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"STATE_SAVE_FAILED": {
		Code:        "STATE_SAVE_FAILED",
		Severity:    SeverityManual,
		Title:       "Updater Could Not Save Its Progress",
		UserMessage: msgUnchanged + "The updater could not write to its state folder, so it stopped before changing anything (it never changes PayRam's container without recording what it did).",
		SSHSteps: numbered(
			stepCheckHealth,
			"Check free space and the updater's state folder (read-only): df -h /var/lib/payram-updater; ls -ld /var/lib/payram-updater",
			"Free space if the disk is full (keep PayRam's images and data folders), then restart the updater: sudo systemctl restart payram-updater",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/configuration",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"INTERNAL_ERROR": {
		Code:        "INTERNAL_ERROR",
		Severity:    SeverityManual,
		Title:       "Updater Error",
		UserMessage: "PayRam was put back on the version it had before, unless the message above says otherwise; the updater hit an internal error and stopped the update. Your data is safe: the updater never deletes payment data.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers,
			stepStartPrevious,
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/health",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUncertain,
	},

	"PAYRAM_CORE_PORT_NOT_FOUND": {
		Code:        "PAYRAM_CORE_PORT_NOT_FOUND",
		Severity:    SeverityManual,
		Title:       "PayRam Address Not Found",
		UserMessage: msgUnchanged + "The updater could not work out which address PayRam answers on.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See the ports PayRam publishes (read-only): docker port <container_name>",
			"Tell the updater the address that answered in step 1 (https://127.0.0.1 or http://127.0.0.1): echo 'CORE_BASE_URL=https://127.0.0.1' | sudo tee -a /etc/payram/updater.env && sudo systemctl restart payram-updater",
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/configuration",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"PAYRAM_VERSION_PARSE_FAILED": {
		Code:        "PAYRAM_VERSION_PARSE_FAILED",
		Severity:    SeverityManual,
		Title:       "Running Version Unknown",
		UserMessage: msgUnchanged + "The updater could not tell which PayRam version is running from the container's image.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See the image PayRam runs (read-only): docker inspect <container_name> --format '{{.Config.Image}}'",
			"Tell the updater PayRam's container name (replace NAME): echo 'TARGET_CONTAINER_NAME=NAME' | sudo tee -a /etc/payram/updater.env && sudo systemctl restart payram-updater",
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/container",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	// ---- Step codes. They appear as CauseCode under UPGRADE_ROLLED_BACK /
	// ROLLBACK_FAILED, and as the top-level code in job files written by
	// updaters before the container switch. ----

	"HEALTHCHECK_FAILED": {
		Code:        "HEALTHCHECK_FAILED",
		Severity:    SeverityManual,
		Title:       "Health Check Failed",
		UserMessage: "PayRam may well be running fine: check before doing anything else. The updater could not confirm that PayRam answered its health check (updater v0.2.3 reports this on every install that publishes only ports 80/443, even when PayRam is healthy). Your payments have not been deleted. If the check in step 1 replies \"status\":\"ok\", nothing needs fixing and nothing must be restored.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers,
			"If step 1 replied ok, PayRam is healthy: clear the failed status with payram-updater sync and stop here.",
			stepStartStopped,
			stepStartPrevious,
			"If it is running but the check still does not reply ok after 10 minutes, see why (read-only): docker logs --tail 200 <container_name>",
			stepContactSupport,
			stepLastResort,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/health",
		DataRisk:    DataRiskPossible,
		PayRamState: PayRamUncertain,
	},

	"VERSION_MISMATCH": {
		Code:        "VERSION_MISMATCH",
		Severity:    SeverityManual,
		Title:       "Version Mismatch",
		UserMessage: "PayRam is probably running: check before doing anything else. After the update PayRam reported a different version than the one the updater installed. Your payments have not been deleted, and nothing must be restored while PayRam answers its health check.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers+". The IMAGE column shows the version each one runs.",
			"If step 1 replied ok, PayRam is healthy: record the version it runs with payram-updater sync, then retry the update if you still want the newer version.",
			stepStartStopped,
			stepStartPrevious,
			stepContactSupport,
			stepLastResort,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/version",
		DataRisk:    DataRiskPossible,
		PayRamState: PayRamUncertain,
	},

	"DOCKER_ERROR": {
		Code:        "DOCKER_ERROR",
		Severity:    SeverityManual,
		Title:       "Docker Operation Failed",
		UserMessage: "PayRam may or may not be running: a Docker command failed during the update. Check before doing anything else. Your payments have not been deleted.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers,
			stepStartStopped,
			stepStartPrevious,
			"If PayRam will not start because a port is taken, see which program holds ports 80 and 443 (read-only): sudo ss -tlnp | grep -E ':(80|443) '",
			"See PayRam's recent output (read-only): docker logs --tail 200 <container_name>",
			stepKeepData,
			stepContactSupport,
			stepLastResort,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/docker",
		DataRisk:    DataRiskPossible,
		PayRamState: PayRamUncertain,
	},

	// No updater emits the migration codes today; their advice stays safe.
	"MIGRATION_FAILED": {
		Code:        "MIGRATION_FAILED",
		Severity:    SeverityManual,
		Title:       "Database Migration Failed",
		UserMessage: "PayRam may be down: the new version could not finish updating the database. Your payments have not been deleted, but the database may be partly updated. Do not retry the update and do not restore a backup on your own; check PayRam first and contact PayRam support.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See what the database update reported (read-only): docker logs --tail 200 <container_name>",
			"Do not retry the update until PayRam support has looked at these logs.",
			stepListContainers,
			stepStartPrevious,
			stepContactSupport,
			stepLastResort,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/migrations",
		DataRisk:    DataRiskLikely,
		PayRamState: PayRamUncertain,
	},

	"MIGRATION_TIMEOUT": {
		Code:        "MIGRATION_TIMEOUT",
		Severity:    SeverityManual,
		Title:       "Database Migration Timeout",
		UserMessage: "PayRam may still be updating its database: it had not finished after the updater's time limit. Your payments have not been deleted. This is often just slow; check before doing anything else, and do not restore a backup while it is still working.",
		SSHSteps: numbered(
			stepCheckHealth+" If it replies ok, the update finished and nothing needs fixing.",
			"Follow the database update (read-only): docker logs -f --tail 100 <container_name> (Ctrl+C stops following)",
			"Wait while the logs keep moving and repeat step 1 every few minutes.",
			"If step 1 now replies ok, record the version PayRam runs: payram-updater sync",
			stepContactSupport,
			stepLastResort,
		),
		DocsURL:     "https://docs.payram.com/troubleshooting/migrations",
		DataRisk:    DataRiskPossible,
		PayRamState: PayRamUncertain,
	},

	// ---- Stale active jobs: no progress saved for HeartbeatStaleAfter ----

	"STALE_PREPARING": {
		Code:        "STALE_PREPARING",
		Severity:    SeverityRetryable,
		Title:       "Updater Stopped While Preparing",
		UserMessage: "PayRam was not changed and is still running; your data is safe. The updater stopped responding while it was preparing an update (no progress for more than 5 minutes). Restart the updater, then retry.",
		SSHSteps: numbered(
			stepCheckHealth,
			"See the updater's state (read-only): sudo systemctl status payram-updater --no-pager",
			stepRestartUpdater,
			stepUpdaterWontRun,
			stepRetry,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/recovery",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUnchanged,
	},

	"STALE_BACKING_UP": {
		Code:        "STALE_BACKING_UP",
		Severity:    SeverityRetryable,
		Title:       "Updater Stopped During Backup",
		UserMessage: "PayRam was not switched to the new version and your data is safe, but its background programs may still be paused for the backup. The updater stopped responding during the safety backup (no progress for more than 5 minutes). Restart the updater: it resumes PayRam on its own.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepPrograms,
			stepRestartUpdater,
			stepUpdaterWontRun,
			"After a minute, check again: payram-updater status should no longer say BACKING_UP. "+stepStartPrograms,
			stepSendLogs,
		),
		DocsURL:     "https://docs.payram.com/recovery",
		DataRisk:    DataRiskNone,
		PayRamState: PayRamUncertain,
	},

	"STALE_EXECUTING": {
		Code:        "STALE_EXECUTING",
		Severity:    SeverityManual,
		Title:       "Updater Stopped While Switching Versions",
		UserMessage: "PayRam may be down until the updater is restarted. The updater stopped responding while switching PayRam to the new version (no progress for more than 5 minutes). Your payments have not been deleted. Restart the updater: it puts PayRam back on its own.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers,
			stepRestartUpdater,
			"After two minutes, repeat the check from step 1; payram-updater status should no longer say EXECUTING.",
			stepUpdaterWontRun,
			"Only if the updater cannot be started and PayRam does not answer: "+switchBackByHand,
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/recovery",
		DataRisk:    DataRiskPossible,
		PayRamState: PayRamUncertain,
	},

	"STALE_VERIFYING": {
		Code:        "STALE_VERIFYING",
		Severity:    SeverityManual,
		Title:       "Updater Stopped While Checking the New Version",
		UserMessage: "PayRam is probably running the new version, but the updater stopped responding before it finished checking it (no progress for more than 5 minutes). Your payments have not been deleted. Restart the updater: it finishes the check or switches back on its own.",
		SSHSteps: numbered(
			stepCheckHealth,
			stepListContainers,
			stepRestartUpdater,
			"After two minutes, repeat the check from step 1; payram-updater status should no longer say VERIFYING.",
			stepUpdaterWontRun,
			"Only if the updater cannot be started and PayRam does not answer: "+switchBackByHand,
			stepContactSupport,
		),
		DocsURL:     "https://docs.payram.com/recovery",
		DataRisk:    DataRiskPossible,
		PayRamState: PayRamUncertain,
	},
}

// unknownPlaybook is returned when a failure code is not recognized.
var unknownPlaybook = Playbook{
	Code:        "UNKNOWN",
	Severity:    SeverityManual,
	Title:       "Unknown Failure",
	UserMessage: "PayRam may or may not be running: something unexpected stopped the update. The updater never deletes payment data on its own. Check whether PayRam is running before doing anything else; do not restore a backup or remove containers.",
	SSHSteps: numbered(
		stepCheckHealth,
		stepListContainers,
		stepReadReason,
		stepStartStopped,
		stepContactSupport,
	),
	DocsURL:     "https://docs.payram.com/troubleshooting",
	DataRisk:    DataRiskUnknown,
	PayRamState: PayRamUncertain,
}

// GetPlaybook returns the recovery playbook for the given failure code.
// If the code is not recognized, returns a default "Unknown failure" playbook.
func GetPlaybook(code string) Playbook {
	if playbook, ok := playbooks[code]; ok {
		return playbook
	}
	// Return unknown playbook with the actual code preserved
	result := unknownPlaybook
	result.Code = code
	return result
}

// AllCodes returns all known failure codes.
func AllCodes() []string {
	codes := make([]string, 0, len(playbooks))
	for code := range playbooks {
		codes = append(codes, code)
	}
	return codes
}

// IsRetryable returns true if the failure can be safely retried.
func IsRetryable(code string) bool {
	return GetPlaybook(code).Severity == SeverityRetryable
}

// RequiresManualIntervention returns true if manual action is needed.
func RequiresManualIntervention(code string) bool {
	return GetPlaybook(code).Severity == SeverityManual
}

// HasDataRisk returns true if there's potential for data loss.
func HasDataRisk(code string) bool {
	playbook := GetPlaybook(code)
	return playbook.DataRisk == DataRiskPossible ||
		playbook.DataRisk == DataRiskLikely ||
		playbook.DataRisk == DataRiskUnknown
}

// RenderJobPlaybook returns the playbook for a failed job, rendered with the
// containers, URL, backup and cause the job recorded on top of ctx.
func RenderJobPlaybook(job *jobs.Job, ctx PlaybookContext) Playbook {
	if job == nil {
		return RenderPlaybook("", ctx)
	}
	return RenderPlaybook(job.FailureCode, ctx.WithJob(job))
}

// RenderPlaybook returns a playbook with all placeholders replaced by context values.
// Supports: <container_name>, <previous_container>, <next_container>,
// <image_repo>, <backup_dir>, <backup_path>, <target_version>, <health_check>.
// An unknown backup path or version renders as BACKUP_FILE or VERSION, a word
// the merchant replaces, never a shell redirect like "<backup_path>".
func RenderPlaybook(code string, ctx PlaybookContext) Playbook {
	playbook := GetPlaybook(code)
	ctx = ctx.withDefaults()

	if ctx.BackupPath != "" {
		playbook.BackupPath = ctx.BackupPath
	}

	renderedSteps := make([]string, len(playbook.SSHSteps))
	for i, step := range playbook.SSHSteps {
		renderedSteps[i] = renderTemplate(step, ctx)
	}
	playbook.SSHSteps = renderedSteps

	playbook.UserMessage = renderTemplate(playbook.UserMessage, ctx)
	if ctx.CauseCode != "" && ctx.CauseCode != code {
		playbook.CauseCode = ctx.CauseCode
		playbook.UserMessage += " What failed: " + GetPlaybook(ctx.CauseCode).Title + " (" + ctx.CauseCode + ")."
	}

	return playbook
}

// renderTemplate replaces placeholders in a string with context values.
// It expects a context that went through withDefaults.
func renderTemplate(text string, ctx PlaybookContext) string {
	return strings.NewReplacer(
		"<health_check>", healthCheckCommand(ctx.BaseURL),
		"<container_name>", ctx.ContainerName,
		"<previous_container>", ctx.PreviousContainer,
		"<next_container>", ctx.NextContainer,
		"<image_repo>", ctx.ImageRepo,
		"<backup_dir>", ctx.BackupDir,
		"<backup_path>", valueOr(ctx.BackupPath, "BACKUP_FILE"),
		"<target_version>", valueOr(ctx.TargetVersion, "VERSION"),
	).Replace(text)
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// standardHealthChecks reaches PayRam through the bundled web server on the
// ports every install publishes (443 on HTTPS installs, 80 on HTTP-only ones).
const standardHealthChecks = "curl -sk https://127.0.0.1/api/v1/health (HTTPS install) or curl -s http://127.0.0.1/api/v1/health (HTTP-only install)"

// healthCheckCommand returns the command that asks PayRam whether it is
// healthy. It uses the URL the updater used when known; when that URL is not
// the standard one it adds the standard checks, so a wrong guess (such as the
// unpublished port 8080) never makes a healthy PayRam look down.
func healthCheckCommand(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(base)
	if base == "" || err != nil || parsed.Host == "" {
		return standardHealthChecks
	}

	command := "curl -s "
	if parsed.Scheme == "https" {
		command = "curl -sk "
	}
	command += base + "/api/v1/health"

	if isStandardLocalURL(parsed) {
		return command
	}
	return command + " (if that cannot connect, try " + standardHealthChecks + ")"
}

func isStandardLocalURL(u *url.URL) bool {
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return false
	}
	switch u.Port() {
	case "":
		return true
	case "443":
		return u.Scheme == "https"
	case "80":
		return u.Scheme == "http"
	}
	return false
}
