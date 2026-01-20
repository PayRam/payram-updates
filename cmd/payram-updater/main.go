package main

import (
	"fmt"
	"log"
	"os"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/http"
	"github.com/payram/payram-updater/internal/jobs"
)

func main() {
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
	server := http.New(cfg, jobStore)
	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
