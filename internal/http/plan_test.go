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

// buildPolicyFile writes a policy file and returns its path.
// releases should include every version that might be used as a cap.
// breakpoints and stopPoints are slices of maps with keys: version, reason, docs.
func buildPolicyFile(t *testing.T, latest string, releases []string, breakpoints []map[string]string) string {
	return buildPolicyFileWithStopPoints(t, latest, releases, breakpoints, nil)
}

// buildPolicyFileWithStopPoints is like buildPolicyFile but also accepts stop_points.
func buildPolicyFileWithStopPoints(t *testing.T, latest string, releases []string, breakpoints []map[string]string, stopPoints []map[string]string) string {
	t.Helper()
	type entry struct {
		Version string `json:"version"`
		Reason  string `json:"reason"`
		Docs    string `json:"docs,omitempty"`
	}
	type pol struct {
		Latest      string   `json:"latest"`
		Releases    []string `json:"releases"`
		Breakpoints []entry  `json:"breakpoints"`
		StopPoints  []entry  `json:"stop_points"`
	}

	toEntries := func(ms []map[string]string) []entry {
		out := make([]entry, 0, len(ms))
		for _, m := range ms {
			out = append(out, entry{Version: m["version"], Reason: m["reason"], Docs: m["docs"]})
		}
		return out
	}

	p := pol{
		Latest:      latest,
		Releases:    releases,
		Breakpoints: toEntries(breakpoints),
		StopPoints:  toEntries(stopPoints),
	}

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
		name               string
		currentVersion     string // empty = caller did not provide it
		requestedTarget    string
		wantState          jobs.JobState
		wantFailureCode    string
		wantResolved       string
		wantSteppingStone  string // non-empty when a two-hop chain is expected
	}{
		// --- below stepping stone: chain through stepping stone to breakpoint in one job ---
		{
			name:              "1.7.5 to 1.9.9: chains through 1.7.9 (stepping stone) to 1.8.0 (breakpoint)",
			currentVersion:    "1.7.5",
			requestedTarget:   "1.9.9",
			wantState:         jobs.JobStateReady,
			wantResolved:      "1.8.0",
			wantSteppingStone: "1.7.9",
		},
		{
			name:              "1.7.0 to latest (1.9.9): chains through 1.7.9 to 1.8.0",
			currentVersion:    "1.7.0",
			requestedTarget:   "latest",
			wantState:         jobs.JobStateReady,
			wantResolved:      "1.8.0",
			wantSteppingStone: "1.7.9",
		},
		{
			name:              "1.7.5 to 2.0.0: first breakpoint (1.8.0) wins, chains through 1.7.9",
			currentVersion:    "1.7.5",
			requestedTarget:   "2.0.0",
			wantState:         jobs.JobStateReady,
			wantResolved:      "1.8.0",
			wantSteppingStone: "1.7.9",
		},

		// --- at stepping stone → direct advance through the breakpoint version (no chain) ---
		{
			name:            "1.7.9 to 1.9.9: at stepping stone, advances through 1.8.0 directly",
			currentVersion:  "1.7.9",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.8.0",
		},
		{
			name:            "1.7.9 to 1.8.0: at stepping stone, advances through 1.8.0 directly",
			currentVersion:  "1.7.9",
			requestedTarget: "1.8.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.8.0",
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

		// --- below stepping stone before second breakpoint → chain through 1.9.9 to 2.0.0 ---
		{
			name:              "1.9.0 to 2.0.0: chains through 1.9.9 (stepping stone) to 2.0.0 (breakpoint)",
			currentVersion:    "1.9.0",
			requestedTarget:   "2.0.0",
			wantState:         jobs.JobStateReady,
			wantResolved:      "2.0.0",
			wantSteppingStone: "1.9.9",
		},

		// --- at stepping stone before second breakpoint → advance through it ---
		{
			name:            "1.9.9 to 2.0.0: at stepping stone, advances through 2.0.0 directly",
			currentVersion:  "1.9.9",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "2.0.0",
		},

		// --- already on latest, no upgrade needed ---
		{
			name:            "already on 1.9.9, target 1.9.9 proceeds (no crossing)",
			currentVersion:  "1.9.9",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},

		// --- no currentVersion: gate logic skipped, upgrade proceeds as-is ---
		{
			name:            "no currentVersion, targeting 1.8.0 (breakpoint) proceeds as-is",
			currentVersion:  "",
			requestedTarget: "1.8.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.8.0",
		},
		{
			name:            "no currentVersion, targeting 1.9.9 proceeds as-is",
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

			if plan.SteppingStone != tt.wantSteppingStone {
				t.Errorf("SteppingStone: got %q, want %q", plan.SteppingStone, tt.wantSteppingStone)
			}
		})
	}
}

