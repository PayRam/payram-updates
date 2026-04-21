package http

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/corecompat"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/history"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
	"github.com/payram/payram-updater/internal/manifest"
	"github.com/payram/payram-updater/internal/network"
	"github.com/payram/payram-updater/internal/policy"
)

// discoverCoreBaseURL discovers the Payram Core base URL by:
// 1. Finding the running Payram container via docker ps
// 2. Extracting runtime state (ports) via docker inspect
// 3. Probing each exposed port for "Welcome to Payram Core"
// This allows the updater to work without CORE_BASE_URL being explicitly configured.
func discoverCoreBaseURL(dockerBin string, imagePattern string) (string, error) {
	ctx := context.Background()

	// Step 1: Discover the Payram container
	discoverer := container.NewDiscoverer(dockerBin, imagePattern, logger.StdLogger())
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to discover Payram container: %w", err)
	}

	return discoverCoreBaseURLByName(ctx, dockerBin, discovered.Name)
}

// discoverCoreBaseURLByName discovers the Payram Core base URL for a specific container.
func discoverCoreBaseURLByName(ctx context.Context, dockerBin string, containerName string) (string, error) {
	// Extract runtime state to get ports
	inspector := container.NewInspector(dockerBin, logger.StdLogger())
	runtimeState, err := inspector.ExtractRuntimeState(ctx, containerName)
	if err != nil {
		return "", fmt.Errorf("failed to extract runtime state: %w", err)
	}

	// Identify which port serves Payram Core
	identifier := container.NewPortIdentifier(logger.StdLogger())
	identifiedPort, err := identifier.IdentifyPayramCorePort(ctx, runtimeState)
	if err != nil {
		return "", fmt.Errorf("failed to identify Payram Core port: %w", err)
	}

	// Build the base URL from the identified port using the discovered scheme
	baseURL := fmt.Sprintf("%s://127.0.0.1:%s", identifiedPort.Scheme, identifiedPort.HostPort)
	return baseURL, nil
}

// Server represents the HTTP server.
type Server struct {
	httpServer          *http.Server
	port                int
	config              *config.Config
	jobStore            *jobs.Store
	dockerRunner        *dockerexec.Runner
	coreClient          *coreclient.Client
	backupManager       *backup.Manager
	containerBackupExec *backup.ContainerBackupExecutor
	historyStore        *history.Store
}

// New creates a new HTTP server instance.
func New(cfg *config.Config, jobStore *jobs.Store) *Server {
	// Create docker runner
	dockerRunner := &dockerexec.Runner{
		DockerBin: cfg.DockerBin,
		Logger:    logger.StdLogger(),
	}

	// Always discover CoreBaseURL dynamically via docker inspect
	logger.Infof("Server", "New", "Discovering Payram Core port via docker inspect...")
	// Use imagePattern for discovery (default to payramapp/payram if not overridden)
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	var coreBaseURL string
	var err error
	if cfg.TargetContainerName != "" {
		// Use explicit container name if set
		coreBaseURL, err = discoverCoreBaseURLByName(context.Background(), cfg.DockerBin, cfg.TargetContainerName)
		if err != nil {
			logger.Error("Server", "New", err)
			logger.Warnf("Server", "New", "Falling back to http://127.0.0.1:8080 (this may not work if Payram Core is on a different port)")
			coreBaseURL = "http://127.0.0.1:8080"
		} else {
			logger.Infof("Server", "New", "Discovered Payram Core at: %s (from TARGET_CONTAINER_NAME=%s)", coreBaseURL, cfg.TargetContainerName)
		}
	} else {
		// Fall back to semver-based discovery
		coreBaseURL, err = discoverCoreBaseURL(cfg.DockerBin, imagePattern)
		if err != nil {
			logger.Error("Server", "New", err)
			logger.Warnf("Server", "New", "Falling back to http://127.0.0.1:8080 (this may not work if Payram Core is on a different port)")
			coreBaseURL = "http://127.0.0.1:8080"
		} else {
			logger.Infof("Server", "New", "Discovered Payram Core at: %s", coreBaseURL)
		}
	}

	// Discover Payram container IP for access control
	logger.Infof("Server", "New", "Discovering Payram container IP for access control...")
	var payramContainerIP string
	if cfg.TargetContainerName != "" {
		// Use explicit container name if set
		payramContainerIP, err = network.GetContainerIPByName(cfg.DockerBin, cfg.TargetContainerName)
		if err != nil {
			logger.Error("Server", "New", err)
			logger.Warnf("Server", "New", "API access will be restricted to localhost only")
			payramContainerIP = ""
		} else {
			logger.Infof("Server", "New", "Payram container IP: %s (from TARGET_CONTAINER_NAME=%s)", payramContainerIP, cfg.TargetContainerName)
		}
	} else {
		// Fall back to semver-based discovery
		payramContainerIP, err = network.GetPayramContainerIP(cfg.DockerBin, imagePattern)
		if err != nil {
			logger.Error("Server", "New", err)
			logger.Warnf("Server", "New", "API access will be restricted to localhost only")
			payramContainerIP = ""
		} else {
			logger.Infof("Server", "New", "Payram container IP: %s (API access restricted to localhost and this container)", payramContainerIP)
		}
	}

	// Create core API client
	coreClient := coreclient.NewClient(coreBaseURL)

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
		coreClient:          coreClient,
		backupManager:       backupMgr,
		containerBackupExec: containerBackupExec,
		historyStore:        history.NewStore(cfg.StateDir),
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
	mux.HandleFunc("/history", s.HandleHistory())
	mux.HandleFunc("/upgrade/history", s.HandleHistory())

	// Apply IP restriction middleware to allow only localhost and Payram container
	allowedIPs := []string{
		"127.0.0.1", // localhost IPv4
		"::1",       // localhost IPv6
	}
	if payramContainerIP != "" {
		allowedIPs = append(allowedIPs, payramContainerIP)
	}
	handler := network.AllowedIPsMiddleware(allowedIPs, logger.StdLogger())(mux)
	logger.Infof("Server", "New", "API access restricted to: %v", allowedIPs)

	// Bind only to localhost and docker bridge (local machine only)
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	return s
}

