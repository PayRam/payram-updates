package http

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/cli"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/jobs"
)

func writeManifest(t *testing.T, repo string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "manifest.json")
	body := `{"image": {"repo": "` + repo + `"}}`
	if err := os.WriteFile(f, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPlanUpgrade_RefusesWhatThePolicyDoesNotAllow(t *testing.T) {
	releases := []string{"3.8.0", "3.9.0", "3.10.0"}
	policyPath := buildPolicyFile(t, "3.10.0", releases, nil)

	cases := []struct {
		name           string
		repo           string
		mode           jobs.JobMode
		target         string
		currentVersion string
		wantFailure    string
	}{
		{"published release", "payramapp/payram", jobs.JobModeDashboard, "3.10.0", "3.9.0", ""},
		{"docker.io prefix is the same image", "docker.io/payramapp/payram", jobs.JobModeDashboard, "3.10.0", "3.9.0", ""},
		{"foreign image", "evil/payram", jobs.JobModeDashboard, "3.10.0", "3.9.0", jobs.FailureTargetNotAllowed},
		{"foreign image even in manual mode", "evil/payram", jobs.JobModeManual, "3.10.0", "3.9.0", jobs.FailureTargetNotAllowed},
		{"unpublished version", "payramapp/payram", jobs.JobModeDashboard, "3.11.0-rc1", "3.9.0", jobs.FailureTargetNotAllowed},
		{"downgrade", "payramapp/payram", jobs.JobModeDashboard, "3.8.0", "3.10.0", jobs.FailureTargetNotAllowed},
		{"downgrade from an arm64 build", "payramapp/payram", jobs.JobModeDashboard, "3.8.0", "3.9.0-arm64", jobs.FailureTargetNotAllowed},
		{"same version (reinstall) is allowed", "payramapp/payram", jobs.JobModeDashboard, "3.10.0", "3.10.0", ""},
		{"manual mode may pick any version", "payramapp/payram", jobs.JobModeManual, "3.8.0", "3.10.0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, policyPath, writeManifest(t, tc.repo))
			plan := srv.PlanUpgrade(context.Background(), tc.mode, tc.target, tc.currentVersion)
			if tc.wantFailure == "" && plan.State == jobs.JobStateFailed {
				t.Fatalf("plan failed: %s %s", plan.FailureCode, plan.Message)
			}
			if tc.wantFailure != "" && plan.FailureCode != tc.wantFailure {
				t.Fatalf("failure = %q (%s), want %q", plan.FailureCode, plan.Message, tc.wantFailure)
			}
		})
	}
}

func TestPlanUpgrade_ImageRepoOverrideIsAllowed(t *testing.T) {
	policyPath := buildPolicyFile(t, "3.10.0", []string{"3.10.0"}, nil)
	srv := newTestServer(t, policyPath, writeManifest(t, "payramapp/payram"))
	srv.config.ImageRepoOverride = "localhost:5000/payram"

	plan := srv.PlanUpgrade(context.Background(), jobs.JobModeDashboard, "3.10.0", "3.9.0")
	if plan.State == jobs.JobStateFailed {
		t.Fatalf("plan failed: %s %s", plan.FailureCode, plan.Message)
	}
	if plan.Manifest.Image.Repo != "localhost:5000/payram" {
		t.Fatalf("repo = %s, want the override", plan.Manifest.Image.Repo)
	}
}

