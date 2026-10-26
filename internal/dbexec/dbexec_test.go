package dbexec

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockExecutor implements CommandExecutor for testing.
type mockExecutor struct {
	executeFunc func(ctx context.Context, name string, args []string, env []string) ([]byte, error)
	calls       []mockCall
}

type mockCall struct {
	Name string
	Args []string
	Env  []string
}

func (e *mockExecutor) Execute(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	e.calls = append(e.calls, mockCall{Name: name, Args: args, Env: env})
	if e.executeFunc != nil {
		return e.executeFunc(ctx, name, args, env)
	}
	return nil, nil
}

type mockLogger struct{}

func (l *mockLogger) Printf(format string, v ...interface{}) {}

// fakeRunner is a Runner that records every command and answers with handle.
// It reads stdin to the end before calling handle, unless skipStdin says not
// to (to play a psql that dies without reading).
type fakeRunner struct {
	mu        sync.Mutex
	calls     []fakeCall
	handle    func(cmd Command, stdin []byte, stdout io.Writer) (stderr []byte, err error)
	skipStdin func(cmd Command) bool
}

type fakeCall struct {
	Cmd   Command
	Stdin []byte
}

func (f *fakeRunner) Run(ctx context.Context, cmd Command, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	var in []byte
	if stdin != nil && (f.skipStdin == nil || !f.skipStdin(cmd)) {
		in, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Cmd: cmd, Stdin: in})
	f.mu.Unlock()
	if stdout == nil {
		stdout = io.Discard
	}
	if f.handle == nil {
		return nil, nil
	}
	return f.handle(cmd, in, stdout)
}

func (f *fakeRunner) find(t *testing.T, tool string, withArg string) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if toolOf(c.Cmd) == tool && (withArg == "" || hasArg(c.Cmd, withArg)) {
			return c
		}
	}
	t.Fatalf("no %s call with %q among %d calls", tool, withArg, len(f.calls))
	return fakeCall{}
}

func (f *fakeRunner) count(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if toolOf(c.Cmd) == tool {
			n++
		}
	}
	return n
}

// toolOf names the PostgreSQL tool a command runs, through docker exec or not.
func toolOf(cmd Command) string {
	for _, tool := range []string{"psql", "pg_dump", "pg_restore", "pg_isready"} {
		if filepath.Base(cmd.Name) == tool {
			return tool
		}
		for _, a := range cmd.Args {
			if a == tool {
				return tool
			}
		}
	}
	return filepath.Base(cmd.Name)
}

func hasArg(cmd Command, arg string) bool {
	for _, a := range cmd.Args {
		if a == arg {
			return true
		}
	}
	return false
}

func hasEnv(cmd Command, kv string) bool {
	for _, e := range cmd.Env {
		if e == kv {
			return true
		}
	}
	return false
}

func containerDB() DBContext {
	return DBContext{
		Mode:          DBModeInContainer,
		ContainerName: "payram",
		Creds: DBCreds{
			Host: "localhost", Port: "5432", Database: "payramdb", Username: "payram", Password: "s3cret-pw",
		},
	}
}

func externalDB() DBContext {
	return DBContext{
		Mode: DBModeExternal,
		Creds: DBCreds{
			Host: "db.example.com", Port: "5432", Database: "payramdb", Username: "payram", Password: "s3cret-pw", SSLMode: "require",
		},
	}
}

