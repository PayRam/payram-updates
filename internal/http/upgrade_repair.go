package http

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
)

// repairDockerWait is how often a starting updater that is waiting for Docker
// (to repair an interrupted update) says so in its log.
const repairDockerWait = 5 * time.Minute

// repairInterruptedUpgrade runs when the updater starts. If the last job is
// still marked as running, the updater that ran it stopped mid-update
// (restart, crash, reboot): no one else will ever finish it, and an "active"
// job would refuse every later update. The containers say how far it got:
//   - the new version is up, healthy and the right version: the update is
//     finished (old container removed, job marked complete);
//   - anything else: PayRam is put back on the previous container.
//
// Jobs from older updaters (no switch bookkeeping) get the best that can be
// done without it: PayRam's container is started again if it exists.
func (s *Server) repairInterruptedUpgrade(ctx context.Context) {
	job, err := s.jobStore.LoadLatest()
	if err != nil {
		logger.Error("Server", "repairInterruptedUpgrade", err)
		return
	}
	if job == nil || !job.IsActive() {
		return
	}
	// The slot can be held for a moment by a request that is about to be
	// refused (it sees this active job); a repair that gave up then would
	// leave the job active, and every later update refused, until the next
	// restart. So it waits for the slot.
	if !s.waitForUpgradeSlot(ctx) {
		return
	}
	s.repairing.Store(true)
	s.upgradeWg.Add(1)
	defer func() {
		s.repairing.Store(false)
		s.releaseUpgradeSlot()
		s.upgradeWg.Done()
	}()

	logger.Warnf("Server", "repairInterruptedUpgrade", "Update %s was left %s by an updater that stopped; putting PayRam right", job.JobID, job.State)
	s.jobStore.AppendLog(fmt.Sprintf("The updater restarted while this update was %s; checking PayRam's containers", strings.ToLower(string(job.State))))

	// Docker operations below must not be cut off by shutdown: a half-done
	// repair is worse than a slow one. ctx only ends the waiting.
	work := context.Background()
	if !s.waitForDocker(ctx, job) {
		return
	}
	release, ok := s.waitForContainerLock(ctx)
	if !ok {
		return
	}
	defer release()
	// A CLI restore may have held the lock and finished its own switch
	// meanwhile: work from the job as it is now.
	// (If that restore died mid-switch, its job is the one to repair.)
	fresh, err := s.jobStore.LoadLatest()
	if err != nil || fresh == nil || !fresh.IsActive() {
		return
	}
	job = fresh
	stopHeartbeat := s.startHeartbeat()
	defer stopHeartbeat()

	s.currentPolicyInit = s.fetchPolicyInitVersion(work)
	sw := s.newSwitch()

	if job.ContainerName == "" {
		s.repairJobFromOlderUpdater(work, job)
		return
	}

	if job.SwitchStage == jobs.SwitchStageSwitched || job.SwitchStage == jobs.SwitchStageVerified {
		target := baseVersionTag(job.ResolvedTarget)
		if state, err := s.dockerRunner.State(work, job.ContainerName); err == nil && state.Running {
			probe := s.newProbe(job.CoreBaseURL, job.ContainerName, s.shouldUseLegacyForTarget(s.currentPolicyInit, target))
			// Only what is left of the window: PayRam is down while a
			// broken new version is being waited for.
			sw.Timing.HealthTimeout = sw.HealthWindowLeft(job.SwitchedAt)
			_, err := sw.WaitHealthy(work, job.ContainerName, probe, target)
			sw.Timing.HealthTimeout = s.switchTiming.HealthTimeout
			if err == nil {
				s.jobStore.AppendLog(fmt.Sprintf("%s is up and healthy; finishing the update", target))
				sw.Finish(work, job)
				repo, tag := container.SplitImage(state.Image)
				s.finalizeUpgrade(work, job, repo, tag)
				return
			}
		}
	}

	s.putPayramBack(work, sw, job, jobs.FailureUpgradeInterrupted, "the updater was restarted in the middle of the update", stageReachedCutover(job.SwitchStage))
}