func TestResolveMode_ManualOnlyForRootsCLI(t *testing.T) {
	cases := []struct {
		mode, source string
		rootCLI      bool
		want         jobs.JobMode
		wantErr      error
	}{
		{"MANUAL", "CLI", true, jobs.JobModeManual, nil},
		// any other program on the server, or the container, claiming to be the CLI
		{"MANUAL", "CLI", false, "", errManualNeedsRoot},
		{"manual", "cli", false, "", errManualNeedsRoot},
		{"MANUAL", "DASHBOARD", true, jobs.JobModeDashboard, nil},
		{"MANUAL", "", false, jobs.JobModeDashboard, nil},
		{"", "CLI", true, jobs.JobModeDashboard, nil},
		{"", "CLI", false, jobs.JobModeDashboard, nil},
		{"DASHBOARD", "CLI", false, jobs.JobModeDashboard, nil},
	}
	for _, tc := range cases {
		got, err := resolveMode(tc.mode, tc.source, tc.rootCLI)
		if got != tc.want || !errors.Is(err, tc.wantErr) {
			t.Errorf("resolveMode(%q, %q, rootCLI=%v) = %v, %v; want %v, %v", tc.mode, tc.source, tc.rootCLI, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestHandleUpgradePlan_ManualModeNeedsRootsToken: manual mode skips the
// release policy, so a program on this machine gets it only with the token
// root's CLI reads from the state directory.
func TestHandleUpgradePlan_ManualModeNeedsRootsToken(t *testing.T) {
	policyPath := buildPolicyFile(t, "3.10.0", []string{"3.10.0"}, nil)
	srv := newTestServer(t, policyPath, writeManifest(t, "payramapp/payram"))
	srv.jobStore = jobs.NewStore(t.TempDir())
	srv.cliToken = "root-only-token"
	srv.versionLookup = func(context.Context) string { return "3.10.0" }

	plan := func(remote, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/upgrade/plan", strings.NewReader(`{"mode":"MANUAL","source":"CLI","requestedTarget":"3.9.0"}`))
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set(cli.TokenHeader, token)
		}
		w := httptest.NewRecorder()
		srv.HandleUpgradePlan()(w, req)
		return w
	}

	for _, tc := range []struct{ name, remote, token string }{
		{"local program without the token", "127.0.0.1:40000", ""},
		{"local program with a wrong token", "127.0.0.1:40000", "guess"},
		{"the container with the token", "172.17.0.2:40000", "root-only-token"},
	} {
		if w := plan(tc.remote, tc.token); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "sudo") {
			t.Errorf("%s: status %d %q, want 403 telling the operator to use sudo", tc.name, w.Code, w.Body.String())
		}
	}

	w := plan("127.0.0.1:40000", "root-only-token")
	var resp PlanResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if w.Code != http.StatusOK || resp.Mode != string(jobs.JobModeManual) {
		t.Fatalf("root's CLI: status %d, mode %q (%s); want a MANUAL plan", w.Code, resp.Mode, resp.Message)
	}
}

func TestHandleUpgradeRun_RefusesWhileAnUpdateHoldsTheSlot(t *testing.T) {
	srv := &Server{config: &config.Config{FetchTimeoutSeconds: 1}, jobStore: jobs.NewStore(t.TempDir())}
	if err := srv.claimUpgradeSlot(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", strings.NewReader(`{"requestedTarget":"3.10.0"}`))
	w := httptest.NewRecorder()
	srv.HandleUpgradeRun()(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while an update runs", w.Code)
	}
	var body map[string]string
	json.NewDecoder(w.Body).Decode(&body)
	if !strings.Contains(body["message"], "already running") {
		t.Fatalf("message = %q", body["message"])
	}
	if !srv.upgradeRunning.Load() {
		t.Fatal("a refused request must not release the running update's slot")
	}
}

func TestHandleUpgradeRun_RefusedPlanReleasesTheSlot(t *testing.T) {
	policyPath := buildPolicyFile(t, "3.10.0", []string{"3.10.0"}, nil)
	srv := newTestServer(t, policyPath, writeManifest(t, "payramapp/payram"))
	srv.jobStore = jobs.NewStore(t.TempDir())

	req := httptest.NewRequest(http.MethodPost, "/upgrade/run", strings.NewReader(`{"requestedTarget":"9.9.9","currentVersion":"3.10.0"}`))
	w := httptest.NewRecorder()
	srv.HandleUpgradeRun()(w, req)

	var resp RunResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.FailureCode != jobs.FailureTargetNotAllowed {
		t.Fatalf("failure = %q, want TARGET_NOT_ALLOWED", resp.FailureCode)
	}
	if srv.upgradeRunning.Load() {
		t.Fatal("a refused plan must release the update slot, or every later update is refused")
	}
}

// TestStartUpgrade_CountsTheJob guards the auto-update crash: the job must be
// added to the wait group before it runs, or its deferred Done() panics with
// "negative WaitGroup counter" and kills the updater.
func TestStartUpgrade_CountsTheJob(t *testing.T) {
	policyPath := buildPolicyFile(t, "3.10.0", []string{"3.10.0"}, nil)
	srv := newTestServer(t, policyPath, writeManifest(t, "payramapp/payram"))
	srv.jobStore = jobs.NewStore(t.TempDir())
	srv.config.StateDir = t.TempDir()
	srv.config.DockerBin = "false" // every docker call fails: the job ends before changing anything

	plan := srv.PlanUpgrade(context.Background(), jobs.JobModeDashboard, "3.10.0", "")
	if plan.State == jobs.JobStateFailed {
		t.Fatalf("plan failed: %s", plan.Message)
	}
	if err := srv.claimUpgradeSlot(); err != nil {
		t.Fatal(err)
	}
	job := jobs.NewJob("job-auto", jobs.JobModeDashboard, "3.10.0")
	job.ResolvedTarget = "3.10.0"
	srv.startUpgrade(job, plan)
	srv.upgradeWg.Wait()

	if srv.upgradeRunning.Load() {
		t.Fatal("the finished job must release the update slot")
	}
	saved, _ := srv.jobStore.LoadLatest()
	if saved == nil || saved.State != jobs.JobStateFailed || !strings.Contains(saved.Message, "Nothing was changed") {
		t.Fatalf("job = %+v, want a failure that says nothing was changed", saved)
	}
}

func TestApplyArchSuffix_NeverDoublesTheSuffix(t *testing.T) {
	srv := &Server{jobStore: jobs.NewStore(t.TempDir())}
	job := jobs.NewJob("j", jobs.JobModeDashboard, "2.0.0")
	support := map[string]string{"arm64": "1.9.1"}

	// Hop 2 of a breakpoint update: the running container is the arm64
	// stepping stone and the target comes from the plan without a suffix.
	if got := srv.applyArchSuffix(job, "1.9.7-arm64", baseVersionTag("2.0.0"), support); got != "2.0.0-arm64" {
		t.Fatalf("got %s, want 2.0.0-arm64", got)
	}
	if got := srv.applyArchSuffix(job, "1.9.7-arm64", baseVersionTag("2.0.0-arm64"), support); got != "2.0.0-arm64" {
		t.Fatalf("got %s, want 2.0.0-arm64 (no -arm64-arm64)", got)
	}
	if got := srv.applyArchSuffix(job, "1.8.0-arm64", "1.8.5", map[string]string{"arm64": "1.9.0"}); got != "1.8.5" {
		t.Fatalf("got %s, want 1.8.5 (below the arm64 minimum)", got)
	}
	if got := srv.applyArchSuffix(job, "1.9.7", "2.0.0", support); got != "2.0.0" {
		t.Fatalf("got %s, want 2.0.0 on amd64", got)
	}
}

func TestCheckNewPortsFree_RefusesAPortAnotherProgramHolds(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port

	current := &container.RuntimeState{Ports: []container.PortMapping{{HostPort: "443", ContainerPort: "443", Protocol: "tcp"}}}
	spec := &container.ContainerSpec{Options: []string{"-p", "443:443/tcp", "-p", "127.0.0.1:" + itoa(busyPort) + ":9000/tcp"}}
	if err := checkNewPortsFree(current, spec); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("got %v, want the busy new port refused", err)
	}

	// The container's own ports are in use by itself; they are not checked.
	spec = &container.ContainerSpec{Options: []string{"-p", "443:443/tcp"}}
	if err := checkNewPortsFree(current, spec); err != nil {
		t.Fatalf("existing ports must not be checked: %v", err)
	}
}

func TestSplitPortMapping(t *testing.T) {
	cases := map[string][3]string{
		"443:443/tcp":             {"", "443", "tcp"},
		"127.0.0.1:8080:8080/tcp": {"127.0.0.1", "8080", "tcp"},
		"[::1]:8443:8443/tcp":     {"::1", "8443", "tcp"},
		"53:53/udp":               {"", "53", "udp"},
	}
	for mapping, want := range cases {
		ip, port, proto := splitPortMapping(mapping)
		if ip != want[0] || port != want[1] || proto != want[2] {
			t.Errorf("splitPortMapping(%q) = %q %q %q, want %v", mapping, ip, port, proto, want)
		}
	}
}

func TestStartHeartbeat_KeepsTheSavedJobFresh(t *testing.T) {
	store := jobs.NewStore(t.TempDir())
	job := jobs.NewJob("j", jobs.JobModeDashboard, "3.10.0")
	job.State = jobs.JobStateBackingUp
	job.UpdatedAt = time.Now().Add(-time.Hour).UTC()
	store.Save(job)

	srv := &Server{jobStore: store, heartbeatEvery: 10 * time.Millisecond}
	stop := srv.startHeartbeat()
	time.Sleep(60 * time.Millisecond)
	stop()

	saved, _ := store.LoadLatest()
	if time.Since(saved.UpdatedAt) > time.Second {
		t.Fatalf("UpdatedAt = %s, want refreshed by the heartbeat", saved.UpdatedAt)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
