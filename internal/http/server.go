package http

import (
	"context"
	"fmt"
	"log"
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
	discoverer := container.NewDiscoverer(dockerBin, imagePattern, log.Default())
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to discover Payram container: %w", err)
	}

	// Step 2: Extract runtime state to get ports
	inspector := container.NewInspector(dockerBin, log.Default())
	runtimeState, err := inspector.ExtractRuntimeState(ctx, discovered.Name)
	if err != nil {
		return "", fmt.Errorf("failed to extract runtime state: %w", err)
	}

	// Step 3: Identify which port serves Payram Core
	identifier := container.NewPortIdentifier(log.Default())
	identifiedPort, err := identifier.IdentifyPayramCorePort(ctx, runtimeState)
	if err != nil {
		return "", fmt.Errorf("failed to identify Payram Core port: %w", err)
	}

	// Build the base URL from the identified port
	baseURL := fmt.Sprintf("http://127.0.0.1:%s", identifiedPort.HostPort)
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
		Logger:    log.Default(),
	}

	// Always discover CoreBaseURL dynamically via docker inspect
	log.Println("Discovering Payram Core port via docker inspect...")
	// Use imagePattern for discovery (default to payramapp/payram if not overridden)
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}
	coreBaseURL, err := discoverCoreBaseURL(cfg.DockerBin, imagePattern)
	if err != nil {
		log.Printf("WARNING: Failed to discover Payram Core URL: %v", err)
		log.Println("Falling back to http://127.0.0.1:8080 (this may not work if Payram Core is on a different port)")
		coreBaseURL = "http://127.0.0.1:8080"
	} else {
		log.Printf("Discovered Payram Core at: %s", coreBaseURL)
	}

	// Discover Payram container IP for access control
	log.Println("Discovering Payram container IP for access control...")
	payramContainerIP, err := network.GetPayramContainerIP(cfg.DockerBin, imagePattern)
	if err != nil {
		log.Printf("WARNING: Failed to discover Payram container IP: %v", err)
		log.Println("API access will be restricted to localhost only")
		payramContainerIP = ""
	} else {
		log.Printf("Payram container IP: %s (API access restricted to localhost and this container)", payramContainerIP)
	}

	// Create core API client
	coreClient := coreclient.NewClient(coreBaseURL)

	// Create backup manager (legacy, for backward compatibility with existing backups)
	// Backups are always enabled
	backupCfg := backup.Config{
		Dir:          cfg.Backup.Dir,
		Retention:    cfg.Backup.Retention,
		PGHost:       cfg.Backup.PGHost,
		PGPort:       cfg.Backup.PGPort,
		PGDB:         cfg.Backup.PGDB,
		PGUser:       cfg.Backup.PGUser,
		PGPassword:   cfg.Backup.PGPassword,
		ImagePattern: imagePattern,
	}
	backupMgr := backup.NewManager(backupCfg, &backup.RealExecutor{}, log.Default())

	// Create container-aware backup executor
	containerBackupExec := backup.NewContainerBackupExecutor(
		cfg.DockerBin,
		"pg_dump",
		cfg.Backup.Dir,
		log.Default(),
	)

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
	mux.HandleFunc("/upgrade", s.HandleUpgrade())
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
	handler := network.AllowedIPsMiddleware(allowedIPs, log.Default())(mux)
	log.Printf("API access restricted to: %v", allowedIPs)

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
			log.Printf("WARNING: Could not detect Docker bridge IP: %v", err)
			log.Printf("Starting HTTP server on localhost only: http://127.0.0.1:%d", s.port)
		} else {
			log.Printf("Starting HTTP server on local interfaces")
			log.Printf("  - Localhost: http://127.0.0.1:%d", s.port)
			log.Printf("  - Docker bridge: http://%s:%d", dockerIP, s.port)
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
				log.Printf("WARNING: Failed to bind docker bridge listener (%s): %v", bridgeAddr, bridgeErr)
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
		log.Printf("Received signal %v, initiating graceful shutdown", sig)
	}

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}

	log.Println("Server stopped gracefully")
	return nil
}

