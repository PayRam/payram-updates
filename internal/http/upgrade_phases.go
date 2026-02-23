package http

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/corecompat"
	"github.com/payram/payram-updater/internal/diskspace"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
	"github.com/payram/payram-updater/internal/manifest"
)

// upgradePhase represents discrete upgrade execution phases.
// Each phase is responsible for one logical step of the upgrade process.

// resolveTargetContainer determines the target container name using resolution logic.
// Returns container name or fails the job with appropriate error code.
func (s *Server) resolveTargetContainer(ctx context.Context, job *jobs.Job, manifestData *manifest.Manifest) (string, bool) {
	resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, logger.StdLogger())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		if resErr, ok := err.(*container.ResolutionError); ok && resErr.GetFailureCode() == "CONTAINER_NAME_UNRESOLVED" {
			imagePattern := "payramapp/payram:"
			if s.config.ImageRepoOverride != "" {
				imagePattern = s.config.ImageRepoOverride + ":"
			}
			discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, logger.StdLogger())
			discovered, discoverErr := discoverer.DiscoverPayramContainer(ctx)
			if discoverErr != nil {
				job.State = jobs.JobStateFailed
				job.FailureCode = resErr.GetFailureCode()
				job.Message = resErr.Error()
				job.UpdatedAt = time.Now().UTC()
				s.jobStore.Save(job)
				s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
				return "", false
			}
			resolved = &container.ResolvedContainer{Name: discovered.Name}
		} else if resErr, ok := err.(*container.ResolutionError); ok {
			job.State = jobs.JobStateFailed
			job.FailureCode = resErr.GetFailureCode()
			job.Message = resErr.Error()
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
			return "", false
		} else {
			job.State = jobs.JobStateFailed
			job.FailureCode = "CONTAINER_NAME_UNRESOLVED"
			job.Message = err.Error()
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
			return "", false
		}
	}
	containerName := resolved.Name
	s.jobStore.AppendLog(fmt.Sprintf("Target container resolved as: %s", containerName))
	return containerName, true
}

// prepareUpgradeArgs extracts runtime state and builds docker run arguments.
// Returns docker args or fails the job with appropriate error code.
func (s *Server) prepareUpgradeArgs(ctx context.Context, job *jobs.Job, containerName string, manifestData *manifest.Manifest, imageTag string) ([]string, bool) {
	s.jobStore.AppendLog("Extracting runtime state from container...")
	inspector := container.NewInspector(s.config.DockerBin, logger.StdLogger())
	runtimeState, err := inspector.ExtractRuntimeState(ctx, containerName)
	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "RUNTIME_INSPECTION_FAILED"
		job.Message = fmt.Sprintf("Failed to inspect runtime state: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (container not modified)", job.FailureCode, job.Message))
		return nil, false
	}
	s.jobStore.AppendLog(fmt.Sprintf("Runtime state extracted: %d ports, %d mounts, %d env vars",
		len(runtimeState.Ports), len(runtimeState.Mounts), len(runtimeState.Env)))

	// Build docker run arguments from runtime state + manifest overlays
	builder := container.NewDockerRunBuilder(logger.StdLogger())
	dockerArgs, err := builder.BuildUpgradeArgs(runtimeState, manifestData, imageTag)
	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_RUN_BUILD_FAILED"
		job.Message = fmt.Sprintf("Failed to build docker run args: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (container not modified)", job.FailureCode, job.Message))
		return nil, false
	}
	s.jobStore.AppendLog("Docker run arguments built successfully (runtime parity preserved)")
	return dockerArgs, true
}

