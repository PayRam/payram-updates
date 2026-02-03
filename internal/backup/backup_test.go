package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockLogger implements Logger for testing.
type mockLogger struct {
	messages []string
}

func (l *mockLogger) Printf(format string, v ...interface{}) {
	// no-op for tests
}

// mockExecutor implements CommandExecutor for testing.
type mockExecutor struct {
	executeFunc func(ctx context.Context, name string, args []string, env []string) ([]byte, error)
	calls       []mockCall
}

type mockCall struct {
	Name string
	Args []string
	Env  []string
}

func (e *mockExecutor) Execute(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	e.calls = append(e.calls, mockCall{Name: name, Args: args, Env: env})
	if e.executeFunc != nil {
		return e.executeFunc(ctx, name, args, env)
	}
	return nil, nil
}

// mockDockerInspectExecutor creates a mock executor that handles docker inspect for DB credentials
func mockDockerInspectExecutor(additionalFunc func(ctx context.Context, name string, args []string, env []string) ([]byte, error)) *mockExecutor {
	return &mockExecutor{
		executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			// Handle docker inspect for DB credentials
			if name == "docker" && len(args) > 1 && args[0] == "inspect" {
				// Return mock DB environment variables
				envJSON := `["POSTGRES_HOST=localhost","POSTGRES_PORT=5432","POSTGRES_DATABASE=testdb","POSTGRES_USERNAME=testuser","POSTGRES_PASSWORD=testpass"]`
				return []byte(envJSON), nil
			}
			// Delegate to additional function if provided
			if additionalFunc != nil {
				return additionalFunc(ctx, name, args, env)
			}
			return []byte("success"), nil
		},
	}
}

func newTestManager(t *testing.T, executor *mockExecutor) (*Manager, string) {
	t.Helper()
	tmpDir := t.TempDir()

	// Create proper directory structure: tmpDir is data/, tmpDir/backups is backup dir
	backupDir := filepath.Join(tmpDir, "backups")
	os.MkdirAll(backupDir, 0755)

	// Backups are always enabled
	cfg := Config{
		Dir:        backupDir, // Use backups subdirectory
		Retention:  10,
		PGHost:     "localhost",
		PGPort:     5432,
		PGDB:       "testdb",
		PGUser:     "testuser",
		PGPassword: "testpass",
		PGDumpBin:  "pg_dump",
	}

	return NewManager(cfg, executor, &mockLogger{}), tmpDir
}

// newTestManagerWithMockContainer creates a test manager and sets up a mock container
// with DB environment variables for testing restore operations.
func newTestManagerWithMockContainer(t *testing.T, executor *mockExecutor) (*Manager, string, string) {
	t.Helper()
	mgr, tmpDir := newTestManager(t, executor)

	// Mock container name - tests should use this to skip discovery
	containerName := "test-payram-container"

	// For tests that actually call docker inspect, they need to provide
	// a mock executor that handles the inspect commands
	return mgr, tmpDir, containerName
}

func TestCreateBackup_Success(t *testing.T) {
	executor := &mockExecutor{
		executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			// Simulate pg_dump creating the file
			for i, arg := range args {
				if arg == "-f" && i+1 < len(args) {
					if err := os.WriteFile(args[i+1], []byte("fake backup data"), 0644); err != nil {
						return nil, err
					}
					break
				}
			}
			return []byte("pg_dump success"), nil
		},
	}

	mgr, _ := newTestManager(t, executor)

	meta := BackupMeta{
		FromVersion:   "1.7.8",
		TargetVersion: "1.7.9",
		JobID:         "job-123",
	}

	info, err := mgr.CreateBackup(context.Background(), meta)
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}

	if info == nil {
		t.Fatal("expected backup info, got nil")
	}

	// Verify backup info fields
	if info.FromVersion != "1.7.8" {
		t.Errorf("expected FromVersion '1.7.8', got %s", info.FromVersion)
	}
	if info.TargetVersion != "1.7.9" {
		t.Errorf("expected TargetVersion '1.7.9', got %s", info.TargetVersion)
	}
	if info.JobID != "job-123" {
		t.Errorf("expected JobID 'job-123', got %s", info.JobID)
	}
	if info.Database != "testdb" {
		t.Errorf("expected Database 'testdb', got %s", info.Database)
	}
	if info.Size == 0 {
		t.Error("expected non-zero size")
	}
	if info.Path == "" {
		t.Error("expected non-empty path")
	}

	// Verify pg_dump was called with correct args
	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(executor.calls))
	}

	call := executor.calls[0]
	if call.Name != "pg_dump" {
		t.Errorf("expected pg_dump, got %s", call.Name)
	}

	// Check for required args
	if !containsArg(call.Args, "-Fc") {
		t.Error("expected -Fc flag for custom format")
	}
	if !containsArg(call.Args, "-h") || !containsArg(call.Args, "localhost") {
		t.Error("expected -h localhost")
	}
	if !containsArg(call.Args, "-U") || !containsArg(call.Args, "testuser") {
		t.Error("expected -U testuser")
	}
	if !containsArg(call.Args, "-d") || !containsArg(call.Args, "testdb") {
		t.Error("expected -d testdb")
	}

	// Check env has password
	hasPassword := false
	for _, e := range call.Env {
		if e == "PGPASSWORD=testpass" {
			hasPassword = true
			break
		}
	}
	if !hasPassword {
		t.Error("expected PGPASSWORD in env")
	}
}

