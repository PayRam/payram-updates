package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/payram/payram-updater/internal/autoupdate"
	"github.com/payram/payram-updater/internal/config"
	internalhttp "github.com/payram/payram-updater/internal/http"
	"github.com/payram/payram-updater/internal/jobs"
)

func runServe() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	settingsPath, err := autoupdate.DefaultPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve auto update config path: %v\n", err)
		os.Exit(1)
	}
	settings, err := autoupdate.Load(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "Updater is not initialized. Run 'payram-updater init' first.")
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "Failed to load auto update config: %v\n", err)
		os.Exit(1)
	}
	if !settings.Initialized {
		fmt.Fprintln(os.Stderr, "Updater is not initialized. Run 'payram-updater init' first.")
		os.Exit(1)
	}

	cfg.AutoUpdateEnabled = settings.AutoUpdateEnabled
	cfg.AutoUpdateInterval = settings.AutoUpdateIntervalHours

	log.Printf("payram-updater starting with config:")
	log.Printf("  Port: %d", cfg.Port)
	log.Printf("  PolicyURL: %s", cfg.PolicyURL)
	log.Printf("  RuntimeManifestURL: %s", cfg.RuntimeManifestURL)
	log.Printf("  FetchTimeout: %d seconds", cfg.FetchTimeoutSeconds)
	log.Printf("  StateDir: %s", cfg.StateDir)
	log.Printf("  LogDir: %s", cfg.LogDir)
	log.Printf("  CoreBaseURL: %s", cfg.CoreBaseURL)
	log.Printf("  ExecutionMode: %s", cfg.ExecutionMode)
	log.Printf("  DockerBin: %s", cfg.DockerBin)
	log.Printf("  AutoUpdateEnabled: %v", cfg.AutoUpdateEnabled)
	log.Printf("  AutoUpdateIntervalHours: %d", cfg.AutoUpdateInterval)

	// Create job store
	jobStore := jobs.NewStore(cfg.StateDir)

	// Create and start the HTTP server
	server := internalhttp.New(cfg, jobStore)
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
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