// executeDryRun logs planned upgrade steps and completes the job in dry-run mode.
func (s *Server) executeDryRun(job *jobs.Job, imageRepo, imageTag, containerName string, dockerArgs []string) {
	s.jobStore.AppendLog("DRY-RUN mode: would execute the following steps:")
	s.jobStore.AppendLog(fmt.Sprintf("  0. Pull image: %s:%s", imageRepo, imageTag))
	s.jobStore.AppendLog("  1. Quiesce supervisor programs (stop non-DB processes)")
	s.jobStore.AppendLog("  2. Create database backup")
	s.jobStore.AppendLog(fmt.Sprintf("  3. Stop container: %s", containerName))
	s.jobStore.AppendLog(fmt.Sprintf("  4. Remove container: %s", containerName))
	s.jobStore.AppendLog(fmt.Sprintf("  5. Run new container: docker %s", strings.Join(dockerArgs, " ")))
	s.jobStore.AppendLog("  6. Verify: container running")
	s.jobStore.AppendLog("  7. Verify: /api/v1/health endpoint")
	s.jobStore.AppendLog("  8. Verify: /api/v1/version matches target")

	job.State = jobs.JobStateReady
	job.Message = "Dry-run validation complete"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog("Dry-run complete - no changes made")
}

// preflightChecks verifies Docker daemon is running.
// Returns false if checks fail (job is already marked failed).
func (s *Server) preflightChecks(ctx context.Context, job *jobs.Job, containerName string) bool {
	s.jobStore.AppendLog("Pre-flight: Checking Docker daemon...")
	if err := backup.CheckDockerDaemon(ctx, s.config.DockerBin); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_DAEMON_DOWN"
		job.Message = "Docker daemon is not running"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		s.jobStore.AppendLog("Next steps: Start Docker daemon with 'sudo systemctl start docker' and retry.")
		return false
	}
	s.jobStore.AppendLog("Docker daemon is running")

	// Query actual database size for accurate space calculation
	s.jobStore.AppendLog("Pre-flight: Querying database size...")
	var backupSpaceGB float64 = 2.0 // Default fallback if query fails

	inspector := backup.NewDockerInspector(s.config.DockerBin, nil)
	dbConfig, err := inspector.GetDBConfig(ctx, containerName)
	if err == nil {
		dbSizeChecker := diskspace.NewDBSizeChecker(s.config.DockerBin)

		// Convert ContainerDBConfig to diskspace.DBConfig
		diskspaceDBConfig := &diskspace.DBConfig{
			Host:     dbConfig.Host,
			Port:     dbConfig.Port,
			Database: dbConfig.Database,
			Username: dbConfig.Username,
			Password: dbConfig.Password,
		}

		dbSizeBytes, queryErr := dbSizeChecker.GetDatabaseSize(ctx, containerName, diskspaceDBConfig)
		if queryErr == nil && dbSizeBytes > 0 {
			dbSizeGB := float64(dbSizeBytes) / (1024 * 1024 * 1024)
			// Require 1.5x database size for backup (accounts for compression variation and safety margin)
			backupSpaceGB = dbSizeGB * 1.5
			if backupSpaceGB < 1.0 {
				backupSpaceGB = 1.0 // Minimum 1GB
			}
			s.jobStore.AppendLog(fmt.Sprintf("Database size: %.2f GB, requiring %.2f GB backup space (1.5x for safety)", dbSizeGB, backupSpaceGB))
		} else {
			s.jobStore.AppendLog(fmt.Sprintf("Warning: Unable to query database size, assuming %.1f GB for backup space calculation", backupSpaceGB))
		}
	} else {
		s.jobStore.AppendLog(fmt.Sprintf("Warning: Unable to detect database config, assuming %.1f GB for backup space calculation", backupSpaceGB))
	}

	// Check disk space requirements with dynamic backup space
	s.jobStore.AppendLog("Pre-flight: Checking disk space availability...")
	requirements := []diskspace.SpaceRequirement{
		{
			Path:          s.config.Backup.Dir,
			MinFreeGB:     backupSpaceGB,
			PurposeDesc:   "Backup directory",
			FailIfMissing: true,
		},
		{
			Path:          "/var/lib/docker",
			MinFreeGB:     4.0, // ~4GB for typical Payram image
			PurposeDesc:   "Docker storage",
			FailIfMissing: false, // Don't fail if custom Docker root
		},
		{
			Path:          "/",
			MinFreeGB:     0.5, // At least 500MB for general operations
			PurposeDesc:   "System root",
			FailIfMissing: true,
		},
	}

	results, allSufficient := diskspace.CheckAvailableSpace(requirements)

	// Log all check results
	for _, line := range diskspace.FormatCheckResults(results) {
		s.jobStore.AppendLog(line)
	}

	if !allSufficient {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DISK_SPACE_LOW"
		job.Message = "Insufficient disk space for upgrade"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		s.jobStore.AppendLog("Next steps: Free up disk space and retry. Run 'df -h' to check usage.")
		s.jobStore.AppendLog("Suggested cleanup: docker system prune -a")
		return false
	}
	s.jobStore.AppendLog("Disk space checks passed")

	return true
}

