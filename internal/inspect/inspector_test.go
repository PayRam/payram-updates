package inspect

import (
	"context"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/recovery"
)

func TestNewInspector(t *testing.T) {
	jobStore := jobs.NewStore(t.TempDir())

	inspector := NewInspector(
		jobStore,
		"/usr/bin/docker",
		"payram-core",
		"http://localhost:8080",
		"http://example.com/policy.json",
		"http://example.com/manifest.json",
		false, // debugMode
	)

	if inspector == nil {
		t.Fatal("expected non-nil inspector")
	}
	if inspector.dockerBin != "/usr/bin/docker" {
		t.Errorf("expected dockerBin to be /usr/bin/docker, got %s", inspector.dockerBin)
	}
	if inspector.containerName != "payram-core" {
		t.Errorf("expected containerName to be payram-core, got %s", inspector.containerName)
	}
}

func TestInspector_Run_NoJobOK(t *testing.T) {
	// Test when there's no previous job - should be OK
	jobStore := jobs.NewStore(t.TempDir())

	inspector := NewInspector(
		jobStore,
		"docker", // Will fail, but that's OK for this test
		"payram-core",
		"http://localhost:8080",
		"http://example.com/policy.json",
		"http://example.com/manifest.json",
		false, // debugMode
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := inspector.Run(ctx)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Verify checks map has expected keys
	expectedChecks := []string{"lastJob", "dockerDaemon", "container", "policy", "manifest", "health"}
	for _, check := range expectedChecks {
		if _, ok := result.Checks[check]; !ok {
			t.Errorf("expected check %q in result.Checks", check)
		}
	}

	// last_job should be OK when no job exists
	if result.Checks["lastJob"].Status != "OK" {
		t.Errorf("expected lastJob status to be OK when no job exists, got %s", result.Checks["lastJob"].Status)
	}
}

func TestInspector_Run_FailedJobWithPlaybook(t *testing.T) {
	// Test when there's a failed job - should include recovery playbook
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	// Create a failed job with MIGRATION_FAILED error code
	job := jobs.NewJob("test-job-1", jobs.JobModeDashboard, "v2.0.0")
	job.State = jobs.JobStateFailed
	job.FailureCode = "MIGRATION_FAILED"
	job.Message = "migration failed during upgrade"
	if err := jobStore.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	inspector := NewInspector(
		jobStore,
		"docker",
		"payram-core",
		"http://localhost:8080",
		"http://example.com/policy.json",
		"http://example.com/manifest.json",
		false, // debugMode
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := inspector.Run(ctx)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Should be BROKEN state due to MIGRATION_FAILED
	if result.OverallState != StateBroken {
		t.Errorf("expected BROKEN state for MIGRATION_FAILED, got %s", result.OverallState)
	}

	// Should have the job attached
	if result.LastJob == nil {
		t.Error("expected LastJob to be attached")
	}

	// Should have recovery playbook
	if result.RecoveryPlaybook == nil {
		t.Fatal("expected RecoveryPlaybook to be attached for failed job")
	}

	if result.RecoveryPlaybook.Code != "MIGRATION_FAILED" {
		t.Errorf("expected playbook code MIGRATION_FAILED, got %s", result.RecoveryPlaybook.Code)
	}

	// MIGRATION_FAILED has data risk
	if result.RecoveryPlaybook.DataRisk == recovery.DataRiskNone {
		t.Error("expected MIGRATION_FAILED to have data risk")
	}

	// last_job check should be FAILED
	if result.Checks["lastJob"].Status != "FAILED" {
		t.Errorf("expected lastJob check status to be FAILED, got %s", result.Checks["lastJob"].Status)
	}

	// Should have issues
	if len(result.Issues) == 0 {
		t.Error("expected at least one issue for failed job")
	}

	// Should have recommendations
	if len(result.Recommendations) == 0 {
		t.Error("expected at least one recommendation for failed job")
	}
}

func TestInspector_Run_CompletedJobOK(t *testing.T) {
	// Test when there's a completed job - lastJob check should be OK
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	// Create a completed job (JobStateReady is the success state)
	job := jobs.NewJob("test-job-2", jobs.JobModeDashboard, "v2.0.0")
	job.State = jobs.JobStateReady
	if err := jobStore.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	inspector := NewInspector(
		jobStore,
		"docker",
		"payram-core",
		"http://localhost:8080",
		"http://example.com/policy.json",
		"http://example.com/manifest.json",
		false, // debugMode
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := inspector.Run(ctx)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// lastJob check should be OK
	if result.Checks["lastJob"].Status != "OK" {
		t.Errorf("expected lastJob check status to be OK for completed job, got %s", result.Checks["lastJob"].Status)
	}

	// Should have the job attached
	if result.LastJob == nil {
		t.Error("expected LastJob to be attached")
	}

	// Should NOT have recovery playbook for completed job
	if result.RecoveryPlaybook != nil {
		t.Error("expected no RecoveryPlaybook for completed job")
	}
}

func TestInspector_Run_RetryableErrorDegraded(t *testing.T) {
	// Test when there's a retryable error - should still be BROKEN (any failed upgrade is BROKEN)
	tmpDir := t.TempDir()
	jobStore := jobs.NewStore(tmpDir)

	// Create a failed job with retryable error
	job := jobs.NewJob("test-job-3", jobs.JobModeDashboard, "v2.0.0")
	job.State = jobs.JobStateFailed
	job.FailureCode = "DOCKER_PULL_FAILED"
	job.Message = "network timeout"
	if err := jobStore.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	inspector := NewInspector(
		jobStore,
		"docker",
		"payram-core",
		"http://localhost:8080",
		"http://example.com/policy.json",
		"http://example.com/manifest.json",
		false, // debugMode
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result := inspector.Run(ctx)

	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Any failed upgrade results in BROKEN state (regardless of retryability)
	if result.OverallState != StateBroken {
		t.Errorf("expected BROKEN state for failed upgrade, got %s", result.OverallState)
	}

	// Should have recovery playbook
	if result.RecoveryPlaybook == nil {
		t.Fatal("expected RecoveryPlaybook for failed job")
	}

	if result.RecoveryPlaybook.Code != "DOCKER_PULL_FAILED" {
		t.Errorf("expected playbook code DOCKER_PULL_FAILED, got %s", result.RecoveryPlaybook.Code)
	}

	// Verify DOCKER_PULL_FAILED is retryable (can check via recovery package)
	if !recovery.IsRetryable("DOCKER_PULL_FAILED") {
		t.Error("expected DOCKER_PULL_FAILED to be retryable")
	}
}

func TestOverallStateValues(t *testing.T) {
	tests := []struct {
		state OverallState
		want  string
	}{
		{StateOK, "OK"},
		{StateDegraded, "DEGRADED"},
		{StateBroken, "BROKEN"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.state) != tt.want {
				t.Errorf("expected %s, got %s", tt.want, tt.state)
			}
		})
	}
}

func TestCheckResultStatus(t *testing.T) {
	// Verify CheckResult structure works correctly
	check := CheckResult{
		Status:  "OK",
		Message: "All good",
	}

	if check.Status != "OK" {
		t.Errorf("expected status OK, got %s", check.Status)
	}
	if check.Message != "All good" {
		t.Errorf("expected message 'All good', got %s", check.Message)
	}
}

func TestIssueStructure(t *testing.T) {
	issue := Issue{
		Component:   "docker",
		Description: "Container not running",
		Severity:    "CRITICAL",
	}

	if issue.Component != "docker" {
		t.Errorf("expected component docker, got %s", issue.Component)
	}
	if issue.Severity != "CRITICAL" {
		t.Errorf("expected severity CRITICAL, got %s", issue.Severity)
	}
}

func TestRecommendationStructure(t *testing.T) {
	rec := Recommendation{
		Action:      "Restart container",
		Description: "Run: docker start payram-core",
		Priority:    1,
	}

	if rec.Priority != 1 {
		t.Errorf("expected priority 1, got %d", rec.Priority)
	}
	if rec.Action != "Restart container" {
		t.Errorf("expected action 'Restart container', got %s", rec.Action)
	}
}