// assertNoSecretInArgs fails when the password appears on a command line.
func assertNoSecretInArgs(t *testing.T, f *fakeRunner) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		for _, a := range c.Cmd.Args {
			if strings.Contains(a, "s3cret-pw") {
				t.Fatalf("password visible in argv of %s: %v", c.Cmd.Name, c.Cmd.Args)
			}
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const completeSQLDump = "SET statement_timeout = 0;\nCREATE TABLE public.payments (id bigint);\n--\n-- PostgreSQL database dump complete\n--\n\n\\unrestrict abc\n"

// TestDiscoverDBContext_FromEnv tests discovery from environment variables.
func TestDiscoverDBContext_FromEnv(t *testing.T) {
	t.Setenv("POSTGRES_HOST", "rds.amazonaws.com")
	t.Setenv("POSTGRES_PORT", "5432")
	t.Setenv("POSTGRES_DATABASE", "mydb")
	t.Setenv("POSTGRES_USER", "admin")
	t.Setenv("POSTGRES_PASSWORD", "password123")

	dbCtx, err := DiscoverDBContext(context.Background(), &mockExecutor{}, DiscoverOpts{Logger: &mockLogger{}})
	if err != nil {
		t.Fatalf("DiscoverDBContext failed: %v", err)
	}
	if dbCtx.Mode != DBModeExternal {
		t.Errorf("expected mode %s, got %s", DBModeExternal, dbCtx.Mode)
	}
	if dbCtx.CredSource != CredFromEnv {
		t.Errorf("expected cred source %s, got %s", CredFromEnv, dbCtx.CredSource)
	}
	if dbCtx.Creds.Host != "rds.amazonaws.com" {
		t.Errorf("expected host 'rds.amazonaws.com', got '%s'", dbCtx.Creds.Host)
	}
	if dbCtx.Creds.SSLMode != "" {
		t.Errorf("unset POSTGRES_SSLMODE must stay unset (libpq default), got %q", dbCtx.Creds.SSLMode)
	}
}

func TestDiscoverDBContext_RunningContainerNamesAppContainer(t *testing.T) {
	executor := &mockExecutor{executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		return []byte(`["POSTGRES_HOST=db.internal","POSTGRES_DATABASE=payram","POSTGRES_USERNAME=payram","POSTGRES_PASSWORD=x"]`), nil
	}}
	dbCtx, err := DiscoverDBContext(context.Background(), executor, DiscoverOpts{ContainerName: "payram-core", Logger: &mockLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	if dbCtx.Mode != DBModeExternal || dbCtx.ContainerName != "" || dbCtx.AppContainer != "payram-core" {
		t.Fatalf("got mode=%s container=%q app=%q", dbCtx.Mode, dbCtx.ContainerName, dbCtx.AppContainer)
	}
}

// TestIsLocalDB tests the IsLocalDB helper.
func TestIsLocalDB(t *testing.T) {
	tests := []struct {
		host     string
		expected bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"db.example.com", false},
		{"192.168.1.1", false},
		{"", false},
	}

	for _, tt := range tests {
		if result := IsLocalDB(tt.host); result != tt.expected {
			t.Errorf("IsLocalDB(%q) = %v, expected %v", tt.host, result, tt.expected)
		}
	}
}

func TestDockerDump_WritesPrivateVerifiedFileWithoutPasswordInArgs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "payram-backup-x.dump")
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		switch {
		case toolOf(cmd) == "pg_dump":
			io.WriteString(stdout, "PGDMP-archive")
		case toolOf(cmd) == "pg_restore" && hasArg(cmd, "--list"):
			if string(stdin) != "PGDMP-archive" {
				t.Errorf("pg_restore --list read %q, want the dump", stdin)
			}
			io.WriteString(stdout, "; Archive created\n218; 1259 16385 TABLE public payments payram\n")
		}
		return nil, nil
	}}

	if err := NewDockerPGExecutor(runner, &mockLogger{}).Dump(context.Background(), containerDB(), out, "dump"); err != nil {
		t.Fatalf("Dump: %v", err)
	}

	dump := runner.find(t, "pg_dump", "")
	if dump.Cmd.Name != "docker" {
		t.Errorf("expected docker, got %s", dump.Cmd.Name)
	}
	want := []string{"exec", "-e", "PGPASSWORD", "payram", "pg_dump", "-h", "localhost", "-p", "5432", "-U", "payram", "-d", "payramdb", "-Fc", "--no-owner", "--no-acl"}
	if strings.Join(dump.Cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("pg_dump args:\n got %v\nwant %v", dump.Cmd.Args, want)
	}
	if !hasEnv(dump.Cmd, "PGPASSWORD=s3cret-pw") {
		t.Error("PGPASSWORD must be in docker's environment")
	}
	list := runner.find(t, "pg_restore", "--list")
	if !hasArg(list.Cmd, "-i") {
		t.Error("pg_restore --list reads the dump on stdin and needs docker exec -i")
	}
	assertNoSecretInArgs(t, runner)

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("backup mode = %o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(out + PartialSuffix); !os.IsNotExist(err) {
		t.Error("partial file left behind")
	}
}

