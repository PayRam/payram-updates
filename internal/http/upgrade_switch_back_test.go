package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/cli"
	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/dbexec"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/switchover"
)

// switchDocker is Docker for the containers of one failed update. (The
// switchover package's own fake lives in its internal tests, which another
// package cannot import.)
type switchDocker struct {
	mu         sync.Mutex
	containers map[string]*dockerContainer
}

type dockerContainer struct {
	image   string
	status  string // created, running, exited
	restart string
}

func (d *switchDocker) RunWithEnv(context.Context, []string, []string) error {
	return errors.New("not used")
}
func (d *switchDocker) ConnectNetwork(context.Context, string, string) error { return nil }

func (d *switchDocker) Start(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return fmt.Errorf("No such container: %s", name)
	}
	c.status = "running"
	return nil
}

func (d *switchDocker) Stop(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.containers[name]; ok && c.status == "running" {
		c.status = "exited"
	}
	return nil
}

func (d *switchDocker) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.containers, name)
	return nil
}

func (d *switchDocker) Rename(_ context.Context, from, to string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[from]
	if !ok {
		return fmt.Errorf("No such container: %s", from)
	}
	if _, taken := d.containers[to]; taken {
		return fmt.Errorf("name %s in use", to)
	}
	delete(d.containers, from)
	d.containers[to] = c
	return nil
}

func (d *switchDocker) UpdateRestartPolicy(_ context.Context, name, policy string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.containers[name]; ok {
		c.restart = policy
	}
	return nil
}

func (d *switchDocker) State(_ context.Context, name string) (dockerexec.ContainerState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return dockerexec.ContainerState{}, nil
	}
	return dockerexec.ContainerState{Exists: true, Running: c.status == "running", Status: c.status, Image: c.image, HasStarted: c.status != "created"}, nil
}

func (d *switchDocker) get(name string) dockerContainer {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.containers[name]; ok {
		return *c
	}
	return dockerContainer{}
}

type switchPrograms struct{}

func (switchPrograms) Running(context.Context, string) ([]string, error) {
	return []string{"payram-core"}, nil
}
func (switchPrograms) Stop(context.Context, string, []string) error  { return nil }
func (switchPrograms) Start(context.Context, string, []string) error { return nil }

type switchBackDatabase struct {
	mu         sync.Mutex
	now        *jobs.DatabaseState
	readErr    error
	restoreErr error
	restores   int
}

func (f *switchBackDatabase) IsBundled(context.Context, string) (bool, error) { return true, nil }
func (f *switchBackDatabase) ReadState(context.Context, string) (*jobs.DatabaseState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now, f.readErr
}
func (f *switchBackDatabase) RestoreBackup(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores++
	if f.restoreErr != nil {
		return "", f.restoreErr
	}
	f.now = stateAtBackup()
	return "/backups/payram-backup-20261026-101500-pre-restore-safety.dump", nil
}

func stateAtBackup() *jobs.DatabaseState {
	return &jobs.DatabaseState{MigrationIDs: []string{"202605071600"}, SeedNames: []string{}, SchemaDigest: "s1", MoneyRecords: map[string]string{"withdrawals": "w1"}}
}

func stateMigrated(withdrawals string) *jobs.DatabaseState {
	return &jobs.DatabaseState{MigrationIDs: []string{"202605071600", "202606131200"}, SeedNames: []string{}, SchemaDigest: "s2", MoneyRecords: map[string]string{"withdrawals": withdrawals}}
}

