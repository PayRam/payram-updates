package main

import (
	"fmt"
	"os"

	"github.com/payram/payram-updater/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("payram-updater starting with config:\n")
	fmt.Printf("  Port: %d\n", cfg.Port)
	fmt.Printf("  PolicyURL: %s\n", cfg.PolicyURL)
	fmt.Printf("  RuntimeManifestURL: %s\n", cfg.RuntimeManifestURL)
	fmt.Printf("  FetchTimeout: %d seconds\n", cfg.FetchTimeoutSeconds)
	fmt.Printf("  StateDir: %s\n", cfg.StateDir)
	fmt.Printf("  LogDir: %s\n", cfg.LogDir)
}