func TestDump_FailedVerificationLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.dump")
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		if toolOf(cmd) == "pg_dump" {
			io.WriteString(stdout, "garbage")
			return nil, nil
		}
		return []byte("pg_restore: error: input file does not appear to be a valid archive"), errors.New("exit status 1")
	}}
	err := NewDockerPGExecutor(runner, nil).Dump(context.Background(), containerDB(), out, "dump")
	if err == nil || !strings.Contains(err.Error(), "could not be read back") {
		t.Fatalf("expected read-back failure, got %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected no files, found %d", len(entries))
	}
}

func TestDump_PgDumpFailureLeavesNoFileAndTruncatesOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.dump")
	long := strings.Repeat("x", 10000)
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		io.WriteString(stdout, "partial")
		return []byte("pg_dump: error: " + long), errors.New("exit status 1")
	}}
	err := NewHostPGExecutor(runner, nil).Dump(context.Background(), externalDB(), out, "dump")
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Error()) > 3000 {
		t.Errorf("error message not truncated: %d bytes", len(err.Error()))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected no files, found %d", len(entries))
	}
}

func TestDump_SQLFormatNeedsFooter(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.sql")
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		io.WriteString(stdout, "CREATE TABLE t ();\n") // no footer: cut short
		return nil, nil
	}}
	err := NewHostPGExecutor(runner, nil).Dump(context.Background(), externalDB(), out, "sql")
	if err == nil || !strings.Contains(err.Error(), "dump complete") {
		t.Fatalf("expected missing-footer error, got %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("incomplete .sql backup left in place")
	}
}

func TestHostPGExecutor_DumpUsesToolsNextToPgDump(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.dump")
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		io.WriteString(stdout, "PGDMP")
		return nil, nil
	}}
	host := NewHostPGExecutor(runner, nil)
	host.UseToolsNextTo("/usr/lib/postgresql/17/bin/pg_dump")
	if err := host.Dump(context.Background(), externalDB(), out, "dump"); err != nil {
		t.Fatal(err)
	}
	dump := runner.find(t, "pg_dump", "")
	if dump.Cmd.Name != "/usr/lib/postgresql/17/bin/pg_dump" {
		t.Errorf("pg_dump bin = %s", dump.Cmd.Name)
	}
	if hasArg(dump.Cmd, "-f") {
		t.Error("pg_dump must write to stdout (the updater creates the file with 0600), not -f")
	}
	if !hasEnv(dump.Cmd, "PGPASSWORD=s3cret-pw") || !hasEnv(dump.Cmd, "PGSSLMODE=require") {
		t.Error("PGPASSWORD and PGSSLMODE must be in the environment")
	}
	list := runner.find(t, "pg_restore", "--list")
	if list.Cmd.Name != "/usr/lib/postgresql/17/bin/pg_restore" {
		t.Errorf("pg_restore bin = %s", list.Cmd.Name)
	}
	assertNoSecretInArgs(t, runner)
}

func TestUseToolsNextTo_BareNameKeepsPathLookup(t *testing.T) {
	host := NewHostPGExecutor(nil, nil)
	host.UseToolsNextTo("pg_dump")
	if host.PSQLBin != "psql" || host.PGRestoreBin != "pg_restore" {
		t.Fatalf("bare pg_dump must keep bare tool names, got psql=%s pg_restore=%s", host.PSQLBin, host.PGRestoreBin)
	}
}

