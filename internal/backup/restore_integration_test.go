//go:build integration

package backup

// Integration tests for backup and restore against a real PostgreSQL in
// Docker (postgres:17-alpine stands in for the PayRam container with its
// bundled database). Run with:
//
//	go test -tags integration -run Integration -v ./internal/backup/

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
)

const (
	itImage    = "postgres:17-alpine"
	itDB       = "payram"
	itUser     = "payram"
	itPassword = "it-pass-Zq9"
)

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(format string, v ...interface{}) { l.t.Logf(format, v...) }

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func uniqueName(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// startPostgres runs a disposable PostgreSQL container that looks like the
// PayRam container to the updater (POSTGRES_HOST=localhost in its env).
func startPostgres(t *testing.T, image string, publish bool) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := uniqueName("payram-updater-it")
	args := []string{"run", "-d", "--name", name,
		"-e", "POSTGRES_DB=" + itDB, "-e", "POSTGRES_USER=" + itUser, "-e", "POSTGRES_PASSWORD=" + itPassword,
		"-e", "POSTGRES_HOST=localhost", "-e", "POSTGRES_PORT=5432"}
	if publish {
		args = append(args, "-p", "127.0.0.1::5432")
	}
	args = append(args, image)
	dockerOut(t, args...)
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	return name
}