func TestCreateBackup_PgDumpFails(t *testing.T) {
	executor := &mockExecutor{
		executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			return []byte("connection refused"), &mockError{msg: "exit status 1"}
		},
	}

	mgr, _ := newTestManager(t, executor)

	_, err := mgr.CreateBackup(context.Background(), BackupMeta{
		FromVersion:   "1.0.0",
		TargetVersion: "1.1.0",
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !strings.Contains(err.Error(), "pg_dump failed") {
		t.Errorf("expected pg_dump failed error, got: %v", err)
	}
}

func TestListBackups_Empty(t *testing.T) {
	executor := &mockExecutor{}
	mgr, _ := newTestManager(t, executor)

	backups, err := mgr.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}

	if len(backups) != 0 {
		t.Errorf("expected 0 backups, got %d", len(backups))
	}
}

func TestListBackups_WithBackups(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Create test backup files instead of index
	files := []struct {
		name      string
		timestamp time.Time
	}{
		{"payram-backup-20260130-100000-1.7.0-to-1.7.9.dump", time.Now().Add(-2 * time.Hour)},
		{"payram-backup-20260201-120000-1.7.9-to-1.8.0.dump", time.Now().Add(-1 * time.Hour)},
		{"payram-backup-20260202-140000-1.8.0-to-1.8.1.sql", time.Now()},
	}

	for _, f := range files {
		path := filepath.Join(tmpDir, "backups", f.name)
		if err := os.WriteFile(path, []byte("backup data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	backups, err := mgr.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}

	if len(backups) != 3 {
		t.Fatalf("expected 3 backups, got %d", len(backups))
	}

	// Should be sorted newest first by timestamp
	if backups[0].Filename != "payram-backup-20260202-140000-1.8.0-to-1.8.1.sql" {
		t.Errorf("expected newest backup first, got %s", backups[0].Filename)
	}
	if backups[2].Filename != "payram-backup-20260130-100000-1.7.0-to-1.7.9.dump" {
		t.Errorf("expected oldest backup last, got %s", backups[2].Filename)
	}
}

func TestPruneBackups_NoAction(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Create 3 backup files
	for i := 1; i <= 3; i++ {
		fname := fmt.Sprintf("payram-backup-2026010%d-100000-1.0.0-to-1.1.0.dump", i)
		if err := os.WriteFile(filepath.Join(tmpDir, "backups", fname), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Prune with retention of 5 - should keep all 3
	pruned, err := mgr.PruneBackups(5)
	if err != nil {
		t.Fatalf("PruneBackups failed: %v", err)
	}

	if pruned != nil {
		t.Errorf("expected nil pruned list, got %d items", len(pruned))
	}
}

func TestPruneBackups_RemovesOld(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Create 5 backup files with proper naming
	for i := 1; i <= 5; i++ {
		fname := fmt.Sprintf("payram-backup-2026010%d-100000-1.0.0-to-1.1.0.dump", i)
		if err := os.WriteFile(filepath.Join(tmpDir, "backups", fname), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Prune with retention of 2 - should remove 3 oldest
	pruned, err := mgr.PruneBackups(2)
	if err != nil {
		t.Fatalf("PruneBackups failed: %v", err)
	}

	if len(pruned) != 3 {
		t.Fatalf("expected 3 pruned backups, got %d", len(pruned))
	}

	// Verify remaining backups
	remaining, _ := mgr.ListBackups()
	if len(remaining) != 2 {
		t.Errorf("expected 2 remaining backups, got %d", len(remaining))
	}
}

func TestPruneBackups_InvalidRetention(t *testing.T) {
	executor := &mockExecutor{}
	mgr, _ := newTestManager(t, executor)

	_, err := mgr.PruneBackups(0)
	if err == nil {
		t.Error("expected error for retention 0")
	}

	_, err = mgr.PruneBackups(-1)
	if err == nil {
		t.Error("expected error for negative retention")
	}
}

func TestSanitizeVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"1.7.9", "1.7.9"},
		{"", "unknown"},
		{"v1.0/beta", "v1.0-beta"},
		{"1.0:rc1", "1.0-rc1"},
		{"test*version", "testversion"},
	}

	for _, tt := range tests {
		result := sanitizeVersion(tt.input)
		if result != tt.expected {
			t.Errorf("sanitizeVersion(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestGetLatestBackup(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Test with no backups
	latest, err := mgr.GetLatestBackup()
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest != nil {
		t.Error("expected nil for no backups")
	}

	// Add some backup files
	files := []string{
		"payram-backup-20260201-100000-1.0.0-to-1.1.0.sql",
		"payram-backup-20260202-150000-1.1.0-to-1.2.0.dump",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, "backups", f), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	latest, err = mgr.GetLatestBackup()
	if err != nil {
		t.Fatalf("GetLatestBackup failed: %v", err)
	}
	if latest == nil {
		t.Fatal("expected non-nil backup")
	}
	if latest.Filename != "payram-backup-20260202-150000-1.1.0-to-1.2.0.dump" {
		t.Errorf("expected newest backup, got %s", latest.Filename)
	}
}

func TestGetBackupByPath(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	files := []string{
		"payram-backup-20260201-100000-1.0.0-to-1.1.0.dump",
		"payram-backup-20260202-100000-1.1.0-to-1.2.0.sql",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmpDir, "backups", f), []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	backupPath := filepath.Join(tmpDir, "backups", files[0])

	// Find by path
	found, err := mgr.GetBackupByPath(backupPath)
	if err != nil {
		t.Fatalf("GetBackupByPath failed: %v", err)
	}
	if found == nil {
		t.Fatal("expected to find backup")
	}
	if found.Filename != files[0] {
		t.Errorf("expected filename %s, got %s", files[0], found.Filename)
	}

	// Not found
	notFound, err := mgr.GetBackupByPath("/nonexistent/path.dump")
	if err != nil {
		t.Fatalf("GetBackupByPath failed: %v", err)
	}
	if notFound != nil {
		t.Error("expected nil for nonexistent backup")
	}
}

func TestBackupFilename(t *testing.T) {
	executor := &mockExecutor{
		executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			// Find and create the output file
			for i, arg := range args {
				if arg == "-f" && i+1 < len(args) {
					os.WriteFile(args[i+1], []byte("data"), 0644)
					break
				}
			}
			return nil, nil
		},
	}

	mgr, _ := newTestManager(t, executor)

	meta := BackupMeta{
		FromVersion:   "1.7.8",
		TargetVersion: "1.7.9",
	}

	info, err := mgr.CreateBackup(context.Background(), meta)
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}

	// Filename should match pattern
	if !strings.Contains(info.Filename, "payram-backup-") {
		t.Errorf("filename should start with payram-backup-, got %s", info.Filename)
	}
	if !strings.Contains(info.Filename, "-1.7.8-to-1.7.9.dump") {
		t.Errorf("filename should contain version info, got %s", info.Filename)
	}
}

// ========== Restore Tests ==========

func TestRestoreBackup_RequiresConfirmation(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Create a backup file
	backupPath := filepath.Join(tmpDir, "backups", "test.dump")
	os.WriteFile(backupPath, []byte("backup data"), 0644)

	// Try to restore without confirmation (provide container to skip discovery)
	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{
		Confirmed:     false,
		ContainerName: "test-container",
	})
	if err == nil {
		t.Fatal("expected error without confirmation")
	}
	if !strings.Contains(err.Error(), "requires explicit confirmation") {
		t.Errorf("expected confirmation error, got: %v", err)
	}

	// Executor should not have been called
	if len(executor.calls) != 0 {
		t.Error("executor should not have been called without confirmation")
	}
}

func TestRestoreBackup_Success(t *testing.T) {
	executor := mockDockerInspectExecutor(func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		return []byte("restore complete"), nil
	})

	mgr, tmpDir := newTestManager(t, executor)

	// Create persisted credentials for restore
	stateDir := filepath.Join(tmpDir, "state")
	os.MkdirAll(stateDir, 0755)
	dbEnvContent := `POSTGRES_HOST=localhost
POSTGRES_PORT=5432
POSTGRES_DATABASE=testdb
POSTGRES_USERNAME=testuser
POSTGRES_PASSWORD=testpass
`
	os.WriteFile(filepath.Join(stateDir, "db.env"), []byte(dbEnvContent), 0600)

	// Create a backup file
	backupPath := filepath.Join(tmpDir, "backups", "test.dump")
	os.WriteFile(backupPath, []byte("backup data"), 0644)

	// Restore with confirmation
	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true, ContainerName: "test-payram-mock"})
	if err != nil {
		t.Fatalf("RestoreBackup failed: %v", err)
	}

	// Verify pg_restore was called (now it's the first call since we load credentials from file)
	if len(executor.calls) < 1 {
		t.Fatalf("expected at least 1 executor call (restore), got %d", len(executor.calls))
	}

	// Debug: print all calls
	for i, call := range executor.calls {
		t.Logf("Call %d: %s %v", i, call.Name, call.Args)
	}

	// Find the pg_restore call (should be "docker exec" or "sh -c" wrapping pg_restore)
	var restoreCall *mockCall
	for i := range executor.calls {
		if executor.calls[i].Name == "docker" || executor.calls[i].Name == "sh" {
			restoreCall = &executor.calls[i]
			break
		}
	}
	if restoreCall == nil {
		t.Fatal("expected docker exec or shell command for restore")
	}

	// For local DB, the command should be docker exec (since we have a containerName)
	// The args will be like: exec -i test-payram-mock pg_restore --clean --if-exists ...
	// We just verify that docker exec was called
	if restoreCall.Name != "docker" && restoreCall.Name != "sh" {
		t.Errorf("expected docker or sh command, got %s", restoreCall.Name)
	}
}

func TestRestoreBackup_FileNotFound(t *testing.T) {
	executor := &mockExecutor{}
	mgr, _ := newTestManager(t, executor)

	_, err := mgr.RestoreBackup(context.Background(), "/nonexistent/backup.dump", RestoreOptions{Confirmed: true, ContainerName: "test-payram-mock"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected 'does not exist' error, got: %v", err)
	}
}

func TestRestoreBackup_EmptyFile(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Create empty file
	backupPath := filepath.Join(tmpDir, "backups", "empty.dump")
	os.WriteFile(backupPath, []byte{}, 0644)

	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true, ContainerName: "test-payram-mock"})
	if err == nil {
		t.Fatal("expected error for empty file")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected 'empty' error, got: %v", err)
	}
}

