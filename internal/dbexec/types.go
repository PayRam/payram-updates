package dbexec

import (
	"context"
	"io"
	"time"
)

// CommandExecutor defines the interface for executing system commands.
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args []string, env []string) ([]byte, error)
}

// Command is one invocation of a docker or PostgreSQL client program.
type Command struct {
	Name string
	Args []string
	// Env is the full environment of the process. It carries PGPASSWORD, so
	// the password never appears on a command line (visible to every local
	// user in `ps`); docker exec gets only the name (`-e PGPASSWORD`).
	Env []string
}

// Runner runs a Command with the given stdin (nil for none) and stdout
// (nil to discard). It returns what the program wrote to stderr, capped so a
// noisy program cannot exhaust memory. A non-zero exit is an error.
type Runner interface {
	Run(ctx context.Context, cmd Command, stdin io.Reader, stdout io.Writer) (stderr []byte, err error)
}

// DBMode indicates where the database is running.
type DBMode string

const (
	DBModeInContainer DBMode = "in_container"
	DBModeExternal    DBMode = "external"
)

// CredentialSource indicates where credentials were obtained from.
type CredentialSource string

const (
	CredFromRunningContainer CredentialSource = "running_container"
	CredFromPersistedFile    CredentialSource = "persisted_file"
	CredFromEnv              CredentialSource = "env"
)

// DBCreds contains database connection credentials.
type DBCreds struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
	SSLMode  string
}

// Validate checks that required credentials are present.
func (c *DBCreds) Validate() error {
	if c.Host == "" {
		return &DBError{Code: "INVALID_DB_CONFIG", Message: "database host is required"}
	}
	if c.Port == "" {
		return &DBError{Code: "INVALID_DB_CONFIG", Message: "database port is required"}
	}
	if c.Database == "" {
		return &DBError{Code: "INVALID_DB_CONFIG", Message: "database name is required"}
	}
	if c.Username == "" {
		return &DBError{Code: "INVALID_DB_CONFIG", Message: "database username is required"}
	}
	return nil
}

// DBContext contains all information needed to execute database operations.
type DBContext struct {
	Mode          DBMode
	Creds         DBCreds
	CredSource    CredentialSource
	ContainerName string // set only for in_container mode
	// AppContainer is the running PayRam container whose settings named this
	// database, in either mode. Its programs are the database's writers.
	// Empty when the credentials did not come from a running container.
	AppContainer string
}

// DatabaseSummary describes what a database holds after a restore.
type DatabaseSummary struct {
	// Tables is the number of tables in the public schema.
	Tables int
	// HasMigrations reports whether public.migrations exists.
	HasMigrations bool
	// MigrationRows is the row count of public.migrations (0 when absent).
	MigrationRows int
}

// PGExecutor runs PostgreSQL operations against one database, either inside
// the PayRam container (DockerPGExecutor) or from the host (HostPGExecutor).
type PGExecutor interface {
	// Dump writes a backup to outFile (mode 0600) and checks it can be read
	// back before returning; on any failure nothing is left at outFile.
	// format is "sql" for plain SQL or "dump" for custom format.
	Dump(ctx context.Context, db DBContext, outFile string, format string) error

	// VerifyDumpFile checks that a backup file is complete: pg_restore --list
	// must read a "dump" file, and a "sql" file must end with pg_dump's
	// completion footer. It changes nothing.
	VerifyDumpFile(ctx context.Context, db DBContext, file string, format string) error

	// Restore replaces the public schema with the backup in one transaction.
	// Any error rolls the whole transaction back and leaves the database
	// exactly as it was.
	Restore(ctx context.Context, db DBContext, inFile string, format string) error

	// DescribeDatabase counts the tables and migration rows in the database.
	DescribeDatabase(ctx context.Context, db DBContext) (DatabaseSummary, error)

	// CheckTools confirms pg_dump, pg_restore and psql exist where they run.
	CheckTools(ctx context.Context, db DBContext) error

	// Query runs SELECT statements in one read-only transaction and returns
	// the rows as unaligned, tuples-only text: one row per line, columns
	// separated by "|". It can never change the database.
	Query(ctx context.Context, db DBContext, queries ...string) (string, error)

	// Ping runs SELECT 1 as the configured user.
	Ping(ctx context.Context, db DBContext) error

	// WaitReady polls pg_isready until the server accepts connections or
	// timeout passes.
	WaitReady(ctx context.Context, db DBContext, timeout time.Duration) error
}

// DBError represents a database operation error with a code.
type DBError struct {
	Code    string
	Message string
	Err     error
}

// Error codes
const (
	ErrCodeContainerNotFound = "CONTAINER_NOT_FOUND"
	ErrCodeInvalidConfig     = "INVALID_DB_CONFIG"
	ErrCodeBackupFailed      = "BACKUP_FAILED"
	ErrCodeRestoreFailed     = "RESTORE_FAILED"
	// ErrCodeBackupDamaged: the backup file is incomplete or unreadable.
	ErrCodeBackupDamaged = "BACKUP_DAMAGED"
	// ErrCodeToolMissing: pg_dump, pg_restore, psql or pg_isready is not
	// installed where it has to run.
	ErrCodeToolMissing = "PG_TOOL_NOT_FOUND"
	// ErrCodeDatabaseUnreachable: the database refused or did not answer.
	ErrCodeDatabaseUnreachable = "DATABASE_UNREACHABLE"
)

func (e *DBError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Message + ": " + e.Err.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *DBError) Unwrap() error {
	return e.Err
}