var errSupervisorUnavailable = errors.New("supervisorctl not available")

func (s *Server) supervisorctlStatus(ctx context.Context, containerName string) (string, error) {
	cmd := exec.CommandContext(ctx, s.config.DockerBin, "exec", containerName, "supervisorctl", "status")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), nil
	}

	outputStr := string(output)
	if strings.Contains(outputStr, "supervisorctl: not found") ||
		strings.Contains(outputStr, "command not found") ||
		strings.Contains(outputStr, "executable file not found") ||
		strings.Contains(outputStr, "No such file or directory") {
		return "", errSupervisorUnavailable
	}

	return "", fmt.Errorf("supervisorctl status failed: %w: %s", err, outputStr)
}

func parseSupervisorStatus(output string) map[string]string {
	status := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		status[fields[0]] = fields[1]
	}
	return status
}

func (s *Server) supervisorctlStop(ctx context.Context, containerName string, programs []string) error {
	if len(programs) == 0 {
		return nil
	}
	args := append([]string{"exec", containerName, "supervisorctl", "stop"}, programs...)
	cmd := exec.CommandContext(ctx, s.config.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("supervisorctl stop failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *Server) supervisorctlStart(ctx context.Context, containerName string, programs []string) error {
	if len(programs) == 0 {
		return nil
	}
	args := append([]string{"exec", containerName, "supervisorctl", "start"}, programs...)
	cmd := exec.CommandContext(ctx, s.config.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("supervisorctl start failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s *Server) quiesceSupervisorPrograms(ctx context.Context, job *jobs.Job, containerName string) ([]string, bool, bool) {
	statusOutput, err := s.supervisorctlStatus(ctx, containerName)
	if err != nil {
		if errors.Is(err, errSupervisorUnavailable) {
			s.jobStore.AppendLog("Supervisor not available; falling back to backup-before-stop flow")
			return nil, false, true
		}
		job.State = jobs.JobStateFailed
		job.FailureCode = "SUPERVISORCTL_FAILED"
		job.Message = err.Error()
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		return nil, false, false
	}

	status := parseSupervisorStatus(statusOutput)
	excludeSet := make(map[string]struct{}, len(s.config.SupervisorExclude))
	for _, name := range s.config.SupervisorExclude {
		excludeSet[name] = struct{}{}
	}
	includeSet := make(map[string]struct{}, len(s.config.SupervisorInclude))
	for _, name := range s.config.SupervisorInclude {
		includeSet[name] = struct{}{}
	}

	var programsToStop []string
	var programsStopped []string
	for name, state := range status {
		if len(includeSet) > 0 {
			if _, ok := includeSet[name]; !ok {
				continue
			}
		} else {
			if _, ok := excludeSet[name]; ok {
				continue
			}
		}

		programsToStop = append(programsToStop, name)
		if state == "RUNNING" || state == "STARTING" {
			programsStopped = append(programsStopped, name)
		}
	}

	if len(programsToStop) == 0 {
		s.jobStore.AppendLog("No supervisor programs to stop (after filters)")
		return nil, true, true
	}

	sort.Strings(programsToStop)
	sort.Strings(programsStopped)
	s.jobStore.AppendLog(fmt.Sprintf("Stopping supervisor programs: %s", strings.Join(programsToStop, ", ")))
	if err := s.supervisorctlStop(ctx, containerName, programsToStop); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "SUPERVISORCTL_FAILED"
		job.Message = err.Error()
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		return nil, false, false
	}

	if len(programsStopped) > 0 {
		s.jobStore.AppendLog(fmt.Sprintf("Supervisor programs stopped: %s", strings.Join(programsStopped, ", ")))
	} else {
		s.jobStore.AppendLog("No running supervisor programs needed stopping")
	}

	return programsStopped, true, true
}

func (s *Server) createPreUpgradeBackupBeforeStop(ctx context.Context, job *jobs.Job, containerName, imageTag, policyInitVersion string) (string, bool) {
	// Get current version for backup metadata
	currentVersion := "unknown"
	if versionInfo, _, err := s.resolveCoreVersion(ctx, containerName, policyInitVersion); err == nil && versionInfo != "" {
		currentVersion = versionInfo
	}

	job.State = jobs.JobStateBackingUp
	job.Message = "Creating database backup"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)

	s.jobStore.AppendLog(fmt.Sprintf("Creating pre-upgrade backup (from %s to %s)...", currentVersion, imageTag))
	s.recordHistory(history.Event{
		Type:    "backup",
		Status:  "started",
		Message: "Backup started",
		Data: map[string]string{
			"jobId":         job.JobID,
			"fromVersion":   currentVersion,
			"targetVersion": imageTag,
			"container":     containerName,
		},
	})

	backupResult := s.containerBackupExec.ExecuteBackup(ctx, containerName, backup.BackupMeta{
		FromVersion:   currentVersion,
		TargetVersion: imageTag,
		JobID:         job.JobID,
	})

	if !backupResult.Success {
		job.State = jobs.JobStateFailed
		job.FailureCode = backupResult.FailureCode
		job.Message = backupResult.ErrorMessage
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		s.recordHistory(history.Event{
			Type:    "backup",
			Status:  "failed",
			Message: backupResult.ErrorMessage,
			Data: map[string]string{
				"jobId":         job.JobID,
				"fromVersion":   currentVersion,
				"targetVersion": imageTag,
				"failureCode":   backupResult.FailureCode,
			},
		})

		// Provide context-specific recovery guidance
		switch backupResult.FailureCode {
		case "DOCKER_DAEMON_DOWN":
			s.jobStore.AppendLog("Next steps: Start Docker daemon with 'sudo systemctl start docker' and retry.")
		case "CONTAINER_NOT_FOUND":
			s.jobStore.AppendLog(fmt.Sprintf("Next steps: Ensure container '%s' is running and retry.", containerName))
		case "INVALID_DB_CONFIG":
			s.jobStore.AppendLog("Next steps: Verify container has POSTGRES_* environment variables set.")
		case "BACKUP_TIMEOUT":
			s.jobStore.AppendLog("Next steps: Check database connectivity and size. Increase timeout if needed.")
		default:
			s.jobStore.AppendLog("Next steps: Check logs and database connectivity, then retry.")
		}
		return "", false
	}

	job.BackupPath = backupResult.Path
	s.jobStore.AppendLog(fmt.Sprintf("Backup created successfully: %s (%.2f MB)", backupResult.Filename, float64(backupResult.Size)/(1024*1024)))
	if backupResult.DBConfig != nil {
		dbType := "external"
		if backupResult.DBConfig.IsLocalDB() {
			dbType = "local (in-container)"
		}
		s.jobStore.AppendLog(fmt.Sprintf("Database: %s@%s:%s (%s)", backupResult.DBConfig.Database, backupResult.DBConfig.Host, backupResult.DBConfig.Port, dbType))
	}
	backupData := map[string]string{
		"jobId":         job.JobID,
		"fromVersion":   currentVersion,
		"targetVersion": imageTag,
		"backupPath":    backupResult.Path,
		"sizeBytes":     fmt.Sprintf("%d", backupResult.Size),
	}
	if backupResult.DBConfig != nil {
		backupData["dbHost"] = backupResult.DBConfig.Host
		backupData["dbPort"] = backupResult.DBConfig.Port
		backupData["dbName"] = backupResult.DBConfig.Database
	}
	s.recordHistory(history.Event{
		Type:    "backup",
		Status:  "succeeded",
		Message: "Backup completed",
		Data:    backupData,
	})

	// Prune old backups (using legacy manager for retention logic)
	if _, err := s.backupManager.PruneBackups(s.backupManager.Config.Retention); err != nil {
		s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to prune old backups: %v", err))
	}

	return backupResult.Path, true
}

func (s *Server) createPreUpgradeBackupAfterQuiesce(ctx context.Context, job *jobs.Job, containerName, imageTag, policyInitVersion string, maxAttempts int, stoppedPrograms []string) (string, bool) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	// Get current version for backup metadata
	currentVersion := "unknown"
	if versionInfo, _, err := s.resolveCoreVersion(ctx, containerName, policyInitVersion); err == nil && versionInfo != "" {
		currentVersion = versionInfo
	}

	var lastResult *backup.BackupResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		job.State = jobs.JobStateBackingUp
		job.Message = fmt.Sprintf("Creating database backup (attempt %d/%d)", attempt, maxAttempts)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)

		s.jobStore.AppendLog(fmt.Sprintf("Creating pre-upgrade backup (from %s to %s)...", currentVersion, imageTag))
		s.recordHistory(history.Event{
			Type:    "backup",
			Status:  "started",
			Message: "Backup started",
			Data: map[string]string{
				"jobId":         job.JobID,
				"fromVersion":   currentVersion,
				"targetVersion": imageTag,
				"container":     containerName,
				"attempt":       fmt.Sprintf("%d", attempt),
			},
		})

		backupResult := s.containerBackupExec.ExecuteBackup(ctx, containerName, backup.BackupMeta{
			FromVersion:   currentVersion,
			TargetVersion: imageTag,
			JobID:         job.JobID,
		})
		lastResult = backupResult

		if backupResult.Success {
			job.BackupPath = backupResult.Path
			s.jobStore.AppendLog(fmt.Sprintf("Backup created successfully: %s (%.2f MB)", backupResult.Filename, float64(backupResult.Size)/(1024*1024)))
			if backupResult.DBConfig != nil {
				dbType := "external"
				if backupResult.DBConfig.IsLocalDB() {
					dbType = "local (in-container)"
				}
				s.jobStore.AppendLog(fmt.Sprintf("Database: %s@%s:%s (%s)", backupResult.DBConfig.Database, backupResult.DBConfig.Host, backupResult.DBConfig.Port, dbType))
			}
			backupData := map[string]string{
				"jobId":         job.JobID,
				"fromVersion":   currentVersion,
				"targetVersion": imageTag,
				"backupPath":    backupResult.Path,
				"sizeBytes":     fmt.Sprintf("%d", backupResult.Size),
			}
			if backupResult.DBConfig != nil {
				backupData["dbHost"] = backupResult.DBConfig.Host
				backupData["dbPort"] = backupResult.DBConfig.Port
				backupData["dbName"] = backupResult.DBConfig.Database
			}
			s.recordHistory(history.Event{
				Type:    "backup",
				Status:  "succeeded",
				Message: "Backup completed",
				Data:    backupData,
			})

			// Prune old backups (using legacy manager for retention logic)
			if _, err := s.backupManager.PruneBackups(s.backupManager.Config.Retention); err != nil {
				s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to prune old backups: %v", err))
			}

			return backupResult.Path, true
		}

		s.jobStore.AppendLog(fmt.Sprintf("Backup attempt %d/%d failed: %s - %s", attempt, maxAttempts, backupResult.FailureCode, backupResult.ErrorMessage))
		if attempt < maxAttempts {
			time.Sleep(2 * time.Second)
		}
	}

	job.State = jobs.JobStateFailed
	job.FailureCode = "BACKUP_FAILED_AFTER_QUIESCE"
	job.Message = fmt.Sprintf("Backup failed after %d attempts: %s", maxAttempts, lastResult.ErrorMessage)
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (attempting to restart supervisor programs)", job.FailureCode, job.Message))
	s.recordHistory(history.Event{
		Type:    "backup",
		Status:  "failed",
		Message: lastResult.ErrorMessage,
		Data: map[string]string{
			"jobId":         job.JobID,
			"fromVersion":   currentVersion,
			"targetVersion": imageTag,
			"failureCode":   job.FailureCode,
			"causeCode":     lastResult.FailureCode,
		},
	})

	// Provide context-specific recovery guidance
	switch lastResult.FailureCode {
	case "DOCKER_DAEMON_DOWN":
		s.jobStore.AppendLog("Next steps: Start Docker daemon with 'sudo systemctl start docker' and retry.")
	case "CONTAINER_NOT_FOUND":
		s.jobStore.AppendLog(fmt.Sprintf("Next steps: Ensure container '%s' exists and retry.", containerName))
	case "INVALID_DB_CONFIG":
		s.jobStore.AppendLog("Next steps: Verify container has POSTGRES_* environment variables set.")
	case "BACKUP_TIMEOUT":
		s.jobStore.AppendLog("Next steps: Check database connectivity and size. Increase timeout if needed.")
	default:
		s.jobStore.AppendLog("Next steps: Check logs and database connectivity, then retry.")
	}

	if err := s.supervisorctlStart(ctx, containerName, stoppedPrograms); err != nil {
		s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to restart supervisor programs: %v", err))
		s.jobStore.AppendLog("Attempting to restart container as last resort...")
		if restartErr := s.dockerRunner.Restart(ctx, containerName); restartErr != nil {
			s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to restart container: %v", restartErr))
		}
		return "", false
	}

	if len(stoppedPrograms) > 0 {
		s.jobStore.AppendLog(fmt.Sprintf("Supervisor programs restarted: %s", strings.Join(stoppedPrograms, ", ")))
	}
	return "", false
}

