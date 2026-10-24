package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/payram/payram-updater/internal/cli"
)

func runDryRun() {
	// Parse flags for dry-run command
	dryRunCmd := flag.NewFlagSet("dry-run", flag.ExitOnError)
	mode := dryRunCmd.String("mode", "manual", "Upgrade mode (dashboard or manual)")
	to := dryRunCmd.String("to", "", "Target version")

	// Parse arguments after "dry-run"
	dryRunCmd.Parse(os.Args[2:])

	// Use shared validation
	req, err := cli.ParseUpgradeRequest(*mode, *to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	status, body := postToDaemon("/upgrade/plan", map[string]string{
		"mode":            string(req.Mode),
		"requestedTarget": req.RequestedTarget,
		"source":          "CLI",
	})

	// Pretty-print JSON
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		// If JSON formatting fails, just print raw response
		fmt.Println(string(body))
		if status != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	fmt.Println(prettyJSON.String())

	// Check if plan failed
	var planResp struct {
		State       string `json:"state"`
		FailureCode string `json:"failureCode"`
	}
	if err := json.Unmarshal(body, &planResp); err == nil {
		if planResp.State == "FAILED" {
			os.Exit(1)
		}
	}

	if status != http.StatusOK {
		os.Exit(1)
	}
}

func runRun() {
	// Parse flags for run command
	runCmd := flag.NewFlagSet("run", flag.ExitOnError)
	mode := runCmd.String("mode", "manual", "Upgrade mode (dashboard or manual)")
	to := runCmd.String("to", "", "Target version")
	yes := runCmd.Bool("yes", false, "Skip confirmation prompt")

	// Parse arguments after "run"
	runCmd.Parse(os.Args[2:])

	// Use shared validation
	req, err := cli.ParseUpgradeRequest(*mode, *to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Step 1: Call /upgrade/plan to validate and get resolved values
	_, planBody := postToDaemon("/upgrade/plan", map[string]string{
		"mode":            string(req.Mode),
		"requestedTarget": req.RequestedTarget,
		"source":          "CLI",
	})

	// Parse plan response
	var plan struct {
		State           string `json:"state"`
		Mode            string `json:"mode"`
		RequestedTarget string `json:"requestedTarget"`
		ResolvedTarget  string `json:"resolvedTarget"`
		FailureCode     string `json:"failureCode"`
		Message         string `json:"message"`
		ImageRepo       string `json:"imageRepo"`
		ContainerName   string `json:"containerName"`
	}
	if err := json.Unmarshal(planBody, &plan); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse plan response: %v\n", err)
		os.Exit(1)
	}

	// Step 2: If planning failed, show the error and exit (no prompt)
	if plan.State == "FAILED" {
		fmt.Fprintf(os.Stderr, "Upgrade validation failed:\n")
		fmt.Fprintf(os.Stderr, "  Code: %s\n", plan.FailureCode)
		fmt.Fprintf(os.Stderr, "  Message: %s\n", plan.Message)
		os.Exit(1)
	}

	// Step 3: Planning succeeded - prompt for confirmation
	summary := &cli.UpgradeSummary{
		Mode:            plan.Mode,
		RequestedTarget: plan.RequestedTarget,
		ResolvedTarget:  plan.ResolvedTarget,
		ImageRepo:       plan.ImageRepo,
		ContainerName:   plan.ContainerName,
	}

	confirmer := cli.NewConfirmer()
	confirmer.ConfirmOrExit(summary, *yes)

	// Step 4: User confirmed - call /upgrade/run to start the job
	runStatus, runBody := postToDaemon("/upgrade/run", map[string]string{
		"mode":            string(req.Mode),
		"requestedTarget": req.RequestedTarget,
		"source":          "CLI",
	})

	// Handle conflict (409)
	if runStatus == http.StatusConflict {
		var conflictResp struct {
			Error string `json:"error"`
			JobID string `json:"jobId"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(runBody, &conflictResp); err == nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", conflictResp.Error)
			fmt.Fprintf(os.Stderr, "Active job: %s (state=%s)\n", conflictResp.JobID, conflictResp.State)
			fmt.Fprintf(os.Stderr, "Use 'payram-updater status' to check the current job.\n")
		} else {
			fmt.Fprintf(os.Stderr, "An upgrade job is already running.\n")
		}
		os.Exit(1)
	}

	// Parse run response
	var runResult struct {
		JobID           string `json:"jobId"`
		State           string `json:"state"`
		Mode            string `json:"mode"`
		RequestedTarget string `json:"requestedTarget"`
		ResolvedTarget  string `json:"resolvedTarget"`
		FailureCode     string `json:"failureCode"`
		Message         string `json:"message"`
	}
	if err := json.Unmarshal(runBody, &runResult); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse run response: %v\n", err)
		os.Exit(1)
	}

	// Check if run failed immediately (e.g., policy fetch failed after plan)
	if runResult.State == "FAILED" {
		fmt.Fprintf(os.Stderr, "Upgrade failed to start:\n")
		fmt.Fprintf(os.Stderr, "  Code: %s\n", runResult.FailureCode)
		fmt.Fprintf(os.Stderr, "  Message: %s\n", runResult.Message)
		os.Exit(1)
	}

	// Success - print job info
	fmt.Printf("Started upgrade job %s (state=%s).\n", runResult.JobID, runResult.State)
	fmt.Println("Use 'payram-updater status' to check progress and 'payram-updater logs' for details.")
}
