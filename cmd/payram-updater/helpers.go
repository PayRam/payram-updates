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
)

// discoverCoreBaseURLWithContainer returns the PayRam API address, the same way
// the daemon finds it (container.FindCoreBaseURL). It returns "" (with a
// warning) when PayRam's container cannot be found; callers then report that
// PayRam is not reachable instead of probing a guessed port.
func discoverCoreBaseURLWithContainer(ctx context.Context, cfg *config.Config, containerNameOverride string) string {
	if cfg.CoreBaseURL != "" {
		return cfg.CoreBaseURL
	}

	// Use a null logger to suppress discovery logs for CLI commands
	nullLogger := log.New(io.Discard, "", 0)

	name := containerNameOverride
	if name == "" {
		name = cfg.TargetContainerName
	}
	if name == "" {
		imagePattern := "payramapp/payram:"
		if cfg.ImageRepoOverride != "" {
			imagePattern = cfg.ImageRepoOverride + ":"
		}
		discovered, err := container.NewDiscoverer(cfg.DockerBin, imagePattern, nullLogger).DiscoverPayramContainer(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: could not find a running PayRam container: %v\n", err)
			return ""
		}
		name = discovered.Name
	}

	url, how, err := container.FindCoreBaseURL(ctx, cfg.DockerBin, container.ContainerName(name), "", nullLogger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: %v\n", err)
		return ""
	}
	if how == container.CoreURLGuessed {
		fmt.Fprintf(os.Stderr, "Note: PayRam is not answering right now; using %s\n", url)
	}
	return url
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
		return 2567
	}
	return cfg.Port
}