// failedUpdateServer is a server whose update to 3.10.0 just failed its
// health check after the switch: 3.1.3 is set aside as payram-previous.
func failedUpdateServer(t *testing.T, db *switchBackDatabase, newStatus string) (*Server, *switchDocker, *jobs.Job) {
	t.Helper()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","db":"ok"}`))
	}))
	t.Cleanup(healthy.Close)

	docker := &switchDocker{containers: map[string]*dockerContainer{
		"payram-previous": {image: "payramapp/payram:3.1.3", status: "exited", restart: "no"},
		"payram":          {image: "payramapp/payram:3.10.0", status: newStatus, restart: "unless-stopped"},
	}}
	srv := &Server{
		config:       &config.Config{DockerBin: "docker"},
		jobStore:     jobs.NewStore(t.TempDir()),
		switchTiming: switchover.Timing{HealthTimeout: 2 * time.Second, StableFor: -1, PollInterval: 10 * time.Millisecond},
		questionWait: 2 * time.Second,
		switchWiring: func(sw *switchover.Switch) {
			sw.Docker = docker
			sw.Programs = switchPrograms{}
			sw.Database = db
		},
	}
	job := jobs.NewJob("job-1", jobs.JobModeDashboard, "3.10.0")
	job.ResolvedTarget = "3.10.0"
	job.FromVersion = "3.1.3"
	job.ContainerName = "payram"
	job.CoreBaseURL = healthy.URL
	job.PreviousImage = "payramapp/payram:3.1.3"
	job.PreviousRestartPolicy = "unless-stopped"
	job.SwitchStage = jobs.SwitchStageSwitched
	job.SwitchedAt = time.Now().Add(-10 * time.Minute)
	job.BackupPath = "/backups/payram-backup-20261026-101500-3.1.3-to-3.10.0.dump"
	job.BackupTakenAt = time.Date(2026, 10, 26, 10, 15, 0, 0, time.UTC)
	job.DatabaseAtBackup = stateAtBackup()
	job.DatabaseBeforeSwitch = stateAtBackup()
	if err := srv.jobStore.Save(job); err != nil {
		t.Fatal(err)
	}
	return srv, docker, job
}

func runFailedSwitchBack(srv *Server, job *jobs.Job) {
	srv.putPayramBack(context.Background(), srv.newSwitch(), job, "HEALTHCHECK_FAILED", "PayRam did not answer its health check within 10m0s", true)
}

func TestPutPayramBack_TellsWhatHappenedToTheDatabase(t *testing.T) {
	cases := []struct {
		name         string
		db           *switchBackDatabase
		newStatus    string
		wantCode     string
		wantRestores int
		wantRunning  string // image the container named payram runs at the end, "" when stopped
		wantInMsg    []string
		notInMsg     []string
	}{
		{
			name: "no migration: switched back, database kept", db: &switchBackDatabase{now: stateAtBackup()}, newStatus: "running",
			wantCode: jobs.FailureUpgradeRolledBack, wantRunning: "payramapp/payram:3.1.3",
			wantInMsg: []string{"switched back to 3.1.3", "did not change the database's structure", "every payment recorded during the update is kept"},
		},
		{
			name: "never started: switched back", db: &switchBackDatabase{now: stateMigrated("w1")}, newStatus: "created",
			wantCode: jobs.FailureUpgradeRolledBack, wantRunning: "payramapp/payram:3.1.3",
			wantInMsg: []string{"The new version never started"},
		},
		{
			name: "migrated, no money moved: backup restored, switched back", db: &switchBackDatabase{now: stateMigrated("w1")}, newStatus: "running",
			wantCode: jobs.FailureUpgradeRolledBackRestored, wantRestores: 1, wantRunning: "payramapp/payram:3.1.3",
			wantInMsg: []string{"restored the backup taken at 2026-10-26 10:15:00 UTC", "switched PayRam back to 3.1.3", "found again", "pre-restore-safety.dump"},
			notInMsg:  []string{"Nothing was removed"},
		},
		{
			name: "migrated and a payout sent: left on the new version", db: &switchBackDatabase{now: stateMigrated("w2")}, newStatus: "running",
			wantCode: jobs.FailureUpgradeMoneyMoved, wantRunning: "payramapp/payram:3.10.0",
			wantInMsg: []string{"requested, signed or sent a payout", "Do not restore the backup", "left on 3.10.0 (container payram, running)", "Contact PayRam support"},
		},
		{
			name: "restore fails: left on the new version", db: &switchBackDatabase{now: stateMigrated("w1"), restoreErr: &dbexec.RestoreError{Unchanged: true, Reason: "lock timeout"}}, newStatus: "running",
			wantCode: jobs.FailureUpgradeRestoreFailed, wantRestores: 1, wantRunning: "payramapp/payram:3.10.0",
			wantInMsg: []string{"the restore failed and nothing was changed", "left on 3.10.0"},
		},
		{
			name: "database unreadable: left on the new version", db: &switchBackDatabase{readErr: errors.New("psql: connection refused")}, newStatus: "running",
			wantCode: jobs.FailureUpgradeDatabaseUnchecked, wantRunning: "payramapp/payram:3.10.0",
			wantInMsg: []string{"could not check the database", "connection refused", "Do not restore the backup on your own"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, docker, job := failedUpdateServer(t, tc.db, tc.newStatus)
			runFailedSwitchBack(srv, job)

			saved, err := srv.jobStore.LoadLatest()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %s", saved.FailureCode, saved.Message)
			if saved.State != jobs.JobStateFailed || saved.FailureCode != tc.wantCode || saved.CauseCode != "HEALTHCHECK_FAILED" {
				t.Fatalf("job = %s %s/%s: %s", saved.State, saved.FailureCode, saved.CauseCode, saved.Message)
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(saved.Message, want) {
					t.Errorf("message misses %q:\n%s", want, saved.Message)
				}
			}
			for _, bad := range tc.notInMsg {
				if strings.Contains(saved.Message, bad) {
					t.Errorf("message says %q:\n%s", bad, saved.Message)
				}
			}
			if tc.db.restores != tc.wantRestores {
				t.Errorf("restores = %d, want %d", tc.db.restores, tc.wantRestores)
			}
			payram := docker.get("payram")
			if tc.wantRunning == "" {
				if payram.status == "running" {
					t.Errorf("payram runs %s, want it stopped", payram.image)
				}
			} else if payram.status != "running" || payram.image != tc.wantRunning {
				t.Errorf("payram = %+v, want %s running", payram, tc.wantRunning)
			}
			// Never two PayRam containers that can start by themselves.
			if prev := docker.get("payram-previous"); prev.image != "" && (prev.status == "running" || prev.restart != "no") {
				t.Errorf("payram-previous = %+v, want stopped with restart no", prev)
			}
		})
	}
}

