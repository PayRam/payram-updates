package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/autoupdate"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	internalhttp "github.com/payram/payram-updater/internal/http"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/logger"
	"github.com/payram/payram-updater/internal/network"
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

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	if err := checkPayramContainer(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Init check failed: %v\n", err)
		os.Exit(1)
	}

	if err := checkUpdaterPortExposure(cfg.Port); err != nil {
		fmt.Fprintf(os.Stderr, "Init check failed: %v\n", err)
		os.Exit(1)
	}

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

	if err := ensureSupervisorEnvConfig("/etc/payram/updater.env"); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to update supervisor config in updater.env: %v\n", err)
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

func checkPayramContainer(cfg *config.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, logger.StdLogger())
	if _, err := discoverer.DiscoverPayramContainer(ctx); err != nil {
		return fmt.Errorf("Payram container not found: %w", err)
	}

	return nil
}

func checkUpdaterPortExposure(port int) error {
	allowedIPs := map[string]struct{}{
		"127.0.0.1": {},
		"::1":       {},
	}

	if dockerIP, err := network.GetDockerBridgeIP(); err == nil && dockerIP != "" {
		allowedIPs[dockerIP] = struct{}{}
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return fmt.Errorf("failed to enumerate network interfaces: %w", err)
	}

	for _, addr := range addrs {
		ip := extractIP(addr)
		if ip == nil {
			continue
		}
		if !ip.IsGlobalUnicast() || ip.IsLoopback() {
			continue
		}

		ipStr := ip.String()
		if _, allowed := allowedIPs[ipStr]; allowed {
			continue
		}

		if portReachable(ipStr, port) {
			return fmt.Errorf("updater port %d is reachable on %s (should be localhost or docker bridge only)", port, ipStr)
		}
	}

	return nil
}

func ensureSupervisorEnvConfig(path string) error {
	contentBytes, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: %s not found; supervisor config not persisted\n", path)
			return nil
		}
		return err
	}

	content := string(contentBytes)
	appendLines := []string{}
	if !strings.Contains(content, "SUPERVISOR_EXCLUDE=") {
		appendLines = append(appendLines, "SUPERVISOR_EXCLUDE=postgres,postgresql")
	}
	if !strings.Contains(content, "SUPERVISOR_INCLUDE=") {
		appendLines = append(appendLines, "SUPERVISOR_INCLUDE=")
	}
	if len(appendLines) == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	if !strings.HasSuffix(content, "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	for _, line := range appendLines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func extractIP(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

func portReachable(host string, port int) bool {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
