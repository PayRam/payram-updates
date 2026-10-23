package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
)

// dockerAnswers plays the docker CLI for DockerInspector: daemon up, the
// container exists, its environment names dbHost, and it runs when running.
func dockerAnswers(dbHost string, running bool, daemonUp bool) *mockExecutor {
	return &mockExecutor{executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "info":
			if !daemonUp {
				return []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), &mockError{msg: "exit status 1"}
			}
			return []byte("ok"), nil
		case strings.Contains(joined, "{{json .Config.Env}}"):
			return []byte(`["POSTGRES_HOST=` + dbHost + `","POSTGRES_PORT=5432","POSTGRES_DATABASE=payram","POSTGRES_USERNAME=payram","POSTGRES_PASSWORD=s3cret-pw"]`), nil
		case strings.Contains(joined, "{{.State.Running}}"):
			if running {
				return []byte("true\n"), nil
			}
			return []byte("false\n"), nil
		case args[0] == "inspect":
			return []byte("[{}]"), nil
		}
		return nil, nil
	}}
}

func newTestContainerBackupExecutor(t *testing.T, docker *mockExecutor, runner dbexec.Runner) *ContainerBackupExecutor {
	t.Helper()
	e := NewContainerBackupExecutor("docker", "pg_dump", filepath.Join(t.TempDir(), "backups"), &mockLogger{})
	e.DockerInspector = NewDockerInspector("docker", docker)
	e.Runner = runner
	return e
}

