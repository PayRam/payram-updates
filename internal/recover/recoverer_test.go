package recover

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/recovery"
)

// recordingDocker implements the whole container lifecycle so a test can
// prove recover only ever reads.
type recordingDocker struct {
	running    bool
	inspectErr error
	calls      []string
}

func (d *recordingDocker) InspectRunning(_ context.Context, name string) (bool, error) {
	d.calls = append(d.calls, "inspect "+name)
	return d.running, d.inspectErr
}
func (d *recordingDocker) Stop(_ context.Context, name string) error {
	d.calls = append(d.calls, "stop "+name)
	return nil
}
func (d *recordingDocker) Start(_ context.Context, name string) error {
	d.calls = append(d.calls, "start "+name)
	return nil
}
func (d *recordingDocker) Restart(_ context.Context, name string) error {
	d.calls = append(d.calls, "restart "+name)
	return nil
}
func (d *recordingDocker) Remove(_ context.Context, name string) error {
	d.calls = append(d.calls, "rm "+name)
	return nil
}

func (d *recordingDocker) assertOnlyInspected(t *testing.T) {
	t.Helper()
	for _, call := range d.calls {
		if !strings.HasPrefix(call, "inspect ") {
			t.Errorf("recover changed a container: %s (calls: %v)", call, d.calls)
		}
	}
}

func saveJob(t *testing.T, store *jobs.Store, mutate func(*jobs.Job)) *jobs.Job {
	t.Helper()
	job := jobs.NewJob("test-job", jobs.JobModeDashboard, "1.9.0")
	job.State = jobs.JobStateFailed
	mutate(job)
	if err := store.Save(job); err != nil {
		t.Fatalf("save job: %v", err)
	}
	return job
}

func runRecover(t *testing.T, docker ContainerStatusReader, mutate func(*jobs.Job)) *RecoveryResult {
	t.Helper()
	store := jobs.NewStore(t.TempDir())
	if mutate != nil {
		saveJob(t, store, mutate)
	}
	result, err := NewRecoverer(store, docker, "payram", "https://127.0.0.1").Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return result
}

func TestRecover_NothingChangedAndRunning(t *testing.T) {
	for _, code := range []string{"DOCKER_PULL_FAILED", "POLICY_FETCH_FAILED", "BACKUP_FAILED", "DISK_SPACE_LOW", "CONCURRENCY_BLOCKED", jobs.FailureDockerCreateFailed, jobs.FailureTargetNotAllowed} {
		t.Run(code, func(t *testing.T) {
			docker := &recordingDocker{running: true}
			result := runRecover(t, docker, func(j *jobs.Job) { j.FailureCode = code })

			if !result.Success || result.Action != ActionNone {
				t.Errorf("want success with action none, got %+v", result)
			}
			if !strings.HasPrefix(result.Message, "Nothing to recover: PayRam was not changed and is running") {
				t.Errorf("message does not say nothing changed: %s", result.Message)
			}
			if !strings.Contains(result.Message, "retry the update") {
				t.Errorf("message does not say to retry: %s", result.Message)
			}
			if result.Playbook == nil || result.Playbook.Code != code {
				t.Errorf("playbook missing for %s", code)
			}
			if len(docker.calls) != 1 || docker.calls[0] != "inspect payram" {
				t.Errorf("want one inspect of payram, got %v", docker.calls)
			}
			docker.assertOnlyInspected(t)
		})
	}
}

// Before this release `recover` stopped a healthy PayRam after DOCKER_PULL_FAILED.
func TestRecover_DockerPullFailedDoesNotStopPayRam(t *testing.T) {
	docker := &recordingDocker{running: true}
	result := runRecover(t, docker, func(j *jobs.Job) { j.FailureCode = "DOCKER_PULL_FAILED" })
	docker.assertOnlyInspected(t)
	if result.Action == "stopped_container" || strings.Contains(strings.ToLower(result.Message), "stopped") {
		t.Errorf("recover must not stop PayRam: %+v", result)
	}
}

func TestRecover_NothingChangedButStopped(t *testing.T) {
	docker := &recordingDocker{running: false}
	result := runRecover(t, docker, func(j *jobs.Job) {
		j.FailureCode = "BACKUP_FAILED"
		j.ContainerName = "payram-shop"
	})
	if result.Success || result.Action != ActionStartContainer {
		t.Errorf("want start_container, got %+v", result)
	}
	if !strings.Contains(result.Message, "docker start payram-shop") {
		t.Errorf("message does not say how to start the job's container: %s", result.Message)
	}
	docker.assertOnlyInspected(t)
}

func TestRecover_DockerUnreachable(t *testing.T) {
	docker := &recordingDocker{inspectErr: errors.New("Cannot connect to the Docker daemon")}
	result := runRecover(t, docker, func(j *jobs.Job) { j.FailureCode = "DOCKER_PULL_FAILED" })
	if result.Success || result.Action != ActionCheckDockerState {
		t.Errorf("want check_docker_state, got %+v", result)
	}
	docker.assertOnlyInspected(t)
}

