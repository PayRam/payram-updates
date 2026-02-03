package dbexec

import "context"

// CommandExecutor defines the interface for executing system commands.
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args []string, env []string) ([]byte, error)
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
}

// PGExecutor defines the interface for executing PostgreSQL operations.
type PGExecutor interface {
	// Dump creates a database backup.
	// format should be "sql" for plain SQL or "dump" for custom format.
	Dump(ctx context.Context, db DBContext, outFile string, format string) error

	// Restore restores a database from a backup.
	// format should be "sql" for plain SQL or "dump" for custom format.
	Restore(ctx context.Context, db DBContext, inFile string, format string) error
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