// restoreRunner plays pg_restore (--list and -f -) and psql for a restore.
func restoreRunner(t *testing.T, body string, bodyErr error, psqlStderr string, psqlErr error) *fakeRunner {
	return &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		switch {
		case toolOf(cmd) == "pg_restore" && hasArg(cmd, "--list"):
			io.WriteString(stdout, "; Archive\n218; 1259 16385 TABLE public payments payram\n")
		case toolOf(cmd) == "pg_restore":
			io.WriteString(stdout, body)
			if bodyErr != nil {
				return []byte("pg_restore: error: could not read from input file: end of file"), bodyErr
			}
		case toolOf(cmd) == "psql":
			return []byte(psqlStderr), psqlErr
		}
		return nil, nil
	}}
}

func TestDockerRestore_OneTransactionStreamAndCommands(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "b.dump", "PGDMP-archive")
	runner := restoreRunner(t, "CREATE TABLE public.payments (id bigint);\n", nil, "", nil)

	if err := NewDockerPGExecutor(runner, &mockLogger{}).Restore(context.Background(), containerDB(), dump, "dump"); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	psql := runner.find(t, "psql", "")
	wantArgs := []string{"exec", "-i", "-e", "PGPASSWORD", "payram", "psql", "-X", "-v", "ON_ERROR_STOP=1",
		"-h", "localhost", "-p", "5432", "-U", "payram", "-d", "payramdb", "-f", "-"}
	if strings.Join(psql.Cmd.Args, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("psql args:\n got %v\nwant %v", psql.Cmd.Args, wantArgs)
	}
	if hasArg(psql.Cmd, "--single-transaction") || hasArg(psql.Cmd, "-1") {
		t.Error("--single-transaction commits a stream that ends early; the stream carries its own BEGIN/COMMIT")
	}
	if !hasEnv(psql.Cmd, "PGPASSWORD=s3cret-pw") {
		t.Error("PGPASSWORD must be in docker's environment")
	}

	convert := runner.find(t, "pg_restore", "-f")
	for _, flag := range []string{"-i", "--no-owner", "--no-privileges", "-"} {
		if !hasArg(convert.Cmd, flag) {
			t.Errorf("pg_restore -f - missing %s: %v", flag, convert.Cmd.Args)
		}
	}
	if string(convert.Stdin) != "PGDMP-archive" {
		t.Errorf("pg_restore read %q, want the dump", convert.Stdin)
	}
	assertNoSecretInArgs(t, runner)

	stream := string(psql.Stdin)
	begin := strings.Index(stream, "BEGIN;")
	drop := strings.Index(stream, "DROP SCHEMA IF EXISTS public CASCADE;")
	create := strings.Index(stream, "CREATE SCHEMA public;")
	body := strings.Index(stream, "CREATE TABLE public.payments")
	commit := strings.LastIndex(stream, "COMMIT;")
	if begin != 0 || !(begin < drop && drop < create && create < body && body < commit) {
		t.Fatalf("stream out of order (begin=%d drop=%d create=%d body=%d commit=%d):\n%s", begin, drop, create, body, commit, stream)
	}
	if strings.Count(stream, "COMMIT;") != 1 {
		t.Errorf("stream must hold exactly one COMMIT:\n%s", stream)
	}
	if !strings.HasSuffix(strings.TrimSpace(stream), "COMMIT;") {
		t.Errorf("COMMIT must be the last statement:\n%s", stream)
	}
}

func TestRestore_BackupThatCannotBeReadNeverCommits(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "b.dump", "PGDMP-truncated")
	runner := restoreRunner(t, "CREATE TABLE public.payments (id bigint);\nCOPY public.payments (id) FROM stdin;\n1\n", errors.New("exit status 1"), "", nil)

	err := NewDockerPGExecutor(runner, nil).Restore(context.Background(), containerDB(), dump, "dump")
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || !restoreErr.Unchanged {
		t.Fatalf("expected an unchanged RestoreError, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "Restore failed and was rolled back: the database was not changed.") {
		t.Errorf("message: %v", err)
	}
	if !strings.Contains(err.Error(), "could not be read to the end") {
		t.Errorf("message should blame the backup: %v", err)
	}
	if stream := string(runner.find(t, "psql", "").Stdin); strings.Contains(stream, "COMMIT;") {
		t.Fatalf("COMMIT sent although the backup was not read completely:\n%s", stream)
	}
}

