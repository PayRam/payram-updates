package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/payram/payram-updater/internal/autoupdate"
	"github.com/payram/payram-updater/internal/config"
	internalhttp "github.com/payram/payram-updater/internal/http"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
)

func runServe() {
	logger.Init()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("Daemon", "runServe", err)
		os.Exit(1)
	}

	settingsPath, err := autoupdate.DefaultPath()
	if err != nil {
		logger.Error("Daemon", "runServe", err)
		os.Exit(1)
	}
	settings, err := autoupdate.Load(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			logger.ErrorMsg("Daemon", "runServe", "Updater is not initialized. Run 'payram-updater init' first.")
			os.Exit(1)
		}
		logger.Error("Daemon", "runServe", err)
		os.Exit(1)
	}
	if !settings.Initialized {
		logger.ErrorMsg("Daemon", "runServe", "Updater is not initialized. Run 'payram-updater init' first.")
		os.Exit(1)
	}

	cfg.AutoUpdateEnabled = settings.AutoUpdateEnabled
	cfg.AutoUpdateInterval = settings.AutoUpdateIntervalHours

	logger.Infof("Daemon", "runServe", "payram-updater starting with config:")
	logger.Infof("Daemon", "runServe", "Port: %d", cfg.Port)
	logger.Infof("Daemon", "runServe", "PolicyURL: %s", cfg.PolicyURL)
	logger.Infof("Daemon", "runServe", "RuntimeManifestURL: %s", cfg.RuntimeManifestURL)
	logger.Infof("Daemon", "runServe", "FetchTimeout: %d seconds", cfg.FetchTimeoutSeconds)
	logger.Infof("Daemon", "runServe", "StateDir: %s", cfg.StateDir)
	logger.Infof("Daemon", "runServe", "CoreBaseURL: %s", cfg.CoreBaseURL)
	logger.Infof("Daemon", "runServe", "ExecutionMode: %s", cfg.ExecutionMode)
	logger.Infof("Daemon", "runServe", "DockerBin: %s", cfg.DockerBin)
	logger.Infof("Daemon", "runServe", "AutoUpdateEnabled: %v", cfg.AutoUpdateEnabled)
	logger.Infof("Daemon", "runServe", "AutoUpdateIntervalHours: %d", cfg.AutoUpdateInterval)

	// Create job store
	jobStore := jobs.NewStore(cfg.StateDir)

	// Create and start the HTTP server
	server := internalhttp.New(cfg, jobStore)
	if err := server.Start(); err != nil {
		logger.Error("Daemon", "runServe", err)
		os.Exit(1)
	}
}

func runInit() {
	reader := bufio.NewReader(os.Stdin)

	defaultEnabled := config.DefaultAutoUpdateEnabled
	autoUpdateEnabled := promptYesNo(reader, "Enable auto updates?", defaultEnabled)

	defaultInterval := config.DefaultAutoUpdateIntervalHours
	autoUpdateInterval := defaultInterval
	if autoUpdateEnabled {
		autoUpdateInterval = promptInt(reader, "Auto update interval (hours)", defaultInterval)
	}

	settings := &autoupdate.Settings{
		AutoUpdateEnabled:       autoUpdateEnabled,
		AutoUpdateIntervalHours: autoUpdateInterval,
		Initialized:             true,
	}

	settingsPath, err := autoupdate.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve auto update config path: %v\n", err)
		os.Exit(1)
	}

	if err := autoupdate.Save(settingsPath, settings); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write auto update config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Initialization complete. Updated %s\n", settingsPath)
}

func runRestart() {
	fmt.Println("Restarting payram-updater service...")

	// Check if systemctl is available
	systemctlPath := "/usr/bin/systemctl"
	if _, err := os.Stat(systemctlPath); os.IsNotExist(err) {
		systemctlPath = "/bin/systemctl"
		if _, err := os.Stat(systemctlPath); os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "Error: systemctl not found. This command requires systemd.")
			os.Exit(1)
		}
	}

	// Execute systemctl restart
	cmd := exec.Command("sudo", systemctlPath, "restart", "payram-updater")
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to restart service: %v\n", err)
		if len(output) > 0 {
			fmt.Fprintf(os.Stderr, "Output: %s\n", string(output))
		}
		os.Exit(1)
	}

	fmt.Println("Service restarted successfully.")

	// Wait a moment for the service to start
	time.Sleep(2 * time.Second)

	// Show status
	fmt.Println("\nService status:")
	statusCmd := exec.Command("sudo", systemctlPath, "status", "payram-updater", "--no-pager", "-l")
	statusOutput, _ := statusCmd.CombinedOutput()
	if len(statusOutput) > 0 {
		fmt.Println(string(statusOutput))
	}
}
