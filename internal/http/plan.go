package http

import (
	"context"
	"fmt"
	"strings"
	"time"

	goversion "github.com/hashicorp/go-version"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/policy"
)

// UpgradePlan represents the result of upgrade planning (read-only validation).
type UpgradePlan struct {
	State           jobs.JobState      `json:"state"`
	Mode            jobs.JobMode       `json:"mode"`
	RequestedTarget string             `json:"requestedTarget"`
	ResolvedTarget  string             `json:"resolvedTarget"`
	FailureCode     string             `json:"failureCode,omitempty"`
	Message         string             `json:"message"`
	Manifest        *manifest.Manifest `json:"manifest,omitempty"`
	ArchSupport     map[string]string  `json:"-"` // arch variant min versions, not serialized

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
//
// currentVersion is the running version of the core container. When non-empty it
// enables breakpoint-aware target capping: if the requested upgrade would cross a
// breakpoint, the target is automatically capped at the highest release below that
// breakpoint so the dashboard can advance the user up to the SSH boundary without
// skipping it. If the user is already at the cap, MANUAL_UPGRADE_REQUIRED is
// returned so the operator knows to SSH through the breakpoint before retrying.
// When empty, the check falls back to exact-match so directly targeting a
// breakpoint version is still blocked.
func (s *Server) PlanUpgrade(ctx context.Context, mode jobs.JobMode, requestedTarget string, currentVersion string) *UpgradePlan {
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
	// If "latest" is requested, resolve it from the policy
	resolvedTarget := requestedTarget
	if strings.EqualFold(requestedTarget, "latest") {
		if policyData != nil && strings.TrimSpace(policyData.Latest) != "" {
			resolvedTarget = strings.TrimSpace(policyData.Latest)
		} else {
			// No policy or empty latest field
			plan.State = jobs.JobStateFailed
			plan.FailureCode = "POLICY_REQUIRED"
			plan.Message = "Cannot resolve 'latest': policy not available or latest field is empty"
			return plan
		}
	}

	// Breakpoint enforcement (DASHBOARD mode only).
	//
	// Breakpoints mark versions that require a manual SSH upgrade. The dashboard
	// must never jump over one automatically.
	//
	// Algorithm when currentVersion is known:
	//   1. Find the lowest breakpoint that sits between currentVersion (exclusive)
	//      and resolvedTarget (inclusive) — the first one the user would cross.
	//   2. If such a breakpoint exists, cap the target at the highest release that
	//      is strictly below that breakpoint (the "safe ceiling").
	//   3. If the user is already at or above the safe ceiling (i.e. cap <= current),
	//      they must SSH through the breakpoint — return MANUAL_UPGRADE_REQUIRED.
	//   4. Otherwise, redirect resolvedTarget to the cap and allow the upgrade.
	//
	// This means:
	//   • 1.7.5 → 1.9.9, breakpoint 1.8.0: caps to 1.7.9, upgrade proceeds ✓
	//   • 1.7.9 → 1.9.9, breakpoint 1.8.0: cap == current, MANUAL_UPGRADE_REQUIRED ✓
	//   • 1.8.0 → 1.9.9, breakpoint 2.0.0: not crossed, upgrade proceeds ✓
	//   • 1.9.9 → 2.0.0, breakpoint 2.0.0: cap == current, MANUAL_UPGRADE_REQUIRED ✓
	//
	// When currentVersion is empty the check falls back to exact-match so that
	// directly targeting a breakpoint version is still blocked.
	if mode == jobs.JobModeDashboard && policyData != nil {
		normalizeVer := func(v string) string {
			return strings.TrimPrefix(strings.TrimSpace(v), "v")
		}

		if currentVersion != "" {
			cur, curErr := goversion.NewVersion(normalizeVer(currentVersion))
			tgt, tgtErr := goversion.NewVersion(normalizeVer(resolvedTarget))

			if curErr == nil && tgtErr == nil {
				// Step 1: find the lowest breakpoint crossed by this upgrade.
				var firstBPVer *goversion.Version
				var firstBP policy.Breakpoint
				for _, bp := range policyData.Breakpoints {
					bpv, err := goversion.NewVersion(normalizeVer(bp.Version))
					if err != nil {
						continue
					}
					// crossed = current < breakpoint <= target
					if cur.LessThan(bpv) && tgt.GreaterThanOrEqual(bpv) {
						if firstBPVer == nil || bpv.LessThan(firstBPVer) {
							firstBPVer = bpv
							firstBP = bp
						}
					}
				}

				if firstBPVer != nil {
					// Step 2: find the highest release strictly below that breakpoint.
					var capVer *goversion.Version
					for _, rel := range policyData.Releases {
						rv, err := goversion.NewVersion(normalizeVer(rel))
						if err != nil || !rv.LessThan(firstBPVer) {
							continue
						}
						if capVer == nil || rv.GreaterThan(capVer) {
							capVer = rv
						}
					}

					// Step 3/4: redirect or block.
					if capVer != nil && capVer.GreaterThan(cur) {
						// Safe to upgrade up to the cap; proceed with capped target.
						resolvedTarget = capVer.Original()
					} else {
						// Already at (or past) the cap — must SSH through the breakpoint.
						plan.State = jobs.JobStateFailed
						plan.FailureCode = "MANUAL_UPGRADE_REQUIRED"
						plan.Message = fmt.Sprintf("%s %s", firstBP.Reason, firstBP.Docs)
						return plan
					}
				}
			}
		} else {
			// Fallback: no currentVersion — block only if target IS a breakpoint.
			for _, bp := range policyData.Breakpoints {
				if bp.Version == resolvedTarget {
					plan.State = jobs.JobStateFailed
					plan.FailureCode = "MANUAL_UPGRADE_REQUIRED"
					plan.Message = fmt.Sprintf("%s %s", bp.Reason, bp.Docs)
					return plan
				}
			}
		}
	}

	plan.ResolvedTarget = resolvedTarget
	plan.State = jobs.JobStateReady
	plan.Message = "Upgrade plan validated successfully"

	// Carry arch_support from policy so executeUpgrade can guard arch-specific tags
	if policyData != nil && len(policyData.ArchSupport) > 0 {
		plan.ArchSupport = policyData.ArchSupport
	}

	return plan
}
