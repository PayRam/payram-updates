package dbexec

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/payram/payram-updater/internal/dockerexec"
)

// PartialSuffix marks a backup that is still being written. A dump is
// written as <name>.partial and renamed only after it was read back, so a
// file with the final name is always a complete backup.
const PartialSuffix = ".partial"

// maxOutputInError caps how much program output goes into an error message:
// psql and pg_restore errors can quote row values from the payments tables.
const maxOutputInError = 2048

// maxCapturedStderr caps how much stderr a Runner keeps in memory.
const maxCapturedStderr = 64 * 1024

// sqlDumpFooter is the comment pg_dump writes after the last statement of a
// plain-SQL dump. A .sql file without it was cut short.
const sqlDumpFooter = "-- PostgreSQL database dump complete"

// readyPollInterval is how often WaitReady asks pg_isready.
var readyPollInterval = 2 * time.Second

// restorePreamble opens the restore transaction and empties the public
// schema, so tables, columns and sequences created after the backup go too.
// lock_timeout turns "something still uses the database" into a clean,
// rolled-back failure instead of a restore that hangs forever.
const restorePreamble = `BEGIN;
SET LOCAL client_min_messages = warning;
SET LOCAL lock_timeout = '60s';
DROP SCHEMA IF EXISTS public CASCADE;
CREATE SCHEMA public;
GRANT USAGE ON SCHEMA public TO PUBLIC;
`

// restoreEpilogue is sent only after the whole backup was streamed without
// an error. The check inside the transaction refuses a backup that restored
// nothing, and COMMIT is the only commit in the stream: if the stream stops
// early for any reason, psql reaches end of input inside an open
// transaction and PostgreSQL rolls everything back.
//
// psql --single-transaction is deliberately not used: it sends COMMIT at end
// of input, so a backup cut short at a statement boundary (truncated file,
// pg_restore dying, a dropped docker exec) would commit a half-restored
// database. Verified against PostgreSQL 17: a truncated stream run with
// --single-transaction exited 0 and left the payments table empty.
const restoreEpilogue = `
DO $payram_restore$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_tables WHERE schemaname = 'public') THEN
    RAISE EXCEPTION 'the backup holds no tables in the public schema';
  END IF;
END
$payram_restore$;
COMMIT;
`

// RestoreError is returned by Restore. Unchanged reports whether the
// database is known to be exactly as it was before the restore started.
type RestoreError struct {
	Unchanged bool
	Reason    string
	Err       error
}

func (e *RestoreError) Error() string {
	if e.Unchanged {
		return "Restore failed and was rolled back: the database was not changed. " + e.Reason
	}
	return "Restore was interrupted while committing; it is not certain whether the database was replaced. " + e.Reason
}

func (e *RestoreError) Unwrap() error { return e.Err }

// errPsqlExited is what the restore stream's writer sees once psql is gone.
var errPsqlExited = errors.New("psql stopped reading the restore stream")

// toolCommander is what differs between running PostgreSQL client tools
// inside the PayRam container and on the host.
type toolCommander interface {
	// checkTarget refuses a database this executor cannot reach.
	checkTarget(db DBContext) error
	// toolCommand builds the invocation of one client tool.
	toolCommand(db DBContext, tool string, withStdin bool, args ...string) Command
	// where says where the tools run, for messages ("inside container x").
	where(db DBContext) string
	runner() Runner
	logger() Logger
}

// OSRunner runs commands with os/exec.
type OSRunner struct{}

// Run implements Runner.
func (OSRunner) Run(ctx context.Context, c Command, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	if c.Env != nil {
		cmd.Env = c.Env
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	if stdout != nil {
		cmd.Stdout = stdout
	}
	stderr := &cappedBuffer{limit: maxCapturedStderr}
	cmd.Stderr = stderr
	err := cmd.Run()
	return stderr.Bytes(), err
}

// cappedBuffer keeps the first limit bytes written to it and drops the rest.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
	// dropped reports whether anything was dropped.
	dropped bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room < len(p) {
		b.dropped = true
	}
	if room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *cappedBuffer) String() string { return b.buf.String() }

// ClientEnv is the environment for a docker or PostgreSQL client process:
// the updater's own environment plus PGPASSWORD (and PGSSLMODE when set).
func ClientEnv(password, sslMode string) []string {
	env := os.Environ()
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	if sslMode != "" {
		env = append(env, "PGSSLMODE="+sslMode)
	}
	return env
}