func TestExecuteBackup_BundledDBWritesCompressedVerifiedPrivateDump(t *testing.T) {
	runner := &fakeRunner{handle: postgresTools(nil)}
	e := newTestContainerBackupExecutor(t, dockerAnswers("localhost", true, true), runner)

	result := e.ExecuteBackup(context.Background(), "payram", BackupMeta{FromVersion: "1.7.0", TargetVersion: "1.8.0"})
	if !result.Success {
		t.Fatalf("backup failed: %s %s", result.FailureCode, result.ErrorMessage)
	}
	if !strings.HasSuffix(result.Filename, "-1.7.0-to-1.8.0.dump") {
		t.Errorf("filename = %s, want a custom-format .dump", result.Filename)
	}
	info, err := os.Stat(result.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup missing or not 0600: %v", err)
	}
	if dirInfo, _ := os.Stat(e.BackupDir); dirInfo.Mode().Perm() != 0700 {
		t.Errorf("backup dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}

	dump := runner.callsOf("pg_dump")[0].Cmd
	if dump.Name != "docker" || !cmdHasArg(dump, "-Fc") || !cmdHasArg(dump, "payram") {
		t.Errorf("expected docker exec pg_dump -Fc, got %s %v", dump.Name, dump.Args)
	}
	if !cmdHasArg(dump, "PGPASSWORD") || !cmdHasEnv(dump, "PGPASSWORD=s3cret-pw") {
		t.Errorf("PGPASSWORD must be passed by name with the value in the environment: %v", dump.Args)
	}
	if len(runner.callsOf("pg_restore --list")) != 1 {
		t.Errorf("the backup must be verified with pg_restore --list: %v", runner.steps())
	}
	runner.assertNoSecretInArgs(t, "s3cret-pw")
}

func TestExecuteBackup_ExternalDBUsesHostTools(t *testing.T) {
	runner := &fakeRunner{handle: postgresTools(nil)}
	e := newTestContainerBackupExecutor(t, dockerAnswers("db.example.com", true, true), runner)
	e.PGDumpBin = "/usr/lib/postgresql/17/bin/pg_dump"

	result := e.ExecuteBackup(context.Background(), "payram", BackupMeta{FromVersion: "1.7.0", TargetVersion: "1.8.0"})
	if !result.Success {
		t.Fatalf("backup failed: %s", result.ErrorMessage)
	}
	if got := runner.callsOf("pg_dump")[0].Cmd.Name; got != "/usr/lib/postgresql/17/bin/pg_dump" {
		t.Errorf("pg_dump = %s", got)
	}
	if got := runner.callsOf("pg_restore --list")[0].Cmd.Name; got != "/usr/lib/postgresql/17/bin/pg_restore" {
		t.Errorf("pg_restore = %s", got)
	}
}

func TestExecuteBackup_RetryableOnlyForTransientFailures(t *testing.T) {
	cases := []struct {
		name      string
		stderr    string
		code      string
		retryable bool
	}{
		{"missing pg_dump", `OCI runtime exec failed: exec: "pg_dump": executable file not found in $PATH`, FailureBackupFailed, false},
		{"wrong password", `pg_dump: error: connection to server at "localhost" failed: FATAL:  password authentication failed for user "payram"`, FailureInvalidDBConfig, false},
		{"disk full", `pg_dump: error: could not write to output file: No space left on device`, FailureDiskSpaceLow, false},
		{"starting up", `pg_dump: error: connection to server at "localhost" failed: FATAL:  the database system is starting up`, FailureBackupFailed, true},
		{"refused", `pg_dump: error: connection to server at "localhost" (127.0.0.1), port 5432 failed: Connection refused`, FailureBackupFailed, true},
		{"unknown", `pg_dump: error: query failed: something odd`, FailureBackupFailed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{handle: func(cmd dbexec.Command, stdin []byte, stdout io.Writer) ([]byte, error) {
				return []byte(tc.stderr), &mockError{msg: "exit status 1"}
			}}
			e := newTestContainerBackupExecutor(t, dockerAnswers("localhost", true, true), runner)
			result := e.ExecuteBackup(context.Background(), "payram", BackupMeta{FromVersion: "1.7.0", TargetVersion: "1.8.0"})
			if result.Success {
				t.Fatal("expected failure")
			}
			if result.FailureCode != tc.code || result.Retryable != tc.retryable {
				t.Errorf("got code=%s retryable=%v, want %s/%v (%s)", result.FailureCode, result.Retryable, tc.code, tc.retryable, result.ErrorMessage)
			}
			entries, _ := os.ReadDir(e.BackupDir)
			if len(entries) != 0 {
				t.Errorf("a failed backup must leave nothing, found %d files", len(entries))
			}
		})
	}
}

// blockingRunner never finishes a command before its context ends.
type blockingRunner struct{}

func (blockingRunner) Run(ctx context.Context, cmd dbexec.Command, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestExecuteBackup_TimeoutIsNotRetryable(t *testing.T) {
	e := newTestContainerBackupExecutor(t, dockerAnswers("localhost", true, true), blockingRunner{})
	e.BackupTimeout = 50 * time.Millisecond

	result := e.ExecuteBackup(context.Background(), "payram", BackupMeta{FromVersion: "1.7.0", TargetVersion: "1.8.0"})
	if result.Success || result.FailureCode != FailureBackupTimeout || result.Retryable {
		t.Fatalf("got success=%v code=%s retryable=%v", result.Success, result.FailureCode, result.Retryable)
	}
	entries, _ := os.ReadDir(e.BackupDir)
	if len(entries) != 0 {
		t.Errorf("a timed-out backup must leave nothing, found %d files", len(entries))
	}
}

func TestExecuteBackup_DaemonDownIsNotRetryable(t *testing.T) {
	e := newTestContainerBackupExecutor(t, dockerAnswers("localhost", true, false), &fakeRunner{})
	result := e.ExecuteBackup(context.Background(), "payram", BackupMeta{})
	if result.Success || result.FailureCode != FailureDockerDaemonDown || result.Retryable {
		t.Fatalf("got %+v", result)
	}
}

func TestCheckBackupReady_HealthySetupChangesNothing(t *testing.T) {
	runner := &fakeRunner{handle: postgresTools(nil)}
	e := newTestContainerBackupExecutor(t, dockerAnswers("localhost", true, true), runner)

	if err := e.CheckBackupReady(context.Background(), "payram"); err != nil {
		t.Fatalf("CheckBackupReady: %v", err)
	}
	for _, step := range runner.steps() {
		switch step {
		case "pg_dump --version", "pg_restore --version", "psql --version", "psql ping":
		default:
			t.Errorf("readiness may only ask for versions and SELECT 1, ran %q", step)
		}
	}
	if len(runner.callsOf("psql ping")) != 1 {
		t.Errorf("expected one SELECT 1, got %v", runner.steps())
	}
	if _, err := os.Stat(e.BackupDir); !os.IsNotExist(err) {
		t.Error("the readiness check must not create the backup directory")
	}
	runner.assertNoSecretInArgs(t, "s3cret-pw")
}

func TestCheckBackupReady_ExplainsWhatToFix(t *testing.T) {
	missingDump := func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (bool, []byte, error) {
		if step == "pg_dump --version" {
			return true, []byte(`exec: "pg_dump": executable file not found in $PATH`), &mockError{msg: "exit status 127"}
		}
		return false, nil, nil
	}
	badPassword := func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (bool, []byte, error) {
		if step == "psql ping" {
			return true, []byte(`psql: error: FATAL:  password authentication failed for user "payram"`), &mockError{msg: "exit status 2"}
		}
		return false, nil, nil
	}
	cases := []struct {
		name     string
		docker   *mockExecutor
		override func(string, dbexec.Command, []byte, io.Writer) (bool, []byte, error)
		want     string
	}{
		{"daemon down", dockerAnswers("localhost", true, false), nil, "Start Docker"},
		{"container stopped", dockerAnswers("localhost", false, true), nil, "is not running"},
		{"pg_dump missing", dockerAnswers("localhost", true, true), missingDump, "pg_dump is not installed inside container payram"},
		{"bad password", dockerAnswers("localhost", true, true), badPassword, "did not answer SELECT 1 as user payram"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestContainerBackupExecutor(t, tc.docker, &fakeRunner{handle: postgresTools(tc.override)})
			err := e.CheckBackupReady(context.Background(), "payram")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckDirWritable(t *testing.T) {
	base := t.TempDir()
	if err := checkDirWritable(filepath.Join(base, "not", "yet", "there")); err != nil {
		t.Errorf("a creatable directory must pass: %v", err)
	}
	file := filepath.Join(base, "file")
	os.WriteFile(file, []byte("x"), 0600)
	if err := checkDirWritable(file); err == nil {
		t.Error("a file is not a backup directory")
	}
	if err := checkDirWritable(""); err == nil {
		t.Error("an empty path must fail")
	}
}

func TestClassifyBackupError_ToolMissingCode(t *testing.T) {
	code, retryable := classifyBackupError(&dbexec.DBError{Code: dbexec.ErrCodeToolMissing, Message: "pg_dump is not installed"})
	if code != FailureBackupFailed || retryable {
		t.Fatalf("got %s/%v", code, retryable)
	}
	if code, retryable := classifyBackupError(errors.New("could not connect to server")); code != FailureBackupFailed || !retryable {
		t.Fatalf("connection failure should be retryable, got %s/%v", code, retryable)
	}
}

func TestDockerInspector_NotFoundIgnoresCase(t *testing.T) {
	for _, output := range []string{
		"Error: No such object: payram",
		"error: no such object: payram",
		"Error response from daemon: No such container: payram",
	} {
		missing := &mockExecutor{executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			return []byte(output), &mockError{msg: "exit status 1"}
		}}
		inspector := NewDockerInspector("docker", missing)
		exists, err := inspector.ContainerExists(context.Background(), "payram")
		if err != nil || exists {
			t.Errorf("%q: ContainerExists = %v, %v; want false, nil", output, exists, err)
		}
		running, err := inspector.ContainerRunning(context.Background(), "payram")
		if err != nil || running {
			t.Errorf("%q: ContainerRunning = %v, %v; want false, nil", output, running, err)
		}
		if _, err := inspector.GetContainerEnv(context.Background(), "payram"); err == nil || !strings.Contains(err.Error(), "container not found") {
			t.Errorf("%q: GetContainerEnv error = %v", output, err)
		}
	}

	daemonDown := &mockExecutor{executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		return []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), &mockError{msg: "exit status 1"}
	}}
	if _, err := NewDockerInspector("docker", daemonDown).ContainerExists(context.Background(), "payram"); err == nil {
		t.Error("a daemon failure must not be reported as a missing container")
	}
}