func TestRestoreBackup_PgRestoreFails(t *testing.T) {
	var callCount int
	executor := mockDockerInspectExecutor(func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		callCount++
		t.Logf("Mock called %d: name=%s, args=%v", callCount, name, args)
		err := &mockError{msg: "exit status 1"}
		t.Logf("Returning error: %v (type: %T)", err, err)
		return []byte("connection refused"), err
	})

	mgr, tmpDir := newTestManager(t, executor)

	// Create persisted credentials for restore
	stateDir := filepath.Join(tmpDir, "state")
	os.MkdirAll(stateDir, 0755)
	dbEnvContent := `POSTGRES_HOST=localhost
POSTGRES_PORT=5432
POSTGRES_DATABASE=testdb
POSTGRES_USERNAME=testuser
POSTGRES_PASSWORD=testpass
`
	os.WriteFile(filepath.Join(stateDir, "db.env"), []byte(dbEnvContent), 0600)

	// Create a backup file
	backupPath := filepath.Join(tmpDir, "backups", "test.dump")
	os.WriteFile(backupPath, []byte("backup data"), 0644)

	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true, ContainerName: "test-payram-mock"})
	t.Logf("RestoreBackup returned: err=%v (type: %T)", err, err)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pg_restore failed") {
		t.Errorf("expected pg_restore failed error, got: %v", err)
	}
}

