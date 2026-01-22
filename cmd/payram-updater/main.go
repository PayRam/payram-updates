package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/payram/payram-updater/internal/config"
	internalhttp "github.com/payram/payram-updater/internal/http"
	"github.com/payram/payram-updater/internal/jobs"
)

func main() {
	if len(os.Args) < 2 {
		// Default command is "serve"
		runServe()
		return
	}

	command := os.Args[1]
	// Handle help flags
	if command == "-h" || command == "--help" || command == "help" {
		printHelp()
		return
	}

	switch command {
	case "serve":
		runServe()
	case "status":
		runStatus()
	case "logs":
		runLogs()
	case "dry-run":
		runDryRun()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Print(`payram-updater - Payram runtime upgrade manager

USAGE:
  payram-updater [COMMAND]

COMMANDS:
  serve            Start the upgrade daemon (default)
  status           Get current upgrade status
  logs             Get upgrade logs
  dry-run          Initiate a dry-run upgrade
  help             Show this help message

DRY-RUN FLAGS:
  --mode string    Upgrade mode: 'dashboard' or 'manual' (required)
  --to string      Target version (required)

EXAMPLES:
  payram-updater serve
  payram-updater status
  payram-updater logs
  payram-updater dry-run --mode dashboard --to latest
  payram-updater dry-run --mode manual --to v1.2.3

CONFIG:
  Configuration is loaded from environment variables first,
  then from /etc/payram/updater.env if it exists.

`)
}

func runServe() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	log.Printf("payram-updater starting with config:")
	log.Printf("  Port: %d", cfg.Port)
	log.Printf("  PolicyURL: %s", cfg.PolicyURL)
	log.Printf("  RuntimeManifestURL: %s", cfg.RuntimeManifestURL)
	log.Printf("  FetchTimeout: %d seconds", cfg.FetchTimeoutSeconds)
	log.Printf("  StateDir: %s", cfg.StateDir)
	log.Printf("  LogDir: %s", cfg.LogDir)

	// Create job store
	jobStore := jobs.NewStore(cfg.StateDir)

	// Create and start the HTTP server
	server := internalhttp.New(cfg, jobStore)
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

func runStatus() {
	port := getPort()
	url := fmt.Sprintf("http://127.0.0.1:%d/upgrade/status", port)

	resp, err := http.Get(url)
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

	// Pretty-print JSON
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to format JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(prettyJSON.String())
}

func runLogs() {
	port := getPort()
	url := fmt.Sprintf("http://127.0.0.1:%d/upgrade/logs", port)

	resp, err := http.Get(url)
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

	// Print plain text logs directly
	fmt.Print(string(body))
}

func runDryRun() {
	// Parse flags for dry-run command
	dryRunCmd := flag.NewFlagSet("dry-run", flag.ExitOnError)
	mode := dryRunCmd.String("mode", "", "Upgrade mode (dashboard or manual)")
	to := dryRunCmd.String("to", "", "Target version")

	// Parse arguments after "dry-run"
	dryRunCmd.Parse(os.Args[2:])

	// Validate required flags
	if *mode == "" {
		fmt.Fprintf(os.Stderr, "Error: --mode flag is required (dashboard or manual)\n")
		os.Exit(1)
	}
	if *to == "" {
		fmt.Fprintf(os.Stderr, "Error: --to flag is required\n")
		os.Exit(1)
	}

	// Validate mode value
	upperMode := strings.ToUpper(*mode)
	if upperMode != "DASHBOARD" && upperMode != "MANUAL" {
		fmt.Fprintf(os.Stderr, "Error: --mode must be 'dashboard' or 'manual'\n")
		os.Exit(1)
	}

	port := getPort()
	url := fmt.Sprintf("http://127.0.0.1:%d/upgrade", port)

	// Create request payload
	payload := map[string]string{
		"mode":             upperMode,
		"requested_target": *to,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create request: %v\n", err)
		os.Exit(1)
	}

	// Send POST request
	resp, err := http.Post(url, "application/json", bytes.NewReader(payloadBytes))
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

	// Pretty-print JSON
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		// If JSON formatting fails, just print raw response
		fmt.Println(string(body))
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	fmt.Println(prettyJSON.String())
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
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