func TestPutPayramBack_AsksTheAttendedPersonBeforeRestoring(t *testing.T) {
	t.Run("no: both versions stopped", func(t *testing.T) {
		db := &switchBackDatabase{now: stateMigrated("w1")}
		srv, docker, job := failedUpdateServer(t, db, "running")
		job.Attended = true
		done := answerWhenAsked(t, srv, false)
		runFailedSwitchBack(srv, job)
		<-done

		saved, _ := srv.jobStore.LoadLatest()
		if saved.FailureCode != jobs.FailureUpgradeRestoreDeclined || db.restores != 0 {
			t.Fatalf("job %s with %d restores: %s", saved.FailureCode, db.restores, saved.Message)
		}
		if saved.Question != nil {
			t.Fatal("the answered question is still in the job")
		}
		if c := docker.get("payram"); c.status == "running" || c.restart != "no" {
			t.Fatalf("payram = %+v, want the new version stopped", c)
		}
		if c := docker.get("payram-previous"); c.status == "running" {
			t.Fatal("the previous version must stay stopped after a no")
		}
	})
	t.Run("yes: restored and switched back", func(t *testing.T) {
		db := &switchBackDatabase{now: stateMigrated("w1")}
		srv, docker, job := failedUpdateServer(t, db, "running")
		job.Attended = true
		done := answerWhenAsked(t, srv, true)
		runFailedSwitchBack(srv, job)
		<-done

		saved, _ := srv.jobStore.LoadLatest()
		if saved.FailureCode != jobs.FailureUpgradeRolledBackRestored || db.restores != 1 {
			t.Fatalf("job %s with %d restores: %s", saved.FailureCode, db.restores, saved.Message)
		}
		if c := docker.get("payram"); c.status != "running" || c.image != "payramapp/payram:3.1.3" {
			t.Fatalf("payram = %+v", c)
		}
	})
}

