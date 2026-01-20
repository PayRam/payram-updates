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
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		fmt.Fprintf(os.Stderr, "Available commands: serve, status, logs, dry-run\n")
		os.Exit(1)
	}
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
		fmt.Fprintf(os.Stderr, "Failed to get status: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "Failed to get logs: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "Failed to initiate upgrade: %v\n", err)
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
	// Try to get port from environment
	if portStr := os.Getenv("UPDATER_PORT"); portStr != "" {
		var port int
		if _, err := fmt.Sscanf(portStr, "%d", &port); err == nil {
			return port
		}
	}
	// Default port
	return 2359
}
