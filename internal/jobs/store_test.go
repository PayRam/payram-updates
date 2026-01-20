package jobs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewStore(t *testing.T) {
	stateDir := "/tmp/test-state"
	store := NewStore(stateDir)

	if store == nil {
		t.Fatal("expected store to be created, got nil")
	}
	if store.stateDir != stateDir {
		t.Errorf("expected stateDir %q, got %q", stateDir, store.stateDir)
	}
}

func TestStore_SaveAndLoad(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	job := NewJob("test-job-123", JobModeDashboard, "v1.2.3")
	job.State = JobStateReady
	job.ResolvedTarget = "v1.2.3"
	job.Message = "Test job"

	if err := store.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	statusPath := filepath.Join(tmpDir, "jobs", "latest", "status.json")
	if _, err := os.Stat(statusPath); os.IsNotExist(err) {
		t.Error("status.json was not created")
	}

	loadedJob, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("failed to load job: %v", err)
	}

	if loadedJob == nil {
		t.Fatal("expected job to be loaded, got nil")
	}

	if loadedJob.JobID != job.JobID {
		t.Errorf("expected JobID %q, got %q", job.JobID, loadedJob.JobID)
	}
	if loadedJob.Mode != job.Mode {
		t.Errorf("expected Mode %q, got %q", job.Mode, loadedJob.Mode)
	}
	if loadedJob.RequestedTarget != job.RequestedTarget {
		t.Errorf("expected RequestedTarget %q, got %q", job.RequestedTarget, loadedJob.RequestedTarget)
	}
	if loadedJob.ResolvedTarget != job.ResolvedTarget {
		t.Errorf("expected ResolvedTarget %q, got %q", job.ResolvedTarget, loadedJob.ResolvedTarget)
	}
	if loadedJob.State != job.State {
		t.Errorf("expected State %q, got %q", job.State, loadedJob.State)
	}
	if loadedJob.Message != job.Message {
		t.Errorf("expected Message %q, got %q", job.Message, loadedJob.Message)
	}
}

func TestStore_LoadLatest_NoFile(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	job, err := store.LoadLatest()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if job != nil {
		t.Errorf("expected nil job, got %+v", job)
	}
}

func TestStore_AppendLog(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	lines := []string{
		"Starting job",
		"Fetching policy",
		"Policy fetched successfully",
	}

	for _, line := range lines {
		if err := store.AppendLog(line); err != nil {
			t.Fatalf("failed to append log: %v", err)
		}
	}

	logsPath := filepath.Join(tmpDir, "jobs", "latest", "logs.txt")
	if _, err := os.Stat(logsPath); os.IsNotExist(err) {
		t.Error("logs.txt was not created")
	}

	logs, err := store.ReadLogs()
	if err != nil {
		t.Fatalf("failed to read logs: %v", err)
	}

	for _, line := range lines {
		if !strings.Contains(logs, line) {
			t.Errorf("expected logs to contain %q", line)
		}
	}

	logLines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(logLines) != len(lines) {
		t.Errorf("expected %d log lines, got %d", len(lines), len(logLines))
	}
}

func TestStore_ReadLogs_NoFile(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	logs, err := store.ReadLogs()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if logs != "" {
		t.Errorf("expected empty logs, got %q", logs)
	}
}

func TestStore_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	job := NewJob("test-job", JobModeManual, "v1.0.0")

	for i := 0; i < 5; i++ {
		job.Message = string(rune(65 + i))
		if err := store.Save(job); err != nil {
			t.Fatalf("failed to save job (iteration %d): %v", i, err)
		}
	}

	loadedJob, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("failed to load job: %v", err)
	}

	if loadedJob.Message != "E" {
		t.Errorf("expected final message 'E', got %q", loadedJob.Message)
	}

	jobDir := filepath.Join(tmpDir, "jobs", "latest")
	entries, err := os.ReadDir(jobDir)
	if err != nil {
		t.Fatalf("failed to read job dir: %v", err)
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("found temp file: %s", entry.Name())
		}
	}
}

func TestStore_SaveInvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	statusPath := filepath.Join(tmpDir, "jobs", "latest", "status.json")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.WriteFile(statusPath, []byte("invalid json"), 0644); err != nil {
		t.Fatalf("failed to write invalid json: %v", err)
	}

	job, err := store.LoadLatest()
	if err == nil {
		t.Error("expected error loading invalid JSON, got nil")
	}
	if job != nil {
		t.Errorf("expected nil job, got %+v", job)
	}
}

func TestStore_JSONFormatting(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewStore(tmpDir)

	job := NewJob("test-job", JobModeDashboard, "v1.0.0")
	if err := store.Save(job); err != nil {
		t.Fatalf("failed to save job: %v", err)
	}

	statusPath := filepath.Join(tmpDir, "jobs", "latest", "status.json")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("failed to read status file: %v", err)
	}

	var parsedJob Job
	if err := json.Unmarshal(data, &parsedJob); err != nil {
		t.Errorf("status file contains invalid JSON: %v", err)
	}

	if !strings.Contains(string(data), "\n") {
		t.Error("expected formatted JSON with newlines")
	}
	if !strings.Contains(string(data), "  ") {
		t.Error("expected formatted JSON with indentation")
	}
}
