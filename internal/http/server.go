package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	goversion "github.com/hashicorp/go-version"
	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/cli"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/corecompat"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
	"github.com/payram/payram-updater/internal/network"
	"github.com/payram/payram-updater/internal/policy"
	"github.com/payram/payram-updater/internal/switchover"
)

// shutdownGrace is how long a stopping updater waits for an update in
// progress to finish or switch back. The systemd unit allows 15 minutes.
const shutdownGrace = 14 * time.Minute

// coreURLCacheFor is how long a found PayRam API address is reused by the
// read-only endpoints before it is looked up again.
const coreURLCacheFor = time.Minute

var (
	errUpgradeBusy  = errors.New("an update is already running")
	errRepairing    = errors.New("the updater is finishing an update that was interrupted")
	errShuttingDown = errors.New("the updater is shutting down")
)

// Server represents the HTTP server.
type Server struct {
	httpServer          *http.Server
	port                int
	config              *config.Config
	jobStore            *jobs.Store
	dockerRunner        *dockerexec.Runner
	backupManager       *backup.Manager
	containerBackupExec *backup.ContainerBackupExecutor
	historyStore        *history.Store
	allowList           *network.AllowList

	// upgradeRunning is held from the moment an update (or the repair of an
	// interrupted one) is accepted until it ends, so a second one is refused
	// instead of racing it on the same container. upgradeWg lets shutdown
	// wait for it.
	upgradeRunning atomic.Bool
	repairing      atomic.Bool
	shuttingDown   atomic.Bool
	upgradeWg      sync.WaitGroup

	// switchTiming is how patient the container switch is (zero = defaults).
	switchTiming switchover.Timing
	// heartbeatEvery is how often a running job's UpdatedAt is refreshed.
	heartbeatEvery time.Duration
	// currentPolicyInit is the policy's updater_api_init_version for the job
	// in the update slot (versions below it are probed the legacy way).
	currentPolicyInit string

	coreURLMu sync.Mutex
	coreURL   string
	coreURLAt time.Time

	// cliToken is the secret root's payram-updater command sends to be
	// granted manual mode (see cli.EnsureToken). Empty: manual mode is off.
	cliToken string

	// versionLookup replaces lookupCurrentVersion in tests.
	versionLookup func(ctx context.Context) string

	// answers carries the answer of the person following an attended update
	// to the question it waits on (POST /upgrade/answer).
	answers answerBox
	// questionWait is how long that question waits (zero: restoreQuestionWait).
	questionWait time.Duration
	// switchWiring replaces newSwitch's Docker, programs and database in
	// tests.
	switchWiring func(*switchover.Switch)
}