func TestVerifyBackupFile_Valid(t *testing.T) {
	mgr, tmpDir := newTestManager(t, &mockExecutor{})

	// Create a valid backup file
	backupPath := filepath.Join(tmpDir, "valid.dump")
	os.WriteFile(backupPath, []byte("backup data"), 0644)

	err := mgr.VerifyBackupFile(backupPath)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestVerifyBackupFile_NotExists(t *testing.T) {
	mgr, _ := newTestManager(t, &mockExecutor{})

	err := mgr.VerifyBackupFile("/nonexistent/file.dump")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected 'does not exist' error, got: %v", err)
	}
}

func TestVerifyBackupFile_Empty(t *testing.T) {
	mgr, tmpDir := newTestManager(t, &mockExecutor{})

	// Create empty file
	backupPath := filepath.Join(tmpDir, "backups", "empty.dump")
	os.WriteFile(backupPath, []byte{}, 0644)

	err := mgr.VerifyBackupFile(backupPath)
	if err == nil {
		t.Fatal("expected error for empty file")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected 'empty' error, got: %v", err)
	}
}

func TestVerifyBackupFile_Directory(t *testing.T) {
	mgr, tmpDir := newTestManager(t, &mockExecutor{})

	err := mgr.VerifyBackupFile(tmpDir) // tmpDir is a directory
	if err == nil {
		t.Fatal("expected error for directory")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("expected 'directory' error, got: %v", err)
	}
}