func (s *Server) startAutoUpdateLoop(ctx context.Context) {
	interval := time.Duration(s.config.AutoUpdateInterval) * time.Hour
	if interval <= 0 {
		log.Printf("Auto update disabled due to invalid interval: %d hours", s.config.AutoUpdateInterval)
		return
	}

	log.Printf("Auto update enabled. Checking every %d hours", s.config.AutoUpdateInterval)

	// Run once at startup
	s.runAutoUpdateOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Auto update loop stopped")
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
		log.Printf("Auto update: failed to load latest job: %v", err)
		return
	}
	if existingJob != nil {
		if isJobActive(existingJob) {
			log.Printf("Auto update: active job %s in state %s, skipping", existingJob.JobID, existingJob.State)
			return
		}
		if existingJob.State == jobs.JobStateFailed {
			log.Printf("Auto update: last job failed (%s), skipping", existingJob.FailureCode)
			return
		}
	}

	// Fetch policy to get latest version
	policyClient := policy.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
	policyCtx, cancel2 := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel2()
	policyData, err := policyClient.Fetch(policyCtx, s.config.PolicyURL)
	if err != nil {
		log.Printf("Auto update: failed to fetch policy: %v", err)
		return
	}
	latest := strings.TrimSpace(policyData.Latest)
	if latest == "" {
		log.Printf("Auto update: policy latest is empty, skipping")
		return
	}
	initVersion := strings.TrimSpace(policyData.UpdaterAPIInitVersion)

	containerName, err := s.discoverContainerName(ctx)
	if err != nil {
		log.Printf("Auto update: failed to discover container: %v", err)
		return
	}

	// Fetch current version (API or label fallback)
	versionCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel()
	currentVersion, _, err := s.resolveCoreVersion(versionCtx, containerName, initVersion)
	if err != nil {
		log.Printf("Auto update: failed to resolve current version: %v", err)
		return
	}

	if currentVersion == latest {
		log.Printf("Auto update: already on latest version %s", latest)
		return
	}

	// Plan upgrade using DASHBOARD mode
	planCtx, cancel3 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel3()
	plan := s.PlanUpgrade(planCtx, jobs.JobModeDashboard, latest)
	if plan.State == jobs.JobStateFailed {
		log.Printf("Auto update: planning failed (%s): %s", plan.FailureCode, plan.Message)
		return
	}

	// Re-check for active job to avoid race
	existingJob, err = s.jobStore.LoadLatest()
	if err == nil && existingJob != nil && isJobActive(existingJob) {
		log.Printf("Auto update: active job %s in state %s, skipping", existingJob.JobID, existingJob.State)
		return
	}

	jobID := fmt.Sprintf("job-%d", time.Now().UnixNano())
	job := jobs.NewJob(jobID, jobs.JobModeDashboard, plan.RequestedTarget)
	job.ResolvedTarget = plan.ResolvedTarget
	job.State = jobs.JobStateReady
	job.Message = "Auto update job created"
	job.UpdatedAt = time.Now().UTC()

	if err := s.jobStore.Save(job); err != nil {
		log.Printf("Auto update: failed to save job: %v", err)
		return
	}

	s.jobStore.AppendLog(fmt.Sprintf("Starting auto update job %s: mode=%s target=%s source=AUTO", jobID, "DASHBOARD", plan.RequestedTarget))
	go s.executeUpgrade(job, plan.Manifest)
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
func (s *Server) executeUpgrade(job *jobs.Job, manifestData *manifest.Manifest) {
	ctx := context.Background()
	isDryRun := s.config.ExecutionMode == "dry-run"
	imageTag := job.ResolvedTarget
	policyInitVersion := s.fetchPolicyInitVersion(ctx)

	upgradeData := map[string]string{
		"job_id":           job.JobID,
		"mode":             string(job.Mode),
		"requested_target": job.RequestedTarget,
		"resolved_target":  job.ResolvedTarget,
		"execution_mode":   s.config.ExecutionMode,
	}
	if isDryRun {
		upgradeData["dry_run"] = "true"
	}
	s.recordHistory(history.Event{
		Type:    "upgrade",
		Status:  "started",
		Message: "Upgrade started",
		Data:    upgradeData,
	})

	defer func() {
		status := ""
		message := job.Message
		data := map[string]string{
			"job_id":           job.JobID,
			"mode":             string(job.Mode),
			"requested_target": job.RequestedTarget,
			"resolved_target":  job.ResolvedTarget,
			"execution_mode":   s.config.ExecutionMode,
		}
		if job.State == jobs.JobStateFailed {
			status = "failed"
			if job.FailureCode != "" {
				data["failure_code"] = job.FailureCode
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

	// All config comes from manifest
	imageRepo := manifestData.Image.Repo

	// Resolve target container name (env > manifest, fallback to discovery)
	resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, log.Default())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		if resErr, ok := err.(*container.ResolutionError); ok && resErr.GetFailureCode() == "CONTAINER_NAME_UNRESOLVED" {
			imagePattern := "payramapp/payram:"
			if s.config.ImageRepoOverride != "" {
				imagePattern = s.config.ImageRepoOverride + ":"
			}
			discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, log.Default())
			discovered, discoverErr := discoverer.DiscoverPayramContainer(ctx)
			if discoverErr != nil {
				job.State = jobs.JobStateFailed
				job.FailureCode = resErr.GetFailureCode()
				job.Message = resErr.Error()
				job.UpdatedAt = time.Now().UTC()
				s.jobStore.Save(job)
				s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
				return
			}
			resolved = &container.ResolvedContainer{Name: discovered.Name}
		} else if resErr, ok := err.(*container.ResolutionError); ok {
			job.State = jobs.JobStateFailed
			job.FailureCode = resErr.GetFailureCode()
			job.Message = resErr.Error()
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
			return
		} else {
			job.State = jobs.JobStateFailed
			job.FailureCode = "CONTAINER_NAME_UNRESOLVED"
			job.Message = err.Error()
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
			return
		}
	}
	containerName := resolved.Name
	s.jobStore.AppendLog(fmt.Sprintf("Target container resolved as: %s", containerName))

	// PHASE E: DYNAMIC DOCKER RUN CONSTRUCTION
	// Extract runtime state from the running container, reconcile with manifest requirements,
	// and build docker run args that preserve ALL existing configuration.
	// This ensures upgrades are truly non-destructive operations.
	s.jobStore.AppendLog("Extracting runtime state from container...")
	inspector := container.NewInspector(s.config.DockerBin, log.Default())
	runtimeState, err := inspector.ExtractRuntimeState(ctx, containerName)
	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "RUNTIME_INSPECTION_FAILED"
		job.Message = fmt.Sprintf("Failed to inspect runtime state: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (container not modified)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog(fmt.Sprintf("Runtime state extracted: %d ports, %d mounts, %d env vars",
		len(runtimeState.Ports), len(runtimeState.Mounts), len(runtimeState.Env)))

	// Build docker run arguments from runtime state + manifest overlays
	builder := container.NewDockerRunBuilder(log.Default())
	dockerArgs, err := builder.BuildUpgradeArgs(runtimeState, manifestData, imageTag)
	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_RUN_BUILD_FAILED"
		job.Message = fmt.Sprintf("Failed to build docker run args: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (container not modified)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog("Docker run arguments built successfully (runtime parity preserved)")

	if isDryRun {
		// DRY-RUN mode: log what would be executed
		s.jobStore.AppendLog("DRY-RUN mode: would execute the following steps:")
		// Backups are always enabled
		s.jobStore.AppendLog("  0. Create database backup")
		s.jobStore.AppendLog(fmt.Sprintf("  1. Pull image: %s:%s", imageRepo, imageTag))
		s.jobStore.AppendLog(fmt.Sprintf("  2. Stop container: %s", containerName))
		s.jobStore.AppendLog(fmt.Sprintf("  3. Remove container: %s", containerName))
		s.jobStore.AppendLog(fmt.Sprintf("  4. Run new container: docker %s", strings.Join(dockerArgs, " ")))
		s.jobStore.AppendLog("  5. Verify: container running")
		s.jobStore.AppendLog("  6. Verify: /api/v1/health endpoint")
		s.jobStore.AppendLog("  7. Verify: /api/v1/version matches target")

		job.State = jobs.JobStateReady
		job.Message = "Dry-run validation complete"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog("Dry-run complete - no changes made")
		return
	}

	// EXECUTE mode: perform actual upgrade

	// Pre-flight check: Verify Docker daemon is running
	s.jobStore.AppendLog("Pre-flight: Checking Docker daemon...")
	if err := backup.CheckDockerDaemon(ctx, s.config.DockerBin); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_DAEMON_DOWN"
		job.Message = "Docker daemon is not running"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		s.jobStore.AppendLog("Next steps: Start Docker daemon with 'sudo systemctl start docker' and retry.")
		return
	}
	s.jobStore.AppendLog("Docker daemon is running")

	// Step 0: Create database backup (before any destructive operations)
	job.State = jobs.JobStateBackingUp
	job.Message = "Creating database backup"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)

	// Get current version for backup metadata
	currentVersion := "unknown"
	if versionInfo, _, err := s.resolveCoreVersion(ctx, containerName, policyInitVersion); err == nil && versionInfo != "" {
		currentVersion = versionInfo
	}

	s.jobStore.AppendLog(fmt.Sprintf("Creating pre-upgrade backup (from %s to %s)...", currentVersion, imageTag))
	s.recordHistory(history.Event{
		Type:    "backup",
		Status:  "started",
		Message: "Backup started",
		Data: map[string]string{
			"job_id":         job.JobID,
			"from_version":   currentVersion,
			"target_version": imageTag,
			"container":      containerName,
		},
	})

	// Use container-aware backup: extracts DB credentials from running container
	backupResult := s.containerBackupExec.ExecuteBackup(ctx, containerName, backup.BackupMeta{
		FromVersion:   currentVersion,
		TargetVersion: imageTag,
		JobID:         job.JobID,
	})

	if !backupResult.Success {
		job.State = jobs.JobStateFailed
		job.FailureCode = backupResult.FailureCode
		job.Message = backupResult.ErrorMessage
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		s.recordHistory(history.Event{
			Type:    "backup",
			Status:  "failed",
			Message: backupResult.ErrorMessage,
			Data: map[string]string{
				"job_id":         job.JobID,
				"from_version":   currentVersion,
				"target_version": imageTag,
				"failure_code":   backupResult.FailureCode,
			},
		})

		// Provide context-specific recovery guidance
		switch backupResult.FailureCode {
		case "DOCKER_DAEMON_DOWN":
			s.jobStore.AppendLog("Next steps: Start Docker daemon with 'sudo systemctl start docker' and retry.")
		case "CONTAINER_NOT_FOUND":
			s.jobStore.AppendLog(fmt.Sprintf("Next steps: Ensure container '%s' is running and retry.", containerName))
		case "INVALID_DB_CONFIG":
			s.jobStore.AppendLog("Next steps: Verify container has POSTGRES_* environment variables set.")
		case "BACKUP_TIMEOUT":
			s.jobStore.AppendLog("Next steps: Check database connectivity and size. Increase timeout if needed.")
		default:
			s.jobStore.AppendLog("Next steps: Check logs and database connectivity, then retry.")
		}
		return
	}

	job.BackupPath = backupResult.Path
	s.jobStore.AppendLog(fmt.Sprintf("Backup created successfully: %s (%.2f MB)", backupResult.Filename, float64(backupResult.Size)/(1024*1024)))
	if backupResult.DBConfig != nil {
		dbType := "external"
		if backupResult.DBConfig.IsLocalDB() {
			dbType = "local (in-container)"
		}
		s.jobStore.AppendLog(fmt.Sprintf("Database: %s@%s:%s (%s)", backupResult.DBConfig.Database, backupResult.DBConfig.Host, backupResult.DBConfig.Port, dbType))
	}
	backupData := map[string]string{
		"job_id":         job.JobID,
		"from_version":   currentVersion,
		"target_version": imageTag,
		"backup_path":    backupResult.Path,
		"size_bytes":     fmt.Sprintf("%d", backupResult.Size),
	}
	if backupResult.DBConfig != nil {
		backupData["db_host"] = backupResult.DBConfig.Host
		backupData["db_port"] = backupResult.DBConfig.Port
		backupData["db_name"] = backupResult.DBConfig.Database
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

	// Step 1: Pull image
	job.State = jobs.JobStateExecuting
	job.UpdatedAt = time.Now().UTC()

	// Step 1: Pull image (always pull from Docker Hub)
	imageWithTag := fmt.Sprintf("%s:%s", imageRepo, imageTag)
	job.Message = "Pulling image"
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Pulling image: %s", imageWithTag))

	if err := s.dockerRunner.Pull(ctx, imageWithTag); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_PULL_FAILED"
		job.Message = fmt.Sprintf("Failed to pull image: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (container still running)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog("Image pulled successfully")

	// Step 2: Stop container
	job.Message = "Stopping container"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Stopping container: %s", containerName))

	if err := s.dockerRunner.Stop(ctx, containerName); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to stop container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog("Container stopped successfully")

	// Step 3: Remove container
	job.Message = "Removing container"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Removing container: %s", containerName))

	if err := s.dockerRunner.Remove(ctx, containerName); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to remove container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog("Container removed successfully")

	// Step 4: Run new container
	job.Message = "Running new container"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("Running new container: %s", containerName))

	if err := s.dockerRunner.Run(ctx, dockerArgs); err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to run container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog("Container started successfully")

	// Step 5: Verify container is running
	job.State = jobs.JobStateVerifying
	job.Message = "Verifying container status"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog("Verifying container is running...")

	running, err := s.dockerRunner.InspectRunning(ctx, containerName)
	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = fmt.Sprintf("Failed to inspect container: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}

	if !running {
		job.State = jobs.JobStateFailed
		job.FailureCode = "DOCKER_ERROR"
		job.Message = "Container is not running after start"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog("Container is running")

	// Step 6: Verify health endpoint with retries
	job.Message = "Verifying health endpoint"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)

	useLegacyHealth := s.shouldUseLegacyForTarget(policyInitVersion, imageTag)
	if useLegacyHealth {
		s.jobStore.AppendLog("Verifying legacy health endpoint (6 retries, 2s apart)...")
	} else {
		s.jobStore.AppendLog("Verifying /api/v1/health endpoint (6 retries, 2s apart)...")
	}

	healthOK := false
	for attempt := 1; attempt <= 6; attempt++ {
		healthCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var healthResp *coreclient.HealthResponse
		var err error
		if useLegacyHealth {
			err = corecompat.LegacyHealth(healthCtx, s.coreClient.BaseURL)
			if err == nil {
				healthResp = &coreclient.HealthResponse{Status: "ok"}
			}
		} else {
			healthResp, err = s.coreClient.Health(healthCtx)
		}
		cancel()

		// Require status == "ok"
		// If db field is present, it must also be "ok"
		if err == nil && healthResp.Status == "ok" {
			// Validate db field only if present
			if healthResp.DB != "" && healthResp.DB != "ok" {
				s.jobStore.AppendLog(fmt.Sprintf("Health check attempt %d: status ok but db=%s (retrying...)", attempt, healthResp.DB))
				if attempt < 6 {
					time.Sleep(2 * time.Second)
				}
				continue
			}
			// Success: status is ok, and db is either not present or is ok
			if healthResp.DB != "" {
				s.jobStore.AppendLog(fmt.Sprintf("Health check passed on attempt %d (status=%s, db=%s)", attempt, healthResp.Status, healthResp.DB))
			} else {
				s.jobStore.AppendLog(fmt.Sprintf("Health check passed on attempt %d (status=%s)", attempt, healthResp.Status))
			}
			healthOK = true
			break
		}

		if attempt < 6 {
			s.jobStore.AppendLog(fmt.Sprintf("Health check attempt %d failed: %v (retrying...)", attempt, err))
			time.Sleep(2 * time.Second)
		} else {
			s.jobStore.AppendLog(fmt.Sprintf("Health check attempt %d failed: %v", attempt, err))
		}
	}

	if !healthOK {
		job.State = jobs.JobStateFailed
		job.FailureCode = "HEALTHCHECK_FAILED"
		job.Message = "Health check failed after 6 attempts"
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}

	// Step 7: Verify version matches target
	job.Message = "Verifying version"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)

	if useLegacyHealth {
		s.jobStore.AppendLog("Verifying container label version matches target...")
	} else {
		s.jobStore.AppendLog("Verifying /api/v1/version matches target...")
	}

	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	var versionResp *coreclient.VersionResponse
	if useLegacyHealth {
		versionValue, labelErr := corecompat.VersionFromLabels(versionCtx, s.config.DockerBin, containerName)
		if labelErr == nil {
			versionResp = &coreclient.VersionResponse{Version: versionValue}
		} else {
			err = labelErr
		}
	} else {
		versionResp, err = s.coreClient.Version(versionCtx)
	}
	cancel()

	if err != nil {
		job.State = jobs.JobStateFailed
		job.FailureCode = "VERSION_MISMATCH"
		job.Message = fmt.Sprintf("Failed to get version: %v", err)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}

	if versionResp.Version != imageTag {
		job.State = jobs.JobStateFailed
		job.FailureCode = "VERSION_MISMATCH"
		job.Message = fmt.Sprintf("Version mismatch: expected %s, got %s", imageTag, versionResp.Version)
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
		return
	}
	s.jobStore.AppendLog(fmt.Sprintf("Version verified: %s", versionResp.Version))

	// Success! All verification passed
	job.State = jobs.JobStateReady
	job.Message = "Upgrade completed successfully"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("SUCCESS: Upgrade to %s completed successfully", imageTag))
}

