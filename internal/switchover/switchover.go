// Package switchover replaces the PayRam container with another version
// without ever leaving the merchant without one.
//
// The new container is created next to the running one (<name>-next, restart
// policy "no"), so a bad image, mount or network is found while PayRam still
// runs. Only then is the old container stopped and renamed aside
// (<name>-previous, restart policy "no"), the new one renamed to <name>, given
// the old one's restart policy and started. Until the new version has
// proven itself, the old container is kept; any failure puts it back.
//
// Restore never trusts the recorded stage alone: it reads which containers
// exist and works from that, so the same code repairs a switch that was cut
// off by a crash, a restart or a reboot.
package switchover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
)

// Docker is the part of the Docker CLI a switchover uses.
type Docker interface {
	// RunWithEnv runs a docker command with env added to its environment
	// (a container's secret values travel there, not on its command line).
	RunWithEnv(ctx context.Context, args []string, env []string) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Rename(ctx context.Context, from, to string) error
	UpdateRestartPolicy(ctx context.Context, name, policy string) error
	ConnectNetwork(ctx context.Context, network, name string) error
	State(ctx context.Context, name string) (dockerexec.ContainerState, error)
}

// Prober asks the running PayRam whether it is up and which version it runs.
type Prober interface {
	Health(ctx context.Context) (*coreclient.HealthResponse, error)
	Version(ctx context.Context) (string, error)
}

// Recorder persists the job and its log. Save must succeed before a step
// that changes a container, so a restarted updater knows what to undo.
type Recorder interface {
	Save(job *jobs.Job) error
	Log(line string)
}

// Timing sets how patient the health check is.
type Timing struct {
	// HealthTimeout is how long a started container gets to answer its
	// health check. PayRam runs its database migrations before it answers,
	// which can take minutes on a large database.
	HealthTimeout time.Duration
	// StableFor is how long it must keep answering once it first does.
	StableFor time.Duration
	// PollInterval is the time between health checks.
	PollInterval time.Duration
	// MaxRestarts is how many times the container may restart while
	// starting before it is judged broken.
	MaxRestarts int
}

// DefaultTiming is used for every zero field of Switch.Timing.
var DefaultTiming = Timing{
	HealthTimeout: 10 * time.Minute,
	StableFor:     30 * time.Second,
	PollInterval:  2 * time.Second,
	MaxRestarts:   3,
}

// Programs runs PayRam's own programs (everything supervisord runs except
// PostgreSQL) inside a running container.
type Programs interface {
	// Running lists the PayRam programs that are running or starting. It
	// returns ErrNoSupervisor when the container has no supervisorctl.
	Running(ctx context.Context, container string) ([]string, error)
	Stop(ctx context.Context, container string, programs []string) error
	// Start starts programs; one that already runs is fine.
	Start(ctx context.Context, container string, programs []string) error
}

// ErrNoSupervisor means the container has no supervisorctl, so its programs
// cannot be stopped one by one.
var ErrNoSupervisor = errors.New("supervisorctl is not available in the container")

// Switch performs one container switchover.
type Switch struct {
	Docker Docker
	Record Recorder
	Timing Timing
	// Programs starts and stops PayRam's programs inside a container.
	// Restore starts the ones the backup stopped when the container itself
	// was never stopped; CheckDatabase stops the new version's while it
	// reads the database. Nil: programs are never stopped or started.
	Programs Programs
	// Database reaches PayRam's database for CheckDatabase and
	// RestoreDatabase. Nil: the database cannot be checked.
	Database Database
}

// StepError is a failed switch step. Code is the step's failure code
// (DOCKER_CREATE_FAILED, DOCKER_ERROR, HEALTHCHECK_FAILED, ...). Changed says
// whether PayRam's container had already been stopped, i.e. whether a restore
// is a switch back rather than just tidying up.
type StepError struct {
	Code    string
	Message string
	Changed bool
}

func (e *StepError) Error() string { return e.Message }

// NextName is the name the new version's container is created under.
func NextName(name string) string { return name + "-next" }

// PreviousName is the name the old container is kept under during a switch.
func PreviousName(name string) string { return name + "-previous" }

// FailedName is the name a new container that failed is kept under, so its
// logs can be read. It never restarts on its own.
func FailedName(name string) string { return name + "-failed" }

func (w *Switch) timing() Timing {
	t := w.Timing
	if t.HealthTimeout <= 0 {
		t.HealthTimeout = DefaultTiming.HealthTimeout
	}
	if t.StableFor < 0 {
		t.StableFor = 0
	} else if t.StableFor == 0 {
		t.StableFor = DefaultTiming.StableFor
	}
	if t.PollInterval <= 0 {
		t.PollInterval = DefaultTiming.PollInterval
	}
	if t.MaxRestarts <= 0 {
		t.MaxRestarts = DefaultTiming.MaxRestarts
	}
	return t
}

