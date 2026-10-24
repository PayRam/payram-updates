package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/switchover"
)

// containerRollback is a switch of the PayRam container to an older version
// that is not final yet: a full recovery restores the database next, and only
// a restored database makes the older version the one to keep. Until Commit,
// the newer container is kept aside and Revert puts it back.
type containerRollback struct {
	sw    *switchover.Switch
	job   *jobs.Job
	store *jobs.Store
	stop  func()
}

// cliRecorder saves the rollback's progress in the job store, so the daemon
// (which refuses updates while it is active) and, if this command dies, the
// daemon's startup repair know about it.
type cliRecorder struct {
	store *jobs.Store
}

func (r cliRecorder) Save(job *jobs.Job) error { return r.store.Save(job) }
func (r cliRecorder) Log(line string) {
	r.store.AppendLog(line)
	log.Print(line)
}

// alwaysUp is the probe used while the older version runs on the newer
// database: it may not serve until the restore, so the switch only checks
// that its container comes up and stays up.
type alwaysUp struct{}

func (alwaysUp) Health(context.Context) (*coreclient.HealthResponse, error) {
	return &coreclient.HealthResponse{Status: "ok"}, nil
}
func (alwaysUp) Version(context.Context) (string, error) { return "", nil }

// beginContainerRollback switches PayRam's container to targetVersion the
// same way an update switches forward (create alongside, keep the current
// one aside, switch back on any failure). The caller holds the container
// lock. On error PayRam is back on the version it ran.
func beginContainerRollback(ctx context.Context, cfg *config.Config, targetVersion string) (*containerRollback, error) {
	name, err := discoverPayramContainer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("could not find PayRam's container, so nothing was changed: %w", err)
	}
	name = container.ContainerName(name)

	runtimeState, err := container.NewInspector(cfg.DockerBin, log.Default()).ExtractRuntimeState(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("could not read PayRam's container, so nothing was changed: %w", err)
	}
	if err := container.CheckReplaceable(runtimeState); err != nil {
		return nil, fmt.Errorf("nothing was changed: %w", err)
	}
	repo, currentTag := container.SplitImage(runtimeState.Image)
	tag := targetVersion
	if strings.HasSuffix(currentTag, "-arm64") && !strings.HasSuffix(tag, "-arm64") {
		tag += "-arm64"
	}

	runner := &dockerexec.Runner{DockerBin: cfg.DockerBin, Logger: log.Default(), StopTimeoutSeconds: 60}
	image := fmt.Sprintf("%s:%s", repo, tag)
	log.Printf("Downloading %s (PayRam keeps running meanwhile)", image)
	if err := runner.Pull(ctx, image); err != nil {
		return nil, fmt.Errorf("version %s could not be downloaded, so nothing was changed: %w", tag, err)
	}

	builder := container.NewDockerRunBuilder(log.Default())
	spec, err := builder.BuildContainerSpec(runtimeState, &manifest.Manifest{Image: manifest.Image{Repo: repo}}, tag)
	if err != nil {
		return nil, fmt.Errorf("could not build the container settings for %s, so nothing was changed: %w", tag, err)
	}

	store := jobs.NewStore(cfg.StateDir)
	job := jobs.NewJob(fmt.Sprintf("rollback-%d", time.Now().UnixNano()), jobs.JobModeManual, tag)
	job.ResolvedTarget = tag
	job.ContainerName = name
	job.FromVersion = currentTag
	job.State = jobs.JobStateExecuting
	job.Message = fmt.Sprintf("Rolling PayRam back to %s (payram-updater backup restore)", tag)
	if err := store.Save(job); err != nil {
		return nil, fmt.Errorf("could not record the rollback, so nothing was changed: %w", err)
	}

	r := &containerRollback{
		sw: &switchover.Switch{
			Docker: runner,
			Record: cliRecorder{store: store},
			// The older version gets as long as an update does to start.
			Timing: switchover.Timing{HealthTimeout: time.Duration(cfg.HealthCheckTimeoutSeconds) * time.Second},
		},
		job:   job,
		store: store,
		stop:  heartbeat(store),
	}

	err = r.sw.ClearLeftovers(ctx, name)
	if err == nil {
		err = r.sw.CreateNext(ctx, job, spec)
	}
	if err == nil {
		err = r.sw.Cutover(ctx, job, runtimeState.Image, container.FormatRestartPolicy(runtimeState.RestartPolicy))
	}
	if err == nil {
		_, err = r.sw.WaitHealthy(ctx, name, alwaysUp{}, "")
	}
	if err != nil {
		if revertErr := r.Revert(ctx); revertErr != nil {
			return nil, fmt.Errorf("rolling back to %s failed (%v), and switching back to %s failed too: %v", tag, err, currentTag, revertErr)
		}
		return nil, fmt.Errorf("rolling back to %s failed, so PayRam was switched back to %s: %w", tag, currentTag, err)
	}
	log.Printf("PayRam's container now runs %s; %s is kept aside until the database is restored", tag, switchover.PreviousName(name))
	return r, nil
}

