package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/recovery"
)

// questionDaemon plays the daemon of an update that asks before restoring
// the backup, then ends the way the answer says.
type questionDaemon struct {
	mu      sync.Mutex
	answers []bool
	polls   int
}

func (d *questionDaemon) handler(t *testing.T) http.Handler {
	question := &jobs.Question{ID: "job-1-restore-1", Text: "PayRam 3.10.0 failed its checks after it had changed the database. Restore the backup taken at 10:15 and switch back to 3.1.3?", Deadline: time.Now().Add(time.Minute)}
	mux := http.NewServeMux()
	mux.HandleFunc("/upgrade/status", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.polls++
		job := followedJob{Job: jobs.Job{JobID: "job-1", State: jobs.JobStateVerifying, Message: "Checking 3.10.0"}}
		switch {
		case len(d.answers) == 1 && !d.answers[0]:
			job.State, job.FailureCode, job.Message = jobs.JobStateFailed, jobs.FailureUpgradeRestoreDeclined, "PayRam is stopped"
			playbook := recovery.RenderPlaybook(jobs.FailureUpgradeRestoreDeclined, recovery.PlaybookContext{})
			job.RecoveryPlaybook = &playbook
		case len(d.answers) == 1:
			job.State, job.FailureCode, job.Message = jobs.JobStateFailed, jobs.FailureUpgradeRolledBackRestored, "switched back to 3.1.3"
		case d.polls > 1:
			job.Message = "Waiting for an answer at the command line"
			job.Question = question
		}
		json.NewEncoder(w).Encode(job)
	})
	mux.HandleFunc("/upgrade/answer", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			QuestionID string `json:"questionId"`
			Restore    bool   `json:"restore"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.QuestionID != question.ID {
			t.Errorf("answered question %q", req.QuestionID)
		}
		d.mu.Lock()
		d.answers = append(d.answers, req.Restore)
		d.mu.Unlock()
		w.Write([]byte(`{"accepted":true}`))
	})
	return mux
}

func TestFollowUpgrade_AsksBeforeTheBackupIsRestored(t *testing.T) {
	cases := []struct {
		name        string
		typed       string
		wantAnswers []bool
		wantOut     []string
	}{
		{"enter means yes", "\n", []bool{true}, []string{"Restore the backup and switch back? [Y/n]", "Restoring the backup and switching back", "UPGRADE_ROLLED_BACK_RESTORED"}},
		{"no", "no\n", []bool{false}, []string{"Not restoring", "UPGRADE_RESTORE_DECLINED", "RECOVERY: Update Stopped, Restore Declined", "support@payram.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &questionDaemon{}
			srv := httptest.NewServer(daemon.handler(t))
			defer srv.Close()
			in, typed := io.Pipe()
			go func() {
				// The person reads the question, then answers.
				time.Sleep(50 * time.Millisecond)
				io.WriteString(typed, tc.typed)
			}()
			var out strings.Builder
			code := followUpgrade(context.Background(), srv.URL, "job-1", in, &out, 10*time.Millisecond)
			if code != 1 {
				t.Errorf("exit code %d, want 1 (the update failed)", code)
			}
			daemon.mu.Lock()
			answers := daemon.answers
			daemon.mu.Unlock()
			if len(answers) != len(tc.wantAnswers) || answers[0] != tc.wantAnswers[0] {
				t.Fatalf("answers = %v, want %v", answers, tc.wantAnswers)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output misses %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestFollowUpgrade_NoTerminalInputLeavesTheDefault(t *testing.T) {
	daemon := &questionDaemon{}
	srv := httptest.NewServer(daemon.handler(t))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out strings.Builder
	code := followUpgrade(ctx, srv.URL, "job-1", strings.NewReader(""), &out, 10*time.Millisecond)
	if code != 130 {
		t.Fatalf("exit code %d, want 130 (stopped following)", code)
	}
	if len(daemon.answers) != 0 {
		t.Fatalf("answered %v without anyone typing", daemon.answers)
	}
	if !strings.Contains(out.String(), "restores the backup at the deadline") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestFollowUpgrade_ReportsACompletedUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jobs.Job{JobID: "job-1", State: jobs.JobStateReady, Message: "Upgrade completed successfully"})
	}))
	defer srv.Close()
	var out strings.Builder
	if code := followUpgrade(context.Background(), srv.URL, "job-1", strings.NewReader(""), &out, time.Millisecond); code != 0 {
		t.Fatalf("exit code %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "The update finished: Upgrade completed successfully") {
		t.Fatalf("output: %s", out.String())
	}
}
