package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/corecompat"
	"github.com/payram/payram-updater/internal/diskspace"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/switchover"
)

// pullTimeout bounds an image download: a stalled registry connection must
// not hold the update, and the update slot, forever.
const pullTimeout = 30 * time.Minute

// runUpgrade is the update, in order. Nothing about the running PayRam
// changes before the cutover (step 5), and every failure from there on
// switches back to the previous container:
//  1. find the container and how to reach its API
//  2. build the new container's settings from the running one
//  3. pull the image and create the new container next to the running one
//  4. stop PayRam's programs and back up the database
//  5. stop the old container, keep it aside, start the new one
//  6. wait until the new version is healthy, runs the right version, stays up
//  7. remove the old container
//
// A breakpoint update runs steps 3-7 twice (stepping stone, then target),
// with one backup before the first hop.
func (s *Server) runUpgrade(ctx context.Context, job *jobs.Job, plan *UpgradePlan, isDryRun bool) {
	manifestData := plan.Manifest
	imageRepo := manifestData.Image.Repo
	policyInit := plan.policyInitVersion()
	if plan.policyData == nil {
		policyInit = s.fetchPolicyInitVersion(ctx)
	}
	s.currentPolicyInit = policyInit

	containerName, ok := s.resolveTargetContainer(ctx, job, manifestData)
	if !ok {
		return
	}
	containerName = container.ContainerName(containerName)
	job.ContainerName = containerName

	sw := s.newSwitch()
	if err := sw.ClearLeftovers(ctx, containerName); err != nil {
		s.failStep(ctx, sw, job, err)
		return
	}

	coreURL, how, err := container.FindCoreBaseURL(ctx, s.config.DockerBin, containerName, s.config.CoreBaseURL, logger.StdLogger())
	if err != nil {
		s.failBeforeChange(job, "PAYRAM_CORE_PORT_NOT_FOUND", fmt.Sprintf("The updater could not find how to reach PayRam: %v", err))
		return
	}
	job.CoreBaseURL = coreURL
	s.rememberCoreBaseURL(coreURL)
	switch how {
	case container.CoreURLAnswered:
		s.jobStore.AppendLog(fmt.Sprintf("PayRam answers at %s", coreURL))
	case container.CoreURLGuessed:
		s.jobStore.AppendLog(fmt.Sprintf("PayRam is not answering right now; the new version will be checked at %s", coreURL))
	default:
		s.jobStore.AppendLog(fmt.Sprintf("Using CORE_BASE_URL %s", coreURL))
	}
	if from, _, err := s.resolveCoreVersionAt(ctx, coreURL, containerName, policyInit); err == nil {
		job.FromVersion = baseVersionTag(from)
	}

	hops := plan.hops()
	current, spec, tag, ok := s.prepareHop(ctx, job, containerName, manifestData, hops[0], plan.ArchSupport)
	if !ok {
		return
	}

	if isDryRun {
		s.executeDryRun(job, imageRepo, tag, containerName, spec)
		return
	}

	if err := s.preflightChecks(ctx, job, containerName); err != nil {
		s.failStep(ctx, sw, job, err)
		return
	}

	for i, hop := range hops {
		if i > 0 {
			if current, spec, tag, ok = s.prepareHop(ctx, job, containerName, manifestData, hop, plan.ArchSupport); !ok {
				return
			}
		}
		if len(hops) > 1 {
			s.jobStore.AppendLog(fmt.Sprintf("Breakpoint update, step %d of %d: %s", i+1, len(hops), baseVersionTag(tag)))
		}
		if !s.runHop(ctx, sw, job, current, spec, imageRepo, tag, policyInit, i == 0) {
			return
		}
	}

	s.finalizeUpgrade(ctx, job, imageRepo, tag)
}

