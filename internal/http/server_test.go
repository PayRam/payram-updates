package http

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/jobs"
)

func TestNew(t *testing.T) {
	cfg := &config.Config{
		Port: 8080,
	}
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	server := New(cfg, jobStore)

	if server == nil {
		t.Fatal("expected server to be created, got nil")
	}

	if server.port != 8080 {
		t.Errorf("expected port %d, got %d", 8080, server.port)
	}

	if server.httpServer == nil {
		t.Fatal("expected httpServer to be created, got nil")
	}

	expectedAddr := "127.0.0.1:8080"
	if server.httpServer.Addr != expectedAddr {
		t.Errorf("expected addr %q, got %q", expectedAddr, server.httpServer.Addr)
	}

	if server.config == nil {
		t.Fatal("expected config to be set, got nil")
	}

	if server.jobStore == nil {
		t.Fatal("expected jobStore to be set, got nil")
	}
}

// TestRunAutoUpdateOnce_NeverStartsOverAnActiveJob: an update left active by
// an updater that stopped is the startup repair's; auto-update must not start
// another one on top of it.
func TestRunAutoUpdateOnce_NeverStartsOverAnActiveJob(t *testing.T) {
	store := jobs.NewStore(t.TempDir())
	interrupted := jobs.NewJob("job-interrupted", jobs.JobModeDashboard, "3.10.0")
	interrupted.State = jobs.JobStateVerifying
	if err := store.Save(interrupted); err != nil {
		t.Fatal(err)
	}
	srv := &Server{config: &config.Config{FetchTimeoutSeconds: 1, PolicyURL: "http://127.0.0.1:1/unreachable"}, jobStore: store}

	srv.runAutoUpdateOnce(context.Background())

	latest, _ := store.LoadLatest()
	if latest.JobID != "job-interrupted" {
		t.Fatalf("auto-update started %s over the interrupted update", latest.JobID)
	}
	if srv.upgradeRunning.Load() {
		t.Fatal("auto-update kept the update slot after skipping")
	}
}

// TestWaitForUpgradeSlot_WaitsOutABriefHolder: the repair must not give up
// because a request held the slot for a moment.
func TestWaitForUpgradeSlot_WaitsOutABriefHolder(t *testing.T) {
	srv := &Server{}
	if err := srv.claimUpgradeSlot(); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		srv.releaseUpgradeSlot()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !srv.waitForUpgradeSlot(ctx) {
		t.Fatal("waitForUpgradeSlot gave up while the slot was only briefly held")
	}
	if !srv.upgradeRunning.Load() {
		t.Fatal("the slot was not claimed")
	}

	srv.releaseUpgradeSlot()
	srv.shuttingDown.Store(true)
	if srv.waitForUpgradeSlot(ctx) {
		t.Fatal("a shutting-down updater must not start a repair")
	}
}

// TestSecureStoredFiles_FixesWhatOlderUpdatersLeftOpen: on start the updater
// cleans the AES key v0.2.3 wrote into the job log and makes the backup
// folder and its backups root-only, before any backup is taken.
func TestSecureStoredFiles_FixesWhatOlderUpdatersLeftOpen(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	backupDir := filepath.Join(root, "backups")
	for _, dir := range []string{stateDir, backupDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		os.Chmod(dir, 0755)
	}
	oldBackup := filepath.Join(backupDir, "payram-backup-20260501-120000-3.8.1-to-3.9.0.sql")
	if err := os.WriteFile(oldBackup, []byte("-- dump"), 0644); err != nil {
		t.Fatal(err)
	}
	store := jobs.NewStore(stateDir)
	store.AppendLog("  5. Run new container: docker run -d --name payram -e AES_KEY=6b1f0e1d2c3b4a59 payramapp/payram:3.9.0")

	srv := &Server{config: &config.Config{StateDir: stateDir, Backup: config.BackupConfig{Dir: backupDir}}, jobStore: store}
	srv.secureStoredFiles()

	logs, _ := store.ReadLogs()
	if strings.Contains(logs, "6b1f0e1d2c3b4a59") || !strings.Contains(logs, "AES_KEY=***") {
		t.Fatalf("log still carries the key: %q", logs)
	}
	// The state directory itself may be shared (its own files are 0600), so
	// it is left as it was; the backups are the updater's alone.
	for path, want := range map[string]os.FileMode{stateDir: 0755, backupDir: 0700, oldBackup: 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode = %o (%v), want %o", path, info.Mode().Perm(), err, want)
		}
	}
}

// TestWaitForDocker_KeepsTheJobActiveUntilDockerAnswers: with Docker down
// the repair must not end the interrupted job (a failed job is never repaired,
// and PayRam may be stopped); the next start repairs it.
func TestWaitForDocker_KeepsTheJobActiveUntilDockerAnswers(t *testing.T) {
	store := jobs.NewStore(t.TempDir())
	job := jobs.NewJob("job-interrupted", jobs.JobModeDashboard, "3.10.0")
	job.State = jobs.JobStateVerifying
	if err := store.Save(job); err != nil {
		t.Fatal(err)
	}
	srv := &Server{config: &config.Config{DockerBin: "false"}, jobStore: store}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if srv.waitForDocker(ctx, job) {
		t.Fatal("waitForDocker reported Docker up")
	}
	latest, _ := store.LoadLatest()
	if !latest.IsActive() {
		t.Fatalf("the job was ended (%s %s) while Docker was down", latest.State, latest.FailureCode)
	}
}
