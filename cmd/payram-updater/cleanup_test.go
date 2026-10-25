package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/payram/payram-updater/internal/jobs"
)

func TestCheckCleanupTarget_RefusesDangerousPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	link := filepath.Join(t.TempDir(), "to-root")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"":   "not configured",
		"/":  "root directory",
		home: "home directory",
		link: "root directory",
	}
	// As root, HOME is /root and its parent is the root directory, which is
	// refused as such (checked above).
	if parent := filepath.Dir(home); parent != "/" {
		cases[parent] = "home directory"
	}
	for target, want := range cases {
		err := checkCleanupTarget(target, target)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("checkCleanupTarget(%q) = %v, want %q", target, err, want)
		}
	}
	if err := checkCleanupTarget("/var/lib/payram/backups", "/var/lib/payram/other"); err == nil {
		t.Error("a directory other than the configured one must be refused")
	}
	if err := checkCleanupTarget(filepath.Join(t.TempDir(), "backups"), ""); err == nil {
		t.Error("an unconfigured directory must be refused")
	}
	ok := filepath.Join(t.TempDir(), "backups")
	if err := checkCleanupTarget(ok, ok); err != nil {
		t.Errorf("an ordinary backup directory must be accepted: %v", err)
	}
}

func TestRemoveBackupFiles_RemovesOnlyBackups(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"payram-backup-20260101-000000-1.0.0-to-1.1.0.dump",
		"payram-backup-20260102-000000-1.1.0-to-1.2.0.sql",
		"payram-backup-20260103-000000-1.2.0-to-1.3.0.dump.partial",
		"postgres.conf",
	} {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0600)
	}
	os.Mkdir(filepath.Join(dir, "db"), 0700)

	removed, err := removeBackupFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 {
		t.Errorf("removed %d, want 3", removed)
	}
	for _, kept := range []string{"postgres.conf", "db"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s must be kept: %v", kept, err)
		}
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0700 {
		t.Errorf("backup dir mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestRemoveUpdaterState_KeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "jobs", "latest"), 0700)
	os.WriteFile(filepath.Join(dir, "jobs", "latest", "status.json"), []byte("{}"), 0600)
	os.WriteFile(filepath.Join(dir, "history.jsonl"), []byte("{}"), 0600)
	os.WriteFile(filepath.Join(dir, "updater-config.json"), []byte("{}"), 0600)
	os.MkdirAll(filepath.Join(dir, "backups"), 0700)
	os.MkdirAll(filepath.Join(dir, "db", "postgres"), 0700)

	if err := removeUpdaterState(dir); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"jobs", "history.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed", gone)
		}
	}
	for _, kept := range []string{"updater-config.json", "backups", filepath.Join("db", "postgres")} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s must be kept: %v", kept, err)
		}
	}
}

func TestCleanupBlockedByFailedUpdate(t *testing.T) {
	stateDir := t.TempDir()
	backupDir := t.TempDir()
	recovery := filepath.Join(backupDir, "payram-backup-20260101-000000-1.7.0-to-1.8.0.dump")
	os.WriteFile(recovery, []byte("x"), 0600)

	if reason := cleanupBlockedByFailedUpdate(stateDir, backupDir, "backups"); reason != "" {
		t.Fatalf("no job yet, got %q", reason)
	}

	job := jobs.NewJob("job-1", jobs.JobModeManual, "1.8.0")
	job.State = jobs.JobStateFailed
	job.BackupPath = recovery
	if err := jobs.NewStore(stateDir).Save(job); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"backups", "state"} {
		reason := cleanupBlockedByFailedUpdate(stateDir, backupDir, sub)
		if !strings.Contains(reason, recovery) || !strings.Contains(reason, "--force") {
			t.Errorf("%s cleanup must be refused naming the backup and --force, got %q", sub, reason)
		}
	}
	if reason := cleanupBlockedByFailedUpdate(stateDir, t.TempDir(), "backups"); reason != "" {
		t.Errorf("a backup outside the cleaned directory is not at risk, got %q", reason)
	}

	job.State = jobs.JobStateReady
	jobs.NewStore(stateDir).Save(job)
	if reason := cleanupBlockedByFailedUpdate(stateDir, backupDir, "backups"); reason != "" {
		t.Errorf("a finished job does not block cleanup, got %q", reason)
	}
}