// pullUpgradeImage pulls the target image before stopping the container.
// Returns false if the pull fails.
func (s *Server) pullUpgradeImage(ctx context.Context, job *jobs.Job, imageRepo, imageTag string) bool {
	job.State = jobs.JobStateExecuting
	job.UpdatedAt = time.Now().UTC()

	imageWithTag := fmt.Sprintf("%s:%s", imageRepo, imageTag)
	job.Message = "Pulling image"
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Pulling image: %s", imageWithTag))

	if err := s.dockerRunner.Pull(ctx, imageWithTag); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_PULL_FAILED"
		job.Message = fmt.Sprintf("Failed to pull image: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (container still running)", job.FailureCode, job.Message))
		return false
	}
	s.jobStore.AppendLog("Image pulled successfully")
	return true
}

// stopContainerForUpgrade stops the container before replacing it.
// Returns false if stopping fails.
func (s *Server) stopContainerForUpgrade(ctx context.Context, job *jobs.Job, containerName string) bool {
	job.State = jobs.JobStateExecuting
	job.Message = "Stopping container"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Stopping container: %s", containerName))

	if err := s.dockerRunner.Stop(ctx, containerName); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to stop container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}
	s.jobStore.AppendLog("Container stopped successfully")
	return true
}

// replaceContainer removes the old container, runs the new one, and verifies it's running.
// Returns false if any step fails (job is already marked failed).
func (s *Server) replaceContainer(ctx context.Context, job *jobs.Job, containerName string, dockerArgs []string) bool {
	// Step 1: Remove container
	job.Message = "Removing container"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Removing container: %s", containerName))

	if err := s.dockerRunner.Remove(ctx, containerName); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to remove container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}
	s.jobStore.AppendLog("Container removed successfully")

	// Step 2: Run new container
	job.Message = "Running new container"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Running new container: %s", containerName))

	if err := s.dockerRunner.Run(ctx, dockerArgs); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to run container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}
	s.jobStore.AppendLog("Container started successfully")

	// Step 3: Verify container is running
	job.State = jobs.JobStateVerifying
	job.Message = "Verifying container status"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog("Verifying container is running...")

	running, err := s.dockerRunner.InspectRunning(ctx, containerName)
	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to inspect container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}

	if !running {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = "Container is not running after start"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}
	s.jobStore.AppendLog("Container is running")
	return true
}

