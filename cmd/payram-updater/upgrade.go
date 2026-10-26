package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/payram/payram-updater/internal/cli"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/recovery"
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
	yes := runCmd.Bool("yes", false, "Skip confirmation prompts and return once the update has started")

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

	// Step 4: User confirmed - call /upgrade/run to start the job. On a
	// terminal (and without --yes) this command follows the update to its
	// end and answers its question, should it need to restore the backup.
	attended := !*yes && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	runStatus, runBody := postToDaemon("/upgrade/run", map[string]any{
		"mode":            string(req.Mode),
		"requestedTarget": req.RequestedTarget,
		"source":          "CLI",
		"attended":        attended,
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
	if !attended {
		fmt.Println("Use 'payram-updater status' to check progress and 'payram-updater logs' for details.")
		return
	}
	fmt.Println("Following the update (Ctrl+C stops following; the update goes on).")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	code := followUpgrade(ctx, daemonURL(), runResult.JobID, os.Stdin, os.Stdout, 2*time.Second)
	stop()
	os.Exit(code)
}

// followedJob is what /upgrade/status says about the job being followed.
type followedJob struct {
	jobs.Job
	RecoveryPlaybook *recovery.Playbook `json:"recoveryPlaybook,omitempty"`
}

// followUpgrade prints the progress of update jobID until it ends, and asks
// the person at the terminal its question when it has one (whether to
// restore the backup and switch back). It returns the exit code: 0 when the
// update completed, 1 when it failed or cannot be followed, 130 when ctx was
// cancelled (Ctrl+C; the update goes on).
func followUpgrade(ctx context.Context, baseURL, jobID string, in io.Reader, out io.Writer, poll time.Duration) int {
	lines := readLines(in)
	lastMessage := ""
	answered := map[string]bool{}
	var unreachableSince time.Time
	for {
		job, err := fetchFollowedJob(ctx, baseURL)
		switch {
		case ctx.Err() != nil:
			fmt.Fprintln(out, "\nStopped following. The update goes on; 'payram-updater status' shows how it ends.")
			return 130
		case err != nil:
			if unreachableSince.IsZero() {
				unreachableSince = time.Now()
				fmt.Fprintf(out, "The updater is not answering (%v); still trying...\n", err)
			} else if time.Since(unreachableSince) > 15*time.Minute {
				fmt.Fprintln(out, "The updater has not answered for 15 minutes. Check it with: sudo systemctl status payram-updater; then 'payram-updater status'.")
				return 1
			}
		case job.JobID != jobID:
			fmt.Fprintf(out, "Another update (%s) is now the latest; 'payram-updater status' shows it.\n", job.JobID)
			return 1
		default:
			unreachableSince = time.Time{}
			if job.Message != lastMessage {
				fmt.Fprintf(out, "[%s] %s\n", time.Now().Format("15:04:05"), job.Message)
				lastMessage = job.Message
			}
			if q := job.Question; q != nil && !answered[q.ID] {
				answered[q.ID] = true
				answerQuestion(ctx, baseURL, q, lines, out)
				continue
			}
			if !job.IsActive() {
				return printFollowedResult(&job.Job, job.RecoveryPlaybook, out)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// answerQuestion asks q at the terminal and sends the answer: yes (also an
// empty line) restores the backup and switches back, no does not. Without
// an answer by its deadline the updater takes yes by itself.
func answerQuestion(ctx context.Context, baseURL string, q *jobs.Question, lines <-chan string, out io.Writer) {
	// A line typed before the question is not its answer.
	select {
	case <-lines:
	default:
	}
	fmt.Fprintf(out, "\n%s\n", q.Text)
	fmt.Fprintf(out, "Restore the backup and switch back? [Y/n] (without an answer by %s the backup is restored): ", q.Deadline.Local().Format("15:04:05"))
	var answer string
	select {
	case line, ok := <-lines:
		if !ok {
			fmt.Fprintln(out, "\nNo terminal input; the updater restores the backup at the deadline.")
			return
		}
		answer = line
	case <-time.After(time.Until(q.Deadline)):
		fmt.Fprintln(out, "\nNo answer in time: the updater restores the backup.")
		return
	case <-ctx.Done():
		return
	}
	restore := answer != "n" && answer != "no"
	if err := sendAnswer(ctx, baseURL, q.ID, restore); err != nil {
		fmt.Fprintf(out, "The answer was not taken: %v\n", err)
		return
	}
	if restore {
		fmt.Fprintln(out, "Restoring the backup and switching back...")
	} else {
		fmt.Fprintln(out, "Not restoring. Both versions are being stopped; the recovery steps follow.")
	}
}

// readLines delivers what is typed, line by line (as readAnswer reads a
// line: trimmed, lowercased), until in ends. A question waits on it with a
// deadline, which a plain read cannot have.
func readLines(in io.Reader) <-chan string {
	lines := make(chan string)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(in)
		for {
			line, ok := readAnswer(reader)
			if !ok {
				return
			}
			lines <- line
		}
	}()
	return lines
}

// printFollowedResult prints how the followed update ended and returns the
// exit code.
func printFollowedResult(job *jobs.Job, playbook *recovery.Playbook, out io.Writer) int {
	if job.State != jobs.JobStateFailed {
		fmt.Fprintf(out, "\nThe update finished: %s\n", job.Message)
		return 0
	}
	fmt.Fprintf(out, "\nThe update failed (%s): %s\n", job.FailureCode, job.Message)
	if playbook != nil {
		printPlaybook(out, playbook)
	}
	return 1
}

// fetchFollowedJob reads /upgrade/status.
func fetchFollowedJob(ctx context.Context, baseURL string) (*followedJob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/upgrade/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var job followedJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, err
	}
	return &job, nil
}

// sendAnswer posts an answer to /upgrade/answer with root's CLI token.
func sendAnswer(ctx context.Context, baseURL, questionID string, restore bool) error {
	body, _ := json.Marshal(map[string]any{"questionId": questionID, "restore": restore})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/upgrade/answer", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token, err := cli.ReadToken(cliStateDir()); err == nil {
		req.Header.Set(cli.TokenHeader, token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s", strings.TrimSpace(string(text)))
	}
	return nil
}