// TruncateOutput trims program output for an error message, keeping at
// most ~2 KB.
func TruncateOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) <= maxOutputInError {
		return s
	}
	cut := maxOutputInError
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + " ... (output truncated)"
}

// connArgs are the connection flags shared by psql, pg_dump and pg_isready.
func connArgs(db DBContext) []string {
	return []string{
		"-h", db.Creds.Host,
		"-p", db.Creds.Port,
		"-U", db.Creds.Username,
		"-d", db.Creds.Database,
	}
}

// logCommand logs a command line with every secret value hidden.
func logCommand(logger Logger, cmd Command) {
	logger.Printf("Running: %s %s", cmd.Name, strings.Join(dockerexec.RedactArgs(cmd.Args), " "))
}

// describeFailure is the most useful text for a failed command: its stderr
// when it wrote any, else the exit error.
func describeFailure(err error, stderr []byte) string {
	if text := TruncateOutput(stderr); text != "" {
		return text
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

// isToolMissing reports whether a command failed because the program itself
// is not installed (on the host, or inside the container).
func isToolMissing(err error, output []byte) bool {
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	text := strings.ToLower(string(output) + " " + errorText(err))
	return strings.Contains(text, "executable file not found") ||
		strings.Contains(text, "command not found") ||
		strings.Contains(text, ": not found")
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// failureCode picks the error code for a tool that failed.
func failureCode(err error, output []byte, otherwise string) string {
	if isToolMissing(err, output) {
		return ErrCodeToolMissing
	}
	return otherwise
}

// dumpToFile runs pg_dump into <outFile>.partial (mode 0600, written by the
// updater, never by a shell redirect), syncs it, reads it back and only then
// renames it to outFile. On any failure the partial file is deleted.
func dumpToFile(ctx context.Context, tc toolCommander, db DBContext, outFile, format string) error {
	if err := tc.checkTarget(db); err != nil {
		return err
	}
	absOut, err := filepath.Abs(outFile)
	if err != nil {
		return &DBError{Code: ErrCodeBackupFailed, Message: "failed to get absolute path for backup file", Err: err}
	}

	formatFlag := "-Fc"
	if format == "sql" {
		formatFlag = "-Fp"
	}
	args := append(connArgs(db), formatFlag, "--no-owner", "--no-acl")
	cmd := tc.toolCommand(db, "pg_dump", false, args...)
	logCommand(tc.logger(), cmd)

	partial := absOut + PartialSuffix
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return &DBError{Code: ErrCodeBackupFailed, Message: "could not create the backup file", Err: err}
	}
	// O_TRUNC keeps the mode of a file left by an older run; tighten it.
	_ = f.Chmod(0600)

	stderr, runErr := tc.runner().Run(ctx, cmd, nil, f)
	syncErr := f.Sync()
	closeErr := f.Close()

	fail := func(e *DBError) error {
		os.Remove(partial)
		return e
	}
	if runErr != nil {
		return fail(&DBError{
			Code:    failureCode(runErr, stderr, ErrCodeBackupFailed),
			Message: fmt.Sprintf("pg_dump %s failed: %s", tc.where(db), describeFailure(runErr, stderr)),
			Err:     runErr,
		})
	}
	if syncErr != nil {
		return fail(&DBError{Code: ErrCodeBackupFailed, Message: "could not write the backup to disk", Err: syncErr})
	}
	if closeErr != nil {
		return fail(&DBError{Code: ErrCodeBackupFailed, Message: "could not write the backup to disk", Err: closeErr})
	}
	if len(stderr) > 0 {
		tc.logger().Printf("pg_dump warnings: %s", TruncateOutput(stderr))
	}
	info, err := os.Stat(partial)
	if err != nil {
		return fail(&DBError{Code: ErrCodeBackupFailed, Message: "backup file disappeared while it was written", Err: err})
	}
	if info.Size() == 0 {
		return fail(&DBError{Code: ErrCodeBackupFailed, Message: "pg_dump wrote an empty backup (0 bytes)"})
	}
	if err := verifyDumpFile(ctx, tc, db, partial, format); err != nil {
		var dbErr *DBError
		if errors.As(err, &dbErr) {
			return fail(&DBError{Code: dbErr.Code, Message: "the new backup could not be read back: " + dbErr.Message, Err: dbErr.Err})
		}
		return fail(&DBError{Code: ErrCodeBackupFailed, Message: "the new backup could not be read back", Err: err})
	}
	if err := os.Rename(partial, absOut); err != nil {
		return fail(&DBError{Code: ErrCodeBackupFailed, Message: "could not move the finished backup into place", Err: err})
	}
	if err := syncDir(filepath.Dir(absOut)); err != nil {
		os.Remove(absOut)
		return &DBError{Code: ErrCodeBackupFailed, Message: "could not write the backup directory to disk", Err: err}
	}
	tc.logger().Printf("Backup written and verified: %s (%d bytes)", absOut, info.Size())
	return nil
}

// syncDir flushes a directory entry (a rename) to disk. Filesystems that do
// not support syncing a directory are accepted as they are.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// verifyDumpFile checks a backup file is complete without touching the
// database.
func verifyDumpFile(ctx context.Context, tc toolCommander, db DBContext, file, format string) error {
	switch format {
	case "sql":
		return checkSQLFooter(file)
	case "dump":
		_, err := listDumpContents(ctx, tc, db, file)
		return err
	default:
		return &DBError{Code: ErrCodeBackupDamaged, Message: fmt.Sprintf("unsupported backup format %q (must be sql or dump)", format)}
	}
}

// checkSQLFooter confirms a plain-SQL backup ends with pg_dump's completion
// footer. pg_dump 17.6+ writes a \unrestrict line after it, so the footer is
// looked for near the end rather than as the very last line.
func checkSQLFooter(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return &DBError{Code: ErrCodeBackupDamaged, Message: "cannot open the backup file", Err: err}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return &DBError{Code: ErrCodeBackupDamaged, Message: "cannot read the backup file", Err: err}
	}
	const tailSize = 16 * 1024
	offset := info.Size() - tailSize
	if offset < 0 {
		offset = 0
	}
	tail := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(tail, offset); err != nil && err != io.EOF {
		return &DBError{Code: ErrCodeBackupDamaged, Message: "cannot read the backup file", Err: err}
	}
	if !bytes.Contains(tail, []byte(sqlDumpFooter)) {
		return &DBError{
			Code:    ErrCodeBackupDamaged,
			Message: fmt.Sprintf("%s does not end with pg_dump's %q footer, so the backup is incomplete", filepath.Base(file), sqlDumpFooter),
		}
	}
	return nil
}

// listDumpContents runs pg_restore --list on a custom-format backup (fed on
// stdin, so the same command works inside the container and on the host)
// and returns the table of contents.
func listDumpContents(ctx context.Context, tc toolCommander, db DBContext, file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", &DBError{Code: ErrCodeBackupDamaged, Message: "cannot open the backup file", Err: err}
	}
	defer f.Close()
	cmd := tc.toolCommand(db, "pg_restore", true, "--list")
	listing := &cappedBuffer{limit: 8 * 1024 * 1024}
	stderr, err := tc.runner().Run(ctx, cmd, f, listing)
	if err != nil {
		return "", &DBError{
			Code:    failureCode(err, stderr, ErrCodeBackupDamaged),
			Message: fmt.Sprintf("pg_restore %s could not read %s: %s", tc.where(db), filepath.Base(file), describeFailure(err, stderr)),
			Err:     err,
		}
	}
	return listing.String(), nil
}

