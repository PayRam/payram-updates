package recover

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
)

// testLogger creates a logger that discards output for testing
func testLogger() *log.Logger {
	return log.New(log.Writer(), "", 0)
}

func TestCanRecover_MigrationFailedRefused(t *testing.T) {
	// MIGRATION_FAILED must always refuse automated recovery
	canRecover, reason := CanRecover("MIGRATION_FAILED")

	if canRecover {
		t.Error("expected MIGRATION_FAILED to refuse automated recovery")
	}
	if reason == "" {
		t.Error("expected a reason for refusal")
	}
	if reason == "" || len(reason) < 10 {
		t.Errorf("expected meaningful refusal reason, got: %q", reason)
	}
}

func TestCanRecover_RetryableCodesAllowed(t *testing.T) {
	retryableCodes := []string{
		"DOCKER_PULL_FAILED",
		"POLICY_FETCH_FAILED",
		"MANIFEST_FETCH_FAILED",
		"CONCURRENCY_BLOCKED",
	}

	for _, code := range retryableCodes {
		t.Run(code, func(t *testing.T) {
			canRecover, reason := CanRecover(code)
			if !canRecover {
				t.Errorf("expected %s to allow recovery, but got refusal: %s", code, reason)
			}
		})
	}
}

func TestCanRecover_DataRiskRefused(t *testing.T) {
	// VERSION_MISMATCH has data risk - should refuse
	canRecover, reason := CanRecover("VERSION_MISMATCH")

	if canRecover {
		t.Error("expected VERSION_MISMATCH to refuse due to data risk")
	}
	if reason == "" {
		t.Error("expected a reason for refusal")
	}
}

func TestCanRecover_ManualRequiredRefused(t *testing.T) {
	// MANUAL_UPGRADE_REQUIRED should refuse
	canRecover, reason := CanRecover("MANUAL_UPGRADE_REQUIRED")

	if canRecover {
		t.Error("expected MANUAL_UPGRADE_REQUIRED to refuse")
	}
	if reason == "" {
		t.Error("expected a reason for refusal")
	}
}

func TestNewRecoverer(t *testing.T) {
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)
	runner := &dockerexec.Runner{DockerBin: "docker", Logger: testLogger()}

	recoverer := NewRecoverer(jobStore, runner, "payram-core", "http://localhost:8080")

	if recoverer == nil {
		t.Fatal("expected non-nil recoverer")
	}
	if recoverer.containerName != "payram-core" {
		t.Errorf("expected containerName payram-core, got %s", recoverer.containerName)
	}
}

func TestRecoverer_Run_NoJob(t *testing.T) {
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)
	runner := &dockerexec.Runner{DockerBin: "docker", Logger: testLogger()}
	recoverer := NewRecoverer(jobStore, runner, "payram-core", "http://localhost:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := recoverer.Run(ctx)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure when no job exists")
	}
}

func TestRecoverer_Run_JobNotFailed(t *testing.T) {
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	// Create a job that's not in failed state
	job := jobs.NewJob("test-job", jobs.JobModeDashboard, "v2.0.0")
	job.State = jobs.JobStateReady
	if err := jobStore.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	runner := &dockerexec.Runner{DockerBin: "docker", Logger: testLogger()}
	recoverer := NewRecoverer(jobStore, runner, "payram-core", "http://localhost:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := recoverer.Run(ctx)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected failure when job is not in FAILED state")
	}
	if result.Message == "" {
		t.Error("expected a message explaining the failure")
	}
}

func TestRecoverer_Run_MigrationFailedRefused(t *testing.T) {
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	// Create a failed job with MIGRATION_FAILED
	job := jobs.NewJob("test-job", jobs.JobModeDashboard, "v2.0.0")
	job.State = jobs.JobStateFailed
	job.FailureCode = "MIGRATION_FAILED"
	job.Message = "migration failed"
	if err := jobStore.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	runner := &dockerexec.Runner{DockerBin: "docker", Logger: testLogger()}
	recoverer := NewRecoverer(jobStore, runner, "payram-core", "http://localhost:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := recoverer.Run(ctx)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Error("expected MIGRATION_FAILED to refuse automated recovery")
	}
	if result.Refusals == "" {
		t.Error("expected refusal reason to be provided")
	}
	if result.Code != "MIGRATION_FAILED" {
		t.Errorf("expected code MIGRATION_FAILED, got %s", result.Code)
	}
}

func TestRecoverer_Run_DockerPullFailed(t *testing.T) {
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	// Create a failed job with DOCKER_PULL_FAILED (retryable)
	job := jobs.NewJob("test-job", jobs.JobModeDashboard, "v2.0.0")
	job.State = jobs.JobStateFailed
	job.FailureCode = "DOCKER_PULL_FAILED"
	job.Message = "network timeout"
	if err := jobStore.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	// Use a mock runner that doesn't actually call docker
	runner := &dockerexec.Runner{DockerBin: "echo", Logger: testLogger()}
	recoverer := NewRecoverer(jobStore, runner, "payram-core", "http://localhost:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := recoverer.Run(ctx)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected DOCKER_PULL_FAILED recovery to succeed, got: %s", result.Message)
	}
	if result.Action == "" {
		t.Error("expected recovery action to be set")
	}
	if result.Code != "DOCKER_PULL_FAILED" {
		t.Errorf("expected code DOCKER_PULL_FAILED, got %s", result.Code)
	}
}

func TestRecoverer_Run_ConcurrencyBlocked(t *testing.T) {
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	// Create a failed job with CONCURRENCY_BLOCKED
	job := jobs.NewJob("test-job", jobs.JobModeDashboard, "v2.0.0")
	job.State = jobs.JobStateFailed
	job.FailureCode = "CONCURRENCY_BLOCKED"
	job.Message = "another upgrade in progress"
	if err := jobStore.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	runner := &dockerexec.Runner{DockerBin: "echo", Logger: testLogger()}
	recoverer := NewRecoverer(jobStore, runner, "payram-core", "http://localhost:8080")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := recoverer.Run(ctx)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Errorf("expected CONCURRENCY_BLOCKED recovery to succeed, got: %s", result.Message)
	}
}

func TestRecoveryResult_Structure(t *testing.T) {
	result := RecoveryResult{
		Success:  true,
		Message:  "Recovery completed",
		Action:   "restart_container",
		Code:     "DOCKER_ERROR",
		Refusals: "",
	}

	if !result.Success {
		t.Error("expected success to be true")
	}
	if result.Action != "restart_container" {
		t.Errorf("expected action restart_container, got %s", result.Action)
	}
}