func (s *Server) fetchPolicyInitVersion(ctx context.Context) string {
	policyClient := policy.NewClient(time.Duration(s.config.FetchTimeoutSeconds) * time.Second)
	policyCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.FetchTimeoutSeconds)*time.Second)
	defer cancel()
	policyData, err := policyClient.Fetch(policyCtx, s.config.PolicyURL)
	if err != nil {
		log.Printf("Warning: failed to fetch policy for init version: %v", err)
		return ""
	}
	return strings.TrimSpace(policyData.UpdaterAPIInitVersion)
}

func (s *Server) resolveCoreVersion(ctx context.Context, containerName, initVersion string) (string, bool, error) {
	versionResp, err := s.coreClient.Version(ctx)
	if err == nil && versionResp != nil && versionResp.Version != "" {
		legacy, legacyErr := corecompat.IsBeforeInit(versionResp.Version, initVersion)
		if legacyErr != nil {
			log.Printf("Warning: failed to compare versions: %v", legacyErr)
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
		log.Printf("Warning: failed to compare versions: %v", legacyErr)
		return labelVersion, false, nil
	}

	return labelVersion, legacy, nil
}

func (s *Server) shouldUseLegacyForTarget(initVersion, targetVersion string) bool {
	legacy, err := corecompat.IsBeforeInit(targetVersion, initVersion)
	if err != nil {
		log.Printf("Warning: failed to compare target version: %v", err)
		return false
	}
	return legacy
}

func (s *Server) discoverContainerName(ctx context.Context) (string, error) {
	imagePattern := "payramapp/payram:"
	if s.config.ImageRepoOverride != "" {
		imagePattern = s.config.ImageRepoOverride + ":"
	}

	discoverer := container.NewDiscoverer(s.config.DockerBin, imagePattern, log.Default())
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		return "", err
	}

	return discovered.Name, nil
}