// prepareHop reads the running container and builds the next version's
// container settings from it. Nothing is changed.
func (s *Server) prepareHop(ctx context.Context, job *jobs.Job, containerName string, manifestData *manifest.Manifest, baseTag string, archSupport map[string]string) (*container.RuntimeState, *container.ContainerSpec, string, bool) {
	s.jobStore.AppendLog("Reading the running container's settings...")
	inspector := container.NewInspector(s.config.DockerBin, logger.StdLogger())
	runtimeState, err := inspector.ExtractRuntimeState(ctx, containerName)
	if err != nil {
		s.failBeforeChange(job, "RUNTIME_INSPECTION_FAILED", fmt.Sprintf("Could not read the PayRam container's settings: %v", err))
		return nil, nil, "", false
	}
	if repo, _ := container.SplitImage(runtimeState.Image); !s.isAllowedImageRepo(repo) {
		s.failBeforeChange(job, jobs.FailureTargetNotAllowed, fmt.Sprintf("Container %s runs %s, which is not a PayRam image; the updater only replaces PayRam containers", containerName, runtimeState.Image))
		return nil, nil, "", false
	}
	s.jobStore.AppendLog(fmt.Sprintf("Runtime state extracted: %d ports, %d mounts, %d env vars",
		len(runtimeState.Ports), len(runtimeState.Mounts), len(runtimeState.Env)))

	tag := s.applyArchSuffix(job, runtimeState.ImageTag, baseVersionTag(baseTag), archSupport)

	builder := container.NewDockerRunBuilder(logger.StdLogger())
	spec, err := builder.BuildContainerSpec(runtimeState, manifestData, tag)
	if err != nil {
		s.failBeforeChange(job, "DOCKER_RUN_BUILD_FAILED", fmt.Sprintf("Could not build the new container's settings: %v", err))
		return nil, nil, "", false
	}
	s.jobStore.AppendLog("New container settings built (same ports, mounts, settings and networks)")
	return runtimeState, spec, tag, true
}

// runHop switches the container to one version: pull, create alongside,
// back up (first hop only), cut over, verify, finish. It returns false after
// failing the job, with PayRam put back.
func (s *Server) runHop(ctx context.Context, sw *switchover.Switch, job *jobs.Job, current *container.RuntimeState, spec *container.ContainerSpec, imageRepo, tag, policyInit string, firstHop bool) bool {
	name := job.ContainerName

	if err := s.pullUpgradeImage(ctx, job, imageRepo, tag); err != nil {
		s.failStep(ctx, sw, job, err)
		return false
	}

	s.setProgress(job, jobs.JobStateExecuting, fmt.Sprintf("Preparing %s", baseVersionTag(tag)))
	createArgs := container.NewDockerRunBuilder(logger.StdLogger()).BuildCreateArgs(spec, switchover.NextName(name))
	if err := sw.CreateNext(ctx, job, createArgs, spec.ExtraNetworks); err != nil {
		s.failStep(ctx, sw, job, err)
		return false
	}
	if err := checkNewPortsFree(current, spec); err != nil {
		s.failStep(ctx, sw, job, err)
		return false
	}

	if firstHop {
		if err := s.backUpBeforeSwitch(ctx, job, name, tag); err != nil {
			s.failStep(ctx, sw, job, err)
			return false
		}
	}

	s.setProgress(job, jobs.JobStateExecuting, fmt.Sprintf("Switching to %s", baseVersionTag(tag)))
	if err := sw.Cutover(ctx, job, current.Image, container.FormatRestartPolicy(current.RestartPolicy)); err != nil {
		s.failStep(ctx, sw, job, err)
		return false
	}

	s.setProgress(job, jobs.JobStateVerifying, fmt.Sprintf("Checking %s", baseVersionTag(tag)))
	probe := s.newProbe(job.CoreBaseURL, name, s.shouldUseLegacyForTarget(policyInit, baseVersionTag(tag)))
	health, err := sw.WaitHealthy(ctx, name, probe, baseVersionTag(tag))
	if err != nil {
		s.failStep(ctx, sw, job, err)
		return false
	}
	s.logWorkerStates(health)

	sw.Finish(ctx, job)
	job.FromVersion = baseVersionTag(tag)
	return true
}

// failStep ends the job after a failed step and puts PayRam back.
func (s *Server) failStep(ctx context.Context, sw *switchover.Switch, job *jobs.Job, err error) {
	code, reason, changed := "DOCKER_ERROR", err.Error(), false
	var stepErr *switchover.StepError
	if errors.As(err, &stepErr) {
		code, reason, changed = stepErr.Code, stepErr.Message, stepErr.Changed
	}
	switch job.SwitchStage {
	case jobs.SwitchStageSwitched, jobs.SwitchStageVerified, jobs.SwitchStageReverting:
		changed = true
	}
	s.putPayramBack(ctx, sw, job, code, reason, changed)
}

// stageReachedCutover reports whether the switch may have stopped PayRam.
func stageReachedCutover(stage jobs.SwitchStage) bool {
	switch stage {
	case jobs.SwitchStageStopped, jobs.SwitchStageSwitched, jobs.SwitchStageVerified, jobs.SwitchStageReverting:
		return true
	}
	return false
}

