package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/jobs"
)

func runCleanup() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "Usage: payram-updater cleanup <state|backups> [--yes]")
		os.Exit(1)
	}

	subcommand := os.Args[2]
	confirmYes := false
	for _, arg := range os.Args[3:] {
		if arg == "--yes" {
			confirmYes = true
		}
	}

	if subcommand != "state" && subcommand != "backups" {
		fmt.Fprintln(os.Stderr, "Invalid cleanup target. Use 'state' or 'backups'.")
		os.Exit(1)
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	// Block cleanup if a job is active
	jobStore := jobs.NewStore(cfg.StateDir)
	if job, err := jobStore.LoadLatest(); err == nil && job != nil && isJobActive(job) {
		fmt.Fprintln(os.Stderr, "Active job in progress. Cleanup is blocked.")
		os.Exit(1)
	}

	// Require confirmation unless --yes was provided
	if !confirmYes {
		fmt.Printf("WARNING: This will delete %s. Type \"yes\" to continue: ", subcommand)
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		if strings.ToLower(strings.TrimSpace(input)) != "yes" {
			fmt.Fprintln(os.Stderr, "Cleanup cancelled.")
			os.Exit(1)
		}
	}

	var targetDir string
	if subcommand == "state" {
		targetDir = cfg.StateDir
	} else {
		targetDir = cfg.Backup.Dir
	}

	if err := os.RemoveAll(targetDir); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to remove %s directory: %v\n", subcommand, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to recreate %s directory: %v\n", subcommand, err)
		os.Exit(1)
	}

	fmt.Printf("Cleanup complete: %s\n", subcommand)
}