func (w *Switch) logf(format string, args ...interface{}) {
	w.Record.Log(fmt.Sprintf(format, args...))
}

// save persists the job; a switch must not change a container it could not
// record, so callers stop on error.
func (w *Switch) save(job *jobs.Job) error {
	job.UpdatedAt = time.Now().UTC()
	if err := w.Record.Save(job); err != nil {
		return &StepError{Code: "STATE_SAVE_FAILED", Message: fmt.Sprintf("Could not save the update's progress (%v), so nothing was changed. Check that the updater's state directory is writable and has free space.", err)}
	}
	return nil
}

// ClearLeftovers tidies containers an earlier, finished update left behind,
// before a new one starts. A leftover <name>-previous may be the only good
// copy of an older PayRam, so it is never deleted: it is renamed with a
// timestamp and kept from restarting. A <name>-next or <name>-failed that is
// not running is removed (all PayRam data lives in volumes, not containers).
// A leftover that is running is unexpected; the update is refused.
func (w *Switch) ClearLeftovers(ctx context.Context, name string) error {
	for _, leftover := range []string{NextName(name), FailedName(name), PreviousName(name)} {
		state, err := w.Docker.State(ctx, leftover)
		if err != nil {
			return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not check container %s: %v", leftover, err)}
		}
		if !state.Exists {
			continue
		}
		if state.Running {
			return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("A container named %s is running next to %s. Nothing was changed. Check `docker ps` and stop whichever one is not your PayRam before updating.", leftover, name)}
		}
		if leftover == PreviousName(name) {
			kept := fmt.Sprintf("%s-%s", leftover, time.Now().UTC().Format("20060102150405"))
			if err := w.Docker.UpdateRestartPolicy(ctx, leftover, "no"); err != nil {
				return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not set aside the leftover container %s: %v", leftover, err)}
			}
			if err := w.Docker.Rename(ctx, leftover, kept); err != nil {
				return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not set aside the leftover container %s: %v", leftover, err)}
			}
			w.logf("Kept the container an earlier update left behind as %s (remove it with `docker rm %s` once you no longer need it)", kept, kept)
			continue
		}
		if err := w.Docker.Remove(ctx, leftover); err != nil {
			return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not remove the leftover container %s: %v", leftover, err)}
		}
		w.logf("Removed the container %s an earlier update left behind", leftover)
	}
	return nil
}

// CreateNext creates the new version's container from spec next to the
// running one, as NextName(job.ContainerName), with restart policy "no" until
// Cutover starts it. Nothing about the running PayRam changes; on error the
// half-made container is removed.
func (w *Switch) CreateNext(ctx context.Context, job *jobs.Job, spec *container.ContainerSpec) error {
	next := NextName(job.ContainerName)
	job.NextContainer = next
	job.SwitchStage = jobs.SwitchStagePrepared
	if err := w.save(job); err != nil {
		return err
	}

	w.logf("Creating the new version's container as %s (PayRam keeps running meanwhile)", next)
	if err := w.Docker.RunWithEnv(ctx, spec.CreateArgs(next), spec.Env); err != nil {
		w.removeQuietly(ctx, next)
		return &StepError{Code: jobs.FailureDockerCreateFailed, Message: fmt.Sprintf("The new version's container could not be created, so nothing was changed: %s", shortError(err))}
	}
	for _, network := range spec.ExtraNetworks {
		if err := w.Docker.ConnectNetwork(ctx, network, next); err != nil {
			w.removeQuietly(ctx, next)
			return &StepError{Code: jobs.FailureDockerCreateFailed, Message: fmt.Sprintf("The new version's container could not join network %s, so nothing was changed: %s", network, shortError(err))}
		}
	}
	w.logf("New container %s created", next)
	return nil
}

