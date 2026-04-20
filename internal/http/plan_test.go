package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/jobs"
)

// minimalManifest is a valid manifest JSON that satisfies the manifest client.
const minimalManifest = `{
  "image": {"repo": "payramapp/payram"},
  "defaults": {"container_name": "payram-core", "restart_policy": "unless-stopped", "ports": [], "volumes": []}
}`

// buildPolicyJSON writes a policy file and returns its path.
// releases should include every version that might be used as a cap.
func buildPolicyFile(t *testing.T, latest string, releases []string, breakpoints []map[string]string) string {
	t.Helper()
	type bp struct {
		Version string `json:"version"`
		Reason  string `json:"reason"`
		Docs    string `json:"docs"`
	}
	type pol struct {
		Latest      string   `json:"latest"`
		Releases    []string `json:"releases"`
		Breakpoints []bp     `json:"breakpoints"`
	}

	bps := make([]bp, 0, len(breakpoints))
	for _, m := range breakpoints {
		bps = append(bps, bp{Version: m["version"], Reason: m["reason"], Docs: m["docs"]})
	}
	p := pol{Latest: latest, Releases: releases, Breakpoints: bps}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	f := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(f, data, 0600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return f
}

// buildManifestFile writes a minimal manifest file and returns its path.
func buildManifestFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(f, []byte(minimalManifest), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return f
}

// newTestServer builds a minimal Server wired to local policy/manifest files.
func newTestServer(t *testing.T, policyPath, manifestPath string) *Server {
	t.Helper()
	cfg := &config.Config{
		PolicyURL:           policyPath,
		RuntimeManifestURL:  manifestPath,
		FetchTimeoutSeconds: 5,
	}
	return &Server{config: cfg}
}

// TestPlanUpgrade_BreakpointCapping covers the full breakpoint logic in DASHBOARD mode.
func TestPlanUpgrade_BreakpointCapping(t *testing.T) {
	// Releases available in the policy. Ordered arbitrarily — code must sort by semver.
	releases := []string{"1.7.0", "1.7.5", "1.7.9", "1.8.0", "1.9.0", "1.9.9", "2.0.0"}
	breakpoints := []map[string]string{
		{"version": "1.8.0", "reason": "Breaking change at 1.8.0.", "docs": "https://docs.example.com/1.8.0"},
		{"version": "2.0.0", "reason": "Breaking change at 2.0.0.", "docs": "https://docs.example.com/2.0.0"},
	}

	tests := []struct {
		name            string
		currentVersion  string // empty = caller did not provide it
		requestedTarget string
		wantState       jobs.JobState
		wantFailureCode string
		// when state==Ready, wantResolved is the expected resolvedTarget
		wantResolved string
	}{
		// --- crossing first breakpoint → cap to 1.7.9 ---
		{
			name:            "1.7.5 to 1.9.9 is capped at 1.7.9 (crosses 1.8.0)",
			currentVersion:  "1.7.5",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.7.9",
		},
		{
			name:            "1.7.0 to latest (1.9.9) is capped at 1.7.9",
			currentVersion:  "1.7.0",
			requestedTarget: "latest",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.7.9",
		},
		{
			name:            "1.7.5 to 2.0.0 is capped at 1.7.9 (first breakpoint wins)",
			currentVersion:  "1.7.5",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.7.9",
		},

		// --- already at cap → MANUAL_UPGRADE_REQUIRED ---
		{
			name:            "1.7.9 to 1.9.9 requires SSH through 1.8.0",
			currentVersion:  "1.7.9",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateFailed,
			wantFailureCode: "MANUAL_UPGRADE_REQUIRED",
		},
		{
			name:            "1.7.9 to 1.8.0 requires SSH (target is the breakpoint itself)",
			currentVersion:  "1.7.9",
			requestedTarget: "1.8.0",
			wantState:       jobs.JobStateFailed,
			wantFailureCode: "MANUAL_UPGRADE_REQUIRED",
		},

		// --- already past first breakpoint, no second crossing → normal upgrade ---
		{
			name:            "1.8.0 to 1.9.9 proceeds normally (no breakpoint crossed)",
			currentVersion:  "1.8.0",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},
		{
			name:            "1.9.0 to 1.9.9 proceeds normally",
			currentVersion:  "1.9.0",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},

		// --- crossing second breakpoint → MANUAL_UPGRADE_REQUIRED at 1.9.9 cap ---
		{
			name:            "1.9.9 to 2.0.0 requires SSH through 2.0.0",
			currentVersion:  "1.9.9",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateFailed,
			wantFailureCode: "MANUAL_UPGRADE_REQUIRED",
		},
		{
			name:            "1.9.0 to 2.0.0 is capped at 1.9.9 (crosses 2.0.0)",
			currentVersion:  "1.9.0",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},

		// --- already on latest, no upgrade needed (resolved == requested, no breakpoint) ---
		{
			name:            "already on 1.9.9, target 1.9.9 proceeds (no crossing)",
			currentVersion:  "1.9.9",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},

		// --- no currentVersion fallback: block only if target IS a breakpoint ---
		{
			name:            "no currentVersion, targeting 1.8.0 (breakpoint) is blocked",
			currentVersion:  "",
			requestedTarget: "1.8.0",
			wantState:       jobs.JobStateFailed,
			wantFailureCode: "MANUAL_UPGRADE_REQUIRED",
		},
		{
			name:            "no currentVersion, targeting 1.9.9 (not a breakpoint) proceeds",
			currentVersion:  "",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},
	}

	manifestPath := buildManifestFile(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policyPath := buildPolicyFile(t, "1.9.9", releases, breakpoints)
			srv := newTestServer(t, policyPath, manifestPath)

			plan := srv.PlanUpgrade(context.Background(), jobs.JobModeDashboard, tt.requestedTarget, tt.currentVersion)

			if plan.State != tt.wantState {
				t.Errorf("State: got %q, want %q (failureCode=%q, message=%q)",
					plan.State, tt.wantState, plan.FailureCode, plan.Message)
			}

			if tt.wantFailureCode != "" && plan.FailureCode != tt.wantFailureCode {
				t.Errorf("FailureCode: got %q, want %q", plan.FailureCode, tt.wantFailureCode)
			}

			if tt.wantResolved != "" && plan.ResolvedTarget != tt.wantResolved {
				t.Errorf("ResolvedTarget: got %q, want %q", plan.ResolvedTarget, tt.wantResolved)
			}
		})
	}
}