func TestRestore_PermissionErrorIsExplainedAndRolledBack(t *testing.T) {
	dir := t.TempDir()
	sql := writeFile(t, dir, "b.sql", completeSQLDump)
	runner := restoreRunner(t, "", nil, "psql:<stdin>:5: ERROR:  must be owner of schema public\n", errors.New("exit status 3"))

	err := NewHostPGExecutor(runner, nil).Restore(context.Background(), externalDB(), sql, "sql")
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || !restoreErr.Unchanged {
		t.Fatalf("expected an unchanged RestoreError, got %v", err)
	}
	for _, want := range []string{"rolled back", "lacks permission", `"payram"`, "must be owner of schema public"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message missing %q: %v", want, err)
		}
	}
}

func TestRestore_HostSQLStreamsFileThroughHostPsql(t *testing.T) {
	dir := t.TempDir()
	sql := writeFile(t, dir, "b.sql", completeSQLDump)
	runner := restoreRunner(t, "", nil, "", nil)
	host := NewHostPGExecutor(runner, nil)
	host.UseToolsNextTo("/opt/pg/bin/pg_dump")

	if err := host.Restore(context.Background(), externalDB(), sql, "sql"); err != nil {
		t.Fatal(err)
	}
	psql := runner.find(t, "psql", "")
	if psql.Cmd.Name != "/opt/pg/bin/psql" {
		t.Errorf("psql bin = %s", psql.Cmd.Name)
	}
	if !hasEnv(psql.Cmd, "PGSSLMODE=require") {
		t.Error("PGSSLMODE must reach host psql")
	}
	if !strings.Contains(string(psql.Stdin), "CREATE TABLE public.payments") {
		t.Error("the .sql body was not streamed")
	}
	if runner.count("pg_restore") != 0 {
		t.Error("a .sql restore must not run pg_restore")
	}
	assertNoSecretInArgs(t, runner)
}

func TestRestore_IncompleteSQLRefusedBeforeTouchingDatabase(t *testing.T) {
	dir := t.TempDir()
	sql := writeFile(t, dir, "b.sql", "CREATE TABLE public.payments (id bigint);\nCOPY public.payments FROM stdin;\n1\n")
	runner := restoreRunner(t, "", nil, "", nil)

	err := NewHostPGExecutor(runner, nil).Restore(context.Background(), externalDB(), sql, "sql")
	if err == nil || !strings.Contains(err.Error(), "damaged or incomplete") {
		t.Fatalf("expected incomplete-backup error, got %v", err)
	}
	if runner.count("psql") != 0 {
		t.Fatal("psql must not run for an incomplete backup")
	}
}

func TestRestore_LargeObjectsRefused(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "b.dump", "PGDMP")
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		io.WriteString(stdout, "; Archive\n3500; 2613 16400 BLOB - 16400 payram\n")
		return nil, nil
	}}
	err := NewDockerPGExecutor(runner, nil).Restore(context.Background(), containerDB(), dump, "dump")
	if err == nil || !strings.Contains(err.Error(), "large objects") {
		t.Fatalf("expected large-object refusal, got %v", err)
	}
	if runner.count("psql") != 0 {
		t.Fatal("psql must not run")
	}
}

