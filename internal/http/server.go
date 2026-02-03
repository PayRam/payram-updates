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
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/manifest"
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

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return s
}

// Start starts the HTTP server and blocks until shutdown.
// It handles graceful shutdown on SIGINT and SIGTERM.
func (s *Server) Start() error {
	// Create a channel to listen for shutdown signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Create a channel to capture server errors
	serverErrors := make(chan error, 1)

	// Start the server in a goroutine
	go func() {
		log.Printf("Starting HTTP server on 127.0.0.1:%d", s.port)

		// Use a listener to ensure we bind only to 127.0.0.1
		listener, err := net.Listen("tcp", s.httpServer.Addr)
		if err != nil {
			serverErrors <- fmt.Errorf("failed to create listener: %w", err)
			return
		}

		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErrors <- fmt.Errorf("HTTP server error: %w", err)
		}
	}()

	// Wait for either a signal or server error
	select {
	case err := <-serverErrors:
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

	// All config comes from manifest
	imageRepo := manifestData.Image.Repo

	// Resolve target container name (env > manifest, no defaults)
	resolver := container.NewResolver(s.config.TargetContainerName, s.config.DockerBin, log.Default())
	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		if resErr, ok := err.(*container.ResolutionError); ok {
			job.State = jobs.JobStateFailed
			job.FailureCode = resErr.GetFailureCode()
			job.Message = resErr.Error()
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
			return
		}
		job.State = jobs.JobStateFailed
		job.FailureCode = "CONTAINER_NAME_UNRESOLVED"
		job.Message = err.Error()
		job.UpdatedAt = time.Now().UTC()
		s.jobStore.Save(job)
		s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s", job.FailureCode, job.Message))
		return
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
		s.jobStore.AppendLog("  6. Verify: /health endpoint")
		s.jobStore.AppendLog("  7. Verify: /version matches target")
		s.jobStore.AppendLog("  8. Verify: migrations complete")

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
	if versionInfo, err := s.coreClient.Version(ctx); err == nil && versionInfo != nil {
		currentVersion = versionInfo.Version
	}

	s.jobStore.AppendLog(fmt.Sprintf("Creating pre-upgrade backup (from %s to %s)...", currentVersion, imageTag))

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

	// Step 6: Verify /health endpoint with retries
	job.Message = "Verifying health endpoint"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog("Verifying /health endpoint (6 retries, 2s apart)...")

	healthOK := false
	for attempt := 1; attempt <= 6; attempt++ {
		healthCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		healthResp, err := s.coreClient.Health(healthCtx)
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

	// Step 7: Verify /version matches target
	job.Message = "Verifying version"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog("Verifying /version matches target...")

	versionCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	versionResp, err := s.coreClient.Version(versionCtx)
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

	// Step 8: Verify migrations status
	job.Message = "Verifying migrations"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog("Verifying migrations status...")

	// Poll migration status with retries (up to 30 seconds for running migrations)
	migrationsComplete := false
	for attempt := 1; attempt <= 15; attempt++ {
		migrationsCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		migrationsResp, err := s.coreClient.MigrationsStatus(migrationsCtx)
		cancel()

		if err != nil {
			if attempt < 15 {
				s.jobStore.AppendLog(fmt.Sprintf("Migration status check attempt %d failed: %v (retrying...)", attempt, err))
				time.Sleep(2 * time.Second)
				continue
			}
			// Final attempt failed
			job.State = jobs.JobStateFailed
			job.FailureCode = "MIGRATION_FAILED"
			job.Message = fmt.Sprintf("Failed to get migrations status after %d attempts: %v", attempt, err)
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
			return
		}

		// Check migration state
		switch migrationsResp.State {
		case "complete":
			// Success!
			s.jobStore.AppendLog(fmt.Sprintf("Migrations verified: state=%s (attempt %d)", migrationsResp.State, attempt))
			migrationsComplete = true
			break
		case "running":
			// Migrations still in progress - wait and retry
			if attempt < 15 {
				s.jobStore.AppendLog(fmt.Sprintf("Migrations running (attempt %d/15) - waiting 2s...", attempt))
				time.Sleep(2 * time.Second)
				continue
			}
			// Timeout waiting for migrations to complete
			job.State = jobs.JobStateFailed
			job.FailureCode = "MIGRATION_TIMEOUT"
			job.Message = "Migrations still running after 30 seconds"
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
			return
		case "failed":
			// Hard failure - requires manual recovery
			job.State = jobs.JobStateFailed
			job.FailureCode = "MIGRATION_FAILED"
			job.Message = "Database migrations failed"
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
			return
		default:
			// Unknown state - treat as error
			job.State = jobs.JobStateFailed
			job.FailureCode = "MIGRATION_FAILED"
			job.Message = fmt.Sprintf("Unknown migration state: %s", migrationsResp.State)
			job.UpdatedAt = time.Now().UTC()
			s.jobStore.Save(job)
			s.jobStore.AppendLog(fmt.Sprintf("FAILED: %s - %s (manual recovery required)", job.FailureCode, job.Message))
			return
		}

		// Break out of loop if complete
		if migrationsComplete {
			break
		}
	}

	// Success!
	job.State = jobs.JobStateReady
	job.Message = "Upgrade completed successfully"
	job.UpdatedAt = time.Now().UTC()
	s.jobStore.Save(job)
	s.jobStore.AppendLog(fmt.Sprintf("SUCCESS: Upgrade to %s completed successfully", imageTag))
}
