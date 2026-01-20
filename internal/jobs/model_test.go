package jobs

import (
	"testing"
	"time"
)

func TestNewJob(t *testing.T) {
	jobID := "test-job-id"
	mode := JobModeDashboard
	target := "v1.2.3"

	job := NewJob(jobID, mode, target)

	if job.JobID != jobID {
		t.Errorf("expected JobID %q, got %q", jobID, job.JobID)
	}
	if job.Mode != mode {
		t.Errorf("expected Mode %q, got %q", mode, job.Mode)
	}
	if job.RequestedTarget != target {
		t.Errorf("expected RequestedTarget %q, got %q", target, job.RequestedTarget)
	}
	if job.State != JobStateIdle {
		t.Errorf("expected State %q, got %q", JobStateIdle, job.State)
	}
	if job.ResolvedTarget != "" {
		t.Errorf("expected ResolvedTarget to be empty, got %q", job.ResolvedTarget)
	}
	if job.FailureCode != "" {
		t.Errorf("expected FailureCode to be empty, got %q", job.FailureCode)
	}
	if job.Message != "" {
		t.Errorf("expected Message to be empty, got %q", job.Message)
	}
	if job.CreatedAt.IsZero() {
		t.Error("expected CreatedAt to be set")
	}
	if job.UpdatedAt.IsZero() {
		t.Error("expected UpdatedAt to be set")
	}
	if !job.CreatedAt.Equal(job.UpdatedAt) {
		t.Error("expected CreatedAt and UpdatedAt to be equal initially")
	}
}

func TestJobModeValues(t *testing.T) {
	tests := []struct {
		mode     JobMode
		expected string
	}{
		{JobModeDashboard, "DASHBOARD"},
		{JobModeManual, "MANUAL"},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			if string(tt.mode) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(tt.mode))
			}
		})
	}
}

func TestJobStateValues(t *testing.T) {
	tests := []struct {
		state    JobState
		expected string
	}{
		{JobStateIdle, "IDLE"},
		{JobStatePolicyFetching, "POLICY_FETCHING"},
		{JobStateManifestFetching, "MANIFEST_FETCHING"},
		{JobStateReady, "READY"},
		{JobStateFailed, "FAILED"},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if string(tt.state) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(tt.state))
			}
		})
	}
}

func TestJobModification(t *testing.T) {
	job := NewJob("test-id", JobModeManual, "v1.0.0")

	job.State = JobStateReady
	job.ResolvedTarget = "v1.0.0"
	job.Message = "Job completed successfully"
	job.UpdatedAt = time.Now().UTC()

	if job.State != JobStateReady {
		t.Errorf("expected State %q, got %q", JobStateReady, job.State)
	}
	if job.ResolvedTarget != "v1.0.0" {
		t.Errorf("expected ResolvedTarget %q, got %q", "v1.0.0", job.ResolvedTarget)
	}
	if job.Message != "Job completed successfully" {
		t.Errorf("expected Message %q, got %q", "Job completed successfully", job.Message)
	}
}

func TestJobFailedState(t *testing.T) {
	job := NewJob("test-id", JobModeDashboard, "v2.0.0")

	job.State = JobStateFailed
	job.FailureCode = "POLICY_FETCH_ERROR"
	job.Message = "Failed to fetch policy"

	if job.State != JobStateFailed {
		t.Errorf("expected State %q, got %q", JobStateFailed, job.State)
	}
	if job.FailureCode != "POLICY_FETCH_ERROR" {
		t.Errorf("expected FailureCode %q, got %q", "POLICY_FETCH_ERROR", job.FailureCode)
	}
	if job.Message != "Failed to fetch policy" {
		t.Errorf("expected Message %q, got %q", "Failed to fetch policy", job.Message)
	}
}
