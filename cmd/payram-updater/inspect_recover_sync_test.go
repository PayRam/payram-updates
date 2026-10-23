package main

import (
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
)

func TestSyncRefusal(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	job := func(state jobs.JobState, silentFor time.Duration) *jobs.Job {
		j := jobs.NewJob("job-1", jobs.JobModeDashboard, "1.9.0")
		j.State = state
		j.UpdatedAt = now.Add(-silentFor)
		return j
	}

	if got := syncRefusal(nil, now); got != "" {
		t.Errorf("no job: want no refusal, got %q", got)
	}
	for _, state := range []jobs.JobState{jobs.JobStateReady, jobs.JobStateFailed, jobs.JobStateIdle} {
		if got := syncRefusal(job(state, time.Hour), now); got != "" {
			t.Errorf("%s job: want no refusal, got %q", state, got)
		}
	}

	// A running update: sync would hide it and let a second update start.
	for _, state := range []jobs.JobState{jobs.JobStatePolicyFetching, jobs.JobStateBackingUp, jobs.JobStateExecuting, jobs.JobStateVerifying} {
		got := syncRefusal(job(state, 20*time.Second), now)
		if !strings.Contains(got, "running right now") || !strings.Contains(got, "payram-updater status") {
			t.Errorf("fresh %s job: want wait refusal, got %q", state, got)
		}
	}

	// An interrupted update: the restarted updater puts PayRam back; sync must not erase that.
	got := syncRefusal(job(jobs.JobStateExecuting, 10*time.Minute), now)
	if !strings.Contains(got, "sudo systemctl restart payram-updater") {
		t.Errorf("stale job: want restart-the-updater refusal, got %q", got)
	}
}

func TestSyncedJob_KeepsBackupAndSwitchBookkeeping(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	existing := jobs.NewJob("job-1", jobs.JobModeDashboard, "1.9.0")
	existing.State = jobs.JobStateFailed
	existing.FailureCode = "HEALTHCHECK_FAILED"
	existing.ResolvedTarget = "1.9.0"
	existing.BackupPath = "/var/lib/payram/backups/payram-backup-1.8.0-to-1.9.0.dump"
	existing.ContainerName = "payram-shop"
	existing.CoreBaseURL = "http://127.0.0.1:8080"
	existing.FromVersion = "1.8.0"
	existing.SwitchStage = jobs.SwitchStageVerified
	existing.NextContainer = "payram-shop-next"
	existing.PreviousContainer = "payram-shop-previous"
	existing.PreviousImage = "payramapp/payram:1.8.0"
	existing.PreviousRestartPolicy = "unless-stopped"
	existing.QuiescedPrograms = []string{"payram-core"}

	synced := syncedJob(existing, "1.9.0", "discovered", "https://127.0.0.1", now)

	if synced.State != jobs.JobStateReady || synced.ResolvedTarget != "1.9.0" || synced.FailureCode != "" {
		t.Errorf("want READY on 1.9.0 without failure, got %s %s %q", synced.State, synced.ResolvedTarget, synced.FailureCode)
	}
	if synced.BackupPath != existing.BackupPath {
		t.Errorf("BackupPath = %q, want %q", synced.BackupPath, existing.BackupPath)
	}
	if synced.ContainerName != "payram-shop" {
		t.Errorf("ContainerName = %q, want the job's", synced.ContainerName)
	}
	if synced.CoreBaseURL != "https://127.0.0.1" {
		t.Errorf("CoreBaseURL = %q, want the URL sync just verified", synced.CoreBaseURL)
	}
	if synced.FromVersion != "1.8.0" || synced.SwitchStage != jobs.SwitchStageVerified ||
		synced.NextContainer != "payram-shop-next" || synced.PreviousContainer != "payram-shop-previous" ||
		synced.PreviousImage != "payramapp/payram:1.8.0" || synced.PreviousRestartPolicy != "unless-stopped" ||
		len(synced.QuiescedPrograms) != 1 {
		t.Errorf("switch bookkeeping lost: %+v", synced)
	}
	if !strings.Contains(synced.Message, "was 1.9.0, now 1.9.0") {
		t.Errorf("message = %q", synced.Message)
	}
}

func TestSyncedJob_WithoutPreviousJob(t *testing.T) {
	synced := syncedJob(nil, "1.9.0", "payram", "https://127.0.0.1", time.Now())
	if synced.ContainerName != "payram" || synced.BackupPath != "" || !strings.Contains(synced.Message, "was unknown") {
		t.Errorf("unexpected synced job: %+v", synced)
	}
}