// psqlIn runs SQL inside the container over the local socket.
func psqlIn(t *testing.T, container, sql string) string {
	t.Helper()
	return dockerOut(t, "exec", container, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", itUser, "-d", itDB, "-At", "-c", sql)
}

func seedPaymentsDatabase(t *testing.T, container string) {
	t.Helper()
	psqlIn(t, container, `
CREATE TABLE migrations (id text PRIMARY KEY);
INSERT INTO migrations VALUES ('202601010000'), ('202602010000'), ('202603010000');
CREATE TABLE payments (id bigserial PRIMARY KEY, amount numeric(38,18) NOT NULL, note text);
INSERT INTO payments (amount, note) SELECT g * 1.5, 'payment ' || g FROM generate_series(1, 3000) g;`)
}

// fingerprint captures everything a restore must bring back or leave alone.
func fingerprint(t *testing.T, container string) string {
	t.Helper()
	return psqlIn(t, container, `
SELECT (SELECT string_agg(table_name || '.' || column_name, ',' ORDER BY table_name, column_name)
          FROM information_schema.columns WHERE table_schema = 'public')
    || ' | rows=' || (SELECT count(*) FROM payments)
    || ' sum=' || (SELECT coalesce(sum(amount), 0) FROM payments)
    || ' seq=' || (SELECT last_value FROM payments_id_seq)
    || ' migrations=' || (SELECT count(*) FROM migrations)`)
}

func changeAfterBackup(t *testing.T, container string) {
	t.Helper()
	psqlIn(t, container, `
INSERT INTO payments (amount, note) SELECT 9, 'after backup ' || g FROM generate_series(1, 5) g;
ALTER TABLE payments ADD COLUMN extra int;
CREATE TABLE created_after_backup (x int);
INSERT INTO migrations VALUES ('209901010000');`)
}

func newIntegrationManager(t *testing.T, container string) (*Manager, *ContainerBackupExecutor) {
	t.Helper()
	t.Setenv("POSTGRES_HOST", "") // never pick up an external database from the caller's shell
	base := t.TempDir()
	cfg := Config{
		Dir:                 filepath.Join(base, "backups"),
		Retention:           10,
		TargetContainerName: container,
		StateDir:            filepath.Join(base, "state"),
	}
	mgr := NewManager(cfg, &RealExecutor{}, testLogger{t})
	upgradeBackups := NewContainerBackupExecutor("docker", "pg_dump", cfg.Dir, testLogger{t})
	upgradeBackups.BackupTimeout = 2 * time.Minute
	return mgr, upgradeBackups
}

func waitForDatabase(t *testing.T, mgr *Manager, container string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := mgr.WaitForDatabase(ctx, container, 90*time.Second); err != nil {
		t.Fatalf("database never became ready: %v", err)
	}
	// The image's init phase runs a socket-only server and then restarts;
	// wait until the final server answers over TCP as the updater connects.
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := exec.Command("docker", "exec", "-e", "PGPASSWORD="+itPassword, container,
			"psql", "-X", "-h", "localhost", "-U", itUser, "-d", itDB, "-c", "SELECT 1").Run()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("database not answering over TCP: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func truncatedCopy(t *testing.T, src, dst string, cut int64) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) <= cut {
		t.Fatalf("backup too small to truncate (%d bytes)", len(data))
	}
	if err := os.WriteFile(dst, data[:int64(len(data))-cut], 0600); err != nil {
		t.Fatal(err)
	}
}

func assertUnchangedRestoreError(t *testing.T, err error) {
	t.Helper()
	var restoreErr *dbexec.RestoreError
	if !errors.As(err, &restoreErr) || !restoreErr.Unchanged {
		t.Fatalf("expected a RestoreError saying the database was not changed, got %v", err)
	}
	if !strings.HasPrefix(restoreErr.Error(), "Restore failed and was rolled back: the database was not changed.") {
		t.Errorf("message: %v", restoreErr)
	}
}

func TestIntegration_RestoreBundledDatabase(t *testing.T) {
	container := startPostgres(t, itImage, false)
	mgr, upgradeBackups := newIntegrationManager(t, container)
	ctx := context.Background()
	waitForDatabase(t, mgr, container)
	seedPaymentsDatabase(t, container)

	if err := upgradeBackups.CheckBackupReady(ctx, container); err != nil {
		t.Fatalf("CheckBackupReady: %v", err)
	}

	// The backup an upgrade takes (custom format, verified, 0600).
	result := upgradeBackups.ExecuteBackup(ctx, container, BackupMeta{FromVersion: "1.7.0", TargetVersion: "1.8.0"})
	if !result.Success {
		t.Fatalf("upgrade backup failed: %s %s", result.FailureCode, result.ErrorMessage)
	}
	if !strings.HasSuffix(result.Path, "-1.7.0-to-1.8.0.dump") {
		t.Fatalf("upgrade backup path = %s", result.Path)
	}
	if info, _ := os.Stat(result.Path); info.Mode().Perm() != 0600 {
		t.Errorf("upgrade backup mode = %o", info.Mode().Perm())
	}
	if info, _ := os.Stat(mgr.Config.Dir); info.Mode().Perm() != 0700 {
		t.Errorf("backup dir mode = %o", info.Mode().Perm())
	}
	atBackup := fingerprint(t, container)

	// A manual backup through the CLI's code path works too.
	manual, err := mgr.CreateBackup(ctx, BackupMeta{FromVersion: "manual", TargetVersion: "manual"})
	if err != nil {
		t.Fatalf("manual backup: %v", err)
	}
	if info, _ := os.Stat(manual.Path); info.Mode().Perm() != 0600 {
		t.Errorf("manual backup mode = %o", info.Mode().Perm())
	}

	// A legacy plain-SQL backup, as older updaters wrote them.
	legacySQL := filepath.Join(mgr.Config.Dir, "payram-backup-20260101-000000-1.6.0-to-1.7.0.sql")
	plain := dockerOut(t, "exec", container, "pg_dump", "-Fp", "--no-owner", "--no-acl", "-U", itUser, "-d", itDB)
	if err := os.WriteFile(legacySQL, []byte(plain+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("restore reverts rows, columns, tables and sequence", func(t *testing.T) {
		changeAfterBackup(t, container)
		if fingerprint(t, container) == atBackup {
			t.Fatal("test setup: the change after the backup did not change anything")
		}

		restored, err := mgr.RestoreBackup(ctx, result.Path, RestoreOptions{Confirmed: true})
		if err != nil {
			t.Fatalf("RestoreBackup: %v", err)
		}
		if got := fingerprint(t, container); got != atBackup {
			t.Fatalf("database differs from the backup:\n got %s\nwant %s", got, atBackup)
		}
		if got := psqlIn(t, container, "SELECT to_regclass('public.created_after_backup') IS NULL"); got != "t" {
			t.Error("table created after the backup is still there")
		}
		if got := psqlIn(t, container, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'payments' AND column_name = 'extra'"); got != "0" {
			t.Error("column added after the backup is still there")
		}
		next, _ := strconv.Atoi(psqlIn(t, container, "SELECT nextval('payments_id_seq')"))
		max, _ := strconv.Atoi(psqlIn(t, container, "SELECT max(id) FROM payments"))
		if next <= max {
			t.Fatalf("sequence nextval %d collides with existing id (max %d)", next, max)
		}
		if restored.Database.Tables != 2 || !restored.Database.HasMigrations || restored.Database.MigrationRows != 3 {
			t.Errorf("reported counts: %+v", restored.Database)
		}
		if !strings.HasSuffix(restored.SafetyBackupPath, "-pre-restore-safety.dump") {
			t.Fatalf("safety backup = %q", restored.SafetyBackupPath)
		}
		if info, err := os.Stat(restored.SafetyBackupPath); err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("safety backup missing or not 0600: %v", err)
		}

		// The safety backup undoes the restore.
		if _, err := mgr.RestoreBackup(ctx, restored.SafetyBackupPath, RestoreOptions{Confirmed: true, SkipSafetyBackup: true}); err != nil {
			t.Fatalf("restoring the safety backup: %v", err)
		}
		if got := psqlIn(t, container, "SELECT count(*) FROM payments WHERE note LIKE 'after backup%'"); got != "5" {
			t.Errorf("safety backup did not bring back the rows written after the backup (got %s)", got)
		}
	})

	t.Run("truncated dump leaves the database exactly as it was", func(t *testing.T) {
		before := fingerprint(t, container)
		truncated := filepath.Join(mgr.Config.Dir, "payram-backup-20260101-000000-1.7.0-to-1.8.0-cut.dump")
		truncatedCopy(t, result.Path, truncated, 300)

		_, err := mgr.RestoreBackup(ctx, truncated, RestoreOptions{Confirmed: true, SkipSafetyBackup: true})
		if err == nil {
			t.Fatal("restoring a truncated backup succeeded")
		}
		t.Logf("truncated dump error: %v", err)
		assertUnchangedRestoreError(t, err)
		if !strings.Contains(err.Error(), "could not be read to the end") {
			t.Errorf("expected the failure mid-stream (pg_restore --list passes on this file): %v", err)
		}
		if got := fingerprint(t, container); got != before {
			t.Fatalf("database changed:\n got %s\nwant %s", got, before)
		}
	})

	t.Run("sql backup with an error half way is rolled back", func(t *testing.T) {
		before := fingerprint(t, container)
		data, _ := os.ReadFile(legacySQL)
		marker := "\\.\n"
		i := strings.Index(string(data), marker)
		if i < 0 {
			t.Fatal("no COPY block in the plain dump")
		}
		broken := string(data[:i+len(marker)]) + "\nSELECT * FROM table_that_does_not_exist;\n" + string(data[i+len(marker):])
		brokenPath := filepath.Join(mgr.Config.Dir, "payram-backup-20260101-000000-1.6.0-to-1.7.0-broken.sql")
		os.WriteFile(brokenPath, []byte(broken), 0600)

		_, err := mgr.RestoreBackup(ctx, brokenPath, RestoreOptions{Confirmed: true, SkipSafetyBackup: true})
		if err == nil {
			t.Fatal("broken .sql restore succeeded")
		}
		t.Logf("broken sql error: %v", err)
		assertUnchangedRestoreError(t, err)
		if !strings.Contains(err.Error(), "table_that_does_not_exist") {
			t.Errorf("the first psql error should be quoted: %v", err)
		}
		if got := fingerprint(t, container); got != before {
			t.Fatalf("database changed:\n got %s\nwant %s", got, before)
		}
	})

	t.Run("sql backup cut short is refused before anything is dropped", func(t *testing.T) {
		before := fingerprint(t, container)
		cut := filepath.Join(mgr.Config.Dir, "payram-backup-20260101-000000-1.6.0-to-1.7.0-cut.sql")
		truncatedCopy(t, legacySQL, cut, 400)
		_, err := mgr.RestoreBackup(ctx, cut, RestoreOptions{Confirmed: true, SkipSafetyBackup: true})
		if err == nil || !strings.Contains(err.Error(), "damaged or incomplete") {
			t.Fatalf("expected an incomplete-backup refusal, got %v", err)
		}
		if got := fingerprint(t, container); got != before {
			t.Fatalf("database changed:\n got %s\nwant %s", got, before)
		}
	})

	t.Run("garbage file is refused before anything is dropped", func(t *testing.T) {
		before := fingerprint(t, container)
		garbage := filepath.Join(mgr.Config.Dir, "payram-backup-20260101-000000-1.7.0-to-1.8.0-garbage.dump")
		os.WriteFile(garbage, []byte("this is not a pg_dump archive"), 0600)
		_, err := mgr.RestoreBackup(ctx, garbage, RestoreOptions{Confirmed: true})
		if err == nil || !strings.Contains(err.Error(), "damaged or incomplete") {
			t.Fatalf("expected a damaged-backup refusal, got %v", err)
		}
		if got := fingerprint(t, container); got != before {
			t.Fatalf("database changed:\n got %s\nwant %s", got, before)
		}
	})

	t.Run("legacy sql backup restores", func(t *testing.T) {
		// The database still holds the post-backup changes brought back by
		// the safety backup above.
		if fingerprint(t, container) == atBackup {
			changeAfterBackup(t, container)
		}
		if _, err := mgr.RestoreBackup(ctx, legacySQL, RestoreOptions{Confirmed: true, SkipSafetyBackup: true}); err != nil {
			t.Fatalf("legacy .sql restore: %v", err)
		}
		if got := fingerprint(t, container); got != atBackup {
			t.Fatalf("database differs from the backup:\n got %s\nwant %s", got, atBackup)
		}
	})

	t.Run("user without rights to the schema gets a clear message and no change", func(t *testing.T) {
		psqlIn(t, container, "CREATE ROLE limited LOGIN PASSWORD 'limited-pw'; GRANT CONNECT ON DATABASE payram TO limited;")
		before := fingerprint(t, container)
		limited := dbexec.DBContext{
			Mode:          dbexec.DBModeInContainer,
			ContainerName: container,
			Creds:         dbexec.DBCreds{Host: "localhost", Port: "5432", Database: itDB, Username: "limited", Password: "limited-pw"},
		}
		err := dbexec.NewDockerPGExecutor(nil, testLogger{t}).Restore(ctx, limited, result.Path, "dump")
		if err == nil {
			t.Fatal("restore as a user without rights succeeded")
		}
		t.Logf("permission error: %v", err)
		assertUnchangedRestoreError(t, err)
		if !strings.Contains(err.Error(), "lacks permission") {
			t.Errorf("message should say the user lacks permission: %v", err)
		}
		if got := fingerprint(t, container); got != before {
			t.Fatalf("database changed:\n got %s\nwant %s", got, before)
		}
	})

}

var pgMajor = regexp.MustCompile(`\) (\d+)`)

// TestIntegration_RestoreExternalDatabaseWithHostTools runs the host-tools
// path against a database reached over TCP, using a server whose major
// version matches the host's pg_dump (skipped when no such image is local).
func TestIntegration_RestoreExternalDatabaseWithHostTools(t *testing.T) {
	out, err := exec.Command("pg_dump", "--version").Output()
	if err != nil {
		t.Skip("no pg_dump on this host")
	}
	m := pgMajor.FindStringSubmatch(string(out))
	if m == nil {
		t.Skipf("cannot read pg_dump version from %q", out)
	}
	image := ""
	for _, candidate := range []string{"postgres:" + m[1] + "-alpine", "postgres:" + m[1]} {
		if exec.Command("docker", "image", "inspect", candidate).Run() == nil {
			image = candidate
			break
		}
	}
	if image == "" {
		t.Skipf("no local postgres image matching host pg_dump %s", m[1])
	}

	container := startPostgres(t, image, true)
	hostPort := dockerOut(t, "port", container, "5432/tcp")
	port := hostPort[strings.LastIndex(hostPort, ":")+1:]
	db := dbexec.DBContext{
		Mode:  dbexec.DBModeExternal,
		Creds: dbexec.DBCreds{Host: "127.0.0.1", Port: port, Database: itDB, Username: itUser, Password: itPassword},
	}
	host := dbexec.NewHostPGExecutor(nil, testLogger{t})
	ctx := context.Background()
	if err := host.WaitReady(ctx, db, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for host.Ping(ctx, db) != nil {
		if time.Now().After(deadline) {
			t.Fatal("database not answering over TCP")
		}
		time.Sleep(time.Second)
	}
	seedPaymentsDatabase(t, container)

	dir := t.TempDir()
	backupPath := filepath.Join(dir, "payram-backup-20260101-000000-1.7.0-to-1.8.0.dump")
	if err := host.Dump(ctx, db, backupPath, "dump"); err != nil {
		t.Fatalf("host dump: %v", err)
	}
	if info, _ := os.Stat(backupPath); info.Mode().Perm() != 0600 {
		t.Errorf("host backup mode = %o", info.Mode().Perm())
	}
	atBackup := fingerprint(t, container)

	changeAfterBackup(t, container)
	if err := host.Restore(ctx, db, backupPath, "dump"); err != nil {
		t.Fatalf("host restore: %v", err)
	}
	if got := fingerprint(t, container); got != atBackup {
		t.Fatalf("database differs from the backup:\n got %s\nwant %s", got, atBackup)
	}

	before := fingerprint(t, container)
	truncated := filepath.Join(dir, "cut.dump")
	truncatedCopy(t, backupPath, truncated, 300)
	err = host.Restore(ctx, db, truncated, "dump")
	assertUnchangedRestoreError(t, err)
	if got := fingerprint(t, container); got != before {
		t.Fatalf("database changed:\n got %s\nwant %s", got, before)
	}
	fmt.Fprintln(os.Stderr, "host-tools restore verified against", image)
}
