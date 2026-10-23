// Package backup provides database backup and restore functionality.
package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
)

// Failure codes in BackupResult.FailureCode. They match the recovery
// playbook entries.
const (
	FailureDockerDaemonDown  = "DOCKER_DAEMON_DOWN"
	FailureDockerError       = "DOCKER_ERROR"
	FailureContainerNotFound = "CONTAINER_NOT_FOUND"
	FailureInvalidDBConfig   = "INVALID_DB_CONFIG"
	FailureBackupFailed      = "BACKUP_FAILED"
	FailureBackupTimeout     = "BACKUP_TIMEOUT"
	FailureDiskSpaceLow      = "DISK_SPACE_LOW"
)

// readinessTimeout bounds CheckBackupReady.
const readinessTimeout = 2 * time.Minute

// ContainerBackupExecutor handles pg_dump backups with container-sourced credentials.
// It supports both local (inside container) and external database backups.
type ContainerBackupExecutor struct {
	DockerBin       string
	PGDumpBin       string // Used for external DB backups
	BackupDir       string
	BackupTimeout   time.Duration
	Logger          Logger
	DockerInspector *DockerInspector
	// Runner runs pg_dump, pg_restore and psql; nil means dbexec.OSRunner.
	Runner dbexec.Runner
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
		Runner:          dbexec.OSRunner{},
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
	// Retryable is true only when the failure looks transient (the database
	// was starting up, a connection dropped, a lock was busy) and the same
	// backup may succeed if tried again. A timeout, a missing pg_dump, bad
	// database settings or credentials, and a full disk are never retryable:
	// retrying them only keeps PayRam stopped longer.
	Retryable bool
	DBConfig  *ContainerDBConfig // For metadata purposes
}

func failedBackup(code, message string, retryable bool) *BackupResult {
	return &BackupResult{Success: false, FailureCode: code, ErrorMessage: message, Retryable: retryable}
}

func (e *ContainerBackupExecutor) runner() dbexec.Runner {
	if e.Runner == nil {
		return dbexec.OSRunner{}
	}
	return e.Runner
}

// resolveDatabase reads the database settings from the PayRam container and
// picks the executor that reaches the database. It changes nothing.
func (e *ContainerBackupExecutor) resolveDatabase(ctx context.Context, containerName string) (dbexec.DBContext, dbexec.PGExecutor, *ContainerDBConfig, *BackupResult) {
	e.Logger.Printf("Checking Docker daemon...")
	if err := e.DockerInspector.CheckDaemon(ctx); err != nil {
		return dbexec.DBContext{}, nil, nil, failedBackup(FailureDockerDaemonDown,
			fmt.Sprintf("Docker daemon is not running: %s. Start Docker (sudo systemctl start docker) and retry.", dbexec.TruncateOutput([]byte(err.Error()))), false)
	}

	e.Logger.Printf("Checking container %s exists...", containerName)
	exists, err := e.DockerInspector.ContainerExists(ctx, containerName)
	if err != nil {
		return dbexec.DBContext{}, nil, nil, failedBackup(FailureDockerError, fmt.Sprintf("Failed to check container: %v", err), false)
	}
	if !exists {
		return dbexec.DBContext{}, nil, nil, failedBackup(FailureContainerNotFound,
			fmt.Sprintf("Container %s not found. Check TARGET_CONTAINER_NAME or start PayRam.", containerName), false)
	}

	e.Logger.Printf("Extracting database configuration from container...")
	dbConfig, err := e.DockerInspector.GetDBConfig(ctx, containerName)
	if err != nil {
		return dbexec.DBContext{}, nil, nil, failedBackup(FailureInvalidDBConfig,
			fmt.Sprintf("Failed to get database config from container %s: %v. Check its POSTGRES_* environment variables.", containerName, err), false)
	}
	e.Logger.Printf("Database config: host=%s, port=%s, database=%s, user=%s",
		dbConfig.Host, dbConfig.Port, dbConfig.Database, dbConfig.Username)

	db := dbexec.DBContext{
		Mode:       dbexec.DBModeExternal,
		CredSource: dbexec.CredFromRunningContainer,
		Creds: dbexec.DBCreds{
			Host:     dbConfig.Host,
			Port:     dbConfig.Port,
			Database: dbConfig.Database,
			Username: dbConfig.Username,
			Password: dbConfig.Password,
			SSLMode:  dbConfig.SSLMode,
		},
		AppContainer: containerName,
	}
	if dbConfig.IsLocalDB() {
		db.Mode = dbexec.DBModeInContainer
		db.ContainerName = containerName
		docker := dbexec.NewDockerPGExecutor(e.runner(), e.Logger)
		docker.DockerBin = e.DockerBin
		return db, docker, dbConfig, nil
	}
	host := dbexec.NewHostPGExecutor(e.runner(), e.Logger)
	host.UseToolsNextTo(e.PGDumpBin)
	return db, host, dbConfig, nil
}

