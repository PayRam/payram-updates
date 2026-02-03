// Package backup provides database backup and restore functionality.
package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ContainerBackupExecutor handles pg_dump backups with container-sourced credentials.
// It supports both local (inside container) and external database backups.
type ContainerBackupExecutor struct {
	DockerBin       string
	PGDumpBin       string // Used for external DB backups
	BackupDir       string
	BackupTimeout   time.Duration
	Logger          Logger
	DockerInspector *DockerInspector
}

// NewContainerBackupExecutor creates a new ContainerBackupExecutor.
func NewContainerBackupExecutor(dockerBin, pgDumpBin, backupDir string, logger Logger) *ContainerBackupExecutor {
	if dockerBin == "" {
		dockerBin = "docker"
	}
	if pgDumpBin == "" {
		pgDumpBin = "pg_dump"
	}
	return &ContainerBackupExecutor{
		DockerBin:       dockerBin,
		PGDumpBin:       pgDumpBin,
		BackupDir:       backupDir,
		BackupTimeout:   60 * time.Second,
		Logger:          logger,
		DockerInspector: NewDockerInspector(dockerBin, nil),
	}
}

// BackupResult contains the result of a backup operation.
type BackupResult struct {
	Success      bool
	Path         string
	Filename     string
	Size         int64
	FailureCode  string
	ErrorMessage string
	DBConfig     *ContainerDBConfig // For metadata purposes
}

// ExecuteBackup performs a database backup from the specified container.
// It automatically detects whether the database is local or external and
// uses the appropriate backup strategy.
//
// Database credentials are extracted from the running container's environment
// variables (POSTGRES_HOST, POSTGRES_PORT, POSTGRES_DATABASE, POSTGRES_USERNAME,
// POSTGRES_PASSWORD, POSTGRES_SSLMODE).
func (e *ContainerBackupExecutor) ExecuteBackup(ctx context.Context, containerName string, meta BackupMeta) *BackupResult {
	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, e.BackupTimeout)
	defer cancel()

	// Step 1: Verify Docker daemon is running
	e.Logger.Printf("Checking Docker daemon...")
	if err := e.DockerInspector.CheckDaemon(ctx); err != nil {
		return &BackupResult{
			Success:      false,
			FailureCode:  "DOCKER_DAEMON_DOWN",
			ErrorMessage: fmt.Sprintf("Docker daemon is not running: %v", err),
		}
	}

	// Step 2: Verify container exists
	e.Logger.Printf("Checking container %s exists...", containerName)
	exists, err := e.DockerInspector.ContainerExists(ctx, containerName)
	if err != nil {
		return &BackupResult{
			Success:      false,
			FailureCode:  "DOCKER_ERROR",
			ErrorMessage: fmt.Sprintf("Failed to check container: %v", err),
		}
	}
	if !exists {
		return &BackupResult{
			Success:      false,
			FailureCode:  "CONTAINER_NOT_FOUND",
			ErrorMessage: fmt.Sprintf("Container %s not found", containerName),
		}
	}

	// Step 3: Extract DB config from container
	e.Logger.Printf("Extracting database configuration from container...")
	dbConfig, err := e.DockerInspector.GetDBConfig(ctx, containerName)
	if err != nil {
		return &BackupResult{
			Success:      false,
			FailureCode:  "INVALID_DB_CONFIG",
			ErrorMessage: fmt.Sprintf("Failed to get database config from container: %v", err),
		}
	}

	e.Logger.Printf("Database config: host=%s, port=%s, database=%s, user=%s",
		dbConfig.Host, dbConfig.Port, dbConfig.Database, dbConfig.Username)

	// Step 4: Ensure backup directory exists
	if err := os.MkdirAll(e.BackupDir, 0755); err != nil {
		return &BackupResult{
			Success:      false,
			FailureCode:  "BACKUP_FAILED",
			ErrorMessage: fmt.Sprintf("Failed to create backup directory: %v", err),
		}
	}

	// Step 5: Generate backup filename
	timestamp := time.Now().UTC().Format("20060102-150405")
	fromVer := sanitizeVersion(meta.FromVersion)
	toVer := sanitizeVersion(meta.TargetVersion)
	filename := fmt.Sprintf("payram-backup-%s-%s-to-%s.sql", timestamp, fromVer, toVer)
	backupPath := filepath.Join(e.BackupDir, filename)

	e.Logger.Printf("Creating backup: %s", backupPath)

	// Step 6: Execute backup based on database location
	var execErr error
	if dbConfig.IsLocalDB() {
		e.Logger.Printf("Database is local - executing pg_dump inside container")
		execErr = e.executeContainerBackup(ctx, containerName, dbConfig, backupPath)
	} else {
		e.Logger.Printf("Database is external - executing pg_dump on host")
		execErr = e.executeHostBackup(ctx, dbConfig, backupPath)
	}

	// Check for context timeout
	if ctx.Err() == context.DeadlineExceeded {
		// Clean up partial backup file
		os.Remove(backupPath)
		return &BackupResult{
			Success:      false,
			FailureCode:  "BACKUP_TIMEOUT",
			ErrorMessage: fmt.Sprintf("Backup timed out after %v", e.BackupTimeout),
		}
	}

	if execErr != nil {
		// Clean up partial backup file
		os.Remove(backupPath)
		return &BackupResult{
			Success:      false,
			FailureCode:  "BACKUP_FAILED",
			ErrorMessage: fmt.Sprintf("pg_dump failed: %v", execErr),
		}
	}

	// Step 7: Validate backup file
	fileInfo, err := os.Stat(backupPath)
	if err != nil {
		return &BackupResult{
			Success:      false,
			FailureCode:  "BACKUP_FAILED",
			ErrorMessage: fmt.Sprintf("Backup file not created: %v", err),
		}
	}

	if fileInfo.Size() == 0 {
		os.Remove(backupPath)
		return &BackupResult{
			Success:      false,
			FailureCode:  "BACKUP_FAILED",
			ErrorMessage: "Backup file is empty (0 bytes)",
		}
	}

	e.Logger.Printf("Backup completed successfully: %s (%.2f MB)", filename, float64(fileInfo.Size())/(1024*1024))

	return &BackupResult{
		Success:  true,
		Path:     backupPath,
		Filename: filename,
		Size:     fileInfo.Size(),
		DBConfig: dbConfig,
	}
}