func TestRestore_PsqlDyingWithoutReadingIsAFailure(t *testing.T) {
	dir := t.TempDir()
	dump := writeFile(t, dir, "b.dump", "PGDMP")
	runner := restoreRunner(t, strings.Repeat("INSERT INTO t VALUES (1);\n", 20000), nil,
		"psql: error: connection to server at \"localhost\" (::1), port 5432 failed: FATAL:  password authentication failed for user \"payram\"\n",
		errors.New("exit status 2"))
	runner.skipStdin = func(cmd Command) bool { return toolOf(cmd) == "psql" }

	done := make(chan error, 1)
	go func() {
		done <- NewDockerPGExecutor(runner, nil).Restore(context.Background(), containerDB(), dump, "dump")
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "password authentication failed") || !strings.Contains(err.Error(), "not changed") {
			t.Fatalf("expected connection failure, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("restore hung after psql exited")
	}
}

func TestRestore_ExitWithoutErrorAfterCommitIsUncertain(t *testing.T) {
	dir := t.TempDir()
	sql := writeFile(t, dir, "b.sql", completeSQLDump)
	runner := restoreRunner(t, "", nil, "", errors.New("signal: killed"))

	err := NewHostPGExecutor(runner, nil).Restore(context.Background(), externalDB(), sql, "sql")
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Unchanged {
		t.Fatalf("a psql that died after COMMIT was sent must not be reported as unchanged: %v", err)
	}
}

func TestHasLargeObjects(t *testing.T) {
	cases := map[string]bool{
		"; comment BLOB\n218; 1259 16385 TABLE public BLOB payram\n": false,
		"3500; 2613 16400 BLOB - 16400 payram\n":                     true,
		"3501; 0 0 BLOBS - BLOBS \n":                                 true,
		"3502; 2613 16400 BLOB METADATA - 16400 payram\n":            true,
		"3503; 0 0 LARGE OBJECT - 1 payram\n":                        true,
	}
	for listing, want := range cases {
		if got := hasLargeObjects(listing); got != want {
			t.Errorf("hasLargeObjects(%q) = %v, want %v", listing, got, want)
		}
	}
}

func TestTruncateOutput(t *testing.T) {
	if got := TruncateOutput([]byte("  short \n")); got != "short" {
		t.Errorf("got %q", got)
	}
	long := TruncateOutput([]byte(strings.Repeat("é", 3000)))
	if len(long) > maxOutputInError+40 || !strings.HasSuffix(long, "(output truncated)") {
		t.Errorf("long output not truncated: %d bytes", len(long))
	}
}

func TestDescribeDatabase(t *testing.T) {
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		if strings.Contains(strings.Join(cmd.Args, " "), "FROM public.migrations") {
			io.WriteString(stdout, "57\n")
		} else {
			io.WriteString(stdout, "42\nt\n")
		}
		return nil, nil
	}}
	got, err := NewDockerPGExecutor(runner, nil).DescribeDatabase(context.Background(), containerDB())
	if err != nil {
		t.Fatal(err)
	}
	if got != (DatabaseSummary{Tables: 42, HasMigrations: true, MigrationRows: 57}) {
		t.Fatalf("got %+v", got)
	}
}

func TestWaitReady_PollsUntilAccepting(t *testing.T) {
	old := readyPollInterval
	readyPollInterval = time.Millisecond
	defer func() { readyPollInterval = old }()

	attempts := 0
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		attempts++
		if attempts < 3 {
			io.WriteString(stdout, "localhost:5432 - no response")
			return nil, errors.New("exit status 2")
		}
		return nil, nil
	}}
	if err := NewDockerPGExecutor(runner, nil).WaitReady(context.Background(), containerDB(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if toolOf(runner.find(t, "pg_isready", "").Cmd) != "pg_isready" {
		t.Fatal("expected pg_isready")
	}
}

func TestWaitReady_TimesOut(t *testing.T) {
	old := readyPollInterval
	readyPollInterval = time.Millisecond
	defer func() { readyPollInterval = old }()
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		return nil, errors.New("exit status 2")
	}}
	err := NewDockerPGExecutor(runner, nil).WaitReady(context.Background(), containerDB(), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not accept connections") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestCheckTools_MissingPgDump(t *testing.T) {
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		if toolOf(cmd) == "pg_dump" {
			return []byte(`OCI runtime exec failed: exec failed: unable to start container process: exec: "pg_dump": executable file not found in $PATH: unknown`), errors.New("exit status 126")
		}
		return nil, nil
	}}
	err := NewDockerPGExecutor(runner, nil).CheckTools(context.Background(), containerDB())
	var dbErr *DBError
	if !errors.As(err, &dbErr) || dbErr.Code != ErrCodeToolMissing {
		t.Fatalf("expected %s, got %v", ErrCodeToolMissing, err)
	}
	if !strings.Contains(err.Error(), "pg_dump is not installed inside container payram") {
		t.Errorf("message: %v", err)
	}
}