// TestPlanUpgrade_ManualModeIgnoresBreakpoints ensures MANUAL mode bypasses all breakpoint logic.
func TestPlanUpgrade_ManualModeIgnoresBreakpoints(t *testing.T) {
	releases := []string{"1.7.0", "1.7.9", "1.8.0", "1.9.9"}
	breakpoints := []map[string]string{
		{"version": "1.8.0", "reason": "SSH required.", "docs": "https://docs.example.com/1.8.0"},
	}

	tests := []struct {
		name            string
		currentVersion  string
		requestedTarget string
	}{
		{"manual mode: target is a breakpoint", "1.7.9", "1.8.0"},
		{"manual mode: crosses a breakpoint", "1.7.5", "1.9.9"},
		{"manual mode: no currentVersion, target is breakpoint", "", "1.8.0"},
	}

	manifestPath := buildManifestFile(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policyPath := buildPolicyFile(t, "1.9.9", releases, breakpoints)
			srv := newTestServer(t, policyPath, manifestPath)

			plan := srv.PlanUpgrade(context.Background(), jobs.JobModeManual, tt.requestedTarget, tt.currentVersion)

			// MANUAL mode fetches policy on a best-effort basis and continues even
			// on failure, so any non-breakpoint failure (e.g. manifest fetch) would
			// still result in a failed plan — but the failure code must NOT be
			// MANUAL_UPGRADE_REQUIRED.
			if plan.FailureCode == "MANUAL_UPGRADE_REQUIRED" {
				t.Errorf("MANUAL mode should never return MANUAL_UPGRADE_REQUIRED, got it for target %q", tt.requestedTarget)
			}
		})
	}
}

// TestPlanUpgrade_NoBreakpoints confirms normal operation when the policy has no breakpoints.
func TestPlanUpgrade_NoBreakpoints(t *testing.T) {
	releases := []string{"1.0.0", "1.1.0", "1.2.0"}
	manifestPath := buildManifestFile(t)
	policyPath := buildPolicyFile(t, "1.2.0", releases, nil)
	srv := newTestServer(t, policyPath, manifestPath)

	plan := srv.PlanUpgrade(context.Background(), jobs.JobModeDashboard, "1.2.0", "1.0.0")

	if plan.State != jobs.JobStateReady {
		t.Errorf("expected Ready, got %q (%s)", plan.State, plan.Message)
	}
	if plan.ResolvedTarget != "1.2.0" {
		t.Errorf("expected resolvedTarget 1.2.0, got %q", plan.ResolvedTarget)
	}
}

// TestPlanUpgrade_LatestResolution verifies "latest" is resolved from the policy.
func TestPlanUpgrade_LatestResolution(t *testing.T) {
	releases := []string{"1.0.0", "1.1.0", "1.2.0"}
	manifestPath := buildManifestFile(t)
	policyPath := buildPolicyFile(t, "1.2.0", releases, nil)
	srv := newTestServer(t, policyPath, manifestPath)

	plan := srv.PlanUpgrade(context.Background(), jobs.JobModeDashboard, "latest", "1.0.0")

	if plan.State != jobs.JobStateReady {
		t.Errorf("expected Ready, got %q (%s)", plan.State, plan.Message)
	}
	if plan.ResolvedTarget != "1.2.0" {
		t.Errorf("expected resolvedTarget 1.2.0, got %q", plan.ResolvedTarget)
	}
}

