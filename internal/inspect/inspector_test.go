package inspect

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
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

// A failure before PayRam was changed leaves PayRam running: the system is
// degraded (the update did not happen), not broken.
func TestInspector_Run_RetryableErrorDegraded(t *testing.T) {
	result := runInspection(t, fakeContainers{"payram": "running"}, func(j *jobs.Job) {
		j.State = jobs.JobStateFailed
		j.FailureCode = "DOCKER_PULL_FAILED"
		j.Message = "network timeout"
	})

	if result.OverallState != StateDegraded {
		t.Errorf("expected DEGRADED for a failure that left PayRam running, got %s", result.OverallState)
	}
	if result.RecoveryPlaybook == nil || result.RecoveryPlaybook.Code != "DOCKER_PULL_FAILED" {
		t.Fatalf("expected DOCKER_PULL_FAILED playbook, got %+v", result.RecoveryPlaybook)
	}
	if !hasRecommendation(result, "retry") {
		t.Errorf("expected a retry recommendation, got %+v", result.Recommendations)
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

// fakeContainers maps container names to their Docker status; a name that is
// not listed does not exist.
type fakeContainers map[string]string

// writeFakeDocker writes a docker stand-in that answers `info` and
// `inspect --format ... NAME` from containers.
func writeFakeDocker(t *testing.T, containers fakeContainers) string {
	t.Helper()
	names := make([]string, 0, len(containers))
	for name := range containers {
		names = append(names, name)
	}
	sort.Strings(names)
	var cases strings.Builder
	for _, name := range names {
		cases.WriteString("  " + name + ") case \"$3\" in *State.Status*) echo " + containers[name] + " ;; *) echo payramapp/payram:1.8.0 ;; esac; exit 0 ;;\n")
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = info ]; then echo 24.0.0; exit 0; fi\n" +
		"name=\"$4\"\n" +
		"case \"$name\" in\n" + cases.String() + "esac\n" +
		"echo \"Error: No such object: $name\" >&2; exit 1\n"
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return path
}

// runInspection inspects with a fake Docker, no policy/manifest URLs and an
// unreachable core, so only the job and container checks decide the result.
func runInspection(t *testing.T, containers fakeContainers, mutate func(*jobs.Job)) *InspectResult {
	t.Helper()
	store := jobs.NewStore(t.TempDir())
	if mutate != nil {
		job := jobs.NewJob("test-job", jobs.JobModeDashboard, "1.9.0")
		mutate(job)
		if err := store.Save(job); err != nil {
			t.Fatalf("save job: %v", err)
		}
	}
	inspector := NewInspector(store, writeFakeDocker(t, containers), "payram", "http://127.0.0.1:1", "", "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return inspector.Run(ctx)
}

func hasRecommendation(result *InspectResult, action string) bool {
	return findRecommendation(result, action) != nil
}

func findRecommendation(result *InspectResult, action string) *Recommendation {
	for i := range result.Recommendations {
		if result.Recommendations[i].Action == action {
			return &result.Recommendations[i]
		}
	}
	return nil
}

func TestInspector_RunningUpdateIsNotStale(t *testing.T) {
	// The switch stops the old container on purpose; no container advice and
	// no stale playbook while the update saves progress.
	result := runInspection(t, fakeContainers{"payram": "exited", "payram-previous": "exited"}, func(j *jobs.Job) {
		j.State = jobs.JobStateExecuting
		j.UpdatedAt = time.Now().UTC().Add(-20 * time.Second)
	})
	if result.RecoveryPlaybook != nil {
		t.Errorf("running update got a playbook: %s", result.RecoveryPlaybook.Code)
	}
	if !hasRecommendation(result, "wait") {
		t.Errorf("expected wait recommendation, got %+v", result.Recommendations)
	}
	for _, action := range []string{"start_container", "put_previous_back", "restart_updater"} {
		if hasRecommendation(result, action) {
			t.Errorf("running update must not get %s advice: %+v", action, result.Recommendations)
		}
	}
}

func TestInspector_StaleUpdate(t *testing.T) {
	result := runInspection(t, fakeContainers{"payram": "running"}, func(j *jobs.Job) {
		j.State = jobs.JobStateExecuting
		j.UpdatedAt = time.Now().UTC().Add(-10 * time.Minute)
	})
	if result.RecoveryPlaybook == nil || result.RecoveryPlaybook.Code != "STALE_EXECUTING" {
		t.Fatalf("expected STALE_EXECUTING playbook, got %+v", result.RecoveryPlaybook)
	}
	rec := findRecommendation(result, "restart_updater")
	if rec == nil || rec.Priority != 1 || !strings.Contains(rec.Description, "sudo systemctl restart payram-updater") {
		t.Errorf("expected restart_updater first, got %+v", result.Recommendations)
	}
	if strings.Contains(strings.Join(result.RecoveryPlaybook.SSHSteps, " "), "payram-updater sync") {
		t.Error("stale playbook must not tell the merchant to run sync")
	}
}

func TestInspector_StoppedContainer(t *testing.T) {
	result := runInspection(t, fakeContainers{"payram": "exited"}, nil)
	rec := findRecommendation(result, "start_container")
	if rec == nil || !strings.Contains(rec.Description, "docker start payram") {
		t.Errorf("expected docker start payram, got %+v", result.Recommendations)
	}
}

func TestInspector_MissingContainerWithPrevious(t *testing.T) {
	result := runInspection(t, fakeContainers{"payram-previous": "exited"}, func(j *jobs.Job) {
		j.State = jobs.JobStateFailed
		j.FailureCode = jobs.FailureRollbackFailed
	})
	rec := findRecommendation(result, "put_previous_back")
	if rec == nil {
		t.Fatalf("expected put_previous_back, got %+v", result.Recommendations)
	}
	for _, want := range []string{"interrupted", "sudo systemctl restart payram-updater", "docker rename payram-previous payram", "docker start payram"} {
		if !strings.Contains(rec.Description, want) {
			t.Errorf("recommendation misses %q: %s", want, rec.Description)
		}
	}
	if result.OverallState != StateBroken {
		t.Errorf("expected BROKEN after ROLLBACK_FAILED, got %s", result.OverallState)
	}
}

func TestInspector_MissingContainerWithoutPrevious(t *testing.T) {
	result := runInspection(t, fakeContainers{}, nil)
	if result.Checks["container"].Status != "WARNING" {
		t.Errorf("expected container WARNING, got %+v", result.Checks["container"])
	}
	rec := findRecommendation(result, "find_container")
	if rec == nil || !strings.Contains(rec.Description, "docker ps -a") {
		t.Errorf("expected find_container, got %+v", result.Recommendations)
	}
	if hasRecommendation(result, "recover") {
		t.Error("recover cannot bring a container back; do not recommend it")
	}
}

func TestInspector_StoppedContainerAfterFailedSwitch(t *testing.T) {
	result := runInspection(t, fakeContainers{"payram": "exited", "payram-previous": "exited"}, func(j *jobs.Job) {
		j.State = jobs.JobStateFailed
		j.FailureCode = jobs.FailureRollbackFailed
	})
	rec := findRecommendation(result, "put_previous_back")
	if rec == nil || !strings.Contains(rec.Description, "docker rename payram payram-failed-manual && docker rename payram-previous payram") {
		t.Errorf("expected the previous version to be put back, got %+v", result.Recommendations)
	}
	if hasRecommendation(result, "start_container") {
		t.Error("must not start the failed new version when the previous one exists")
	}
}

func TestInspector_DockerUnreachableIsNotAMissingContainer(t *testing.T) {
	store := jobs.NewStore(t.TempDir())
	inspector := NewInspector(store, filepath.Join(t.TempDir(), "no-docker"), "payram", "http://127.0.0.1:1", "", "", false)
	result := inspector.Run(context.Background())
	if result.Checks["container"].Status != "UNKNOWN" {
		t.Errorf("expected UNKNOWN container check, got %+v", result.Checks["container"])
	}
	if hasRecommendation(result, "find_container") || !hasRecommendation(result, "start_docker") {
		t.Errorf("expected start_docker only, got %+v", result.Recommendations)
	}
}

func TestInspector_PlaybookUsesTheJobsCoreURL(t *testing.T) {
	result := runInspection(t, fakeContainers{"payram": "running"}, func(j *jobs.Job) {
		j.State = jobs.JobStateFailed
		j.FailureCode = "HEALTHCHECK_FAILED"
		j.CoreBaseURL = "http://127.0.0.1"
	})
	if result.RecoveryPlaybook == nil {
		t.Fatal("expected playbook")
	}
	first := result.RecoveryPlaybook.SSHSteps[0]
	if !strings.Contains(first, "curl -s http://127.0.0.1/api/v1/health") || strings.Contains(first, "127.0.0.1:1/") {
		t.Errorf("health step must use the job's URL: %s", first)
	}
	if !hasRecommendation(result, "check_health") {
		t.Errorf("expected check_health, got %+v", result.Recommendations)
	}
}