func TestExecutorsRefuseTheWrongDatabase(t *testing.T) {
	if err := NewDockerPGExecutor(nil, nil).Restore(context.Background(), externalDB(), "x.dump", "dump"); err == nil {
		t.Error("docker executor must refuse an external database")
	}
	if err := NewHostPGExecutor(nil, nil).Restore(context.Background(), containerDB(), "x.dump", "dump"); err == nil {
		t.Error("host executor must refuse the in-container database")
	}
}

func TestRestore_SQLWithItsOwnCommitRefused(t *testing.T) {
	dir := t.TempDir()
	sql := writeFile(t, dir, "b.sql", "BEGIN;\nSELECT pg_catalog.lo_open('16400', 131072);\nCOMMIT;\n"+completeSQLDump)
	runner := restoreRunner(t, "", nil, "", nil)
	err := NewHostPGExecutor(runner, nil).Restore(context.Background(), externalDB(), sql, "sql")
	if err == nil || !strings.Contains(err.Error(), "BEGIN/COMMIT") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if runner.count("psql") != 0 {
		t.Fatal("psql must not run")
	}
}

func TestSQLHasTransactionControl_LongLinesAndData(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 200*1024) + "COMMIT;\n" // the text is inside a long line, not a line of its own
	clean := writeFile(t, dir, "clean.sql", "CREATE TABLE t (v text);\nCOPY t (v) FROM stdin;\n"+long+"\\.\n"+completeSQLDump)
	if found, err := sqlHasTransactionControl(clean); err != nil || found {
		t.Fatalf("found=%v err=%v, want no transaction control", found, err)
	}
	dirty := writeFile(t, dir, "dirty.sql", long+"COMMIT;\n")
	if found, _ := sqlHasTransactionControl(dirty); !found {
		t.Fatal("a COMMIT; line after a long line was missed")
	}
}

// TestQuery_RunsInOneReadOnlyTransaction: the switch back's database check
// must never be able to change the database, and its statements must see
// one snapshot.
func TestQuery_RunsInOneReadOnlyTransaction(t *testing.T) {
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		io.WriteString(stdout, "a=1\nb=2\n")
		return nil, nil
	}}
	out, err := NewDockerPGExecutor(runner, nil).Query(context.Background(), containerDB(), "SELECT 'a=1'", "SELECT 'b=2'")
	if err != nil {
		t.Fatal(err)
	}
	if out != "a=1\nb=2\n" {
		t.Fatalf("out = %q", out)
	}
	call := runner.find(t, "psql", "-q")
	var statements []string
	for i, arg := range call.Cmd.Args {
		if arg == "-c" && i+1 < len(call.Cmd.Args) {
			statements = append(statements, call.Cmd.Args[i+1])
		}
	}
	want := []string{"BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY", "SELECT 'a=1'", "SELECT 'b=2'", "COMMIT"}
	if strings.Join(statements, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements = %q, want %q", statements, want)
	}
}

func TestQuery_RefusesAnAnswerThatWasCutShort(t *testing.T) {
	runner := &fakeRunner{handle: func(cmd Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		io.WriteString(stdout, strings.Repeat("x", maxQueryOutput+10))
		return nil, nil
	}}
	if _, err := NewHostPGExecutor(runner, nil).Query(context.Background(), externalDB(), "SELECT 1"); err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("got %v, want an error: a cut answer reads as a different one", err)
	}
}

func TestQuery_RefusesTheWrongDatabaseKind(t *testing.T) {
	if _, err := NewDockerPGExecutor(nil, nil).Query(context.Background(), externalDB(), "SELECT 1"); err == nil {
		t.Fatal("docker executor must refuse an external database")
	}
}