// putPayramBack undoes whatever the switch did, checks that PayRam answers
// again, and ends the job with a message saying where PayRam is:
//   - nothing had changed: the step's code, "PayRam was not changed";
//   - the old container had been stopped: UPGRADE_ROLLED_BACK, with the
//     step's code as CauseCode, "switched back to <version>";
//   - switching back failed or PayRam does not answer: ROLLBACK_FAILED.
func (s *Server) putPayramBack(ctx context.Context, sw *switchover.Switch, job *jobs.Job, causeCode, reason string, changed bool) {
	from := job.FromVersion
	if from == "" {
		from = "the previous version"
	}
	target := baseVersionTag(job.ResolvedTarget)
	s.jobStore.AppendLog(fmt.Sprintf("Step failed (%s): %s", causeCode, reason))

	if job.SwitchStage != jobs.SwitchStageNone {
		s.setProgress(job, jobs.JobStateVerifying, fmt.Sprintf("Putting PayRam %s back", from))
		if err := sw.Restore(ctx, job); err != nil {
			s.finishFailed(job, jobs.FailureRollbackFailed, causeCode, fmt.Sprintf(
				"The update to %s failed (%s), and PayRam could not be switched back to %s automatically: %v. PayRam may be down. Follow the recovery steps, or contact PayRam support with the updater log.",
				target, reason, from, err))
			return
		}
		probe := s.newProbe(job.CoreBaseURL, job.ContainerName, s.shouldUseLegacyForTarget(s.currentPolicyInit, job.FromVersion))
		if _, err := sw.WaitHealthy(ctx, job.ContainerName, probe, ""); err != nil {
			if changed {
				s.finishFailed(job, jobs.FailureRollbackFailed, causeCode, fmt.Sprintf(
					"The update to %s failed (%s). PayRam was switched back to %s, but it is not answering yet (%v). Check `docker logs %s`, or contact PayRam support with the updater log.",
					target, reason, from, err, job.ContainerName))
			} else {
				s.finishFailed(job, causeCode, "", fmt.Sprintf(
					"%s. PayRam was not changed, but it is not answering its health check right now (%v). Check `docker logs %s`.",
					sentence(reason), err, job.ContainerName))
			}
			return
		}
	}

	if changed {
		s.finishFailed(job, jobs.FailureUpgradeRolledBack, causeCode, fmt.Sprintf(
			"The update to %s did not complete (%s), so PayRam was switched back to %s and is running. Nothing was removed from the database. The new version's container is kept as %s for PayRam support.",
			target, reason, from, switchover.FailedName(job.ContainerName)))
		return
	}
	s.finishFailed(job, causeCode, "", fmt.Sprintf("%s. PayRam was not changed and keeps running %s.", sentence(reason), from))
}

// failBeforeChange ends a job that failed before anything was touched.
func (s *Server) failBeforeChange(job *jobs.Job, code, reason string) {
	s.finishFailed(job, code, "", fmt.Sprintf("%s. Nothing was changed.", sentence(reason)))
}

func (s *Server) finishFailed(job *jobs.Job, code, cause, message string) {
	job.State = jobs.JobStateFailed
	job.FailureCode = code
	job.CauseCode = cause
	job.Message = message
	job.UpdatedAt = time.Now().UTC()
	if err := s.jobStore.Save(job); err != nil {
		logger.Error("Server", "finishFailed", err)
	}
	s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", code, message))
}

// setProgress records what the job is doing now.
func (s *Server) setProgress(job *jobs.Job, state jobs.JobState, message string) {
	job.State = state
	job.Message = message
	job.UpdatedAt = time.Now().UTC()
	if err := s.jobStore.Save(job); err != nil {
		logger.Error("Server", "setProgress", err)
	}
}

// sentence makes a reason start with a capital letter and lose its full stop.
func sentence(reason string) string {
	reason = strings.TrimSuffix(strings.TrimSpace(reason), ".")
	if reason == "" {
		return "The update failed"
	}
	return strings.ToUpper(reason[:1]) + reason[1:]
}

// newSwitch returns the container switch wired to Docker and the job store.
func (s *Server) newSwitch() *switchover.Switch {
	return &switchover.Switch{
		Docker:        s.dockerRunner,
		Record:        jobRecorder{store: s.jobStore},
		Timing:        s.switchTiming,
		StartPrograms: s.supervisorctlStart,
	}
}

// jobRecorder saves switch progress into the job store.
type jobRecorder struct {
	store *jobs.Store
}

func (r jobRecorder) Save(job *jobs.Job) error { return r.store.Save(job) }
func (r jobRecorder) Log(line string)          { r.store.AppendLog(line) }

// coreProbe asks PayRam at baseURL whether it is up. Versions older than the
// updater API (legacy) have no /api/v1/health; they are asked the old way and
// their version is read from the container label.
type coreProbe struct {
	client    *coreclient.Client
	legacy    bool
	dockerBin string
	container string
}

func (s *Server) newProbe(baseURL, containerName string, legacy bool) coreProbe {
	return coreProbe{client: coreclient.NewClient(baseURL), legacy: legacy, dockerBin: s.config.DockerBin, container: containerName}
}