// Start starts the HTTP server and blocks until shutdown.
// It handles graceful shutdown on SIGINT and SIGTERM.
func (s *Server) Start() error {
	autoUpdateCtx, autoUpdateCancel := context.WithCancel(context.Background())
	defer autoUpdateCancel()

	// Create a channel to listen for shutdown signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Create a channel to capture server errors
	serverErrors := make(chan error, 1)

	// Start the server in a goroutine
	go func() {
		// Get Docker bridge IP for logging and optional listener
		dockerIP, err := network.GetDockerBridgeIP()
		if err != nil {
			logger.Error("Server", "Start", err)
			logger.Warnf("Server", "Start", "Starting HTTP server on localhost only: http://127.0.0.1:%d", s.port)
		} else {
			logger.Infof("Server", "Start", "Starting HTTP server on local interfaces")
			logger.Infof("Server", "Start", "Localhost: http://127.0.0.1:%d", s.port)
			logger.Infof("Server", "Start", "Docker bridge: http://%s:%d", dockerIP, s.port)
		}

		// Always listen on localhost
		listener, err := net.Listen("tcp", s.httpServer.Addr)
		if err != nil {
			serverErrors <- fmt.Errorf("failed to create listener: %w", err)
			return
		}

		// If docker bridge IP is available, start a second listener on it
		if dockerIP != "" {
			bridgeAddr := fmt.Sprintf("%s:%d", dockerIP, s.port)
			bridgeListener, bridgeErr := net.Listen("tcp", bridgeAddr)
			if bridgeErr != nil {
				logger.Error("Server", "Start", bridgeErr)
				logger.Warnf("Server", "Start", "Failed to bind docker bridge listener (%s)", bridgeAddr)
			} else {
				go func() {
					if err := s.httpServer.Serve(bridgeListener); err != nil && err != http.ErrServerClosed {
						serverErrors <- fmt.Errorf("HTTP server error (docker bridge): %w", err)
					}
				}()
			}
		}

		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErrors <- fmt.Errorf("HTTP server error: %w", err)
		}
	}()

	if s.config.AutoUpdateEnabled {
		go s.startAutoUpdateLoop(autoUpdateCtx)
	}

	// Wait for either a signal or server error
	select {
	case err := <-serverErrors:
		autoUpdateCancel()
		return err
	case sig := <-stop:
		logger.Warnf("Server", "Start", "Received signal %v, initiating graceful shutdown", sig)
	}

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}

	logger.Infof("Server", "Start", "Server stopped gracefully")
	return nil
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

	// Skip if an active job exists
	existingJob, err := s.jobStore.LoadLatest()
	if err != nil {
		logger.Error("Server", "runAutoUpdateOnce", err)
		return
	}
	if existingJob != nil {
		if isJobActive(existingJob) {
			logger.Infof("Server", "runAutoUpdateOnce", "Auto update: active job %s in state %s, skipping", existingJob.JobID, existingJob.State)
			return
		}
		if existingJob.State == jobs.JobStateFailed {
			logger.Warnf("Server", "runAutoUpdateOnce", "Auto update: last job failed (%s), skipping", existingJob.FailureCode)
			return
		}
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

	containerName, err := s.discoverContainerName(ctx)
	if err != nil {
		logger.Error("Server", "runAutoUpdateOnce", err)
		return
	}

	// Fetch current version (API or label fallback)
	versionCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel()
	currentVersion, _, err := s.resolveCoreVersion(versionCtx, containerName, initVersion)
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

	// Re-check for active job to avoid race
	existingJob, err = s.jobStore.LoadLatest()
	if err == nil && existingJob != nil && isJobActive(existingJob) {
		logger.Infof("Server", "runAutoUpdateOnce", "Auto update: active job %s in state %s, skipping", existingJob.JobID, existingJob.State)
		return
	}

	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	job := jobs.NewJob(jobID, jobs.JobModeDashboard, plan.RequestedTarget)
	job.ResolvedTarget = plan.ResolvedTarget
	job.State = jobs.JobStateReady
	job.Message = "Auto update job created"
	job.UpdatedAt = time.Now().UTC()

	if err := s.jobStore.Save(job); err != nil {
		logger.Error("Server", "runAutoUpdateOnce", err)
		return
	}

	s.jobStore.AppendLog(fmt.Sprintf("Starting auto update job %s: mode=%s target=%s source=AUTO", jobID, "DASHBOARD", plan.RequestedTarget))
	go s.executeUpgrade(job, plan.Manifest, plan.ArchSupport, plan.SteppingStone)
}

