package http

import (
	"context"
	"fmt"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/policy"
)

// UpgradePlan represents the result of upgrade planning (read-only validation).
type UpgradePlan struct {
	State           jobs.JobState      `json:"state"`
	Mode            jobs.JobMode       `json:"mode"`
	RequestedTarget string             `json:"requested_target"`
	ResolvedTarget  string             `json:"resolved_target"`
	FailureCode     string             `json:"failure_code,omitempty"`
	Message         string             `json:"message"`
	Manifest        *manifest.Manifest `json:"manifest,omitempty"`

	// Internal fields (not serialized)
	policyData *policy.Policy
}

// PlanUpgrade performs read-only validation and target resolution.
// It does NOT:
// - Create or mutate job state
// - Call docker commands
// - Append to logs
// - Persist anything to disk
//
// It only:
// - Fetches policy (read-only HTTP)
// - Fetches manifest (read-only HTTP)
// - Validates policy constraints
// - Resolves target version
func (s *Server) PlanUpgrade(ctx context.Context, mode jobs.JobMode, requestedTarget string) *UpgradePlan {
	plan := &UpgradePlan{
		Mode:            mode,
		RequestedTarget: requestedTarget,
		State:           jobs.JobStatePolicyFetching,
	}

	// Step 1: Fetch policy
	policyClient := policy.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
	policyCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel()

	policyData, err := policyClient.Fetch(policyCtx, s.config.PolicyURL)
	if err != nil {
		if mode == jobs.JobModeDashboard {
			// DASHBOARD mode: policy fetch failure is fatal
			plan.State = jobs.JobStateFailed
			if err == policy.ErrInvalidJSON {
				plan.FailureCode = "POLICY_INVALID_JSON"
			} else {
				plan.FailureCode = "POLICY_FETCH_FAILED"
			}
			plan.Message = fmt.Sprintf("Failed to fetch policy: %v", err)
			return plan
		}
		// MANUAL mode: continue without policy
	} else {
		plan.policyData = policyData

		// Check for breakpoints (DASHBOARD mode only)
		if mode == jobs.JobModeDashboard {
			for _, breakpoint := range policyData.Breakpoints {
				if breakpoint.Version == requestedTarget {
					// Breakpoint hit - manual upgrade required
					plan.State = jobs.JobStateFailed
					plan.FailureCode = "MANUAL_UPGRADE_REQUIRED"
					plan.Message = fmt.Sprintf("%s %s", breakpoint.Reason, breakpoint.Docs)
					return plan
				}
			}
		}
	}

	// Step 2: Fetch manifest
	plan.State = jobs.JobStateManifestFetching
	manifestClient := manifest.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
	manifestCtx, cancel2 := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel2()

	manifestData, err := manifestClient.Fetch(manifestCtx, s.config.RuntimeManifestURL)
	if err != nil {
		// Manifest fetch failure is fatal for both modes
		plan.State = jobs.JobStateFailed
		if err == manifest.ErrInvalidJSON {
			plan.FailureCode = "MANIFEST_INVALID_JSON"
		} else {
			plan.FailureCode = "MANIFEST_FETCH_FAILED"
		}
		plan.Message = fmt.Sprintf("Failed to fetch manifest: %v", err)
		return plan
	}

	plan.Manifest = manifestData

	// Apply IMAGE_REPO_OVERRIDE if configured (for testing with dummy repos)
	if s.config.ImageRepoOverride != "" {
		plan.Manifest.Image.Repo = s.config.ImageRepoOverride
	}

	// Step 3: Resolve target
	plan.ResolvedTarget = requestedTarget
	plan.State = jobs.JobStateReady
	plan.Message = "Upgrade plan validated successfully"

	return plan
}