// New creates a new HTTP server instance. It does not look for PayRam yet:
// every job finds the container and its API itself, because PayRam may not
// be running when the updater starts (after a reboot) and an update may move
// it to a new address.
func New(cfg *config.Config, jobStore *jobs.Store) *Server {
	dockerRunner := &dockerexec.Runner{
		DockerBin: cfg.DockerBin,
		Logger:    logger.StdLogger(),
		// Docker's default 10s kills the bundled Postgres mid-shutdown.
		StopTimeoutSeconds: 60,
	}

	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	// Create backup manager (legacy, for backward compatibility with existing backups)
	// Backups are always enabled
	backupCfg := backup.Config{
		Dir:                 cfg.Backup.Dir,
		Retention:           cfg.Backup.Retention,
		PGHost:              cfg.Backup.PGHost,
		PGPort:              cfg.Backup.PGPort,
		PGDB:                cfg.Backup.PGDB,
		PGUser:              cfg.Backup.PGUser,
		PGPassword:          cfg.Backup.PGPassword,
		ImagePattern:        imagePattern,
		TargetContainerName: cfg.TargetContainerName,
		// Pruning never deletes the backup of the latest failed update.
		StateDir:          cfg.StateDir,
		DockerBin:         cfg.DockerBin,
		SupervisorExclude: cfg.SupervisorExclude,
	}
	backupMgr := backup.NewManager(backupCfg, &backup.RealExecutor{}, logger.StdLogger())

	// Create container-aware backup executor
	containerBackupExec := backup.NewContainerBackupExecutor(
		cfg.DockerBin,
		"pg_dump",
		cfg.Backup.Dir,
		logger.StdLogger(),
	)
	containerBackupExec.BackupTimeout = time.Duration(cfg.BackupTimeoutSeconds) * time.Second

	s := &Server{
		port:                cfg.Port,
		config:              cfg,
		jobStore:            jobStore,
		dockerRunner:        dockerRunner,
		backupManager:       backupMgr,
		containerBackupExec: containerBackupExec,
		historyStore:        history.NewStore(cfg.StateDir),
		switchTiming:        switchover.Timing{HealthTimeout: time.Duration(cfg.HealthCheckTimeoutSeconds) * time.Second},
		heartbeatEvery:      20 * time.Second,
	}
	if cfg.CoreBaseURL != "" {
		logger.Infof("Server", "New", "PayRam API address set by CORE_BASE_URL: %s", cfg.CoreBaseURL)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", HandleHealth())
	mux.HandleFunc("/upgrade/status", s.HandleUpgradeStatus())
	mux.HandleFunc("/upgrade/logs", s.HandleUpgradeLogs())
	mux.HandleFunc("/upgrade/last", s.HandleUpgradeLast())
	mux.HandleFunc("/upgrade/playbook", s.HandleUpgradePlaybook())
	mux.HandleFunc("/upgrade/inspect", s.HandleUpgradeInspect())
	mux.HandleFunc("/upgrade/plan", s.HandleUpgradePlan())
	mux.HandleFunc("/upgrade/run", s.HandleUpgradeRun())
	mux.HandleFunc("/upgrade/answer", s.HandleUpgradeAnswer())
	mux.HandleFunc("/history", s.HandleHistory())
	mux.HandleFunc("/upgrade/history", s.HandleHistory())

	// Only this machine and the PayRam container may call the API. The
	// container's address is looked up when it first calls, and again when
	// an update gave it a new one.
	s.allowList = network.NewAllowList(s.lookupContainerIPs, 5*time.Second, logger.StdLogger())
	logger.Infof("Server", "New", "API access restricted to this machine and the PayRam container")

	// Bind only to localhost and docker bridge (local machine only)
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.allowList.Middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	return s
}

// Start starts the HTTP server and blocks until shutdown.
// On SIGINT/SIGTERM it lets an update in progress finish (or switch back)
// before exiting, so PayRam is never left half-updated.
func (s *Server) Start() error {
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	serverErrors := make(chan error, 2)

	s.secureStoredFiles()
	if token, err := cli.EnsureToken(s.config.StateDir); err != nil {
		logger.Warnf("Server", "Start", "Manual mode is unavailable: %v (dashboard updates are not affected)", err)
	} else {
		s.cliToken = token
	}

	listener, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("failed to create listener: %w", err)
	}
	logger.Infof("Server", "Start", "Localhost: http://%s", s.httpServer.Addr)
	go func() {
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErrors <- fmt.Errorf("HTTP server error: %w", err)
		}
	}()
	go s.serveOnDockerBridge(backgroundCtx, serverErrors)

	// An updater that stopped mid-update left its job behind: finish or
	// reverse it before taking new work. Auto-update starts only after the
	// repair, so it can never start an update on top of the interrupted one.
	go func() {
		s.repairInterruptedUpgrade(backgroundCtx)
		if s.config.AutoUpdateEnabled {
			s.startAutoUpdateLoop(backgroundCtx)
		}
	}()

	select {
	case err := <-serverErrors:
		cancelBackground()
		return err
	case sig := <-stop:
		logger.Warnf("Server", "Start", "Received signal %v, initiating graceful shutdown", sig)
	}

	s.shuttingDown.Store(true)
	cancelBackground()
	if s.upgradeRunning.Load() {
		logger.Warnf("Server", "Start", "Waiting up to %s for the update in progress to finish, so PayRam is not left half-updated", shutdownGrace)
		finished := make(chan struct{})
		go func() {
			s.upgradeWg.Wait()
			close(finished)
		}()
		select {
		case <-finished:
			logger.Infof("Server", "Start", "Update finished; stopping")
		case <-time.After(shutdownGrace):
			logger.Warnf("Server", "Start", "The update did not finish in time; the next start will put PayRam right")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}

	logger.Infof("Server", "Start", "Server stopped gracefully")
	return nil
}