// waitForUpgradeSlot claims the update slot, retrying while it is briefly
// held. It returns false when the updater is shutting down.
func (s *Server) waitForUpgradeSlot(ctx context.Context) bool {
	for {
		err := s.claimUpgradeSlot()
		if err == nil {
			return true
		}
		if errors.Is(err, errShuttingDown) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// waitForDocker waits until Docker answers, however long that takes: the job
// stays active meanwhile, so if this updater stops first the next one repairs
// it. (Ending the job here would leave PayRam stopped for good: a failed job
// is never repaired, and the old container's restart policy is off.) It
// returns false only when the updater is shutting down.
func (s *Server) waitForDocker(ctx context.Context, job *jobs.Job) bool {
	started := time.Now()
	nextWarning := started.Add(repairDockerWait)
	for {
		if err := backup.CheckDockerDaemon(context.Background(), s.config.DockerBin); err == nil {
			return true
		}
		if time.Now().After(nextWarning) {
			logger.Warnf("Server", "waitForDocker", "Docker has not answered for %s; update %s is repaired once it does (start it with `sudo systemctl start docker`)", time.Since(started).Round(time.Minute), job.JobID)
			s.jobStore.AppendLog(fmt.Sprintf("Docker is not running (%s so far); PayRam is checked and put right as soon as it is. Start it with `sudo systemctl start docker`.", time.Since(started).Round(time.Minute)))
			nextWarning = time.Now().Add(repairDockerWait)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Second):
		}
	}
}

// waitForContainerLock waits while a CLI restore holds the container lock,
// and retries if the lock file cannot be opened: giving up would leave the job
// active and every later update refused.
func (s *Server) waitForContainerLock(ctx context.Context) (func(), bool) {
	warned := false
	for {
		release, err := jobs.AcquireContainerLock(s.config.StateDir)
		if err == nil {
			return release, true
		}
		if !errors.Is(err, jobs.ErrUpgradeLocked) && !warned {
			logger.Error("Server", "waitForContainerLock", err)
			warned = true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(10 * time.Second):
		}
	}
}

// repairJobFromOlderUpdater handles a job left by an updater older than the
// container switch. That updater stopped PayRam's programs for the backup,
// then stopped and REMOVED the old container before starting the new one, so
// the container may be running without its programs, stopped, or gone.
func (s *Server) repairJobFromOlderUpdater(ctx context.Context, job *jobs.Job) {
	name := s.config.TargetContainerName
	if name == "" {
		if discovered, err := s.discoverContainerName(ctx); err == nil {
			name = discovered
		} else {
			name = "payram"
		}
	}
	job.ContainerName = name

	state, err := s.dockerRunner.State(ctx, name)
	if err != nil {
		// PayRam's state is unknown here, so this is not "put back".
		s.finishFailed(job, jobs.FailureRollbackFailed, jobs.FailureUpgradeInterrupted, fmt.Sprintf("The previous updater stopped during the update, and container %s could not be checked (%v), so PayRam may be down. Run `docker ps -a` to see PayRam's container, or contact PayRam support.", name, err))
		return
	}
	if !state.Exists {
		s.finishFailed(job, jobs.FailureRollbackFailed, jobs.FailureUpgradeInterrupted, fmt.Sprintf(
			"The previous updater stopped during the update after removing PayRam's container, so there is no container named %s to start. PayRam's data is safe in its folders and volumes, and the backup is at %s. Re-run the PayRam installer to recreate the container on the same data, or contact PayRam support.",
			name, backupPathOrUnknown(job.BackupPath)))
		return
	}

	if !state.Running {
		s.jobStore.AppendLog(fmt.Sprintf("Starting %s", name))
		if err := s.dockerRunner.Start(ctx, name); err != nil {
			s.finishFailed(job, jobs.FailureRollbackFailed, jobs.FailureUpgradeInterrupted, fmt.Sprintf("The previous updater stopped during the update and %s could not be started again (%v). Check `docker logs %s`, or contact PayRam support.", name, err, name))
			return
		}
	} else if stopped := s.stoppedPrograms(ctx, name); len(stopped) > 0 {
		s.jobStore.AppendLog(fmt.Sprintf("Starting PayRam's programs again: %s", strings.Join(stopped, ", ")))
		if err := s.supervisorctlStart(ctx, name, stopped); err != nil {
			s.jobStore.AppendLog(fmt.Sprintf("Programs did not start (%v); restarting the container", err))
			if err := s.dockerRunner.Restart(ctx, name); err != nil {
				s.finishFailed(job, jobs.FailureRollbackFailed, jobs.FailureUpgradeInterrupted, fmt.Sprintf("The previous updater stopped during the update and PayRam's programs could not be started again (%v). Run `docker restart %s`, or contact PayRam support.", err, name))
				return
			}
		}
	}

	baseURL := s.currentCoreBaseURL(ctx, name)
	job.CoreBaseURL = baseURL
	running := "its current version"
	if version, _, err := s.resolveCoreVersionAt(ctx, baseURL, name, s.currentPolicyInit); err == nil && version != "" {
		running = version
		job.FromVersion = baseVersionTag(version)
	}
	probe := s.newProbe(baseURL, name, s.shouldUseLegacyForTarget(s.currentPolicyInit, job.FromVersion))
	if _, err := s.newSwitch().WaitHealthy(ctx, name, probe, ""); err != nil {
		s.finishFailed(job, jobs.FailureRollbackFailed, jobs.FailureUpgradeInterrupted, fmt.Sprintf("The previous updater stopped during the update. PayRam's container %s is running %s but not answering yet (%v). Check `docker logs %s`, or contact PayRam support.", name, running, err, name))
		return
	}
	s.finishFailed(job, jobs.FailureUpgradeInterrupted, "", fmt.Sprintf("The previous updater stopped during the update. PayRam is running %s and answering; retry the update.", running))
}

// stoppedPrograms lists PayRam programs (database excluded) that supervisor
// reports as stopped.
func (s *Server) stoppedPrograms(ctx context.Context, containerName string) []string {
	output, err := s.supervisorctlStatus(ctx, containerName)
	if err != nil {
		return nil
	}
	programs, _ := backup.ParseSupervisorStatus(output)
	return backup.AppPrograms(programs, s.config.SupervisorInclude, s.config.SupervisorExclude, "STOPPED", "EXITED")
}

func backupPathOrUnknown(path string) string {
	if path == "" {
		return "the updater's backup folder (`payram-updater backup list`)"
	}
	return path
}