// TestHandleUpgradePlan_CurrentVersionWiredThrough verifies the HTTP handler reads
// currentVersion from the request body and forwards it to PlanUpgrade so that
// breakpoint capping works end-to-end over the API.
func TestHandleUpgradePlan_CurrentVersionWiredThrough(t *testing.T) {
	releases := []string{"1.7.0", "1.7.5", "1.7.9", "1.8.0", "1.9.9"}
	breakpoints := []map[string]string{
		{"version": "1.8.0", "reason": "SSH required.", "docs": "https://docs.example.com/1.8.0"},
	}
	manifestPath := buildManifestFile(t)
	policyPath := buildPolicyFile(t, "1.9.9", releases, breakpoints)

	tests := []struct {
		name            string
		body            string
		wantFailureCode string
		wantResolved    string
	}{
		{
			// 1.7.5 → 1.9.9 with breakpoint at 1.8.0: should be capped at 1.7.9
			name:         "crossing breakpoint is capped via API",
			body:         `{"requestedTarget":"1.9.9","currentVersion":"1.7.5"}`,
			wantResolved: "1.7.9",
		},
		{
			// 1.7.9 → 1.9.9 with breakpoint at 1.8.0: already at cap, blocked
			name:            "at cap via API returns MANUAL_UPGRADE_REQUIRED",
			body:            `{"requestedTarget":"1.9.9","currentVersion":"1.7.9"}`,
			wantFailureCode: "MANUAL_UPGRADE_REQUIRED",
		},
		{
			// no currentVersion sent: fallback to exact-match — targeting 1.9.9 is fine
			name:         "no currentVersion, non-breakpoint target proceeds",
			body:         `{"requestedTarget":"1.9.9"}`,
			wantResolved: "1.9.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				PolicyURL:           policyPath,
				RuntimeManifestURL:  manifestPath,
				FetchTimeoutSeconds: 5,
			}
			tmpDir := t.TempDir()
			srv := &Server{config: cfg, jobStore: jobs.NewStore(tmpDir)}

			req := httptest.NewRequest(http.MethodPost, "/upgrade/plan", strings.NewReader(tt.body))
			w := httptest.NewRecorder()
			srv.HandleUpgradePlan()(w, req)

			var resp PlanResponse
			if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if tt.wantFailureCode != "" {
				if resp.FailureCode != tt.wantFailureCode {
					t.Errorf("FailureCode: got %q, want %q (message: %s)", resp.FailureCode, tt.wantFailureCode, resp.Message)
				}
			}
			if tt.wantResolved != "" {
				if resp.ResolvedTarget != tt.wantResolved {
					t.Errorf("ResolvedTarget: got %q, want %q", resp.ResolvedTarget, tt.wantResolved)
				}
			}
		})
	}
}

// TestHandleUpgradeRun_CurrentVersionWiredThrough verifies the /upgrade/run handler
// also reads currentVersion and blocks appropriately before creating a job.
func TestHandleUpgradeRun_CurrentVersionWiredThrough(t *testing.T) {
	releases := []string{"1.7.0", "1.7.5", "1.7.9", "1.8.0", "1.9.9"}
	breakpoints := []map[string]string{
		{"version": "1.8.0", "reason": "SSH required.", "docs": "https://docs.example.com/1.8.0"},
	}
	manifestPath := buildManifestFile(t)
	policyPath := buildPolicyFile(t, "1.9.9", releases, breakpoints)

	cfg := &config.Config{
		PolicyURL:           policyPath,
		RuntimeManifestURL:  manifestPath,
		FetchTimeoutSeconds: 5,
	}
	tmpDir := t.TempDir()
	srv := &Server{config: cfg, jobStore: jobs.NewStore(tmpDir)}

	// 1.7.9 → 1.9.9 crosses 1.8.0 breakpoint and user is at cap → must be blocked.
	body := strings.NewReader(`{"requestedTarget":"1.9.9","currentVersion":"1.7.9"}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", body)
	w := httptest.NewRecorder()
	srv.HandleUpgradeRun()(w, req)

	var resp RunResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.FailureCode != "MANUAL_UPGRADE_REQUIRED" {
		t.Errorf("expected MANUAL_UPGRADE_REQUIRED, got %q (message: %s)", resp.FailureCode, resp.Message)
	}

	// Confirm no job was created.
	job, _ := srv.jobStore.LoadLatest()
	if job != nil {
		t.Errorf("expected no job to be created, found job %s in state %s", job.JobID, job.State)
	}
}