// Cutover stops the running container, keeps it as PreviousName with its
// restart policy switched off, and starts the new one under the original name
// with the old one's restart policy. currentImage and currentRestartPolicy
// describe the running container, so Restore can put it back exactly.
func (w *Switch) Cutover(ctx context.Context, job *jobs.Job, currentImage, currentRestartPolicy string) error {
	name := job.ContainerName
	next := NextName(name)
	previous := PreviousName(name)

	job.PreviousContainer = previous
	job.PreviousImage = currentImage
	job.PreviousRestartPolicy = currentRestartPolicy
	job.SwitchStage = jobs.SwitchStageStopped
	if err := w.save(job); err != nil {
		return err
	}

	// The old container must not start by itself once it is stopped: with
	// "always", Docker would start it again when it restarts, next to the
	// new one, and two PayRam containers on one data folder run two Postgres
	// servers on the same files. So its policy goes off before the stop.
	// (Restore puts the policy back on every failure path from here.)
	if err := w.Docker.UpdateRestartPolicy(ctx, name, "no"); err != nil {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not prepare PayRam's container for the switch: %s", shortError(err))}
	}
	w.logf("Stopping %s", name)
	if err := w.Docker.Stop(ctx, name); err != nil {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("PayRam could not be stopped for the update: %s", shortError(err))}
	}

	// From here PayRam is down until the new or the old container runs.
	if err := w.Docker.Rename(ctx, name, previous); err != nil {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not set the old container aside: %s", shortError(err)), Changed: true}
	}
	job.SwitchStage = jobs.SwitchStageSwitched
	if err := w.save(job); err != nil {
		// The containers already say how far this got; Restore reads them.
		w.logf("Warning: %v", err)
	}
	if err := w.Docker.Rename(ctx, next, name); err != nil {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not put the new container in place: %s", shortError(err)), Changed: true}
	}
	if err := w.Docker.UpdateRestartPolicy(ctx, name, currentRestartPolicy); err != nil {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not give the new container PayRam's restart policy (%s): %s", currentRestartPolicy, shortError(err)), Changed: true}
	}
	w.logf("Starting the new version as %s (the old one is kept as %s)", name, previous)
	job.SwitchedAt = time.Now().UTC()
	if err := w.save(job); err != nil {
		w.logf("Warning: %v", err)
	}
	if err := w.Docker.Start(ctx, name); err != nil {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("The new version's container did not start: %s", shortError(err)), Changed: true}
	}
	return nil
}

// HealthWindowLeft is how much of the health check's window is left for a
// container started at startedAt, never less than a minute (an updater that
// restarted mid-check still looks at the new version before deciding).
func (w *Switch) HealthWindowLeft(startedAt time.Time) time.Duration {
	window := w.timing().HealthTimeout
	if startedAt.IsZero() {
		return window
	}
	left := window - time.Since(startedAt)
	if left < time.Minute {
		left = time.Minute
	}
	return left
}

// WaitHealthy waits until the container answers its health check, runs
// expectVersion (when given) and keeps answering for StableFor. It gives up
// early when the container stops or keeps restarting. It returns the last
// health answer, whose worker list the caller may report.
func (w *Switch) WaitHealthy(ctx context.Context, name string, probe Prober, expectVersion string) (*coreclient.HealthResponse, error) {
	t := w.timing()
	deadline := time.Now().Add(t.HealthTimeout)
	w.logf("Waiting for PayRam to answer its health check (up to %s; it runs database migrations first)", t.HealthTimeout)

	initial, err := w.Docker.State(ctx, name)
	if err != nil {
		return nil, &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Could not read container %s: %s", name, shortError(err))}
	}

	var health *coreclient.HealthResponse
	attempt := 0
	for {
		attempt++
		if stepErr := w.checkContainerAlive(ctx, name, initial.RestartCount, t.MaxRestarts); stepErr != nil {
			return nil, stepErr
		}
		health, err = probeHealth(ctx, probe, t.PollInterval)
		if err == nil && health.Status == "ok" && (health.DB == "" || health.DB == "ok") {
			w.logf("PayRam answered its health check (attempt %d)", attempt)
			break
		}
		if attempt == 1 || attempt%15 == 0 {
			w.logf("Not answering yet (%s); %s left", describeHealth(health, err), time.Until(deadline).Round(time.Second))
		}
		if time.Now().After(deadline) {
			return nil, &StepError{Code: "HEALTHCHECK_FAILED", Message: fmt.Sprintf("PayRam did not answer its health check within %s (last answer: %s)", t.HealthTimeout, describeHealth(health, err))}
		}
		if err := sleep(ctx, t.PollInterval); err != nil {
			return nil, &StepError{Code: "HEALTHCHECK_FAILED", Message: "The update was cancelled while waiting for PayRam"}
		}
	}

	if expectVersion != "" {
		var running string
		var verr error
		for tries := 0; ; tries++ {
			running, verr = probeVersion(ctx, probe, t.PollInterval)
			if verr == nil && running != "" {
				break
			}
			if tries >= 2 && time.Now().After(deadline) {
				return nil, &StepError{Code: "VERSION_MISMATCH", Message: fmt.Sprintf("PayRam is up but did not say which version it runs: %s", shortError(verr))}
			}
			if err := sleep(ctx, t.PollInterval); err != nil {
				return nil, &StepError{Code: "VERSION_MISMATCH", Message: "The update was cancelled while checking the version"}
			}
		}
		if normalizeVersion(running) != normalizeVersion(expectVersion) {
			return nil, &StepError{Code: "VERSION_MISMATCH", Message: fmt.Sprintf("PayRam is up but runs version %s, not %s", running, expectVersion)}
		}
		w.logf("PayRam runs version %s", running)
	}

	if t.StableFor > 0 {
		w.logf("Checking that PayRam stays up for %s", t.StableFor)
		// Restarts while booting were tolerated above; from the first good
		// answer on, any restart means it is not stable.
		answered := initial
		if state, err := w.Docker.State(ctx, name); err == nil && state.Exists {
			answered = state
		}
		misses := 0
		stableUntil := time.Now().Add(t.StableFor)
		for time.Now().Before(stableUntil) {
			if err := sleep(ctx, t.PollInterval); err != nil {
				return nil, &StepError{Code: "HEALTHCHECK_FAILED", Message: "The update was cancelled while checking PayRam"}
			}
			if stepErr := w.checkContainerAlive(ctx, name, answered.RestartCount, 1); stepErr != nil {
				return nil, stepErr
			}
			answer, err := probeHealth(ctx, probe, t.PollInterval)
			if err == nil && answer.Status == "ok" {
				health = answer
				misses = 0
				continue
			}
			misses++
			if misses >= 3 {
				return nil, &StepError{Code: "HEALTHCHECK_FAILED", Message: fmt.Sprintf("PayRam answered once, then stopped answering (%s)", describeHealth(answer, err))}
			}
		}
	}
	return health, nil
}