func (p coreProbe) Health(ctx context.Context) (*coreclient.HealthResponse, error) {
	if p.legacy {
		if err := corecompat.LegacyHealth(ctx, p.client.BaseURL); err != nil {
			return nil, err
		}
		return &coreclient.HealthResponse{Status: "ok"}, nil
	}
	return p.client.Health(ctx)
}

func (p coreProbe) Version(ctx context.Context) (string, error) {
	if p.legacy {
		return corecompat.VersionFromLabels(ctx, p.dockerBin, p.container)
	}
	resp, err := p.client.Version(ctx)
	if err != nil {
		return "", err
	}
	if !isReleaseVersion(resp.Version) {
		// A build that reports a branch name ("main"): its image label says
		// which release it is.
		return corecompat.VersionFromLabels(ctx, p.dockerBin, p.container)
	}
	return resp.Version, nil
}

// logWorkerStates reports PayRam workers that are not running yet. It is
// information only: a worker stuck on an unreachable blockchain node is not
// a reason to undo a good update.
func (s *Server) logWorkerStates(health *coreclient.HealthResponse) {
	if health == nil || len(health.Workers) == 0 {
		return
	}
	var notRunning []string
	for name, state := range health.Workers {
		if state != "RUNNING" {
			notRunning = append(notRunning, fmt.Sprintf("%s (%s)", name, state))
		}
	}
	sort.Strings(notRunning)
	if len(notRunning) == 0 {
		s.jobStore.AppendLog(fmt.Sprintf("All %d PayRam workers are running", len(health.Workers)))
		return
	}
	s.jobStore.AppendLog(fmt.Sprintf("Note: some PayRam workers are not running yet: %s. Check them in the dashboard if they don't start within a few minutes.", strings.Join(notRunning, ", ")))
}

// resolveTargetContainer determines the target container name using resolution logic.
// Returns container name or fails the job with appropriate error code.
func (s *Server) resolveTargetContainer(ctx context.Context, job *jobs.Job, manifestData *manifest.Manifest) (string, bool) {
	resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, logger.StdLogger())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		if resErr, ok := err.(*container.ResolutionError); ok && resErr.GetFailureCode() == "CONTAINER_NAME_UNRESOLVED" {
			name, discoverErr := s.discoverContainerName(ctx)
			if discoverErr != nil {
				s.failBeforeChange(job, resErr.GetFailureCode(), resErr.Error())
				return "", false
			}
			resolved = &container.ResolvedContainer{Name: name}
		} else if resErr, ok := err.(*container.ResolutionError); ok {
			s.failBeforeChange(job, resErr.GetFailureCode(), resErr.Error())
			return "", false
		} else {
			s.failBeforeChange(job, "CONTAINER_NAME_UNRESOLVED", err.Error())
			return "", false
		}
	}
	containerName := resolved.Name
	s.jobStore.AppendLog(fmt.Sprintf("Target container resolved as: %s", containerName))
	return containerName, true
}

// knownArchSuffixes are the only tag suffixes treated as architecture variants.
var knownArchSuffixes = []string{"-arm64"}

// archSuffixFromTag returns the architecture suffix from a container image tag
// if it matches a known arch variant, otherwise returns "".
// e.g. "1.9.1-arm64" → "-arm64", "1.9.1" → "", "1.9.1-beta" → ""
func archSuffixFromTag(tag string) string {
	for _, suffix := range knownArchSuffixes {
		if strings.HasSuffix(tag, suffix) {
			return suffix
		}
	}
	return ""
}

// baseVersionTag strips any architecture suffix from an image tag, returning
// only the semver portion used for version comparisons.
// e.g. "1.9.3-arm64" → "1.9.3", "1.9.3" → "1.9.3"
func baseVersionTag(tag string) string {
	for _, suffix := range knownArchSuffixes {
		if strings.HasSuffix(tag, suffix) {
			return tag[:len(tag)-len(suffix)]
		}
	}
	return tag
}

// applyArchSuffix gives baseTag the architecture suffix of the running
// container (1.9.3 → 1.9.3-arm64) when the policy's arch_support says the
// target has that variant. baseTag must carry no suffix, so the second hop
// of a breakpoint update cannot become "-arm64-arm64".
func (s *Server) applyArchSuffix(job *jobs.Job, runningTag, baseTag string, archSupport map[string]string) string {
	suffix := archSuffixFromTag(runningTag)
	if suffix == "" {
		return baseTag
	}
	archKey := strings.TrimPrefix(suffix, "-") // "-arm64" → "arm64"
	if minVersion, hasMin := archSupport[archKey]; hasMin && minVersion != "" {
		targetV, err1 := version.NewVersion(baseTag)
		minV, err2 := version.NewVersion(minVersion)
		if err1 != nil || err2 != nil || targetV.LessThan(minV) {
			s.jobStore.AppendLog(fmt.Sprintf("Arch suffix %s not applied: target %s is below minimum %s for this variant", suffix, baseTag, minVersion))
			return baseTag
		}
	}
	s.jobStore.AppendLog(fmt.Sprintf("Arch suffix detected from running container: target image tag adjusted to %s", baseTag+suffix))
	return baseTag + suffix
}

