package dbexec

import (
	"context"
	"path/filepath"
	"time"
)

// HostPGExecutor runs PostgreSQL client tools on the host, for a database
// outside the PayRam container.
type HostPGExecutor struct {
	// Runner runs the tools; nil means OSRunner.
	Runner       Runner
	Logger       Logger
	PGDumpBin    string // path to pg_dump binary (optional, defaults to "pg_dump")
	PSQLBin      string // path to psql binary (optional, defaults to "psql")
	PGRestoreBin string // path to pg_restore binary (optional, defaults to "pg_restore")
	PGIsReadyBin string // path to pg_isready binary (optional, defaults to "pg_isready")
}

// NewHostPGExecutor creates a new HostPGExecutor.
func NewHostPGExecutor(runner Runner, logger Logger) *HostPGExecutor {
	if logger == nil {
		logger = &noopLogger{}
	}
	if runner == nil {
		runner = OSRunner{}
	}
	return &HostPGExecutor{
		Runner:       runner,
		Logger:       logger,
		PGDumpBin:    "pg_dump",
		PSQLBin:      "psql",
		PGRestoreBin: "pg_restore",
		PGIsReadyBin: "pg_isready",
	}
}

// UseToolsNextTo points every tool at the directory of pgDumpBin, so a
// configured /usr/lib/postgresql/17/bin/pg_dump brings its own psql and
// pg_restore. A bare name ("pg_dump") keeps PATH lookup for all of them.
func (e *HostPGExecutor) UseToolsNextTo(pgDumpBin string) {
	if pgDumpBin == "" {
		return
	}
	e.PGDumpBin = pgDumpBin
	if filepath.Base(pgDumpBin) == pgDumpBin {
		return
	}
	dir := filepath.Dir(pgDumpBin)
	e.PSQLBin = filepath.Join(dir, "psql")
	e.PGRestoreBin = filepath.Join(dir, "pg_restore")
	e.PGIsReadyBin = filepath.Join(dir, "pg_isready")
}

func (e *HostPGExecutor) checkTarget(db DBContext) error {
	if db.Mode == DBModeInContainer {
		return &DBError{
			Code:    ErrCodeInvalidConfig,
			Message: "HostPGExecutor can only be used with external databases",
		}
	}
	return nil
}

// toolCommand builds the host invocation of a client tool. The password and
// SSL mode travel in the environment.
func (e *HostPGExecutor) toolCommand(db DBContext, tool string, withStdin bool, args ...string) Command {
	return Command{
		Name: e.binFor(tool),
		Args: append([]string(nil), args...),
		Env:  ClientEnv(db.Creds.Password, db.Creds.SSLMode),
	}
}

func (e *HostPGExecutor) binFor(tool string) string {
	pick := func(configured, fallback string) string {
		if configured == "" {
			return fallback
		}
		return configured
	}
	switch tool {
	case "pg_dump":
		return pick(e.PGDumpBin, "pg_dump")
	case "psql":
		return pick(e.PSQLBin, "psql")
	case "pg_restore":
		return pick(e.PGRestoreBin, "pg_restore")
	case "pg_isready":
		return pick(e.PGIsReadyBin, "pg_isready")
	}
	return tool
}

func (e *HostPGExecutor) where(db DBContext) string {
	return "on this host"
}

func (e *HostPGExecutor) runner() Runner {
	if e.Runner == nil {
		return OSRunner{}
	}
	return e.Runner
}

func (e *HostPGExecutor) logger() Logger {
	if e.Logger == nil {
		return &noopLogger{}
	}
	return e.Logger
}

// Dump implements PGExecutor.
func (e *HostPGExecutor) Dump(ctx context.Context, db DBContext, outFile string, format string) error {
	return dumpToFile(ctx, e, db, outFile, format)
}

// VerifyDumpFile implements PGExecutor.
func (e *HostPGExecutor) VerifyDumpFile(ctx context.Context, db DBContext, file string, format string) error {
	if err := e.checkTarget(db); err != nil {
		return err
	}
	return verifyDumpFile(ctx, e, db, file, format)
}

// Restore implements PGExecutor.
func (e *HostPGExecutor) Restore(ctx context.Context, db DBContext, inFile string, format string) error {
	return restoreFromFile(ctx, e, db, inFile, format)
}

// DescribeDatabase implements PGExecutor.
func (e *HostPGExecutor) DescribeDatabase(ctx context.Context, db DBContext) (DatabaseSummary, error) {
	return describeDatabase(ctx, e, db)
}

// CheckTools implements PGExecutor.
func (e *HostPGExecutor) CheckTools(ctx context.Context, db DBContext) error {
	return checkTools(ctx, e, db)
}

// Query implements PGExecutor.
func (e *HostPGExecutor) Query(ctx context.Context, db DBContext, queries ...string) (string, error) {
	return readOnlyQuery(ctx, e, db, queries...)
}

// Ping implements PGExecutor.
func (e *HostPGExecutor) Ping(ctx context.Context, db DBContext) error {
	return ping(ctx, e, db)
}

// WaitReady implements PGExecutor.
func (e *HostPGExecutor) WaitReady(ctx context.Context, db DBContext, timeout time.Duration) error {
	return waitReady(ctx, e, db, timeout)
}