// checkContainerAlive fails fast when the container has stopped or keeps
// restarting, instead of waiting out the whole health timeout.
func (w *Switch) checkContainerAlive(ctx context.Context, name string, initialRestarts, maxRestarts int) *StepError {
	state, err := w.Docker.State(ctx, name)
	if err != nil {
		return nil // a hiccup reading state is not a verdict; the health check decides
	}
	if !state.Exists {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("Container %s disappeared while starting", name)}
	}
	if !state.Running && state.Status != "restarting" {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("The new version's container stopped while starting (status %s, exit code %d)", state.Status, state.ExitCode)}
	}
	if state.RestartCount-initialRestarts >= maxRestarts {
		return &StepError{Code: "DOCKER_ERROR", Message: fmt.Sprintf("The new version's container keeps restarting (%d restarts)", state.RestartCount-initialRestarts)}
	}
	return nil
}

// Finish ends a successful switch: the old container is no longer needed and
// is removed (its image stays, for a manual rollback).
func (w *Switch) Finish(ctx context.Context, job *jobs.Job) {
	job.SwitchStage = jobs.SwitchStageVerified
	if err := w.save(job); err != nil {
		w.logf("Warning: %v", err)
	}
	previous := PreviousName(job.ContainerName)
	if state, err := w.Docker.State(ctx, previous); err == nil && state.Exists {
		if err := w.Docker.Remove(ctx, previous); err != nil {
			w.logf("Warning: could not remove the old container %s (%s); remove it later with `docker rm %s`", previous, shortError(err), previous)
		} else {
			w.logf("Removed the old container %s", previous)
		}
	}
	w.removeQuietly(ctx, NextName(job.ContainerName))
}