// answerWhenAsked plays root's CLI: it waits for the job's question and
// answers it through POST /upgrade/answer.
func answerWhenAsked(t *testing.T, srv *Server, restore bool) <-chan struct{} {
	t.Helper()
	srv.cliToken = "root-token"
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			job, _ := srv.jobStore.LoadLatest()
			if job != nil && job.Question != nil {
				if !strings.Contains(job.Question.Text, "backup taken at 2026-10-26 10:15:00 UTC") || !strings.Contains(job.Question.Text, "found again") {
					t.Errorf("question does not say what a restore does: %s", job.Question.Text)
				}
				body, _ := json.Marshal(AnswerRequest{QuestionID: job.Question.ID, Restore: restore})
				req := httptest.NewRequest(http.MethodPost, "/upgrade/answer", strings.NewReader(string(body)))
				req.RemoteAddr = "127.0.0.1:50000"
				req.Header.Set(cli.TokenHeader, "root-token")
				w := httptest.NewRecorder()
				srv.HandleUpgradeAnswer()(w, req)
				if w.Code != http.StatusOK {
					t.Errorf("answer refused: %d %s", w.Code, w.Body.String())
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Error("the attended update never asked")
	}()
	return done
}

func TestAskToRestore_DefaultsToRestoringWhenNoOneCanAnswer(t *testing.T) {
	newServer := func(t *testing.T) (*Server, *jobs.Job) {
		srv := &Server{jobStore: jobs.NewStore(t.TempDir()), questionWait: 50 * time.Millisecond}
		job := jobs.NewJob("job-1", jobs.JobModeManual, "3.10.0")
		job.Attended = true
		return srv, job
	}
	t.Run("unattended update (dashboard, auto-update)", func(t *testing.T) {
		srv, job := newServer(t)
		job.Attended = false
		if !srv.askToRestore(job, "restore?") {
			t.Fatal("an unattended update must restore")
		}
		if saved, _ := srv.jobStore.LoadLatest(); saved != nil {
			t.Fatal("no question may be posted when nobody can answer it")
		}
	})
	t.Run("repair after an updater restart", func(t *testing.T) {
		srv, job := newServer(t)
		srv.repairing.Store(true)
		if !srv.askToRestore(job, "restore?") {
			t.Fatal("a repair must restore")
		}
	})
	t.Run("no answer before the deadline", func(t *testing.T) {
		srv, job := newServer(t)
		start := time.Now()
		if !srv.askToRestore(job, "restore?") {
			t.Fatal("no answer must mean restore (the default)")
		}
		if time.Since(start) > time.Second {
			t.Fatalf("waited %s, past the deadline", time.Since(start))
		}
		if job.Question != nil {
			t.Fatal("the question must be cleared")
		}
	})
	t.Run("updater stopping", func(t *testing.T) {
		srv, job := newServer(t)
		srv.questionWait = time.Minute
		srv.shuttingDown.Store(true)
		start := time.Now()
		if !srv.askToRestore(job, "restore?") || time.Since(start) > 5*time.Second {
			t.Fatal("a stopping updater must take the default at once")
		}
	})
}

func TestHandleUpgradeAnswer_OnlyRootsCLIAndOnlyAWaitingQuestion(t *testing.T) {
	srv := &Server{cliToken: "root-token"}
	post := func(remote, token, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/upgrade/answer", strings.NewReader(body))
		req.RemoteAddr = remote
		if token != "" {
			req.Header.Set(cli.TokenHeader, token)
		}
		w := httptest.NewRecorder()
		srv.HandleUpgradeAnswer()(w, req)
		return w.Code
	}
	body := `{"questionId":"q-1","restore":false}`
	if code := post("127.0.0.1:1", "", body); code != http.StatusForbidden {
		t.Errorf("without the token: %d, want 403", code)
	}
	if code := post("172.17.0.2:1", "root-token", body); code != http.StatusForbidden {
		t.Errorf("from the container: %d, want 403", code)
	}
	if code := post("127.0.0.1:1", "root-token", body); code != http.StatusConflict {
		t.Errorf("no question waiting: %d, want 409", code)
	}
	answers := srv.answers.open("q-1")
	defer srv.answers.close()
	if code := post("127.0.0.1:1", "root-token", `{"questionId":"q-2","restore":false}`); code != http.StatusConflict {
		t.Errorf("another question's ID: %d, want 409", code)
	}
	if code := post("127.0.0.1:1", "root-token", body); code != http.StatusOK {
		t.Fatalf("answer: %d, want 200", code)
	}
	if restore := <-answers; restore {
		t.Fatal("the answer no arrived as yes")
	}
}

// TestPutPayramBack_SaysWhenTheSwitchedBackContainerPublishesTheDatabase
// (audit L4): only the new container's settings drop port 5432.
func TestPutPayramBack_SaysWhenTheSwitchedBackContainerPublishesTheDatabase(t *testing.T) {
	srv, _, job := failedUpdateServer(t, &switchBackDatabase{now: stateAtBackup()}, "running")
	job.PreviousPublishesDatabase = true
	runFailedSwitchBack(srv, job)
	saved, _ := srv.jobStore.LoadLatest()
	if saved.FailureCode != jobs.FailureUpgradeRolledBack || !strings.Contains(saved.Message, "still publishes PayRam's database port (5432)") {
		t.Fatalf("%s: %s", saved.FailureCode, saved.Message)
	}
}
