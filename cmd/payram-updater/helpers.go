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
// If discovery fails, returns the default http://127.0.0.1:8080.
func discoverCoreBaseURLOrDefault(ctx context.Context, cfg *config.Config) string {
	// Use imagePattern for discovery (default to payramapp/payram if not overridden)
	imagePattern := "payramapp/payram:"
	if cfg.ImageRepoOverride != "" {
		imagePattern = cfg.ImageRepoOverride + ":"
	}

	// Use a null logger to suppress discovery logs for CLI commands
	nullLogger := log.New(io.Discard, "", 0)

	// Discover container
	discoverer := container.NewDiscoverer(cfg.DockerBin, imagePattern, nullLogger)
	discovered, err := discoverer.DiscoverPayramContainer(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to discover Payram container: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	// Extract runtime state to get ports
	inspector := container.NewInspector(cfg.DockerBin, nullLogger)
	runtimeState, err := inspector.ExtractRuntimeState(ctx, discovered.Name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to extract runtime state: %v\n", err)
		fmt.Fprintf(os.Stderr, "Falling back to http://127.0.0.1:8080\n")
		return "http://127.0.0.1:8080"
	}

	// Identify which port serves Payram Core
	identifier := container.NewPortIdentifier(nullLogger)
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
