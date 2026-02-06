// Package recovery provides recovery playbooks for upgrade failures.
// Each failure code maps to a playbook with severity, user messaging,
// SSH recovery steps, and documentation links.
package recovery

import "strings"

// PlaybookContext contains runtime context for rendering playbook templates.
type PlaybookContext struct {
	ContainerName string // e.g. "payram-core"
	BaseURL       string // e.g. "http://127.0.0.1:8080"
	HTTPPort      string // host port mapped to container 8080
	DBPort        string // host port mapped to container 5432
	ImageRepo     string // e.g. "payramapp/payram"
	BackupPath    string // path to backup file
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

// Playbook contains recovery instructions for a specific failure code.
type Playbook struct {
	Code        string   `json:"code"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`
	UserMessage string   `json:"user_message"` // short, dashboard-safe
	SSHSteps    []string `json:"ssh_steps"`    // exact commands or steps
	DocsURL     string   `json:"docs_url,omitempty"`
	DataRisk    DataRisk `json:"data_risk"`
	BackupPath  string   `json:"backup_path,omitempty"` // populated when job has backup
}

// playbooks maps failure codes to their recovery playbooks.
var playbooks = map[string]Playbook{
	"POLICY_FETCH_FAILED": {
		Code:        "POLICY_FETCH_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Policy Fetch Failed",
		UserMessage: "Unable to fetch upgrade policy. This is usually a temporary network issue.",
		SSHSteps: []string{
			"1. Check network connectivity: curl -I https://github.com",
			"2. Verify DNS resolution: nslookup github.com",
			"3. Check firewall rules for outbound HTTPS",
			"4. Retry the upgrade from the dashboard or run: payram-updater upgrade",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/network",
		DataRisk: DataRiskNone,
	},

	"MANUAL_UPGRADE_REQUIRED": {
		Code:        "MANUAL_UPGRADE_REQUIRED",
		Severity:    SeverityManual,
		Title:       "Manual Upgrade Required",
		UserMessage: "This upgrade requires manual intervention due to breaking changes.",
		SSHSteps: []string{
			"1. Review the release notes for breaking changes",
			"2. Back up your database before proceeding",
			"3. Follow the migration guide in the documentation",
			"4. Run the upgrade manually after completing prerequisites",
		},
		DocsURL:  "https://docs.payram.com/upgrades/breaking-changes",
		DataRisk: DataRiskNone,
	},

	"MANIFEST_FETCH_FAILED": {
		Code:        "MANIFEST_FETCH_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Manifest Fetch Failed",
		UserMessage: "Unable to fetch runtime manifest. This is usually a temporary network issue.",
		SSHSteps: []string{
			"1. Check network connectivity: curl -I https://github.com",
			"2. Verify the manifest URL is accessible",
			"3. Check firewall rules for outbound HTTPS",
			"4. Retry the upgrade from the dashboard",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/network",
		DataRisk: DataRiskNone,
	},

	"DOCKER_PULL_FAILED": {
		Code:        "DOCKER_PULL_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Docker Pull Failed",
		UserMessage: "Failed to pull the new container image. Check network and disk space.",
		SSHSteps: []string{
			"1. Check disk space: df -h",
			"2. Check Docker daemon: docker info",
			"3. Test image pull manually: docker pull <image>",
			"4. Check Docker Hub / registry connectivity",
			"5. Retry the upgrade once resolved",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/docker",
		DataRisk: DataRiskNone,
	},

	"DOCKER_ERROR": {
		Code:        "DOCKER_ERROR",
		Severity:    SeverityManual,
		Title:       "Docker Operation Failed",
		UserMessage: "A Docker operation failed. The container may be in an inconsistent state.",
		SSHSteps: []string{
			"1. Check container status: docker ps -a | grep <image_repo>",
			"2. Check container logs: docker logs <container_name>",
			"3. Check port availability: ss -tlnp | grep <http_port>",
			"4. If port blocked, stop conflicting container/process",
			"5. If container missing, run: payram-updater recover",
			"6. If container crashed, check logs and restart manually",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/docker",
		DataRisk: DataRiskPossible,
	},

	"HEALTHCHECK_FAILED": {
		Code:        "HEALTHCHECK_FAILED",
		Severity:    SeverityManual,
		Title:       "Health Check Failed",
		UserMessage: "The new container started but failed health checks. Restore from backup and rollback to previous version.",
		SSHSteps: []string{
			"1. Check if container is running: docker ps | grep <image_repo>",
			"2. Check container logs: docker logs <container_name> --tail 100",
			"3. Test health endpoint manually: curl <base_url>/api/v1/health",
			"4. If health check fails, RESTORE FROM BACKUP:",
			"   - List backups: payram-updater backup list",
			"   - Find backup created by this job (check backup_path in job)",
			"   - Restore: payram-updater backup restore --file <backup_path> --yes",
			"5. Stop and remove the failing container: docker stop <container_name> && docker rm <container_name>",
			"6. Run the previous known-good version with the correct tag",
			"7. Verify health: curl <base_url>/api/v1/health",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/health",
		DataRisk: DataRiskPossible,
	},

	"VERSION_MISMATCH": {
		Code:        "VERSION_MISMATCH",
		Severity:    SeverityManual,
		Title:       "Version Mismatch",
		UserMessage: "The container reports an unexpected version. Restore from backup if data may be corrupted.",
		SSHSteps: []string{
			"1. Check running container image: docker inspect <container_name> --format='{{.Config.Image}}'",
			"2. Check reported version: curl <base_url>/api/v1/version",
			"3. If data may be corrupted, RESTORE FROM BACKUP:",
			"   - List backups: payram-updater backup list",
			"   - Restore: payram-updater backup restore --file <backup_path> --yes",
			"4. Stop the container: docker stop <container_name> && docker rm <container_name>",
			"5. Run the correct version (pin to known-good image tag)",
			"6. Verify: curl <base_url>/api/v1/version",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/version",
		DataRisk: DataRiskPossible,
	},

	"DISK_SPACE_LOW": {
		Code:        "DISK_SPACE_LOW",
		Severity:    SeverityManual,
		Title:       "Disk Space Low",
		UserMessage: "Insufficient disk space for upgrade. Free up space before retrying.",
		SSHSteps: []string{
			"1. Check disk usage: df -h",
			"2. Clean Docker resources: docker system prune -a",
			"3. Remove old images: docker image prune -a",
			"4. Check for large log files: du -sh /var/log/*",
			"5. Ensure at least 2GB free space",
			"6. Retry upgrade after freeing space",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/disk",
		DataRisk: DataRiskNone,
	},

	"CONCURRENCY_BLOCKED": {
		Code:        "CONCURRENCY_BLOCKED",
		Severity:    SeverityRetryable,
		Title:       "Upgrade Already In Progress",
		UserMessage: "Another upgrade is already running. Please wait for it to complete.",
		SSHSteps: []string{
			"1. Check current upgrade status: payram-updater status",
			"2. Wait for the current upgrade to complete",
			"3. If stuck, check logs: payram-updater logs",
			"4. If truly stuck, restart the updater service",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/concurrency",
		DataRisk: DataRiskNone,
	},

	"BACKUP_FAILED": {
		Code:        "BACKUP_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Database Backup Failed",
		UserMessage: "Pre-upgrade database backup failed. The upgrade was safely aborted before any changes.",
		SSHSteps: []string{
			"1. Check disk space: df -h (pg_dump needs space for dump file)",
			"2. Verify PostgreSQL is running: pg_isready -h localhost",
			"3. Test pg_dump manually: pg_dump -Fc -h localhost -U payram -d payram -f /tmp/test.dump",
			"4. Check backup directory permissions: ls -la /var/lib/payram/backups",
			"5. Check PostgreSQL logs for errors",
			"6. Retry the upgrade once resolved (no manual recovery needed)",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/backup",
		DataRisk: DataRiskNone,
	},

	"DOCKER_DAEMON_DOWN": {
		Code:        "DOCKER_DAEMON_DOWN",
		Severity:    SeverityManual,
		Title:       "Docker Daemon Not Running",
		UserMessage: "The Docker daemon is not running. Start Docker before attempting any upgrades.",
		SSHSteps: []string{
			"1. Check Docker daemon status: systemctl status docker",
			"2. Start Docker daemon: sudo systemctl start docker",
			"3. Verify Docker is running: docker info",
			"4. If Docker fails to start, check logs: journalctl -u docker -n 50",
			"5. Retry the upgrade once Docker is running",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/docker",
		DataRisk: DataRiskNone,
	},

	"CONTAINER_NOT_FOUND": {
		Code:        "CONTAINER_NOT_FOUND",
		Severity:    SeverityManual,
		Title:       "Application Container Not Found",
		UserMessage: "The application container was not found. Ensure the container is running before upgrade.",
		SSHSteps: []string{
			"1. Check container status: docker ps -a | grep <image_repo>",
			"2. If container exists but stopped, start it: docker start <container_name>",
			"3. If container doesn't exist, this may be a fresh install",
			"4. Check container logs for errors: docker logs <container_name>",
			"5. Contact support if the container should exist but doesn't",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/container",
		DataRisk: DataRiskNone,
	},

	"INVALID_DB_CONFIG": {
		Code:        "INVALID_DB_CONFIG",
		Severity:    SeverityManual,
		Title:       "Invalid Database Configuration",
		UserMessage: "Database configuration could not be extracted from the container. Check container environment.",
		SSHSteps: []string{
			"1. Check container environment: docker exec <container_name> env | grep POSTGRES",
			"2. Ensure these env vars are set: POSTGRES_HOST, POSTGRES_PORT, POSTGRES_DATABASE, POSTGRES_USERNAME",
			"3. If env vars are missing, the container may need reconfiguration",
			"4. Check the container's entrypoint/startup script",
			"5. Verify the application can connect to the database",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/database",
		DataRisk: DataRiskNone,
	},

	"BACKUP_TIMEOUT": {
		Code:        "BACKUP_TIMEOUT",
		Severity:    SeverityRetryable,
		Title:       "Database Backup Timed Out",
		UserMessage: "Pre-upgrade database backup timed out. The database may be too large or under heavy load.",
		SSHSteps: []string{
			"1. Check database size: psql -c \"SELECT pg_size_pretty(pg_database_size('payram'))\"",
			"2. Check active connections: psql -c \"SELECT * FROM pg_stat_activity\"",
			"3. Consider running backup during low-traffic period",
			"4. Check I/O performance: iostat -x 1 5",
			"5. Retry during a maintenance window with less database activity",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/backup",
		DataRisk: DataRiskNone,
	},

	"CONTAINER_NAME_UNRESOLVED": {
		Code:        "CONTAINER_NAME_UNRESOLVED",
		Severity:    SeverityManual,
		Title:       "Target Container Name Not Specified",
		UserMessage: "No target container name was configured. Set TARGET_CONTAINER_NAME or ensure the manifest specifies container_name.",
		SSHSteps: []string{
			"1. Set TARGET_CONTAINER_NAME environment variable in /etc/payram/updater.env",
			"2. Or ensure your runtime manifest includes container_name in defaults",
			"3. Example: echo 'TARGET_CONTAINER_NAME=payram-core' >> /etc/payram/updater.env",
			"4. Restart the updater service: sudo systemctl restart payram-updater",
			"5. Retry the upgrade",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/configuration",
		DataRisk: DataRiskNone,
	},

	"RUNTIME_INSPECTION_FAILED": {
		Code:        "RUNTIME_INSPECTION_FAILED",
		Severity:    SeverityRetryable,
		Title:       "Runtime State Inspection Failed",
		UserMessage: "Failed to inspect the running container's configuration. The container was not modified.",
		SSHSteps: []string{
			"1. Verify container is running: docker ps | grep payram",
			"2. Test docker inspect manually: docker inspect <container_name>",
			"3. Check Docker daemon: docker info",
			"4. If container is stopped, start it: docker start <container_name>",
			"5. Retry the upgrade (safe - no changes were made)",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/docker",
		DataRisk: DataRiskNone,
	},

	"DOCKER_RUN_BUILD_FAILED": {
		Code:        "DOCKER_RUN_BUILD_FAILED",
		Severity:    SeverityManual,
		Title:       "Docker Run Arguments Build Failed",
		UserMessage: "Failed to construct docker run arguments from runtime state. The container was not modified.",
		SSHSteps: []string{
			"1. Check upgrade logs for specific reconciliation errors: payram-updater logs",
			"2. Verify runtime manifest is valid JSON and accessible",
			"3. Check for conflicting port/mount requirements in manifest",
			"4. Inspect current container config: docker inspect <container_name>",
			"5. Contact support with logs if issue persists (no changes were made)",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/configuration",
		DataRisk: DataRiskNone,
	},

	"MIGRATION_TIMEOUT": {
		Code:        "MIGRATION_TIMEOUT",
		Severity:    SeverityManual,
		Title:       "Database Migration Timeout",
		UserMessage: "Database migrations are still running after 30 seconds. Check migration status and database performance.",
		SSHSteps: []string{
			"1. Check container logs for migration progress: docker logs <container_name> | tail -50",
			"2. Check migration status: curl <base_url>/admin/migrations/status",
			"3. If migrations completed successfully, the upgrade succeeded (false timeout)",
			"4. If migrations are still running, monitor: watch 'curl -s <base_url>/admin/migrations/status'",
			"5. If migrations failed, RESTORE FROM BACKUP:",
			"   - List backups: payram-updater backup list",
			"   - Restore: payram-updater backup restore --file <backup_path> --yes",
			"6. Check database performance: slow migrations may indicate DB issues",
		},
		DocsURL:  "https://docs.payram.com/troubleshooting/migrations",
		DataRisk: DataRiskPossible,
	},
}

// unknownPlaybook is returned when a failure code is not recognized.
var unknownPlaybook = Playbook{
	Code:        "UNKNOWN",
	Severity:    SeverityManual,
	Title:       "Unknown Failure",
	UserMessage: "An unexpected error occurred. Manual investigation required.",
	SSHSteps: []string{
		"1. Check upgrade logs: payram-updater logs",
		"2. Check container status: docker ps -a | grep <image_repo>",
		"3. Check container logs: docker logs <container_name>",
		"4. Run diagnostics: payram-updater inspect",
		"5. Contact support with the diagnostic output",
	},
	DocsURL:  "https://docs.payram.com/troubleshooting",
	DataRisk: DataRiskUnknown,
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
	playbook := GetPlaybook(code)
	return playbook.Severity == SeverityRetryable
}

// RequiresManualIntervention returns true if manual action is needed.
func RequiresManualIntervention(code string) bool {
	playbook := GetPlaybook(code)
	return playbook.Severity == SeverityManual
}

// HasDataRisk returns true if there's potential for data loss.
func HasDataRisk(code string) bool {
	playbook := GetPlaybook(code)
	return playbook.DataRisk == DataRiskPossible ||
		playbook.DataRisk == DataRiskLikely ||
		playbook.DataRisk == DataRiskUnknown
}

// RenderPlaybook returns a playbook with all placeholders replaced by context values.
// Supports: <container_name>, <base_url>, <http_port>, <db_port>, <image_repo>, <backup_path>
func RenderPlaybook(code string, ctx PlaybookContext) Playbook {
	playbook := GetPlaybook(code)

	// Set backup path if provided
	if ctx.BackupPath != "" {
		playbook.BackupPath = ctx.BackupPath
	}

	// Render SSH steps
	renderedSteps := make([]string, len(playbook.SSHSteps))
	for i, step := range playbook.SSHSteps {
		renderedSteps[i] = renderTemplate(step, ctx)
	}
	playbook.SSHSteps = renderedSteps

	// Render user message
	playbook.UserMessage = renderTemplate(playbook.UserMessage, ctx)

	return playbook
}

// renderTemplate replaces placeholders in a string with context values.
func renderTemplate(text string, ctx PlaybookContext) string {
	replacements := map[string]string{
		"<container_name>": ctx.ContainerName,
		"<base_url>":       ctx.BaseURL,
		"<http_port>":      ctx.HTTPPort,
		"<db_port>":        ctx.DBPort,
		"<image_repo>":     ctx.ImageRepo,
		"<backup_path>":    ctx.BackupPath,
	}

	result := text
	for placeholder, value := range replacements {
		if value != "" {
			result = strings.ReplaceAll(result, placeholder, value)
		}
	}

	return result
}

// GetPlaybookWithBackup returns a playbook enriched with the actual backup path.
// Deprecated: Use RenderPlaybook with PlaybookContext instead.
// This function is kept for backward compatibility.
func GetPlaybookWithBackup(code string, backupPath string) Playbook {
	return RenderPlaybook(code, PlaybookContext{
		BackupPath: backupPath,
	})
}