// Restore puts the previous container back as job.ContainerName and starts
// it, from whatever point the switch reached:
//   - before the stop: the container still runs; programs stopped for the
//     backup are started again;
//   - stopped, not yet renamed: the container is started again;
//   - renamed aside: the new container (if any) is stopped and kept as
//     FailedName, the old one gets its name and restart policy back, starts.
//
// The half-made <name>-next is removed in every case. Restore does not wait
// for health; callers do that with WaitHealthy.
func (w *Switch) Restore(ctx context.Context, job *jobs.Job) error {
	name := job.ContainerName
	if name == "" {
		return fmt.Errorf("the job does not say which container it was updating")
	}
	previous := PreviousName(name)
	failed := FailedName(name)

	job.SwitchStage = jobs.SwitchStageReverting
	if err := w.save(job); err != nil {
		w.logf("Warning: %v", err)
	}

	prevState, err := w.Docker.State(ctx, previous)
	if err != nil {
		return fmt.Errorf("could not read container %s: %w", previous, err)
	}
	current, err := w.Docker.State(ctx, name)
	if err != nil {
		return fmt.Errorf("could not read container %s: %w", name, err)
	}

	startedFresh := false
	if prevState.Exists {
		if current.Exists {
			if err := w.StopFailedVersion(ctx, name); err != nil {
				return err
			}
			w.logf("Keeping the new version's container as %s for support", failed)
			w.removeQuietly(ctx, failed)
			if err := w.Docker.Rename(ctx, name, failed); err != nil {
				return fmt.Errorf("could not move the new container aside: %w", err)
			}
		}
		if err := w.Docker.Rename(ctx, previous, name); err != nil {
			return fmt.Errorf("could not give the old container its name back: %w", err)
		}
		w.logf("Old container is %s again", name)
		startedFresh = true
	} else if !current.Exists {
		return fmt.Errorf("neither %s nor %s exists; the previous PayRam container is missing", name, previous)
	} else if !current.Running {
		startedFresh = true
	}

	// The restart policy is put back before the start, and retried after
	// it: failing to restore it must not keep PayRam down.
	policyRestored := job.PreviousRestartPolicy == ""
	if !policyRestored {
		policyRestored = w.Docker.UpdateRestartPolicy(ctx, name, job.PreviousRestartPolicy) == nil
	}
	defer func() {
		if policyRestored {
			return
		}
		if err := w.Docker.UpdateRestartPolicy(ctx, name, job.PreviousRestartPolicy); err != nil {
			w.logf("Warning: PayRam runs, but its restart policy could not be set back to %s (%s). Set it with `docker update --restart %s %s`, or it will not start by itself after a reboot.", job.PreviousRestartPolicy, shortError(err), job.PreviousRestartPolicy, name)
		}
	}()
	if startedFresh {
		w.logf("Starting %s", name)
		if err := w.Docker.Start(ctx, name); err != nil {
			return fmt.Errorf("could not start %s: %w", name, err)
		}
	} else if len(job.QuiescedPrograms) > 0 && w.Programs != nil {
		w.logf("Starting PayRam's programs again: %s", strings.Join(job.QuiescedPrograms, ", "))
		if err := w.Programs.Start(ctx, name, job.QuiescedPrograms); err != nil {
			w.logf("Programs did not start (%s); restarting the container instead", shortError(err))
			if err := w.Docker.Stop(ctx, name); err != nil {
				return fmt.Errorf("could not restart %s: %w", name, err)
			}
			if err := w.Docker.Start(ctx, name); err != nil {
				return fmt.Errorf("could not restart %s: %w", name, err)
			}
		}
	}
	w.removeQuietly(ctx, NextName(name))
	return nil
}

// StopFailedVersion stops the new version's container (named name, after a
// cutover) so that it cannot start again by itself: its restart policy goes
// off before the stop, as Cutover does for the old container. In the other
// order, a Docker restart between the two would start it again next to the
// old one, and two PayRam containers on one data folder run two Postgres
// servers on the same files.
func (w *Switch) StopFailedVersion(ctx context.Context, name string) error {
	if err := w.Docker.UpdateRestartPolicy(ctx, name, "no"); err != nil {
		return fmt.Errorf("could not stop the new version's container %s from restarting: %w", name, err)
	}
	w.logf("Stopping the new version's container %s", name)
	if err := w.Docker.Stop(ctx, name); err != nil {
		return fmt.Errorf("could not stop the new version's container %s: %w", name, err)
	}
	return nil
}

func (w *Switch) removeQuietly(ctx context.Context, name string) {
	if state, err := w.Docker.State(ctx, name); err == nil && state.Exists && !state.Running {
		if err := w.Docker.Remove(ctx, name); err != nil {
			w.logf("Warning: could not remove %s: %s", name, shortError(err))
		}
	}
}

func probeHealth(ctx context.Context, probe Prober, timeout time.Duration) (*coreclient.HealthResponse, error) {
	if timeout < 3*time.Second {
		timeout = 3 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return probe.Health(callCtx)
}

func probeVersion(ctx context.Context, probe Prober, timeout time.Duration) (string, error) {
	if timeout < 3*time.Second {
		timeout = 3 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return probe.Version(callCtx)
}

func describeHealth(health *coreclient.HealthResponse, err error) string {
	switch {
	case err != nil:
		return shortError(err)
	case health == nil:
		return "no answer"
	case health.DB != "" && health.DB != "ok":
		return fmt.Sprintf("status %s, database %s", health.Status, health.DB)
	default:
		return fmt.Sprintf("status %s", health.Status)
	}
}

func normalizeVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	return strings.TrimSuffix(v, "-arm64")
}

// shortError keeps error text readable in a job message: docker output can be
// long, and the last line usually says what went wrong.
func shortError(err error) string {
	if err == nil {
		return "unknown error"
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 400 {
		msg = msg[:400] + "..."
	}
	return msg
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