// executeDryRun logs planned upgrade steps and completes the job in dry-run mode.
func (s *Server) executeDryRun(job *jobs.Job, imageRepo, imageTag, containerName string, spec *container.ContainerSpec) {
	createArgs := container.NewDockerRunBuilder(logger.StdLogger()).BuildCreateArgs(spec, switchover.NextName(containerName))
	s.jobStore.AppendLog("DRY-RUN mode: would execute the following steps:")
	s.jobStore.AppendLog(fmt.Sprintf("  1. Pull image: %s:%s", imageRepo, imageTag))
	s.jobStore.AppendLog(fmt.Sprintf("  2. Create the new container next to the running one: docker %s", strings.Join(dockerexec.RedactArgs(createArgs), " ")))
	s.jobStore.AppendLog("  3. Stop PayRam's programs (not the database) and back up the database")
	s.jobStore.AppendLog(fmt.Sprintf("  4. Stop %s, keep it as %s, start the new container as %s", containerName, switchover.PreviousName(containerName), containerName))
	s.jobStore.AppendLog("  5. Verify /api/v1/health, /api/v1/version and that PayRam stays up; switch back on any failure")
	s.jobStore.AppendLog(fmt.Sprintf("  6. Remove %s", switchover.PreviousName(containerName)))

	job.State = jobs.JobStateReady
	job.Message = "Dry run finished: nothing was changed (EXECUTION_MODE=dry-run)"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog("Dry-run complete - no changes made")
}

// preflightChecks verifies Docker is running and there is room for the
// backup and the image. Nothing is changed.
func (s *Server) preflightChecks(ctx context.Context, job *jobs.Job, containerName string) error {
	s.jobStore.AppendLog("Pre-flight: Checking Docker daemon...")
	if err := backup.CheckDockerDaemon(ctx, s.config.DockerBin); err != nil {
		return &switchover.StepError{Code: "DOCKER_DAEMON_DOWN", Message: "Docker is not running (start it with `sudo systemctl start docker` and retry)"}
	}
	s.jobStore.AppendLog("Docker daemon is running")

	// Query actual database size for accurate space calculation
	s.jobStore.AppendLog("Pre-flight: Querying database size...")
	var backupSpaceGB float64 = 2.0 // Default fallback if query fails

	inspector := backup.NewDockerInspector(s.config.DockerBin, nil)
	dbConfig, err := inspector.GetDBConfig(ctx, containerName)
	if err == nil {
		dbSizeChecker := diskspace.NewDBSizeChecker(s.config.DockerBin)

		// Convert ContainerDBConfig to diskspace.DBConfig
		diskspaceDBConfig := &diskspace.DBConfig{
			Host:     dbConfig.Host,
			Port:     dbConfig.Port,
			Database: dbConfig.Database,
			Username: dbConfig.Username,
			Password: dbConfig.Password,
		}

		dbSizeBytes, queryErr := dbSizeChecker.GetDatabaseSize(ctx, containerName, diskspaceDBConfig)
		if queryErr == nil && dbSizeBytes > 0 {
			dbSizeGB := float64(dbSizeBytes) / (1024 * 1024 * 1024)
			// Require 1.5x database size for backup (accounts for compression variation and safety margin)
			backupSpaceGB = dbSizeGB * 1.5
			if backupSpaceGB < 1.0 {
				backupSpaceGB = 1.0 // Minimum 1GB
			}
			s.jobStore.AppendLog(fmt.Sprintf("Database size: %.2f GB, requiring %.2f GB backup space (1.5x for safety)", dbSizeGB, backupSpaceGB))
		} else {
			s.jobStore.AppendLog(fmt.Sprintf("Warning: Unable to query database size, assuming %.1f GB for backup space calculation", backupSpaceGB))
		}
	} else {
		s.jobStore.AppendLog(fmt.Sprintf("Warning: Unable to detect database config, assuming %.1f GB for backup space calculation", backupSpaceGB))
	}

	// Check disk space requirements with dynamic backup space
	s.jobStore.AppendLog("Pre-flight: Checking disk space availability...")
	requirements := []diskspace.SpaceRequirement{
		{
			Path:          s.config.Backup.Dir,
			MinFreeGB:     backupSpaceGB,
			PurposeDesc:   "Backup directory",
			FailIfMissing: true,
		},
		{
			Path:          "/var/lib/docker",
			MinFreeGB:     4.0, // ~4GB for typical Payram image
			PurposeDesc:   "Docker storage",
			FailIfMissing: false, // Don't fail if custom Docker root
		},
		{
			Path:          "/",
			MinFreeGB:     0.5, // At least 500MB for general operations
			PurposeDesc:   "System root",
			FailIfMissing: true,
		},
	}

	results, allSufficient := diskspace.CheckAvailableSpace(requirements)

	// Log all check results
	for _, line := range diskspace.FormatCheckResults(results) {
		s.jobStore.AppendLog(line)
	}

	if !allSufficient {
		// Never suggest `docker system prune -a`: it deletes the stopped
		// containers and images a switch back would need.
		return &switchover.StepError{Code: "DISK_SPACE_LOW", Message: "There is not enough free disk space for the backup and the new image (free space with `docker image prune` for unused image layers, and check `df -h`)"}
	}
	s.jobStore.AppendLog("Disk space checks passed")
	return nil
}