// executeContainerBackup runs pg_dump inside the container and streams output to host.
func (e *ContainerBackupExecutor) executeContainerBackup(ctx context.Context, containerName string, dbConfig *ContainerDBConfig, backupPath string) error {
	// Build the pg_dump command to run inside the container
	// We use plain text format and stream to stdout, then capture to file on host
	pgDumpCmd := fmt.Sprintf(
		"pg_dump -h %s -p %s -U %s -d %s --no-owner --no-acl",
		dbConfig.Host, dbConfig.Port, dbConfig.Username, dbConfig.Database,
	)

	// Build docker exec command
	args := []string{
		"exec",
	}

	// Set password via environment if provided
	if dbConfig.Password != "" {
		args = append(args, "-e", fmt.Sprintf("PGPASSWORD=%s", dbConfig.Password))
	}

	args = append(args, containerName, "sh", "-c", pgDumpCmd)

	e.Logger.Printf("Executing: docker %s", strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, e.DockerBin, args...)

	// Create backup file
	outFile, err := os.Create(backupPath)
	if err != nil {
		return fmt.Errorf("failed to create backup file: %w", err)
	}
	defer outFile.Close()

	// Capture stdout to file, stderr to buffer
	cmd.Stdout = outFile
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start pg_dump: %w", err)
	}

	// Read stderr in background
	stderrBytes, _ := io.ReadAll(stderrPipe)

	err = cmd.Wait()
	if err != nil {
		stderrStr := string(stderrBytes)
		if stderrStr != "" {
			return fmt.Errorf("%w: %s", err, stderrStr)
		}
		return err
	}

	// Log any stderr warnings (pg_dump sometimes writes warnings to stderr even on success)
	if len(stderrBytes) > 0 {
		e.Logger.Printf("pg_dump stderr: %s", string(stderrBytes))
	}

	return nil
}

// executeHostBackup runs pg_dump on the host with credentials from the container.
func (e *ContainerBackupExecutor) executeHostBackup(ctx context.Context, dbConfig *ContainerDBConfig, backupPath string) error {
	// Convert port to int for validation
	port, err := strconv.Atoi(dbConfig.Port)
	if err != nil {
		return fmt.Errorf("invalid port: %s", dbConfig.Port)
	}

	// Build pg_dump arguments
	args := []string{
		"-h", dbConfig.Host,
		"-p", strconv.Itoa(port),
		"-U", dbConfig.Username,
		"-d", dbConfig.Database,
		"--no-owner",
		"--no-acl",
		"-f", backupPath,
	}

	e.Logger.Printf("Executing: %s %s", e.PGDumpBin, strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, e.PGDumpBin, args...)

	// Set environment variables
	cmd.Env = os.Environ()
	if dbConfig.Password != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("PGPASSWORD=%s", dbConfig.Password))
	}
	if dbConfig.SSLMode != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("PGSSLMODE=%s", dbConfig.SSLMode))
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 0 {
			return fmt.Errorf("%w: %s", err, string(output))
		}
		return err
	}

	// Log any output (warnings, etc.)
	if len(output) > 0 {
		e.Logger.Printf("pg_dump output: %s", string(output))
	}

	return nil
}

// CheckDockerDaemon is a standalone function to verify the Docker daemon is running.
// This can be used as a pre-flight check before any upgrade/backup/recovery operations.
func CheckDockerDaemon(ctx context.Context, dockerBin string) error {
	if dockerBin == "" {
		dockerBin = "docker"
	}
	inspector := NewDockerInspector(dockerBin, nil)
	return inspector.CheckDaemon(ctx)
}