// verifyUpgrade checks health endpoint and version match.
// Returns false if verification fails (job is already marked failed).
func (s *Server) verifyUpgrade(ctx context.Context, job *jobs.Job, containerName, imageTag, policyInitVersion string) bool {
	job.Message = "Verifying health endpoint"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)

	useLegacyHealth := s.shouldUseLegacyForTarget(policyInitVersion, imageTag)
	if useLegacyHealth {
		s.jobStore.AppendLog("Verifying legacy health endpoint (6 retries, 2s apart)...")
	} else {
		s.jobStore.AppendLog("Verifying /api/v1/health endpoint (6 retries, 2s apart)...")
	}

	// Health check with retries
	healthOK := false
	for attempt := 1; attempt <= 6; attempt++ {
		healthCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var healthResp *coreclient.HealthResponse
		var err error
		if useLegacyHealth {
			err = corecompat.LegacyHealth(healthCtx, s.coreClient.BaseURL)
			if err == nil {
				healthResp = &coreclient.HealthResponse{Status: "ok"}
			}
		} else {
			healthResp, err = s.coreClient.Health(healthCtx)
		}
		cancel()

		// Require status == "ok"
		// If db field is present, it must also be "ok"
		if err == nil && healthResp.Status == "ok" {
			// Validate db field only if present
			if healthResp.DB != "" && healthResp.DB != "ok" {
				s.jobStore.AppendLog(fmt.Sprintf("Health check attempt %d: status ok but db=%s (retrying...)", attempt, healthResp.DB))
				if attempt < 6 {
					time.Sleep(2 * time.Second)
				}
				continue
			}
			// Success: status is ok, and db is either not present or is ok
			if healthResp.DB != "" {
				s.jobStore.AppendLog(fmt.Sprintf("Health check passed on attempt %d (status=%s, db=%s)", attempt, healthResp.Status, healthResp.DB))
			} else {
				s.jobStore.AppendLog(fmt.Sprintf("Health check passed on attempt %d (status=%s)", attempt, healthResp.Status))
			}
			healthOK = true
			break
		}

		if attempt < 6 {
			s.jobStore.AppendLog(fmt.Sprintf("Health check attempt %d failed: %v (retrying...)", attempt, err))
			time.Sleep(2 * time.Second)
		} else {
			s.jobStore.AppendLog(fmt.Sprintf("Health check attempt %d failed: %v", attempt, err))
		}
	}

	if !healthOK {
		job.State = jobs.JobStateFailed
		job.FailureCode = "HEALTHCHECK_FAILED"
		job.Message = "Health check failed after 6 attempts"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}

	// Version verification
	job.Message = "Verifying version"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)

	if useLegacyHealth {
		s.jobStore.AppendLog("Verifying container label version matches target...")
	} else {
		s.jobStore.AppendLog("Verifying /api/v1/version matches target...")
	}

	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	var versionResp *coreclient.VersionResponse
	var err error
	if useLegacyHealth {
		versionValue, labelErr := corecompat.VersionFromLabels(versionCtx, s.config.DockerBin, containerName)
		if labelErr == nil {
			versionResp = &coreclient.VersionResponse{Version: versionValue}
		} else {
			err = labelErr
		}
	} else {
		versionResp, err = s.coreClient.Version(versionCtx)
	}
	cancel()

	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "VERSION_MISMATCH"
		job.Message = fmt.Sprintf("Failed to get version: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}

	if versionResp.Version != imageTag {
		job.State = jobs.JobStateFailed
		job.FailureCode = "VERSION_MISMATCH"
		job.Message = fmt.Sprintf("Version mismatch: expected %s, got %s", imageTag, versionResp.Version)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return false
	}
	s.jobStore.AppendLog(fmt.Sprintf("Version verified: %s", versionResp.Version))
	return true
}

// finalizeUpgrade marks job as complete and prunes old images.
func (s *Server) finalizeUpgrade(ctx context.Context, job *jobs.Job, imageRepo, imageTag string) {
	job.State = jobs.JobStateReady
	job.Message = "Upgrade completed successfully"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("SUCCESS: Upgrade to %s completed successfully", imageTag))

	// Best-effort: prune old Payram images after successful upgrade
	pruneCtx, cancelPrune := context.WithTimeout(ctx, 30*time.Second)
	defer cancelPrune()
	if err := s.dockerRunner.PrunePayramImages(pruneCtx, imageRepo, imageTag); err != nil {
		s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to prune Payram images: %v", err))
	} else {
		s.jobStore.AppendLog("Pruned old Payram images")
	}
}