// TestPlanUpgrade_StopPoints covers stop point logic: cap at stepping stone then
// block with MANUAL_UPGRADE_REQUIRED when the user reaches it.
func TestPlanUpgrade_StopPoints(t *testing.T) {
	releases := []string{"1.7.0", "1.7.5", "1.7.9", "1.8.0", "1.9.0", "1.9.9", "2.0.0"}
	stopPoints := []map[string]string{
		{"version": "2.0.0", "reason": "Major release: SSH required."},
	}

	tests := []struct {
		name            string
		currentVersion  string
		requestedTarget string
		wantState       jobs.JobState
		wantFailureCode string
		wantResolved    string
	}{
		// below stepping stone → redirect there (same as breakpoint)
		{
			name:            "1.9.0 to 2.0.0 is capped at 1.9.9 (stepping stone before stop point)",
			currentVersion:  "1.9.0",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},
		// at stepping stone → MANUAL_UPGRADE_REQUIRED (unlike breakpoint which auto-advances)
		{
			name:            "1.9.9 to 2.0.0 requires SSH (at stepping stone for stop point)",
			currentVersion:  "1.9.9",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateFailed,
			wantFailureCode: "MANUAL_UPGRADE_REQUIRED",
		},
		// not crossing the stop point → proceeds normally
		{
			name:            "1.9.0 to 1.9.9 proceeds normally (stop point not crossed)",
			currentVersion:  "1.9.0",
			requestedTarget: "1.9.9",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.9.9",
		},
		// no currentVersion → gate logic skipped
		{
			name:            "no currentVersion, targeting 2.0.0 (stop point) proceeds as-is",
			currentVersion:  "",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "2.0.0",
		},
	}

	manifestPath := buildManifestFile(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policyPath := buildPolicyFileWithStopPoints(t, "2.0.0", releases, nil, stopPoints)
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

// TestPlanUpgrade_MixedGates verifies that when a breakpoint and a stop point both
// exist, the lowest-version gate wins and its specific behaviour applies.
func TestPlanUpgrade_MixedGates(t *testing.T) {
	// breakpoint at 1.8.0 (auto-advance), stop point at 2.0.0 (SSH required)
	releases := []string{"1.7.9", "1.8.0", "1.9.9", "2.0.0"}
	breakpoints := []map[string]string{
		{"version": "1.8.0", "reason": "Step through 1.8.0."},
	}
	stopPoints := []map[string]string{
		{"version": "2.0.0", "reason": "SSH required for 2.0.0."},
	}
	manifestPath := buildManifestFile(t)

	tests := []struct {
		name            string
		currentVersion  string
		requestedTarget string
		wantState       jobs.JobState
		wantFailureCode string
		wantResolved    string
	}{
		// breakpoint (1.8.0) is lower → auto-advance wins
		{
			name:            "1.7.9 to 2.0.0: lowest gate is breakpoint 1.8.0, auto-advances through it",
			currentVersion:  "1.7.9",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "1.8.0",
		},
		// past breakpoint, now hits stop point
		{
			name:            "1.9.9 to 2.0.0: lowest gate is stop point 2.0.0, SSH required",
			currentVersion:  "1.9.9",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateFailed,
			wantFailureCode: "MANUAL_UPGRADE_REQUIRED",
		},
		// past both gates
		{
			name:            "2.0.0 to 2.0.0: already on target, no gate",
			currentVersion:  "2.0.0",
			requestedTarget: "2.0.0",
			wantState:       jobs.JobStateReady,
			wantResolved:    "2.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policyPath := buildPolicyFileWithStopPoints(t, "2.0.0", releases, breakpoints, stopPoints)
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

// TestPlanUpgrade_ManualModeIgnoresBreakpoints ensures MANUAL mode bypasses all gate logic.
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
			// 1.7.5 → 1.9.9 with breakpoint at 1.8.0: below stepping stone → chains to 1.8.0 directly
			name:         "crossing breakpoint chains to breakpoint version via API",
			body:         `{"requestedTarget":"1.9.9","currentVersion":"1.7.5"}`,
			wantResolved: "1.8.0",
		},
		{
			// 1.7.9 → 1.9.9 with breakpoint at 1.8.0: at stepping stone, advances through 1.8.0
			name:         "at stepping stone via API advances through breakpoint",
			body:         `{"requestedTarget":"1.9.9","currentVersion":"1.7.9"}`,
			wantResolved: "1.8.0",
		},
		{
			// no currentVersion sent: gate logic resolved internally from container; in tests no container is
			// running so it falls back to empty, meaning no gate enforcement — targeting 1.9.9 is fine
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

	// 1.7.9 → 1.9.9 with breakpoint at 1.8.0: at stepping stone → redirected to 1.8.0, job created.
	body := strings.NewReader(`{"requestedTarget":"1.9.9","currentVersion":"1.7.9"}`)
	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", body)
	w := httptest.NewRecorder()
	srv.HandleUpgradeRun()(w, req)

	var resp RunResponse
	if err := json.NewDecoder(w.Result().Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.FailureCode != "" {
		t.Errorf("expected no failure, got %q (message: %s)", resp.FailureCode, resp.Message)
	}

	// Confirm a job was created targeting the breakpoint version (1.8.0).
	job, _ := srv.jobStore.LoadLatest()
	if job == nil {
		t.Fatal("expected a job to be created, got nil")
	}
	if job.ResolvedTarget != "1.8.0" {
		t.Errorf("expected job resolvedTarget 1.8.0, got %q", job.ResolvedTarget)
	}
}
