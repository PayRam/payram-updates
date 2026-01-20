package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Store handles persistence of jobs and logs.
type Store struct {
	stateDir string
}

// NewStore creates a new Store with the given state directory.
func NewStore(stateDir string) *Store {
	return &Store{
		stateDir: stateDir,
	}
}

// LoadLatest loads the latest job from disk.
// Returns nil if no job exists.
func (s *Store) LoadLatest() (*Job, error) {
	statusPath := s.statusPath()
	data, err := os.ReadFile(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read status file: %w", err)
	}

	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job: %w", err)
	}

	return &job, nil
}

// Save persists the job to disk atomically.
func (s *Store) Save(job *Job) error {
	if err := s.ensureJobDir(); err != nil {
		return err
	}

	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal job: %w", err)
	}

	statusPath := s.statusPath()
	if err := s.atomicWrite(statusPath, data); err != nil {
		return fmt.Errorf("failed to write status file: %w", err)
	}

	return nil
}

// AppendLog appends a log line to the job's log file.
func (s *Store) AppendLog(line string) error {
	if err := s.ensureJobDir(); err != nil {
		return err
	}

	logsPath := s.logsPath()
	f, err := os.OpenFile(logsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("failed to write log: %w", err)
	}

	return nil
}

// ReadLogs reads all logs from the job's log file.
// Returns empty string if no logs exist.
func (s *Store) ReadLogs() (string, error) {
	logsPath := s.logsPath()
	data, err := os.ReadFile(logsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read log file: %w", err)
	}

	return string(data), nil
}

// statusPath returns the path to the status.json file.
func (s *Store) statusPath() string {
	return filepath.Join(s.stateDir, "jobs", "latest", "status.json")
}

// logsPath returns the path to the logs.txt file.
func (s *Store) logsPath() string {
	return filepath.Join(s.stateDir, "jobs", "latest", "logs.txt")
}

// ensureJobDir creates the job directory if it doesn't exist.
func (s *Store) ensureJobDir() error {
	jobDir := filepath.Join(s.stateDir, "jobs", "latest")
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return fmt.Errorf("failed to create job directory: %w", err)
	}
	return nil
}

// atomicWrite writes data to a file atomically using a temporary file and rename.
func (s *Store) atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".status-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Clean up temp file on error
	defer func() {
		if tmpFile != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}
	tmpFile = nil // Prevent cleanup

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}