// hasLargeObjects reports whether a pg_restore --list table of contents
// holds large objects. pg_restore restores those inside their own
// BEGIN/COMMIT, which would commit the restore transaction half way.
func hasLargeObjects(listing string) bool {
	for _, line := range strings.Split(listing, "\n") {
		if strings.HasPrefix(line, ";") {
			continue
		}
		// "<dumpId>; <tableoid> <oid> <description...> <schema> <name> <owner>"
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		switch fields[3] {
		case "BLOB", "BLOBS":
			return true
		case "LARGE":
			if len(fields) > 4 && fields[4] == "OBJECT" {
				return true
			}
		}
	}
	return false
}

// sqlHasTransactionControl reports whether a plain-SQL backup has a line
// that is exactly BEGIN; or COMMIT; — pg_dump writes those around large
// objects, and one would end the restore transaction early.
func sqlHasTransactionControl(file string) (bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64*1024)
	atLineStart := true
	for {
		line, err := r.ReadSlice('\n')
		if atLineStart {
			switch strings.TrimRight(string(line), "\r\n") {
			case "BEGIN;", "COMMIT;":
				return true, nil
			}
		}
		// A line longer than the buffer comes in pieces; only its first piece
		// starts a line.
		atLineStart = err != bufio.ErrBufferFull
		if err == io.EOF {
			return false, nil
		}
		if err != nil && err != bufio.ErrBufferFull {
			return false, err
		}
	}
}

