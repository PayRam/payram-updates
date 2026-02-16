package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/recovery"
)

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

	// Parse response to check for recovery playbook
	var statusResp struct {
		State            string             `json:"state"`
		FailureCode      string             `json:"failureCode"`
		Message          string             `json:"message"`
		RecoveryPlaybook *recovery.Playbook `json:"recoveryPlaybook,omitempty"`
	}

	if err := json.Unmarshal(body, &statusResp); err == nil && statusResp.RecoveryPlaybook != nil {
		// Format with human-readable playbook
		printStatusWithPlaybook(body, statusResp.RecoveryPlaybook)
		return
	}

	// Pretty-print JSON (no playbook or parsing failed)
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to format JSON: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(prettyJSON.String())
}

// printStatusWithPlaybook formats status output with human-readable playbook
func printStatusWithPlaybook(body []byte, playbook *recovery.Playbook) {
	// First print the JSON status
	var prettyJSON bytes.Buffer
	if err := json.Indent(&prettyJSON, body, "", "  "); err != nil {
		fmt.Println(string(body))
	} else {
		fmt.Println(prettyJSON.String())
	}

	// Then print formatted recovery instructions
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Printf("⚠️  RECOVERY: %s\n", playbook.Title)
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nSeverity: %s\n", playbook.Severity)
	fmt.Printf("Data Risk: %s\n", playbook.DataRisk)
	fmt.Printf("\n%s\n", playbook.UserMessage)
	fmt.Println("\n--- Recovery Steps (SSH) ---")
	for _, step := range playbook.SSHSteps {
		fmt.Printf("  %s\n", step)
	}
	if playbook.DocsURL != "" {
		fmt.Printf("\nDocumentation: %s\n", playbook.DocsURL)
	}
	fmt.Println(strings.Repeat("=", 60))
}

func runLogs() {
	logsCmd := flag.NewFlagSet("logs", flag.ExitOnError)
	followShort := logsCmd.Bool("f", false, "Follow logs (like tail -f)")
	followLong := logsCmd.Bool("follow", false, "Follow logs (like tail -f)")
	logsCmd.Parse(os.Args[2:])

	follow := *followShort || *followLong

	port := getPort()
	url := fmt.Sprintf("http://127.0.0.1:%d/upgrade/logs", port)

	fetchLogs := func() (string, int, error) {
		resp, err := http.Get(url)
		if err != nil {
			return "", 0, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", resp.StatusCode, err
		}
		return string(body), resp.StatusCode, nil
	}

	if !follow {
		body, status, err := fetchLogs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
			fmt.Fprintf(os.Stderr, "Is the payram-updater daemon running?\n")
			os.Exit(1)
		}
		if status != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Failed to read logs: HTTP %d\n", status)
			os.Exit(1)
		}

		// Print plain text logs directly
		fmt.Print(body)
		return
	}

	lastSize := 0
	first := true
	for {
		body, status, err := fetchLogs()
		if err != nil {
			if first {
				fmt.Fprintf(os.Stderr, "Failed to connect to daemon: %v\n", err)
				fmt.Fprintf(os.Stderr, "Is the payram-updater daemon running?\n")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Warning: failed to fetch logs: %v\n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		if status != http.StatusOK {
			if first {
				fmt.Fprintf(os.Stderr, "Failed to read logs: HTTP %d\n", status)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Warning: failed to read logs: HTTP %d\n", status)
			time.Sleep(1 * time.Second)
			continue
		}

		if len(body) < lastSize {
			fmt.Print(body)
			lastSize = len(body)
		} else if len(body) > lastSize {
			fmt.Print(body[lastSize:])
			lastSize = len(body)
		}

		first = false
		time.Sleep(1 * time.Second)
	}
}