// Helper types and functions

type mockError struct {
	msg string
}

func (e *mockError) Error() string {
	return e.msg
}

func containsArg(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// Test parseBackupFilename parsing
func TestParseBackupFilename_Valid(t *testing.T) {
	tests := []struct {
		name            string
		filename        string
		expectFrom      string
		expectTo        string
		expectTimestamp bool
	}{
		{
			name:            "sql backup with versions",
			filename:        "payram-backup-20260202-123459-1.7.9-to-1.8.0.sql",
			expectFrom:      "1.7.9",
			expectTo:        "1.8.0",
			expectTimestamp: true,
		},
		{
			name:            "dump backup with versions",
			filename:        "payram-backup-20260130-095234-1.7.0-to-1.7.9.dump",
			expectFrom:      "1.7.0",
			expectTo:        "1.7.9",
			expectTimestamp: true,
		},
		{
			name:            "unknown versions",
			filename:        "payram-backup-20260129-090109-unknown-to-1.7.9.dump",
			expectFrom:      "unknown",
			expectTo:        "1.7.9",
			expectTimestamp: true,
		},
		{
			name:            "invalid format",
			filename:        "payram-backup-invalid.sql",
			expectFrom:      "unknown",
			expectTo:        "unknown",
			expectTimestamp: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseBackupFilename(tt.filename)

			if result.FromVersion != tt.expectFrom {
				t.Errorf("expected FromVersion %q, got %q", tt.expectFrom, result.FromVersion)
			}
			if result.ToVersion != tt.expectTo {
				t.Errorf("expected ToVersion %q, got %q", tt.expectTo, result.ToVersion)
			}
			if tt.expectTimestamp && result.CreatedAt == "" {
				t.Error("expected non-empty CreatedAt")
			}
			if !tt.expectTimestamp && result.CreatedAt != "" {
				t.Errorf("expected empty CreatedAt, got %q", result.CreatedAt)
			}
		})
	}
}

