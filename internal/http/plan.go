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
	// SteppingStone is set when a breakpoint requires a transparent intermediate hop.
	// The executor upgrades through SteppingStone first, then continues to ResolvedTarget,
	// all within a single job. Empty for stop points and when no chaining is needed.
	SteppingStone   string             `json:"steppingStone,omitempty"`
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
// enables gate enforcement: breakpoints force automatic stepping-stone upgrades
// (no SSH), stop points require manual SSH through that version before the
// dashboard can continue. When empty, gate logic is skipped.
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

	// Gate enforcement (DASHBOARD mode only, requires currentVersion).
	//
	// Two kinds of upgrade gates are supported:
	//
	// BREAKPOINT — automatic stepping stone (no SSH needed):
	//   The dashboard stops at the highest release below the breakpoint version,
	//   then on the next run advances through it automatically.
	//   Example (breakpoint at 2.0.0, last release before it is 1.9.7):
	//     • 1.9.1 → 2.0.0: redirected to 1.9.7 first
	//     • 1.9.7 → 2.0.0: redirected through 2.0.0 automatically
	//
	// STOP POINT — manual SSH required:
	//   The dashboard stops at the highest release below the stop point version
	//   and returns MANUAL_UPGRADE_REQUIRED. The operator must SSH and manually
	//   upgrade through that version before the dashboard can continue.
	//   Example (stop point at 2.0.0, last release before it is 1.9.7):
	//     • 1.9.1 → 2.0.0: redirected to 1.9.7 first
	//     • 1.9.7 → 2.0.0: MANUAL_UPGRADE_REQUIRED
	//
	// When both kinds exist, the lowest-version gate wins.
	// When currentVersion is empty, gate logic is skipped.
	if mode == jobs.JobModeDashboard && policyData != nil && currentVersion != "" {
		normalizeVer := func(v string) string {
			return strings.TrimPrefix(strings.TrimSpace(v), "v")
		}

		cur, curErr := goversion.NewVersion(normalizeVer(currentVersion))
		tgt, tgtErr := goversion.NewVersion(normalizeVer(resolvedTarget))

		if curErr == nil && tgtErr == nil {
			// Find the lowest gate (breakpoint or stop point) crossed by this upgrade.
			// crossed = current < gate <= target
			type gateKind int
			const (
				gateBreakpoint gateKind = iota
				gateStopPoint
			)
			type gate struct {
				ver    *goversion.Version
				kind   gateKind
				reason string
				docs   string
			}

			var firstGate *gate
			for _, bp := range policyData.Breakpoints {
				bpv, err := goversion.NewVersion(normalizeVer(bp.Version))
				if err != nil {
					continue
				}
				if cur.LessThan(bpv) && tgt.GreaterThanOrEqual(bpv) {
					if firstGate == nil || bpv.LessThan(firstGate.ver) {
						firstGate = &gate{ver: bpv, kind: gateBreakpoint, reason: bp.Reason, docs: bp.Docs}
					}
				}
			}
			for _, sp := range policyData.StopPoints {
				spv, err := goversion.NewVersion(normalizeVer(sp.Version))
				if err != nil {
					continue
				}
				if cur.LessThan(spv) && tgt.GreaterThanOrEqual(spv) {
					if firstGate == nil || spv.LessThan(firstGate.ver) {
						firstGate = &gate{ver: spv, kind: gateStopPoint, reason: sp.Reason, docs: sp.Docs}
					}
				}
			}

			if firstGate != nil {
				// Find the highest release strictly below this gate (the "stepping stone").
				var capVer *goversion.Version
				for _, rel := range policyData.Releases {
					rv, err := goversion.NewVersion(normalizeVer(rel))
					if err != nil || !rv.LessThan(firstGate.ver) {
						continue
					}
					if capVer == nil || rv.GreaterThan(capVer) {
						capVer = rv
					}
				}

				if capVer != nil && capVer.GreaterThan(cur) {
					// User is below the stepping stone.
					switch firstGate.kind {
					case gateBreakpoint:
						// Chain both hops into one job: executor upgrades to stepping stone
						// first (silently), then continues to the breakpoint version.
						plan.SteppingStone = capVer.Original()
						resolvedTarget = firstGate.ver.Original()
					case gateStopPoint:
						// Redirect to stepping stone — user must SSH after reaching it.
						resolvedTarget = capVer.Original()
					}
				} else {
					// User is at or past the stepping stone — apply gate-specific behaviour.
					switch firstGate.kind {
					case gateBreakpoint:
						// Already at stepping stone: advance through breakpoint directly (no chain needed).
						resolvedTarget = firstGate.ver.Original()
					case gateStopPoint:
						// Block — operator must SSH through this version manually.
						plan.State = jobs.JobStateFailed
						plan.FailureCode = "MANUAL_UPGRADE_REQUIRED"
						plan.Message = fmt.Sprintf("%s %s", firstGate.reason, firstGate.docs)
						return plan
					}
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