// secureStoredFiles makes what the updater keeps on disk root-only before it
// serves anything. Updaters up to v0.2.3 logged docker command lines with
// AES_KEY in them (the dashboard shows that log), and left backups, which
// hold the whole payments database, readable by every user on the server
// until the next backup was taken. (The state directory's own files are
// root-only already, and the directory itself may be shared, so it is left
// as it is; the installer restricts its default one.)
func (s *Server) secureStoredFiles() {
	if err := s.jobStore.RewriteLogs(dockerexec.RedactLine); err != nil {
		logger.Warnf("Server", "secureStoredFiles", "Could not clean the update log of secret values: %v", err)
	}
	if s.config.Backup.Dir != "" {
		if err := backup.SecureBackupDir(s.config.Backup.Dir, logger.StdLogger()); err != nil {
			logger.Warnf("Server", "secureStoredFiles", "Could not restrict the backup folder to root: %v", err)
		}
	}
}

// serveOnDockerBridge listens on the docker0 address the PayRam container
// calls. docker0 only exists once Docker is up, which after a reboot can be
// later than the updater, so it keeps trying instead of giving up once.
func (s *Server) serveOnDockerBridge(ctx context.Context, serverErrors chan<- error) {
	warned := false
	for {
		dockerIP, err := network.GetDockerBridgeIP()
		if err == nil {
			bridgeAddr := fmt.Sprintf("%s:%d", dockerIP, s.port)
			bridgeListener, listenErr := net.Listen("tcp", bridgeAddr)
			if listenErr == nil {
				logger.Infof("Server", "Start", "Docker bridge: http://%s", bridgeAddr)
				if err := s.httpServer.Serve(bridgeListener); err != nil && err != http.ErrServerClosed {
					serverErrors <- fmt.Errorf("HTTP server error (docker bridge): %w", err)
				}
				return
			}
			err = listenErr
		}
		if !warned {
			logger.Warnf("Server", "Start", "The PayRam dashboard cannot reach the updater yet (%v); retrying every 10s", err)
			warned = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// claimUpgradeSlot reserves the single update slot.
func (s *Server) claimUpgradeSlot() error {
	if s.shuttingDown.Load() {
		return errShuttingDown
	}
	if !s.upgradeRunning.CompareAndSwap(false, true) {
		if s.repairing.Load() {
			return errRepairing
		}
		return errUpgradeBusy
	}
	return nil
}

func (s *Server) releaseUpgradeSlot() {
	s.upgradeRunning.Store(false)
}

// startUpgrade runs a job whose slot was claimed with claimUpgradeSlot.
func (s *Server) startUpgrade(job *jobs.Job, plan *UpgradePlan) {
	s.upgradeWg.Add(1)
	go s.executeUpgrade(job, plan)
}

func (s *Server) startAutoUpdateLoop(ctx context.Context) {
	interval := time.Duration(s.config.AutoUpdateInterval) * time.Hour
	if interval <= 0 {
		logger.Warnf("Server", "startAutoUpdateLoop", "Auto update disabled due to invalid interval: %d hours", s.config.AutoUpdateInterval)
		return
	}

	logger.Infof("Server", "startAutoUpdateLoop", "Auto update enabled. Checking every %d hours", s.config.AutoUpdateInterval)

	// Run once at startup
	s.runAutoUpdateOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Infof("Server", "startAutoUpdateLoop", "Auto update loop stopped")
			return
		case <-ticker.C:
			s.runAutoUpdateOnce(ctx)
		}
	}
}

func (s *Server) recordHistory(event history.Event) {
	if s.historyStore == nil {
		return
	}
	if err := s.historyStore.Append(event); err != nil {
		logger.Error("Server", "recordHistory", err)
		if s.jobStore != nil {
			s.jobStore.AppendLog(fmt.Sprintf("Warning: failed to record history: %v", err))
		}
	}
}

func (s *Server) runAutoUpdateOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if err := s.claimUpgradeSlot(); err != nil {
		logger.Infof("Server", "runAutoUpdateOnce", "Auto update: skipping (%v)", err)
		return
	}
	started := false
	defer func() {
		if !started {
			s.releaseUpgradeSlot()
		}
	}()

	existingJob, err := s.jobStore.LoadLatest()
	if err != nil {
		logger.Error("Server", "runAutoUpdateOnce", err)
		return
	}
	if existingJob != nil && existingJob.IsActive() {
		logger.Warnf("Server", "runAutoUpdateOnce", "Auto update: update %s is still marked %s, skipping", existingJob.JobID, existingJob.State)
		return
	}
	if existingJob != nil && existingJob.State == jobs.JobStateFailed {
		logger.Warnf("Server", "runAutoUpdateOnce", "Auto update: last job failed (%s), skipping", existingJob.FailureCode)
		return
	}

	// Fetch policy to get latest version
	policyClient := policy.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
	policyCtx, cancel2 := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel2()
	policyData, err := policyClient.Fetch(policyCtx, s.config.PolicyURL)
	if err != nil {
		logger.Error("Server", "runAutoUpdateOnce", err)
		return
	}
	latest := strings.TrimSpace(policyData.Latest)
	if latest == "" {
		logger.Warnf("Server", "runAutoUpdateOnce", "Auto update: policy latest is empty, skipping")
		return
	}
	initVersion := strings.TrimSpace(policyData.UpdaterAPIInitVersion)

	// Unlike a dashboard request, auto-update does not go ahead without
	// knowing what runs.
	currentVersion, err := s.runningVersion(ctx, initVersion)
	if err != nil {
		logger.Error("Server", "runAutoUpdateOnce", err)
		return
	}

	if currentVersion == latest {
		logger.Infof("Server", "runAutoUpdateOnce", "Auto update: already on latest version %s", latest)
		return
	}

	// Plan upgrade using DASHBOARD mode
	planCtx, cancel3 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel3()
	plan := s.PlanUpgrade(planCtx, jobs.JobModeDashboard, latest, currentVersion)
	if plan.State == jobs.JobStateFailed {
		logger.Warnf("Server", "runAutoUpdateOnce", "Auto update: planning failed (%s): %s", plan.FailureCode, plan.Message)
		return
	}

	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	job := jobs.NewJob(jobID, jobs.JobModeDashboard, plan.RequestedTarget)
	job.ResolvedTarget = plan.ResolvedTarget
	job.State = jobs.JobStateExecuting
	job.Message = "Preparing the update"
	job.UpdatedAt = time.Now().UTC()

	if err := s.jobStore.Save(job); err != nil {
		logger.Error("Server", "runAutoUpdateOnce", err)
		return
	}

	s.jobStore.AppendLog(fmt.Sprintf("Starting auto update job %s: mode=%s target=%s source=AUTO", jobID, "DASHBOARD", plan.RequestedTarget))
	started = true
	s.startUpgrade(job, plan)
}