// executeUpgrade runs the upgrade execution in the background.
// It updates job state and logs progress as it executes.
// All configuration comes from the manifest - no environment overrides.
//
// FAIL-FAST GUARANTEES (Phase G):
// ================================
// This function enforces strict fail-fast behavior. If ANY step cannot be
// completed safely, the upgrade FAILS IMMEDIATELY with:
//  1. Explicit failure code (for playbook lookup)
//  2. Human-readable error message
//  3. Container left in safe state (running or recoverable)
//  4. No guessing, no fallback logic, no silent failures
//
// SAFETY ZONES:
// - Before backup: Container untouched, fully running (SAFE)
// - After backup, before stop: Container still running, backup exists (SAFE)
// - After stop: Container stopped but recoverable via backup + restart (RECOVERABLE)
// - After health check fails: NEW container running but unhealthy, backup exists (RECOVERABLE)
//
// ALL FAILURE CODES HAVE RECOVERY PLAYBOOKS:
// See internal/recovery/playbook.go for complete recovery instructions.
// Every failure includes next steps for manual recovery.
func (s *Server) executeUpgrade(job *jobs.Job, manifestData *manifest.Manifest, archSupport map[string]string, steppingStone string) {
	ctx := context.Background()
	isDryRun := s.config.ExecutionMode == "dry-run"
	imageTag := job.ResolvedTarget
	imageRepo := manifestData.Image.Repo
	policyInitVersion := s.fetchPolicyInitVersion(ctx)

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

	// Phase 1: Resolve target container name
	containerName, ok := s.resolveTargetContainer(ctx, job, manifestData)
	if !ok {
		return
	}

	// Phase 2: Prepare upgrade arguments (extract runtime state & build docker args).
	// Also applies arch suffix from current container tag (e.g. 1.9.3 → 1.9.3-arm64).
	dockerArgs, imageTag, ok := s.prepareUpgradeArgs(ctx, job, containerName, manifestData, imageTag, archSupport)
	if !ok {
		return
	}

	// Phase 3: Execute dry-run if configured
	if isDryRun {
		s.executeDryRun(job, imageRepo, imageTag, containerName, dockerArgs)
		return
	}

	// EXECUTE mode: perform actual upgrade

	// Phase 4: Pre-flight checks
	if !s.preflightChecks(ctx, job, containerName) {
		return
	}

	if steppingStone != "" {
		// TWO-HOP UPGRADE: breakpoint chaining.
		// Hop 1: upgrade silently through the stepping stone version.
		// Hop 2: upgrade to the resolved target (breakpoint version).
		// Both hops use the same pre-hop backup for rollback safety.

		// Phase 5a: Pull stepping stone image
		steppingArgs, steppingTag, ok := s.prepareUpgradeArgs(ctx, job, containerName, manifestData, steppingStone, archSupport)
		if !ok {
			return
		}
		s.jobStore.AppendLog(fmt.Sprintf("Breakpoint upgrade: passing through stepping stone %s first, then continuing to %s", steppingTag, imageTag))
		if !s.pullUpgradeImage(ctx, job, imageRepo, steppingTag) {
			return
		}

		// Phase 6a: Quiesce + Backup (once, covers both hops)
		stoppedPrograms, usedSupervisor, ok := s.quiesceSupervisorPrograms(ctx, job, containerName)
		if !ok {
			return
		}
		if usedSupervisor {
			if _, ok := s.createPreUpgradeBackupAfterQuiesce(ctx, job, containerName, steppingTag, policyInitVersion, 3, stoppedPrograms); !ok {
				return
			}
		} else {
			if _, ok := s.createPreUpgradeBackupBeforeStop(ctx, job, containerName, steppingTag, policyInitVersion); !ok {
				return
			}
		}

		// Phase 7a: Stop → replace → verify stepping stone
		if !s.stopContainerForUpgrade(ctx, job, containerName) {
			return
		}
		if !s.replaceContainer(ctx, job, containerName, steppingArgs) {
			return
		}
		job.Message = fmt.Sprintf("Passing through %s, upgrading to %s...", steppingTag, imageTag)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		if !s.verifyUpgrade(ctx, job, containerName, steppingTag, policyInitVersion) {
			return
		}
		s.jobStore.AppendLog(fmt.Sprintf("Stepping stone %s healthy, continuing to %s", steppingTag, imageTag))

		// Phase 5b: Pull final image (stepping stone is now running — re-read runtime state)
		dockerArgs, imageTag, ok = s.prepareUpgradeArgs(ctx, job, containerName, manifestData, imageTag, archSupport)
		if !ok {
			return
		}
		if !s.pullUpgradeImage(ctx, job, imageRepo, imageTag) {
			return
		}

		// Phase 7b: Stop stepping stone → replace → verify final target
		if !s.stopContainerForUpgrade(ctx, job, containerName) {
			return
		}
		if !s.replaceContainer(ctx, job, containerName, dockerArgs) {
			return
		}
		if !s.verifyUpgrade(ctx, job, containerName, imageTag, policyInitVersion) {
			// Hop 2 failed. System is on stepping stone (now stopped). Report clearly.
			job.FailureCode = "HEALTHCHECK_FAILED"
			job.Message = fmt.Sprintf(
				"Upgrade to %s failed after passing through stepping stone %s. "+
					"System was on %s (healthy). Backup available at: %s. "+
					"Retry the upgrade to attempt %s again.",
				imageTag, steppingTag, steppingTag, job.BackupPath, imageTag,
			)
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			return
		}

		s.finalizeUpgrade(ctx, job, imageRepo, imageTag)
		return
	}

	// SINGLE-HOP UPGRADE (no stepping stone)

	// Phase 5: Pull image before stopping container
	if !s.pullUpgradeImage(ctx, job, imageRepo, imageTag) {
		return
	}

	// Phase 6: Quiesce supervisor programs (if available)
	stoppedPrograms, usedSupervisor, ok := s.quiesceSupervisorPrograms(ctx, job, containerName)
	if !ok {
		return
	}

	// Phase 7: Create backup (supervisor quiesce or fallback)
	if usedSupervisor {
		if _, ok := s.createPreUpgradeBackupAfterQuiesce(ctx, job, containerName, imageTag, policyInitVersion, 3, stoppedPrograms); !ok {
			return
		}
	} else {
		if _, ok := s.createPreUpgradeBackupBeforeStop(ctx, job, containerName, imageTag, policyInitVersion); !ok {
			return
		}
	}

	// Phase 8: Stop container before replacement
	if !s.stopContainerForUpgrade(ctx, job, containerName) {
		return
	}

	// Phase 9: Replace container with new version
	if !s.replaceContainer(ctx, job, containerName, dockerArgs) {
		return
	}

	// Phase 10: Verify upgrade (health and version checks)
	if !s.verifyUpgrade(ctx, job, containerName, imageTag, policyInitVersion) {
		return
	}

	// Phase 11: Finalize upgrade (mark complete and prune old images)
	s.finalizeUpgrade(ctx, job, imageRepo, imageTag)
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

func (s *Server) resolveCoreVersion(ctx context.Context, containerName, initVersion string) (string, bool, error) {
	versionResp, err := s.coreClient.Version(ctx)
	if err == nil && versionResp != nil && versionResp.Version != "" {
		legacy, legacyErr := corecompat.IsBeforeInit(versionResp.Version, initVersion)
		if legacyErr != nil {
			logger.Error("Server", "resolveCoreVersion", legacyErr)
			return versionResp.Version, false, nil
		}
		return versionResp.Version, legacy, nil
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

	return discovered.Name, nil
}