var errSupervisorUnavailable = errors.New("supervisorctl not available")

func (s *Server) supervisorctlStatus(ctx context.Context, containerName string) (string, error) {
	cmd := exec.CommandContext(ctx, s.config.DockerBin, "exec", containerName, "supervisorctl", "status")
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), nil
	}

	outputStr := string(output)
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ProcessState != nil && exitErr.ProcessState.ExitCode() == 3 {
			return outputStr, nil
		}
	}
	if strings.Contains(outputStr, "supervisorctl: not found") ||
		strings.Contains(outputStr, "command not found") ||
		strings.Contains(outputStr, "executable file not found") ||
		strings.Contains(outputStr, "No such file or directory") {
		return "", errSupervisorUnavailable
	}

	return "", fmt.Errorf("supervisorctl status failed: %w: %s", err, outputStr)
}

func (s *Server) supervisorctlStop(ctx context.Context, containerName string, programs []string) error {
	if len(programs) == 0 {
		return nil
	}
	args := append([]string{"exec", containerName, "supervisorctl", "stop"}, programs...)
	cmd := exec.CommandContext(ctx, s.config.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("supervisorctl stop failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// supervisorctlStart starts programs inside the container. A program that is
// already running is fine: after a partial stop, starting everything that
// was stopped is the way back.
func (s *Server) supervisorctlStart(ctx context.Context, containerName string, programs []string) error {
	if len(programs) == 0 {
		return nil
	}
	args := append([]string{"exec", containerName, "supervisorctl", "start"}, programs...)
	cmd := exec.CommandContext(ctx, s.config.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if allStarted(string(output)) {
			return nil
		}
		return fmt.Errorf("supervisorctl start failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// allStarted reports whether supervisorctl's output says every program is
// started ("x: started" or "x: ERROR (already started)").
func allStarted(output string) bool {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || strings.TrimSpace(output) == "" {
		return false
	}
	for _, line := range lines {
		if !strings.Contains(line, "started") {
			return false
		}
	}
	return true
}

// quiesceForBackup stops PayRam's programs, all but the database, so the
// backup sees no writes in flight. Only programs that are running are
// stopped, and they are saved in the job first, so a failure (or a restarted
// updater) starts exactly those again. It reports whether supervisor was used.
func (s *Server) quiesceForBackup(ctx context.Context, job *jobs.Job, containerName string) (bool, error) {
	statusOutput, err := s.supervisorctlStatus(ctx, containerName)
	if err != nil {
		if errors.Is(err, errSupervisorUnavailable) {
			s.jobStore.AppendLog("Supervisor not available; backing up while PayRam keeps running")
			return false, nil
		}
		return false, &switchover.StepError{Code: "SUPERVISORCTL_FAILED", Message: fmt.Sprintf("Could not read PayRam's program list: %v", err)}
	}

	programs, _ := backup.ParseSupervisorStatus(statusOutput)
	running := backup.AppPrograms(programs, s.config.SupervisorInclude, s.config.SupervisorExclude, "RUNNING", "STARTING")
	if len(running) == 0 {
		s.jobStore.AppendLog("No running PayRam programs need stopping for the backup")
		return true, nil
	}

	job.QuiescedPrograms = running
	job.SwitchStage = jobs.SwitchStageQuiesced
	job.UpdatedAt = time.Now().UTC()
	if err := s.jobStore.Save(job); err != nil {
		return true, &switchover.StepError{Code: "STATE_SAVE_FAILED", Message: fmt.Sprintf("Could not save the update's progress (%v), so PayRam's programs were not stopped", err)}
	}
	s.jobStore.AppendLog(fmt.Sprintf("Stopping PayRam's programs for the backup: %s", strings.Join(running, ", ")))
	if err := s.supervisorctlStop(ctx, containerName, running); err != nil {
		return true, &switchover.StepError{Code: "SUPERVISORCTL_FAILED", Message: fmt.Sprintf("Could not stop PayRam's programs for the backup: %v", err)}
	}
	return true, nil
}

// backUpBeforeSwitch takes the pre-update backup with PayRam's programs
// stopped. On failure the caller's restore starts the programs again.
func (s *Server) backUpBeforeSwitch(ctx context.Context, job *jobs.Job, containerName, imageTag string) error {
	// Check the backup can be taken before stopping anything: a missing
	// pg_dump or wrong credentials must not cost the merchant any downtime.
	s.jobStore.AppendLog("Checking that the database backup can be taken...")
	if err := s.containerBackupExec.CheckBackupReady(ctx, containerName); err != nil {
		return &switchover.StepError{Code: "BACKUP_FAILED", Message: fmt.Sprintf("The database backup cannot be taken (%v)", err)}
	}

	usedSupervisor, err := s.quiesceForBackup(ctx, job, containerName)
	if err != nil {
		return err
	}
	attempts := 1
	if usedSupervisor {
		attempts = 3
	}
	path, err := s.takePreUpgradeBackup(ctx, job, containerName, imageTag, attempts, usedSupervisor)
	if err != nil {
		return err
	}
	job.BackupPath = path
	return nil
}

// takePreUpgradeBackup backs the database up, retrying up to attempts times.
func (s *Server) takePreUpgradeBackup(ctx context.Context, job *jobs.Job, containerName, imageTag string, attempts int, quiesced bool) (string, error) {
	currentVersion := job.FromVersion
	if currentVersion == "" {
		currentVersion = "unknown"
	}

	var lastResult *backup.BackupResult
	for attempt := 1; attempt <= attempts; attempt++ {
		message := "Creating database backup"
		if attempts > 1 {
			message = fmt.Sprintf("Creating database backup (attempt %d/%d)", attempt, attempts)
		}
		s.setProgress(job, jobs.JobStateBackingUp, message)

		s.jobStore.AppendLog(fmt.Sprintf("Creating pre-upgrade backup (from %s to %s)...", currentVersion, imageTag))
		s.recordHistory(history.Event{
			Type:    "backup",
			Status:  "started",
			Message: "Backup started",
			Data: map[string]string{
				"jobId":         job.JobID,
				"fromVersion":   currentVersion,
				"targetVersion": imageTag,
				"container":     containerName,
				"attempt":       fmt.Sprintf("%d", attempt),
			},
		})

		backupResult := s.containerBackupExec.ExecuteBackup(ctx, containerName, backup.BackupMeta{
			FromVersion:   currentVersion,
			TargetVersion: imageTag,
			JobID:         job.JobID,
		})
		lastResult = backupResult

		if backupResult.Success {
			s.jobStore.AppendLog(fmt.Sprintf("Backup created successfully: %s (%.2f MB)", backupResult.Filename, float64(backupResult.Size)/(1024*1024)))
			if backupResult.DBConfig != nil {
				dbType := "external"
				if backupResult.DBConfig.IsLocalDB() {
					dbType = "local (in-container)"
				}
				s.jobStore.AppendLog(fmt.Sprintf("Database: %s@%s:%s (%s)", backupResult.DBConfig.Database, backupResult.DBConfig.Host, backupResult.DBConfig.Port, dbType))
			}
			backupData := map[string]string{
				"jobId":         job.JobID,
				"fromVersion":   currentVersion,
				"targetVersion": imageTag,
				"backupPath":    backupResult.Path,
				"sizeBytes":     fmt.Sprintf("%d", backupResult.Size),
			}
			if backupResult.DBConfig != nil {
				backupData["dbHost"] = backupResult.DBConfig.Host
				backupData["dbPort"] = backupResult.DBConfig.Port
				backupData["dbName"] = backupResult.DBConfig.Database
			}
			s.recordHistory(history.Event{
				Type:    "backup",
				Status:  "succeeded",
				Message: "Backup completed",
				Data:    backupData,
			})

			// Prune old backups (using legacy manager for retention logic)
			if _, err := s.backupManager.PruneBackups(s.backupManager.Config.Retention); err != nil {
				s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to prune old backups: %v", err))
			}
			return backupResult.Path, nil
		}

		s.jobStore.AppendLog(fmt.Sprintf("Backup attempt %d/%d failed: %s - %s", attempt, attempts, backupResult.FailureCode, backupResult.ErrorMessage))
		if !backupResult.Retryable {
			// A timeout, a missing tool or wrong credentials fail the same
			// way again; retrying would only keep PayRam stopped longer.
			break
		}
		if attempt < attempts {
			time.Sleep(2 * time.Second)
		}
	}

	code := lastResult.FailureCode
	if quiesced {
		code = "BACKUP_FAILED_AFTER_QUIESCE"
	}
	if code == "" {
		code = "BACKUP_FAILED"
	}
	s.recordHistory(history.Event{
		Type:    "backup",
		Status:  "failed",
		Message: lastResult.ErrorMessage,
		Data: map[string]string{
			"jobId":         job.JobID,
			"fromVersion":   currentVersion,
			"targetVersion": imageTag,
			"failureCode":   code,
			"causeCode":     lastResult.FailureCode,
		},
	})
	return "", &switchover.StepError{Code: code, Message: fmt.Sprintf("The database backup failed (%s)", lastResult.ErrorMessage)}
}

// pullUpgradeImage downloads the target image while PayRam keeps running.
func (s *Server) pullUpgradeImage(ctx context.Context, job *jobs.Job, imageRepo, imageTag string) error {
	imageWithTag := fmt.Sprintf("%s:%s", imageRepo, imageTag)
	s.setProgress(job, jobs.JobStateExecuting, fmt.Sprintf("Downloading %s", baseVersionTag(imageTag)))
	s.jobStore.AppendLog(fmt.Sprintf("Pulling image: %s", imageWithTag))

	pullCtx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	if err := s.dockerRunner.Pull(pullCtx, imageWithTag); err != nil {
		return &switchover.StepError{Code: "DOCKER_PULL_FAILED", Message: fmt.Sprintf("The new version could not be downloaded (%v)", err)}
	}
	s.jobStore.AppendLog("Image pulled successfully")
	return nil
}

// checkNewPortsFree makes sure every host port the new container publishes
// and the old one did not (a port the manifest added) is free, so `docker
// start` cannot fail on it after PayRam was stopped.
func checkNewPortsFree(current *container.RuntimeState, spec *container.ContainerSpec) error {
	published := map[string]bool{}
	for _, port := range current.Ports {
		published[port.HostPort] = true
	}
	for i, arg := range spec.Options {
		if arg != "-p" || i+1 >= len(spec.Options) {
			continue
		}
		hostIP, hostPort, proto := splitPortMapping(spec.Options[i+1])
		if hostPort == "" || published[hostPort] || proto != "tcp" {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(hostIP, hostPort))
		if err != nil {
			return &switchover.StepError{Code: "PORT_CONFLICT", Message: fmt.Sprintf("Port %s is already used by another program on this server, and the new version needs it", hostPort)}
		}
		listener.Close()
	}
	return nil
}

// splitPortMapping parses [hostIP:]hostPort:containerPort[/proto].
func splitPortMapping(mapping string) (hostIP, hostPort, proto string) {
	proto = "tcp"
	if slash := strings.LastIndex(mapping, "/"); slash >= 0 {
		mapping, proto = mapping[:slash], mapping[slash+1:]
	}
	if strings.HasPrefix(mapping, "[") {
		end := strings.Index(mapping, "]")
		if end < 0 {
			return "", "", proto
		}
		hostIP, mapping = mapping[1:end], strings.TrimPrefix(mapping[end+1:], ":")
	}
	parts := strings.Split(mapping, ":")
	switch len(parts) {
	case 2:
		return hostIP, parts[0], proto
	case 3:
		return parts[0], parts[1], proto
	}
	return hostIP, "", proto
}

// finalizeUpgrade marks the job complete and prunes old images, keeping the
// one a manual rollback would need.
func (s *Server) finalizeUpgrade(ctx context.Context, job *jobs.Job, imageRepo, imageTag string) {
	job.State = jobs.JobStateReady
	job.FailureCode = ""
	job.CauseCode = ""
	job.Message = "Upgrade completed successfully"
	job.UpdatedAt = time.Now().UTC()
	if err := s.jobStore.Save(job); err != nil {
		logger.Error("Server", "finalizeUpgrade", err)
	}
	s.jobStore.AppendLog(fmt.Sprintf("SUCCESS: Upgrade to %s completed successfully", imageTag))

	keep := []string{imageTag}
	if _, previous := container.SplitImage(job.PreviousImage); previous != "" {
		keep = append(keep, previous)
	}
	pruneCtx, cancelPrune := context.WithTimeout(ctx, 30*time.Second)
	defer cancelPrune()
	if err := s.dockerRunner.PrunePayramImages(pruneCtx, imageRepo, keep...); err != nil {
		s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to prune Payram images: %v", err))
	} else {
		s.jobStore.AppendLog(fmt.Sprintf("Pruned old Payram images (kept %s)", strings.Join(keep, ", ")))
	}
}