// ContainerName is the PayRam container now running the older version.
func (r *containerRollback) ContainerName() string { return r.job.ContainerName }

// Commit makes the rollback final: the newer container is removed (its image
// stays) and the job is marked complete. It finishes even after Ctrl-C or a
// closed SSH session (the restore's context is cancelled by then).
func (r *containerRollback) Commit(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	defer r.stop()
	r.sw.Finish(ctx, r.job)
	r.finish(jobs.JobStateReady, "", "", fmt.Sprintf("Rollback to %s completed successfully", r.job.ResolvedTarget))
}

// Revert puts the newer container back (the database was not restored, so
// it still matches the newer version). It runs to the end even when ctx was
// cancelled: a Ctrl-C, a closed SSH session or a stop signal is exactly when
// PayRam must be switched back, not left half-switched.
func (r *containerRollback) Revert(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	defer r.stop()
	if err := r.sw.Restore(ctx, r.job); err != nil {
		r.finish(jobs.JobStateFailed, jobs.FailureRollbackFailed, "",
			fmt.Sprintf("Rolling back to %s failed and PayRam could not be switched back to %s: %v. PayRam may be down; contact PayRam support.", r.job.ResolvedTarget, r.job.FromVersion, err))
		return err
	}
	if _, err := r.sw.WaitHealthy(ctx, r.job.ContainerName, alwaysUp{}, ""); err != nil {
		r.finish(jobs.JobStateFailed, jobs.FailureRollbackFailed, "",
			fmt.Sprintf("PayRam was switched back to %s, but its container does not stay up: %v. Check `docker logs %s`.", r.job.FromVersion, err, r.job.ContainerName))
		return errors.New("the switched-back container does not stay up")
	}
	r.finish(jobs.JobStateFailed, jobs.FailureUpgradeRolledBack, "",
		fmt.Sprintf("The rollback to %s was not completed, so PayRam was switched back to %s and is running.", r.job.ResolvedTarget, r.job.FromVersion))
	return nil
}

func (r *containerRollback) finish(state jobs.JobState, code, cause, message string) {
	r.job.State = state
	r.job.FailureCode = code
	r.job.CauseCode = cause
	r.job.Message = message
	r.job.UpdatedAt = time.Now().UTC()
	if err := r.store.Save(r.job); err != nil {
		log.Printf("Warning: could not save the rollback's outcome: %v", err)
	}
	r.store.AppendLog(message)
}

// heartbeat keeps the saved job fresh while this command works, so the
// daemon and the installer see an update in progress, not a dead one.
func heartbeat(store *jobs.Store) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				store.Touch()
			}
		}
	}()
	stopped := false
	return func() {
		if !stopped {
			stopped = true
			close(done)
		}
	}
}
