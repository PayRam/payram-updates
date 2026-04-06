package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/jobs"
)

// discoverCoreBaseURLOrDefault discovers the Payram Core base URL dynamically.
// Priority: 1) CORE_BASE_URL env 2) TARGET_CONTAINER_NAME port discovery 3) semver-based discovery 4) fallback
func discoverCoreBaseURLOrDefault(ctx context.Context, cfg *config.Config) string {
	return discoverCoreBaseURLWithContainer(ctx, cfg, "")
}

// discoverCoreBaseURLWithContainer discovers the Payram Core base URL with optional container name override.
func discoverCoreBaseURLWithContainer(ctx context.Context, cfg *config.Config, containerNameOverride string) string {
	// 1. Use explicit CORE_BASE_URL if set
	if cfg.CoreBaseURL != "" {
		return cfg.CoreBaseURL
	}

	// Use a null logger to suppress discovery logs for CLI commands
	nullLogger := log.New(io.Discard, "", 0)
	inspector := container.NewInspector(cfg.DockerBin, nullLogger)
	identifier := container.NewPortIdentifier(nullLogger)

	// 2. Use provided container name override (from already-resolved context)
	if containerNameOverride != "" {
		runtimeState, err := inspector.ExtractRuntimeState(ctx, containerNameOverride)
		if err == nil {
			identifiedPort, err := identifier.IdentifyPayramCorePort(ctx, runtimeState)
			if err == nil {
				return fmt.Sprintf("http://127.0.0.1:%s", identifiedPort.HostPort)
			}
		}
		// Fall through to other methods if this fails
	}

	// 3. Use TARGET_CONTAINER_NAME if set
	if cfg.TargetContainerName != "" {
		runtimeState, err := inspector.ExtractRuntimeState(ctx, cfg.TargetContainerName)
		if err == nil {
			identifiedPort, err := identifier.IdentifyPayramCorePort(ctx, runtimeState)
			if err == nil {
				return fmt.Sprintf("http://127.0.0.1:%s", identifiedPort.HostPort)
			}
		}
		fmt.Fprintf(os.Stderr, "WARNING: Failed to identify port for container %s\n", cfg.TargetContainerName)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	// 4. Fall back to semver-based discovery
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, nullLogger)
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to discover Payram container: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	// Extract runtime state to get ports
	runtimeState, err := inspector.ExtractRuntimeState(ctx, discovered.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to extract runtime state: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	// Identify which port serves Payram Core
	identifiedPort, err := identifier.IdentifyPayramCorePort(ctx, runtimeState)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to identify Payram Core port: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	coreBaseURL := fmt.Sprintf("http://127.0.0.1:%s", identifiedPort.HostPort)
	return coreBaseURL
}

func promptYesNo(reader *bufio.Reader, prompt string, defaultValue bool) bool {
	defaultLabel := "y"
	if !defaultValue {
		defaultLabel = "n"
	}
	for {
		fmt.Printf("%s [y/n] (default: %s): ", prompt, defaultLabel)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(strings.ToLower(input))
		if input == "" {
			return defaultValue
		}
		switch input {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		default:
			fmt.Println("Please enter 'y' or 'n'.")
		}
	}
}

func promptInt(reader *bufio.Reader, prompt string, defaultValue int) int {
	for {
		fmt.Printf("%s (default: %d): ", prompt, defaultValue)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "" {
			return defaultValue
		}
		value, err := strconv.Atoi(input)
		if err != nil || value < 1 {
			fmt.Println("Please enter a valid integer >= 1.")
			continue
		}
		return value
	}
}

func getPort() int {
	// Load config the same way as daemon (env vars first, then /etc/payram/updater.env)
	cfg, err := config.Load()
	if err != nil {
		// If config loading fails, fall back to reading UPDATER_PORT directly
		if portStr := os.Getenv("UPDATER_PORT"); portStr != "" {
			var port int
			if _, err := fmt.Sscanf(portStr, "%d", &port); err == nil {
				return port
			}
		}
		// Default port
		return 2359
	}
	return cfg.Port
}

func isJobActive(job *jobs.Job) bool {
	return job.State == jobs.JobStatePolicyFetching ||
		job.State == jobs.JobStateManifestFetching ||
		job.State == jobs.JobStateExecuting ||
		job.State == jobs.JobStateVerifying ||
		job.State == jobs.JobStateBackingUp
}
