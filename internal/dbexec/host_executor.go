package dbexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// HostPGExecutor executes PostgreSQL operations from the host using local pg_* tools.
type HostPGExecutor struct {
	Executor     CommandExecutor
	Logger       Logger
	PGDumpBin    string // path to pg_dump binary (optional, defaults to "pg_dump")
	PSQLBin      string // path to psql binary (optional, defaults to "psql")
	PGRestoreBin string // path to pg_restore binary (optional, defaults to "pg_restore")
}

// NewHostPGExecutor creates a new HostPGExecutor.
func NewHostPGExecutor(executor CommandExecutor, logger Logger) *HostPGExecutor {
	if logger == nil {
		logger = &noopLogger{}
	}
	return &HostPGExecutor{
		Executor:     executor,
		Logger:       logger,
		PGDumpBin:    "pg_dump",
		PSQLBin:      "psql",
		PGRestoreBin: "pg_restore",
	}
}

// Dump creates a database backup by running pg_dump from the host.
func (e *HostPGExecutor) Dump(ctx context.Context, db DBContext, outFile string, format string) error {
	if db.Mode == DBModeInContainer {
		return &DBError{
			Code:    "INVALID_DB_CONFIG",
			Message: "HostPGExecutor can only be used with external databases",
		}
	}

	e.Logger.Printf("[HostPGExecutor] Executing pg_dump from host to external database: %s:%s", db.Creds.Host, db.Creds.Port)
	e.Logger.Printf("[HostPGExecutor] This will use host pg_dump binary - NOT docker exec")

	// Get absolute path for the output file
	absOutFile, err := filepath.Abs(outFile)
	if err != nil {
		return &DBError{
			Code:    "BACKUP_FAILED",
			Message: "failed to get absolute path for backup file",
			Err:     err,
		}
	}

	// Build pg_dump arguments
	args := []string{
		"-h", db.Creds.Host,
		"-p", db.Creds.Port,
		"-U", db.Creds.Username,
		"-d", db.Creds.Database,
		"-f", absOutFile,
	}

	// Add format flag
	if format == "sql" {
		args = append(args, "-Fp") // plain SQL format
	} else {
		args = append(args, "-Fc") // custom format
	}

	// Build environment with PGPASSWORD
	env := os.Environ()
	if db.Creds.Password != "" {
		env = append(env, fmt.Sprintf("PGPASSWORD=%s", db.Creds.Password))
	}

	e.Logger.Printf("Running: %s (to %s)", e.PGDumpBin, absOutFile)

	output, err := e.Executor.Execute(ctx, e.PGDumpBin, args, env)
	if err != nil {
		return &DBError{
			Code:    "BACKUP_FAILED",
			Message: fmt.Sprintf("pg_dump (host) failed: %v: %s", err, string(output)),
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

// Restore restores a database from a backup by running pg_restore or psql from the host.
func (e *HostPGExecutor) Restore(ctx context.Context, db DBContext, inFile string, format string) error {
	if db.Mode == DBModeInContainer {
		return &DBError{
			Code:    "INVALID_DB_CONFIG",
			Message: "HostPGExecutor can only be used with external databases",
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

	// Build environment with PGPASSWORD
	env := os.Environ()
	if db.Creds.Password != "" {
		env = append(env, fmt.Sprintf("PGPASSWORD=%s", db.Creds.Password))
	}

	var output []byte
	if format == "sql" {
		e.Logger.Printf("Executing psql from host to remote database: %s:%s", db.Creds.Host, db.Creds.Port)
		args := []string{
			"-h", db.Creds.Host,
			"-p", db.Creds.Port,
			"-U", db.Creds.Username,
			"-d", db.Creds.Database,
			"-f", absInFile,
		}
		output, err = e.Executor.Execute(ctx, e.PSQLBin, args, env)
	} else {
		e.Logger.Printf("Executing pg_restore from host to remote database: %s:%s", db.Creds.Host, db.Creds.Port)
		args := []string{
			"--clean",
			"--if-exists",
			"--no-owner",
			"--no-privileges",
			"-h", db.Creds.Host,
			"-p", db.Creds.Port,
			"-U", db.Creds.Username,
			"-d", db.Creds.Database,
			absInFile,
		}
		output, err = e.Executor.Execute(ctx, e.PGRestoreBin, args, env)
	}

	if err != nil {
		return &DBError{
			Code:    "RESTORE_FAILED",
			Message: fmt.Sprintf("restore (host) failed: %v: %s", err, string(output)),
			Err:     err,
		}
	}

	e.Logger.Printf("Database restored successfully from: %s", absInFile)
	return nil
}