// executeUpgrade runs an update job in the background. It ends with the new
// version once it has proven itself, or the previous one, switched back
// automatically (with the backup restored when the new version migrated the
// database), or, when switching back could send a payout twice or the
// database cannot be checked, with PayRam left on the new version and the
// job saying so (see runUpgrade).
func (s *Server) executeUpgrade(job *jobs.Job, plan *UpgradePlan) {
	defer s.upgradeWg.Done()
	defer s.releaseUpgradeSlot()
	ctx := context.Background()

	// A CLI restore and a daemon update must never change the container at
	// the same time.
	release, err := jobs.AcquireContainerLock(s.config.StateDir)
	if err != nil {
		s.failBeforeChange(job, "CONCURRENCY_BLOCKED", fmt.Sprintf("%v", err))
		return
	}
	defer release()

	stopHeartbeat := s.startHeartbeat()
	defer stopHeartbeat()

	isDryRun := s.config.ExecutionMode == "dry-run"

	// Record upgrade start
	upgradeData := map[string]string{
		"jobId":           job.JobID,
		"mode":            string(job.Mode),
		"requestedTarget": job.RequestedTarget,
		"resolvedTarget":  job.ResolvedTarget,
		"executionMode":   s.config.ExecutionMode,
	}
	if isDryRun {
		upgradeData["dryRun"] = "true"
	}
	s.recordHistory(history.Event{
		Type:    "upgrade",
		Status:  "started",
		Message: "Upgrade started",
		Data:    upgradeData,
	})

	// Defer history recording for final state
	defer func() {
		status := ""
		message := job.Message
		data := map[string]string{
			"jobId":           job.JobID,
			"mode":            string(job.Mode),
			"requestedTarget": job.RequestedTarget,
			"resolvedTarget":  job.ResolvedTarget,
			"executionMode":   s.config.ExecutionMode,
		}
		if job.State == jobs.JobStateFailed {
			status = "failed"
			if job.FailureCode != "" {
				data["failureCode"] = job.FailureCode
			}
			if job.CauseCode != "" {
				data["causeCode"] = job.CauseCode
			}
		} else if job.State == jobs.JobStateReady {
			if isDryRun {
				status = "validated"
			} else {
				status = "succeeded"
			}
		}
		if status == "" {
			return
		}
		s.recordHistory(history.Event{
			Type:    "upgrade",
			Status:  status,
			Message: message,
			Data:    data,
		})
	}()

	// A bug must not leave PayRam half-switched.
	defer func() {
		if r := recover(); r != nil {
			logger.ErrorMsg("Server", "executeUpgrade", fmt.Sprintf("panic during update: %v\n%s", r, debug.Stack()))
			s.jobStore.AppendLog(fmt.Sprintf("Internal error: %v", r))
			s.putPayramBack(ctx, s.newSwitch(), job, "INTERNAL_ERROR",
				fmt.Sprintf("the updater hit an internal error (%v)", r), stageReachedCutover(job.SwitchStage))
		}
	}()

	s.runUpgrade(ctx, job, plan, isDryRun)
}