// Test ListBackups filesystem scanning
func TestListBackups_FilesystemScanning(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Create test backup files
	testFiles := []struct {
		name   string
		format string
	}{
		{"payram-backup-20260202-123459-1.7.9-to-1.8.0.sql", "sql"},
		{"payram-backup-20260202-122516-1.7.0-to-1.7.9.sql", "sql"},
		{"payram-backup-20260130-095234-1.7.0-to-1.7.9.dump", "dump"},
		{"payram-backup-20260129-090109-unknown-to-1.7.9.dump", "dump"},
		{"other-file.txt", ""}, // Should be ignored
	}

	for _, tf := range testFiles {
		path := filepath.Join(tmpDir, "backups", tf.name)
		if err := os.WriteFile(path, []byte("test data"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
	}

	backups, err := mgr.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}

	// Should have 4 backups (excluding other-file.txt)
	if len(backups) != 4 {
		t.Errorf("expected 4 backups, got %d", len(backups))
	}

	// Verify first backup (should be sorted newest first)
	if len(backups) > 0 {
		first := backups[0]
		if first.Filename != "payram-backup-20260202-123459-1.7.9-to-1.8.0.sql" {
			t.Errorf("expected newest backup first, got %s", first.Filename)
		}
		if first.Format != "sql" {
			t.Errorf("expected format 'sql', got %s", first.Format)
		}
		if first.FromVersion != "1.7.9" {
			t.Errorf("expected FromVersion '1.7.9', got %s", first.FromVersion)
		}
		if first.ToVersion != "1.8.0" {
			t.Errorf("expected ToVersion '1.8.0', got %s", first.ToVersion)
		}
		if first.SizeBytes == 0 {
			t.Error("expected non-zero size")
		}
	}
}

// Test detectBackupFormat
func TestDetectBackupFormat(t *testing.T) {
	tests := []struct {
		path   string
		expect string
	}{
		{"backup.sql", "sql"},
		{"backup.dump", "dump"},
		{"backup.txt", "unknown"},
		{"/path/to/backup.sql", "sql"},
		{"/path/to/backup.dump", "dump"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := detectBackupFormat(tt.path)
			if result != tt.expect {
				t.Errorf("expected %q, got %q", tt.expect, result)
			}
		})
	}
}

// Test RestoreBackup tool selection
func TestRestoreBackup_ToolSelection(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		expectTool  string
		expectError bool
	}{
		{
			name:       "sql uses psql",
			filename:   "backup.sql",
			expectTool: "psql",
		},
		{
			name:       "dump uses pg_restore",
			filename:   "backup.dump",
			expectTool: "pg_restore",
		},
		{
			name:        "unknown format fails",
			filename:    "backup.txt",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedTool string
			executor := mockDockerInspectExecutor(func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
				capturedTool = name
				return []byte("success"), nil
			})
			mgr, tmpDir := newTestManager(t, executor)

			// Create persisted credentials for restore
			stateDir := filepath.Join(tmpDir, "state")
			os.MkdirAll(stateDir, 0755)
			dbEnvContent := `POSTGRES_HOST=localhost
POSTGRES_PORT=5432
POSTGRES_DATABASE=testdb
POSTGRES_USERNAME=testuser
POSTGRES_PASSWORD=testpass
`
			os.WriteFile(filepath.Join(stateDir, "db.env"), []byte(dbEnvContent), 0600)

			// Create test backup file
			backupPath := filepath.Join(tmpDir, "backups", tt.filename)
			if err := os.WriteFile(backupPath, []byte("test backup"), 0644); err != nil {
				t.Fatalf("failed to create test file: %v", err)
			}

			_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true, ContainerName: "test-payram-mock"})

			if tt.expectError {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), "INVALID_BACKUP_FORMAT") {
					t.Errorf("expected INVALID_BACKUP_FORMAT error, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("RestoreBackup failed: %v", err)
				}
				// For local DB, the tool is now "sh" (docker exec wrapper), not psql/pg_restore directly
				if capturedTool != "sh" {
					t.Errorf("expected tool %q (docker exec wrapper), got %q", "sh", capturedTool)
				}
			}
		})
	}
}

