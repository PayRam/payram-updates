package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/payram/payram-updater/internal/autoupdate"
	"github.com/payram/payram-updater/internal/cli"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/jobs"
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

// postToDaemon POSTs payload as JSON to the local daemon and returns its
// status and body. It sends root's CLI token when this user can read it
// (manual mode needs it). It exits with a readable message when the daemon
// cannot be reached or refuses the request outright (an answer that is not
// JSON, such as "run it with sudo").
func postToDaemon(path string, payload map[string]string) (int, []byte) {
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create request: %v\n", err)
		os.Exit(1)
	}
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", getPort(), path), bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	if token, err := cli.ReadToken(cliStateDir()); err == nil {
		req.Header.Set(cli.TokenHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
		fmt.Fprintf(os.Stderr, "Is the payram-updater daemon running?\n")
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read response: %v\n", err)
		os.Exit(1)
	}
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusConflict && !json.Valid(body) {
		fmt.Fprintf(os.Stderr, "Error: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	return resp.StatusCode, body
}

// loadLatestJob reads the last update's record for a read-only command. The
// record is readable by root only: without sudo the command still runs, and
// says what it is missing instead of reporting "no update".
func loadLatestJob(store *jobs.Store) *jobs.Job {
	job, err := store.LoadLatest()
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintln(os.Stderr, "Note: the last update's record is readable by root only; run this command with sudo to include it.")
		} else {
			fmt.Fprintf(os.Stderr, "Note: could not read the last update's record: %v\n", err)
		}
		return nil
	}
	return job
}

// cliStateDir is the daemon's state directory, as its config says.
func cliStateDir() string {
	if cfg, err := config.Load(); err == nil && cfg.StateDir != "" {
		return cfg.StateDir
	}
	if dir := os.Getenv("STATE_DIR"); dir != "" {
		return dir
	}
	return autoupdate.DefaultStateDir
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