// startHeartbeat refreshes the saved job's UpdatedAt while a job runs, so an
// active job that stops being refreshed is known to belong to a dead updater.
func (s *Server) startHeartbeat() func() {
	every := s.heartbeatEvery
	if every <= 0 {
		every = 20 * time.Second
	}
	return s.jobStore.StartHeartbeat(every, func(err error) {
		logger.Error("Server", "startHeartbeat", err)
	})
}

func (s *Server) fetchPolicyInitVersion(ctx context.Context) string {
	policyClient := policy.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
	policyCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel()
	policyData, err := policyClient.Fetch(policyCtx, s.config.PolicyURL)
	if err != nil {
		logger.Error("Server", "fetchPolicyInitVersion", err)
		return ""
	}
	return strings.TrimSpace(policyData.UpdaterAPIInitVersion)
}

// resolveCoreVersion returns the version PayRam in containerName runs: from
// its API when it answers, else from the container's labels.
func (s *Server) resolveCoreVersion(ctx context.Context, containerName, initVersion string) (string, bool, error) {
	return s.resolveCoreVersionAt(ctx, s.currentCoreBaseURL(ctx, containerName), containerName, initVersion)
}

func (s *Server) resolveCoreVersionAt(ctx context.Context, baseURL, containerName, initVersion string) (string, bool, error) {
	if baseURL != "" {
		versionResp, err := coreclient.NewClient(baseURL).Version(ctx)
		// Some builds report a branch name ("main") instead of a version;
		// the image label is right for those.
		if err == nil && versionResp != nil && isReleaseVersion(versionResp.Version) {
			legacy, legacyErr := corecompat.IsBeforeInit(versionResp.Version, initVersion)
			if legacyErr != nil {
				logger.Error("Server", "resolveCoreVersion", legacyErr)
				return versionResp.Version, false, nil
			}
			return versionResp.Version, legacy, nil
		}
	}

	labelVersion, err := corecompat.VersionFromLabels(ctx, s.config.DockerBin, containerName)
	if err != nil {
		return "", false, err
	}

	legacy, legacyErr := corecompat.IsBeforeInit(labelVersion, initVersion)
	if legacyErr != nil {
		logger.Error("Server", "resolveCoreVersion", legacyErr)
		return labelVersion, false, nil
	}

	return labelVersion, legacy, nil
}