// joinOutput concatenates stdout and stderr for classifying a failure.
func joinOutput(stdout, stderr []byte) []byte {
	out := make([]byte, 0, len(stdout)+len(stderr)+1)
	out = append(out, stdout...)
	out = append(out, '\n')
	return append(out, stderr...)
}

// restoreFromFile replaces the public schema with the backup in a single
// transaction: one stream (BEGIN, empty the schema, the backup's SQL, a
// sanity check, COMMIT) piped into psql with ON_ERROR_STOP.
func restoreFromFile(ctx context.Context, tc toolCommander, db DBContext, inFile, format string) error {
	if err := tc.checkTarget(db); err != nil {
		return err
	}
	unchanged := func(reason string, err error) error {
		return &RestoreError{Unchanged: true, Reason: reason, Err: err}
	}
	absIn, err := filepath.Abs(inFile)
	if err != nil {
		return unchanged("Cannot resolve the backup path.", err)
	}
	if _, err := os.Stat(absIn); err != nil {
		return unchanged(fmt.Sprintf("The backup file %s cannot be read.", absIn), err)
	}

	// Check the file before anything is dropped.
	switch format {
	case "dump":
		listing, err := listDumpContents(ctx, tc, db, absIn)
		if err != nil {
			return unchanged("The backup file is damaged or incomplete: "+messageOf(err), err)
		}
		if hasLargeObjects(listing) {
			return unchanged("The backup contains large objects, which cannot be restored in one transaction. Restore it by hand with pg_restore.", nil)
		}
	case "sql":
		if err := checkSQLFooter(absIn); err != nil {
			return unchanged("The backup file is damaged or incomplete: "+messageOf(err), err)
		}
		found, err := sqlHasTransactionControl(absIn)
		if err != nil {
			return unchanged("The backup file cannot be read.", err)
		}
		if found {
			return unchanged("The .sql backup contains its own BEGIN/COMMIT (pg_dump writes them for large objects), which would commit the restore half way. Restore it by hand with psql.", nil)
		}
	default:
		return unchanged(fmt.Sprintf("Unsupported backup format %q (must be .sql or .dump).", format), nil)
	}

	src, err := os.Open(absIn)
	if err != nil {
		return unchanged(fmt.Sprintf("The backup file %s cannot be opened.", absIn), err)
	}
	defer src.Close()

	psqlArgs := append([]string{"-X", "-v", "ON_ERROR_STOP=1"}, connArgs(db)...)
	psqlArgs = append(psqlArgs, "-f", "-")
	psql := tc.toolCommand(db, "psql", true, psqlArgs...)
	logCommand(tc.logger(), psql)
	tc.logger().Printf("Restoring %s into database %s %s (one transaction; any error rolls it back)", absIn, db.Creds.Database, tc.where(db))

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// An OS pipe, not io.Pipe: psql gets the read end as its stdin directly,
	// so the runner returns the moment psql exits, and closing our read end
	// then makes every further write fail instead of blocking.
	pr, pw, err := os.Pipe()
	if err != nil {
		return unchanged("Could not set up the restore stream.", err)
	}

	type psqlOutcome struct {
		stderr []byte
		err    error
	}
	var psqlExited atomic.Bool
	done := make(chan psqlOutcome, 1)
	go func() {
		stderr, err := tc.runner().Run(streamCtx, psql, pr, nil)
		psqlExited.Store(true)
		pr.Close()
		cancel()
		done <- psqlOutcome{stderr: stderr, err: err}
	}()

	commitSent, streamErr := writeRestoreStream(streamCtx, tc, db, src, format, pw)
	psqlDiedFirst := psqlExited.Load()
	pw.Close()
	out := <-done

	interrupted := ctx.Err() != nil
	switch {
	case streamErr == nil && out.err == nil:
		tc.logger().Printf("Restore committed")
		return nil
	case streamErr != nil && !psqlDiedFirst && !interrupted:
		// The backup itself could not be streamed to the end; psql got end of
		// input inside the open transaction and nothing was committed.
		return unchanged("The backup could not be read to the end: "+messageOf(streamErr), streamErr)
	}

	firstError := firstPSQLError(out.stderr)
	if commitSent && firstError == "" {
		reason := "psql stopped after the final COMMIT was sent: " + describeFailure(out.err, out.stderr)
		if interrupted {
			reason = "The restore was interrupted after the final COMMIT was sent."
		}
		return &RestoreError{Unchanged: false, Reason: reason + " Check the database before using PayRam.", Err: out.err}
	}
	if interrupted && firstError == "" {
		return unchanged("The restore was interrupted.", ctx.Err())
	}
	return unchanged(explainPSQLFailure(db, firstError, out.err, out.stderr), out.err)
}