func TestRecover_RolledBackIsNothingToRecover(t *testing.T) {
	for _, code := range []string{jobs.FailureUpgradeRolledBack, jobs.FailureUpgradeInterrupted} {
		docker := &recordingDocker{running: true}
		result := runRecover(t, docker, func(j *jobs.Job) {
			j.FailureCode = code
			j.CauseCode = "HEALTHCHECK_FAILED"
		})
		if !result.Success || !strings.Contains(result.Message, "already put PayRam back") {
			t.Errorf("%s: want nothing to recover, got %+v", code, result)
		}
		docker.assertOnlyInspected(t)
	}
}

func TestRecover_RefusesWhereAPersonMustLook(t *testing.T) {
	for _, code := range []string{jobs.FailureRollbackFailed, "HEALTHCHECK_FAILED", "VERSION_MISMATCH", "DOCKER_ERROR", "MIGRATION_FAILED", "SOMETHING_NEW"} {
		t.Run(code, func(t *testing.T) {
			docker := &recordingDocker{running: true}
			result := runRecover(t, docker, func(j *jobs.Job) { j.FailureCode = code })
			if result.Success || result.Action != ActionFollowPlaybook {
				t.Errorf("want refusal with follow_playbook, got %+v", result)
			}
			if result.Playbook == nil || len(result.Playbook.SSHSteps) == 0 {
				t.Fatalf("refusal must carry the playbook")
			}
			if !strings.Contains(result.Playbook.SSHSteps[0], "/api/v1/health") {
				t.Errorf("playbook must start with the health check: %s", result.Playbook.SSHSteps[0])
			}
			if len(docker.calls) != 0 {
				t.Errorf("recover touched Docker: %v", docker.calls)
			}
		})
	}
}

func TestRecover_ActiveUpdate(t *testing.T) {
	t.Run("fresh heartbeat: wait", func(t *testing.T) {
		docker := &recordingDocker{}
		result := runRecover(t, docker, func(j *jobs.Job) {
			j.State = jobs.JobStateExecuting
			j.UpdatedAt = time.Now().UTC()
		})
		if result.Success || result.Action != ActionWaitForUpdate {
			t.Errorf("want wait_for_update, got %+v", result)
		}
		if len(docker.calls) != 0 {
			t.Errorf("recover touched Docker: %v", docker.calls)
		}
	})
	t.Run("stale heartbeat: restart the updater", func(t *testing.T) {
		docker := &recordingDocker{}
		result := runRecover(t, docker, func(j *jobs.Job) {
			j.State = jobs.JobStateExecuting
			j.UpdatedAt = time.Now().UTC().Add(-10 * time.Minute)
		})
		if result.Action != ActionRestartUpdater || result.Code != "STALE_EXECUTING" {
			t.Errorf("want restart_updater with STALE_EXECUTING, got %+v", result)
		}
		if !strings.Contains(result.Message, "sudo systemctl restart payram-updater") {
			t.Errorf("message does not say how to restart the updater: %s", result.Message)
		}
		if len(docker.calls) != 0 {
			t.Errorf("recover touched Docker: %v", docker.calls)
		}
	})
}

func TestRecover_NoJobAndFinishedJob(t *testing.T) {
	docker := &recordingDocker{}
	if result := runRecover(t, docker, nil); !result.Success || result.Action != ActionNone {
		t.Errorf("no job: got %+v", result)
	}
	if result := runRecover(t, docker, func(j *jobs.Job) { j.State = jobs.JobStateReady }); !result.Success || result.Action != ActionNone {
		t.Errorf("ready job: got %+v", result)
	}
	if len(docker.calls) != 0 {
		t.Errorf("recover touched Docker: %v", docker.calls)
	}
}

// With the real docker runner, every docker command recover issues for every
// known failure code must be a read (docker inspect).
func TestRecover_RealRunnerOnlyInspects(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls.log")
	fakeDocker := filepath.Join(dir, "docker")
	script := "#!/bin/sh\necho \"$@\" >> '" + callLog + "'\necho true\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	runner := &dockerexec.Runner{DockerBin: fakeDocker, Logger: log.New(os.Stderr, "", 0)}

	codes := append(recovery.AllCodes(), "SOMETHING_NEW")
	for _, code := range codes {
		store := jobs.NewStore(t.TempDir())
		saveJob(t, store, func(j *jobs.Job) { j.FailureCode = code })
		if _, err := NewRecoverer(store, runner, "payram", "").Run(context.Background()); err != nil {
			t.Fatalf("%s: Run: %v", code, err)
		}
	}

	data, err := os.ReadFile(callLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read call log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("recover never asked Docker whether PayRam runs")
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "inspect ") {
			t.Errorf("recover ran a docker command that is not a read: docker %s", line)
		}
	}
}
