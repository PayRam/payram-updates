package jobs

import (
	"time"
)

// JobMode represents how a job was initiated.
type JobMode string

const (
	JobModeDashboard JobMode = "DASHBOARD"
	JobModeManual    JobMode = "MANUAL"
)

// JobState represents the current state of a job.
type JobState string

const (
	JobStateIdle             JobState = "IDLE"
	JobStatePolicyFetching   JobState = "POLICY_FETCHING"
	JobStateManifestFetching JobState = "MANIFEST_FETCHING"
	JobStateReady            JobState = "READY"
	JobStateBackingUp        JobState = "BACKING_UP"
	JobStateExecuting        JobState = "EXECUTING"
	JobStateVerifying        JobState = "VERIFYING"
	JobStateFailed           JobState = "FAILED"
)

// Job represents an update job with its current state.
type Job struct {
	JobID           string    `json:"jobId"`
	Mode            JobMode   `json:"mode"`
	RequestedTarget string    `json:"requestedTarget"`
	ResolvedTarget  string    `json:"resolvedTarget"`
	State           JobState  `json:"state"`
	FailureCode     string    `json:"failureCode"`
	Message         string    `json:"message"`
	BackupPath      string    `json:"backupPath,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// NewJob creates a new job with the given mode and requested target.
func NewJob(jobID string, mode JobMode, requestedTarget string) *Job {
	now := time.Now().UTC()
	return &Job{
		JobID:           jobID,
		Mode:            mode,
		RequestedTarget: requestedTarget,
		State:           JobStateIdle,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}