// writeRestoreStream writes the whole restore stream to w. COMMIT is
// written only after the backup was streamed completely; commitSent says
// whether it was.
func writeRestoreStream(ctx context.Context, tc toolCommander, db DBContext, src *os.File, format string, w io.Writer) (commitSent bool, err error) {
	if _, err := io.WriteString(w, restorePreamble); err != nil {
		return false, err
	}
	switch format {
	case "sql":
		if _, err := io.Copy(w, src); err != nil {
			return false, err
		}
	case "dump":
		// pg_restore turns the archive into SQL text; it never connects.
		cmd := tc.toolCommand(db, "pg_restore", true, "--no-owner", "--no-privileges", "-f", "-")
		logCommand(tc.logger(), cmd)
		stderr, err := tc.runner().Run(ctx, cmd, src, w)
		if err != nil {
			return false, &DBError{
				Code:    failureCode(err, stderr, ErrCodeBackupDamaged),
				Message: fmt.Sprintf("pg_restore %s failed: %s", tc.where(db), describeFailure(err, stderr)),
				Err:     err,
			}
		}
	}
	if _, err := io.WriteString(w, restoreEpilogue); err != nil {
		return true, err
	}
	return true, nil
}

// messageOf is an error's text without a DBError code prefix.
func messageOf(err error) string {
	var dbErr *DBError
	if errors.As(err, &dbErr) {
		return dbErr.Message
	}
	return err.Error()
}

// firstPSQLError returns the first ERROR or FATAL line psql printed.
func firstPSQLError(stderr []byte) string {
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.Contains(line, "ERROR:") || strings.Contains(line, "FATAL:") {
			return TruncateOutput([]byte(line))
		}
	}
	return ""
}

// explainPSQLFailure turns psql's first error into what the operator should
// know and do.
func explainPSQLFailure(db DBContext, firstError string, err error, stderr []byte) string {
	if firstError == "" {
		return "psql failed: " + describeFailure(err, stderr)
	}
	lower := strings.ToLower(firstError)
	switch {
	case strings.Contains(lower, "must be owner of") || strings.Contains(lower, "permission denied"):
		return fmt.Sprintf("The database user %q lacks permission to replace the public schema (it must own the schema and its tables, or be a superuser). psql said: %s", db.Creds.Username, firstError)
	case strings.Contains(lower, "lock timeout"):
		return "Another program is still using PayRam's tables, so they could not be replaced. Stop everything connected to the database and retry. psql said: " + firstError
	case strings.Contains(lower, "password authentication failed") || strings.Contains(lower, "no pg_hba.conf entry") ||
		strings.Contains(lower, "connection to server") || strings.Contains(lower, "could not connect"):
		return "Could not connect to the database: " + firstError
	case strings.Contains(lower, "holds no tables"):
		return "The backup holds no tables in the public schema, so it was not applied: " + firstError
	}
	return "psql stopped at the first error: " + firstError
}

// maxQueryOutput caps what a query may return. A result cut short would be
// read as a different answer, so a longer one is an error instead.
const maxQueryOutput = 1024 * 1024

// runQuery runs SQL statements with psql and returns the unaligned,
// tuples-only output.
func runQuery(ctx context.Context, tc toolCommander, db DBContext, queries ...string) (string, error) {
	// -q keeps command tags (BEGIN, COMMIT) out of the output.
	args := append([]string{"-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1"}, connArgs(db)...)
	for _, q := range queries {
		args = append(args, "-c", q)
	}
	out := &cappedBuffer{limit: maxQueryOutput}
	stderr, err := tc.runner().Run(ctx, tc.toolCommand(db, "psql", false, args...), nil, out)
	if err != nil {
		return "", &DBError{
			Code:    failureCode(err, stderr, ErrCodeDatabaseUnreachable),
			Message: fmt.Sprintf("psql %s could not query database %s as user %s: %s", tc.where(db), db.Creds.Database, db.Creds.Username, describeFailure(err, stderr)),
			Err:     err,
		}
	}
	if out.dropped {
		return "", &DBError{Code: ErrCodeDatabaseUnreachable, Message: fmt.Sprintf("the answer from database %s was longer than %d bytes", db.Creds.Database, maxQueryOutput)}
	}
	return out.String(), nil
}

