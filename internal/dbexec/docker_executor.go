package dbexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DockerPGExecutor executes PostgreSQL operations inside a Docker container.
type DockerPGExecutor struct {
	Executor CommandExecutor
	Logger   Logger
}

// NewDockerPGExecutor creates a new DockerPGExecutor.
func NewDockerPGExecutor(executor CommandExecutor, logger Logger) *DockerPGExecutor {
	if logger == nil {
		logger = &noopLogger{}
	}
	return &DockerPGExecutor{
		Executor: executor,
		Logger:   logger,
	}
}

// Dump creates a database backup by running pg_dump inside the container.
func (e *DockerPGExecutor) Dump(ctx context.Context, db DBContext, outFile string, format string) error {
	if db.Mode != DBModeInContainer {
		return &DBError{
			Code:    "INVALID_DB_CONFIG",
			Message: "DockerPGExecutor can only be used with in-container databases",
		}
	}
	if db.ContainerName == "" {
		return &DBError{
			Code:    "CONTAINER_NOT_FOUND",
			Message: "container name is required for in-container database operations",
		}
	}

	e.Logger.Printf("[DockerPGExecutor] Executing pg_dump inside container: %s", db.ContainerName)
	e.Logger.Printf("[DockerPGExecutor] This will use 'docker exec' - NO host pg_dump")

	// Get absolute path for the output file
	absOutFile, err := filepath.Abs(outFile)
	if err != nil {
		return &DBError{
			Code:    "BACKUP_FAILED",
			Message: "failed to get absolute path for backup file",
			Err:     err,
		}
	}

	// Build pg_dump command with appropriate format flag
	formatFlag := "-Fc" // custom format by default
	if format == "sql" {
		formatFlag = "-Fp" // plain SQL format
	}

	// Build the docker exec command
	// We redirect output to the host file system
	shellCmd := fmt.Sprintf("docker exec %s pg_dump %s -U %s -d %s > %s",
		db.ContainerName,
		formatFlag,
		db.Creds.Username,
		db.Creds.Database,
		absOutFile,
	)

	e.Logger.Printf("[DockerPGExecutor] Running: docker exec %s pg_dump ...", db.ContainerName)

	output, err := e.Executor.Execute(ctx, "sh", []string{"-c", shellCmd}, nil)
	if err != nil {
		return &DBError{
			Code:    "BACKUP_FAILED",
			Message: fmt.Sprintf("pg_dump (container) failed: %v: %s", err, string(output)),
			Err:     err,
		}
	}

	// Verify the backup file was created
	if _, err := os.Stat(absOutFile); os.IsNotExist(err) {
		return &DBError{
			Code:    "BACKUP_FAILED",
			Message: fmt.Sprintf("backup file was not created: %s", absOutFile),
		}
	}

	e.Logger.Printf("Backup created successfully: %s", absOutFile)
	return nil
}

// Restore restores a database from a backup by running pg_restore or psql inside the container.
func (e *DockerPGExecutor) Restore(ctx context.Context, db DBContext, inFile string, format string) error {
	if db.Mode != DBModeInContainer {
		return &DBError{
			Code:    "INVALID_DB_CONFIG",
			Message: "DockerPGExecutor can only be used with in-container databases",
		}
	}
	if db.ContainerName == "" {
		return &DBError{
			Code:    "CONTAINER_NOT_FOUND",
			Message: "container name is required for in-container database operations",
		}
	}

	// Get absolute path for the input file
	absInFile, err := filepath.Abs(inFile)
	if err != nil {
		return &DBError{
			Code:    "RESTORE_FAILED",
			Message: "failed to get absolute path for backup file",
			Err:     err,
		}
	}

	// Verify the backup file exists
	if _, err := os.Stat(absInFile); os.IsNotExist(err) {
		return &DBError{
			Code:    "RESTORE_FAILED",
			Message: fmt.Sprintf("backup file does not exist: %s", absInFile),
			Err:     err,
		}
	}

	var shellCmd string
	if format == "sql" {
		e.Logger.Printf("Executing psql inside container: %s", db.ContainerName)
		shellCmd = fmt.Sprintf("cat %s | docker exec -i %s psql -U %s -d %s",
			absInFile,
			db.ContainerName,
			db.Creds.Username,
			db.Creds.Database,
		)
	} else {
		e.Logger.Printf("Executing pg_restore inside container: %s", db.ContainerName)
		shellCmd = fmt.Sprintf("cat %s | docker exec -i %s pg_restore --clean --if-exists --no-owner --no-privileges -U %s -d %s",
			absInFile,
			db.ContainerName,
			db.Creds.Username,
			db.Creds.Database,
		)
	}

	e.Logger.Printf("Running: sh -c %s", shellCmd)

	output, err := e.Executor.Execute(ctx, "sh", []string{"-c", shellCmd}, nil)
	if err != nil {
		return &DBError{
			Code:    "RESTORE_FAILED",
			Message: fmt.Sprintf("restore (container) failed: %v: %s", err, string(output)),
			Err:     err,
		}
	}

	e.Logger.Printf("Database restored successfully from: %s", absInFile)
	return nil
}