// ExecuteBackup performs a database backup from the specified container.
// It automatically detects whether the database is local or external and
// uses the appropriate backup strategy.
//
// The backup is a custom-format (compressed) pg_dump, written to a .partial
// file with mode 0600, read back with pg_restore --list, synced to disk and
// only then renamed to payram-backup-<ts>-<from>-to-<to>.dump. On failure no
// file is left behind.
//
// Database credentials are extracted from the running container's environment
// variables (POSTGRES_HOST, POSTGRES_PORT, POSTGRES_DATABASE, POSTGRES_USERNAME,
// POSTGRES_PASSWORD, POSTGRES_SSLMODE).
func (e *ContainerBackupExecutor) ExecuteBackup(ctx context.Context, containerName string, meta BackupMeta) *BackupResult {
	ctx, cancel := context.WithTimeout(ctx, e.BackupTimeout)
	defer cancel()

	db, pgExec, dbConfig, failure := e.resolveDatabase(ctx, containerName)
	if failure != nil {
		return failure
	}

	if err := secureBackupDir(e.BackupDir, e.Logger); err != nil {
		return failedBackup(FailureBackupFailed, err.Error(), false)
	}

	filename := backupFilename(time.Now(), meta)
	backupPath := filepath.Join(e.BackupDir, filename)
	if db.Mode == dbexec.DBModeInContainer {
		e.Logger.Printf("Database is local - executing pg_dump inside container")
	} else {
		e.Logger.Printf("Database is external - executing pg_dump on host")
	}
	e.Logger.Printf("Creating backup: %s", backupPath)

	err := pgExec.Dump(ctx, db, backupPath, "dump")
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		os.Remove(backupPath)
		return failedBackup(FailureBackupTimeout,
			fmt.Sprintf("Backup timed out after %v. Raise BACKUP_TIMEOUT_SECONDS if the database is large, or check that it is not overloaded.", e.BackupTimeout), false)
	}
	if err != nil {
		code, retryable := classifyBackupError(err)
		return failedBackup(code, fmt.Sprintf("pg_dump failed: %s", errorMessage(err)), retryable)
	}

	fileInfo, err := os.Stat(backupPath)
	if err != nil {
		return failedBackup(FailureBackupFailed, fmt.Sprintf("Backup file not created: %v", err), false)
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

// CheckBackupReady confirms, before anything is stopped, that a backup of
// containerName's database can be taken: Docker answers, the container exists
// and is running, its database settings are readable, pg_dump, pg_restore and
// psql exist where they will run (inside the container for the bundled
// database, on the host for an external one), SELECT 1 succeeds and the
// backup directory is writable. It is read-only. The error says what to fix.
func (e *ContainerBackupExecutor) CheckBackupReady(ctx context.Context, containerName string) error {
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()

	db, pgExec, _, failure := e.resolveDatabase(ctx, containerName)
	if failure != nil {
		return errors.New(failure.ErrorMessage)
	}

	running, err := e.DockerInspector.ContainerRunning(ctx, containerName)
	if err != nil {
		return fmt.Errorf("could not check whether container %s is running: %v", containerName, err)
	}
	if !running {
		return fmt.Errorf("container %s is not running, so its database cannot be backed up. Start PayRam (docker start %s) and retry", containerName, containerName)
	}

	if err := pgExec.CheckTools(ctx, db); err != nil {
		return errors.New(errorMessage(err))
	}

	if err := pgExec.Ping(ctx, db); err != nil {
		return fmt.Errorf("the database did not answer SELECT 1 as user %s: %s. Check that it is running and that the container's POSTGRES_* settings are right",
			db.Creds.Username, errorMessage(err))
	}

	if err := checkDirWritable(e.BackupDir); err != nil {
		return err
	}
	return nil
}

// checkDirWritable checks that dir, or the nearest directory above it that
// exists (where it would be created), is writable. It creates nothing.
func checkDirWritable(dir string) error {
	if dir == "" {
		return fmt.Errorf("the backup directory is not configured (BACKUP_DIR)")
	}
	path, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("backup directory %s: %v", dir, err)
	}
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("backup directory %s: %s is not a directory", dir, path)
			}
			if err := syscall.Access(path, 0x2 /* W_OK */); err != nil {
				return fmt.Errorf("backup directory %s is not writable (%s: %v); fix its ownership or permissions", dir, path, err)
			}
			return nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return fmt.Errorf("backup directory %s: no existing parent directory", dir)
		}
		path = parent
	}
}

// errorMessage is an error's text without a dbexec code prefix.
func errorMessage(err error) string {
	var dbErr *dbexec.DBError
	if errors.As(err, &dbErr) {
		return dbErr.Message
	}
	return err.Error()
}

// classifyBackupError maps a failed dump to a failure code and whether
// trying again can help.
func classifyBackupError(err error) (code string, retryable bool) {
	var dbErr *dbexec.DBError
	if errors.As(err, &dbErr) && dbErr.Code == dbexec.ErrCodeToolMissing {
		return FailureBackupFailed, false
	}
	text := strings.ToLower(err.Error())
	containsAny := func(needles ...string) bool {
		for _, n := range needles {
			if strings.Contains(text, n) {
				return true
			}
		}
		return false
	}
	switch {
	case errors.Is(err, syscall.ENOSPC) || containsAny("no space left on device", "disk quota exceeded"):
		return FailureDiskSpaceLow, false
	case containsAny("password authentication failed", "authentication failed", "no pg_hba.conf entry",
		"permission denied", "does not exist", "invalid port"):
		return FailureInvalidDBConfig, false
	case containsAny("connection refused", "could not connect", "the database system is starting up",
		"the database system is in recovery mode", "the database system is shutting down",
		"server closed the connection unexpectedly", "terminating connection", "connection reset",
		"deadlock detected", "could not obtain lock", "lock timeout", "could not serialize access",
		"too many clients", "too many connections", "is restarting"):
		return FailureBackupFailed, true
	}
	return FailureBackupFailed, false
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