// readOnlyQuery runs SELECT statements in one read-only transaction (it can
// never change the database) and returns the unaligned, tuples-only output,
// one row per line with columns separated by "|".
func readOnlyQuery(ctx context.Context, tc toolCommander, db DBContext, queries ...string) (string, error) {
	if err := tc.checkTarget(db); err != nil {
		return "", err
	}
	// Every -c shares one session, so the statements run in this one
	// read-only transaction and see one snapshot of the database.
	statements := append([]string{"BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"}, queries...)
	return runQuery(ctx, tc, db, append(statements, "COMMIT")...)
}

// describeDatabase counts tables in public and rows in public.migrations.
func describeDatabase(ctx context.Context, tc toolCommander, db DBContext) (DatabaseSummary, error) {
	if err := tc.checkTarget(db); err != nil {
		return DatabaseSummary{}, err
	}
	out, err := runQuery(ctx, tc, db,
		"SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname = 'public'",
		"SELECT to_regclass('public.migrations') IS NOT NULL")
	if err != nil {
		return DatabaseSummary{}, err
	}
	lines := strings.Fields(out)
	if len(lines) < 2 {
		return DatabaseSummary{}, fmt.Errorf("unexpected psql output while counting tables: %q", TruncateOutput([]byte(out)))
	}
	tables, err := strconv.Atoi(lines[0])
	if err != nil {
		return DatabaseSummary{}, fmt.Errorf("unexpected table count %q", lines[0])
	}
	summary := DatabaseSummary{Tables: tables, HasMigrations: lines[1] == "t"}
	if summary.HasMigrations {
		out, err := runQuery(ctx, tc, db, "SELECT count(*) FROM public.migrations")
		if err != nil {
			return summary, err
		}
		rows, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			return summary, fmt.Errorf("unexpected migrations count %q", strings.TrimSpace(out))
		}
		summary.MigrationRows = rows
	}
	return summary, nil
}

// checkTools confirms the client tools a backup and a restore need exist
// where they will run.
func checkTools(ctx context.Context, tc toolCommander, db DBContext) error {
	if err := tc.checkTarget(db); err != nil {
		return err
	}
	for _, tool := range []string{"pg_dump", "pg_restore", "psql"} {
		cmd := tc.toolCommand(db, tool, false, "--version")
		out := &cappedBuffer{limit: 4096}
		stderr, err := tc.runner().Run(ctx, cmd, nil, out)
		if err != nil {
			code := failureCode(err, joinOutput(out.Bytes(), stderr), ErrCodeBackupFailed)
			msg := fmt.Sprintf("%s could not be run %s: %s", tool, tc.where(db), describeFailure(err, stderr))
			if code == ErrCodeToolMissing {
				msg = fmt.Sprintf("%s is not installed %s (%s). Install the PostgreSQL client tools there and retry.", tool, tc.where(db), describeFailure(err, stderr))
			}
			return &DBError{Code: code, Message: msg, Err: err}
		}
	}
	return nil
}

// ping runs SELECT 1.
func ping(ctx context.Context, tc toolCommander, db DBContext) error {
	if err := tc.checkTarget(db); err != nil {
		return err
	}
	out, err := runQuery(ctx, tc, db, "SELECT 1")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "1" {
		return &DBError{Code: ErrCodeDatabaseUnreachable, Message: fmt.Sprintf("SELECT 1 returned %q", TruncateOutput([]byte(out)))}
	}
	return nil
}

// waitReady polls pg_isready until the server accepts connections.
func waitReady(ctx context.Context, tc toolCommander, db DBContext, timeout time.Duration) error {
	if err := tc.checkTarget(db); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	cmd := tc.toolCommand(db, "pg_isready", false, connArgs(db)...)
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		out := &cappedBuffer{limit: 4096}
		stderr, err := tc.runner().Run(attemptCtx, cmd, nil, out)
		cancel()
		if err == nil {
			return nil
		}
		output := joinOutput(out.Bytes(), stderr)
		last := describeFailure(err, output)
		if isToolMissing(err, output) {
			return &DBError{Code: ErrCodeToolMissing, Message: fmt.Sprintf("pg_isready is not installed %s: %s", tc.where(db), last), Err: err}
		}
		if time.Now().After(deadline) {
			return &DBError{
				Code:    ErrCodeDatabaseUnreachable,
				Message: fmt.Sprintf("the database %s did not accept connections within %s (last answer: %s)", tc.where(db), timeout, last),
				Err:     err,
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}