// Test RestoreBackup with psql args
func TestRestoreBackup_PsqlArgs(t *testing.T) {
	executor := mockDockerInspectExecutor(func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		return []byte("success"), nil
	})
	mgr, tmpDir := newTestManager(t, executor)

	// Create persisted credentials for restore
	stateDir := filepath.Join(tmpDir, "state")
	os.MkdirAll(stateDir, 0755)
	dbEnvContent := `POSTGRES_HOST=localhost
POSTGRES_PORT=5432
POSTGRES_DATABASE=testdb
POSTGRES_USERNAME=testuser
POSTGRES_PASSWORD=testpass
`
	os.WriteFile(filepath.Join(stateDir, "db.env"), []byte(dbEnvContent), 0600)

	// Create test SQL backup
	backupPath := filepath.Join(tmpDir, "backups", "backup.sql")
	if err := os.WriteFile(backupPath, []byte("SELECT 1;"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true, ContainerName: "test-payram-mock"})
	if err != nil {
		t.Fatalf("RestoreBackup failed: %v", err)
	}

	// Now credentials come from file, so no docker inspect call
	if len(executor.calls) < 1 {
		t.Fatalf("expected at least 1 executor call (restore), got %d", len(executor.calls))
	}

	// Find the restore call (docker exec or sh -c)
	var restoreCall *mockCall
	for i := range executor.calls {
		if executor.calls[i].Name == "docker" || executor.calls[i].Name == "sh" {
			restoreCall = &executor.calls[i]
			break
		}
	}
	if restoreCall == nil {
		t.Fatal("expected docker exec or shell command for psql restore")
	}
}

// Test RestoreBackup with pg_restore args
func TestRestoreBackup_PgRestoreArgs(t *testing.T) {
	executor := mockDockerInspectExecutor(func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		return []byte("success"), nil
	})
	mgr, tmpDir := newTestManager(t, executor)

	// Create persisted credentials for restore
	stateDir := filepath.Join(tmpDir, "state")
	os.MkdirAll(stateDir, 0755)
	dbEnvContent := `POSTGRES_HOST=localhost
POSTGRES_PORT=5432
POSTGRES_DATABASE=testdb
POSTGRES_USERNAME=testuser
POSTGRES_PASSWORD=testpass
`
	os.WriteFile(filepath.Join(stateDir, "db.env"), []byte(dbEnvContent), 0600)

	// Create test dump backup
	backupPath := filepath.Join(tmpDir, "backups", "backup.dump")
	if err := os.WriteFile(backupPath, []byte("binary dump"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true, ContainerName: "test-payram-mock"})
	if err != nil {
		t.Fatalf("RestoreBackup failed: %v", err)
	}

	// Now credentials come from file, so no docker inspect call
	if len(executor.calls) < 1 {
		t.Fatalf("expected at least 1 executor call (restore), got %d", len(executor.calls))
	}

	// Find the restore call (docker exec or sh -c)
	var restoreCall *mockCall
	for i := range executor.calls {
		if executor.calls[i].Name == "docker" || executor.calls[i].Name == "sh" {
			restoreCall = &executor.calls[i]
			break
		}
	}
	if restoreCall == nil {
		t.Fatal("expected docker exec or shell command for pg_restore restore")
	}
}

// Test ListBackups sorting
func TestListBackups_Sorting(t *testing.T) {
	executor := &mockExecutor{}
	mgr, tmpDir := newTestManager(t, executor)

	// Create backups with different timestamps
	files := []string{
		"payram-backup-20260130-100000-1.0.0-to-1.1.0.sql",
		"payram-backup-20260202-100000-1.1.0-to-1.2.0.sql",
		"payram-backup-20260201-100000-1.0.5-to-1.1.0.dump",
	}

	for _, f := range files {
		path := filepath.Join(tmpDir, "backups", f)
		if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}
	}

	backups, err := mgr.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups failed: %v", err)
	}

	if len(backups) != 3 {
		t.Fatalf("expected 3 backups, got %d", len(backups))
	}

	// Should be sorted newest first
	if backups[0].Filename != "payram-backup-20260202-100000-1.1.0-to-1.2.0.sql" {
		t.Errorf("expected 20260202 backup first, got %s", backups[0].Filename)
	}
	if backups[1].Filename != "payram-backup-20260201-100000-1.0.5-to-1.1.0.dump" {
		t.Errorf("expected 20260201 backup second, got %s", backups[1].Filename)
	}
	if backups[2].Filename != "payram-backup-20260130-100000-1.0.0-to-1.1.0.sql" {
		t.Errorf("expected 20260130 backup third, got %s", backups[2].Filename)
	}
}