// currentCoreBaseURL returns the PayRam API address for read-only use,
// looked up at most once a minute. Updates look it up themselves.
func (s *Server) currentCoreBaseURL(ctx context.Context, containerName string) string {
	s.coreURLMu.Lock()
	defer s.coreURLMu.Unlock()
	if s.coreURL != "" && time.Since(s.coreURLAt) < coreURLCacheFor {
		return s.coreURL
	}
	if containerName == "" {
		name, err := s.discoverContainerName(ctx)
		if err != nil {
			return s.config.CoreBaseURL
		}
		containerName = name
	}
	url, _, err := container.FindCoreBaseURL(ctx, s.config.DockerBin, containerName, s.config.CoreBaseURL, logger.StdLogger())
	if err != nil {
		logger.Warnf("Server", "currentCoreBaseURL", "Could not find the PayRam API: %v", err)
		return ""
	}
	s.coreURL, s.coreURLAt = url, time.Now()
	return url
}

func (s *Server) rememberCoreBaseURL(url string) {
	s.coreURLMu.Lock()
	defer s.coreURLMu.Unlock()
	s.coreURL, s.coreURLAt = url, time.Now()
}

// isReleaseVersion reports whether v looks like a release version (3.10.0,
// v3.10.0, 3.10.0-arm64) rather than a branch name.
func isReleaseVersion(v string) bool {
	_, err := goversion.NewVersion(baseVersionTag(strings.TrimPrefix(strings.TrimSpace(v), "v")))
	return err == nil
}

func (s *Server) shouldUseLegacyForTarget(initVersion, targetVersion string) bool {
	legacy, err := corecompat.IsBeforeInit(targetVersion, initVersion)
	if err != nil {
		logger.Error("Server", "shouldUseLegacyForTarget", err)
		return false
	}
	return legacy
}

func (s *Server) discoverContainerName(ctx context.Context) (string, error) {
	// Prefer explicit container name (handles non-semver tags like "develop").
	if s.config.TargetContainerName != "" {
		return s.config.TargetContainerName, nil
	}

	imagePattern := "payramapp/payram:"
	if s.config.ImageRepoOverride != "" {
		imagePattern = s.config.ImageRepoOverride + ":"
	}

	discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, logger.StdLogger())
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		return "", err
	}

	return container.ContainerName(discovered.Name), nil
}

// lookupContainerIPs feeds the API allow list.
func (s *Server) lookupContainerIPs(ctx context.Context) ([]string, error) {
	name, err := s.discoverContainerName(ctx)
	if err != nil {
		return nil, err
	}
	return network.ContainerIPs(ctx, s.config.DockerBin, name)
}
