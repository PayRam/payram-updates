package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
	"github.com/payram/payram-updater/internal/jobs"
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
		StateDir:   filepath.Join(tmpDir, "state"),
	}

	return NewManager(cfg, executor, &mockLogger{}), tmpDir
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

// ========== Create, list and prune ==========

func setExternalDBEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POSTGRES_HOST", "external-db.example.com")
	t.Setenv("POSTGRES_PORT", "5432")
	t.Setenv("POSTGRES_DATABASE", "testdb")
	t.Setenv("POSTGRES_USER", "testuser")
	t.Setenv("POSTGRES_PASSWORD", "testpass")
}

func TestCreateBackup_ExternalDBWritesVerifiedPrivateDump(t *testing.T) {
	setExternalDBEnv(t)
	mgr, _ := newTestManager(t, &mockExecutor{})
	runner := &fakeRunner{handle: postgresTools(nil)}
	mgr.Runner = runner

	info, err := mgr.CreateBackup(context.Background(), BackupMeta{FromVersion: "1.7.8", TargetVersion: "1.7.9", JobID: "job-123"})
	if err != nil {
		t.Fatalf("CreateBackup failed: %v", err)
	}
	if info.FromVersion != "1.7.8" || info.TargetVersion != "1.7.9" || info.JobID != "job-123" || info.Database != "testdb" {
		t.Errorf("unexpected info: %+v", info)
	}
	if !strings.HasPrefix(info.Filename, "payram-backup-") || !strings.HasSuffix(info.Filename, "-1.7.8-to-1.7.9.dump") {
		t.Errorf("filename = %s", info.Filename)
	}
	if info.Size == 0 {
		t.Error("expected non-zero size")
	}

	dumps := runner.callsOf("pg_dump")
	if len(dumps) != 1 {
		t.Fatalf("expected 1 pg_dump, got steps %v", runner.steps())
	}
	dump := dumps[0].Cmd
	if dump.Name != "pg_dump" {
		t.Errorf("expected host pg_dump, got %s", dump.Name)
	}
	for _, arg := range []string{"-Fc", "-h", "external-db.example.com", "-U", "testuser", "-d", "testdb"} {
		if !cmdHasArg(dump, arg) {
			t.Errorf("pg_dump missing %s: %v", arg, dump.Args)
		}
	}
	if !cmdHasEnv(dump, "PGPASSWORD=testpass") {
		t.Error("expected PGPASSWORD in env")
	}
	if len(runner.callsOf("pg_restore --list")) != 1 {
		t.Errorf("the new backup must be read back with pg_restore --list: %v", runner.steps())
	}
	runner.assertNoSecretInArgs(t, "testpass")

	st, err := os.Stat(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("backup mode = %o, want 0600", st.Mode().Perm())
	}
	dirInfo, _ := os.Stat(mgr.Config.Dir)
	if dirInfo.Mode().Perm() != 0700 {
		t.Errorf("backup dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestCreateBackup_PgDumpFails(t *testing.T) {
	setExternalDBEnv(t)
	mgr, _ := newTestManager(t, &mockExecutor{})
	mgr.Runner = &fakeRunner{handle: func(cmd dbexec.Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		return []byte("pg_dump: error: connection refused"), &mockError{msg: "exit status 1"}
	}}

	_, err := mgr.CreateBackup(context.Background(), BackupMeta{FromVersion: "1.0.0", TargetVersion: "1.1.0"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pg_dump on this host failed") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("unexpected error: %v", err)
	}
	backups, _ := mgr.ListBackups()
	if len(backups) != 0 {
		t.Errorf("a failed backup must leave no file, found %d", len(backups))
	}
}

func TestSecureBackupDir_TightensDirectoryAndFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "payram-backup-20260101-000000-1.0.0-to-1.1.0.sql")
	other := filepath.Join(dir, "notes.txt")
	os.WriteFile(old, []byte("data"), 0644)
	os.WriteFile(other, []byte("data"), 0644)

	if err := SecureBackupDir(dir, &mockLogger{}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0700 {
		t.Errorf("dir mode = %o, want 0700", info.Mode().Perm())
	}
	if info, _ := os.Stat(old); info.Mode().Perm() != 0600 {
		t.Errorf("legacy backup mode = %o, want 0600", info.Mode().Perm())
	}
	if info, _ := os.Stat(other); info.Mode().Perm() != 0644 {
		t.Errorf("unrelated file must be left alone, mode = %o", info.Mode().Perm())
	}
}

func TestListBackups_SkipsEmptyAndPartialFiles(t *testing.T) {
	mgr, tmpDir := newTestManager(t, &mockExecutor{})
	dir := filepath.Join(tmpDir, "backups")
	os.WriteFile(filepath.Join(dir, "payram-backup-20260101-000000-1.0.0-to-1.1.0.dump"), []byte("data"), 0600)
	os.WriteFile(filepath.Join(dir, "payram-backup-20260102-000000-1.1.0-to-1.2.0.dump"), nil, 0600)
	os.WriteFile(filepath.Join(dir, "payram-backup-20260103-000000-1.2.0-to-1.3.0.dump.partial"), []byte("half"), 0600)

	backups, err := mgr.ListBackups()
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || backups[0].Filename != "payram-backup-20260101-000000-1.0.0-to-1.1.0.dump" {
		t.Fatalf("expected only the complete backup, got %+v", backups)
	}
}

func writeFailedJob(t *testing.T, stateDir, backupPath string) {
	t.Helper()
	store := jobs.NewStore(stateDir)
	job := jobs.NewJob("job-1", jobs.JobModeManual, "1.2.0")
	job.State = jobs.JobStateFailed
	job.FailureCode = jobs.FailureUpgradeRolledBack
	job.BackupPath = backupPath
	if err := store.Save(job); err != nil {
		t.Fatal(err)
	}
}

func TestPruneBackups_NeverDeletesTheFailedUpdatesBackup(t *testing.T) {
	mgr, tmpDir := newTestManager(t, &mockExecutor{})
	dir := filepath.Join(tmpDir, "backups")
	var paths []string
	for i := 1; i <= 4; i++ {
		ext := ".dump"
		if i%2 == 0 {
			ext = ".sql"
		}
		p := filepath.Join(dir, fmt.Sprintf("payram-backup-2026010%d-100000-1.0.%d-to-1.0.%d%s", i, i, i+1, ext))
		os.WriteFile(p, []byte("data"), 0600)
		paths = append(paths, p)
	}
	writeFailedJob(t, mgr.Config.StateDir, paths[0]) // the oldest

	pruned, err := mgr.PruneBackups(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 2 {
		t.Fatalf("expected 2 pruned (.sql and .dump counted alike), got %d", len(pruned))
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("the failed update's backup was deleted: %v", err)
	}
	if _, err := os.Stat(paths[3]); err != nil {
		t.Fatalf("the newest backup was deleted: %v", err)
	}
}

func TestPruneBackups_KeepsProtectedPaths(t *testing.T) {
	mgr, tmpDir := newTestManager(t, &mockExecutor{})
	dir := filepath.Join(tmpDir, "backups")
	oldest := filepath.Join(dir, "payram-backup-20260101-100000-1.0.0-to-1.1.0.dump")
	os.WriteFile(oldest, []byte("data"), 0600)
	os.WriteFile(filepath.Join(dir, "payram-backup-20260102-100000-1.1.0-to-1.2.0.dump"), []byte("data"), 0600)

	if _, err := mgr.PruneBackups(1, oldest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest); err != nil {
		t.Fatalf("protected backup deleted: %v", err)
	}
}

func TestPruneBackups_RemovesStaleLeftoversWithoutCountingThem(t *testing.T) {
	mgr, tmpDir := newTestManager(t, &mockExecutor{})
	dir := filepath.Join(tmpDir, "backups")
	complete := filepath.Join(dir, "payram-backup-20260101-100000-1.0.0-to-1.1.0.dump")
	staleEmpty := filepath.Join(dir, "payram-backup-20260102-100000-1.1.0-to-1.2.0.dump")
	stalePartial := filepath.Join(dir, "payram-backup-20260103-100000-1.2.0-to-1.3.0.dump.partial")
	freshPartial := filepath.Join(dir, "payram-backup-20260104-100000-1.3.0-to-1.4.0.dump.partial")
	os.WriteFile(complete, []byte("data"), 0600)
	os.WriteFile(staleEmpty, nil, 0600)
	os.WriteFile(stalePartial, []byte("half"), 0600)
	os.WriteFile(freshPartial, []byte("being written"), 0600)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(staleEmpty, old, old)
	os.Chtimes(stalePartial, old, old)

	pruned, err := mgr.PruneBackups(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 0 {
		t.Fatalf("the only complete backup must be kept, pruned %+v", pruned)
	}
	for _, gone := range []string{staleEmpty, stalePartial} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", filepath.Base(gone))
		}
	}
	for _, kept := range []string{complete, freshPartial} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s should have been kept: %v", filepath.Base(kept), err)
		}
	}
}

func TestBackupListItem_HasUpgradeVersions(t *testing.T) {
	cases := map[string]bool{
		"payram-backup-20260101-100000-1.7.0-to-1.7.9.dump":        true,
		"payram-backup-20260101-100000-manual-to-manual.dump":      false,
		"payram-backup-20260101-100000-unknown-to-1.7.9.sql":       false,
		"payram-backup-20260101-100000-pre-restore-safety.dump":    false,
		"payram-backup-20260101-100000-1.7.0-rc1-to-1.7.9-rc.dump": true,
	}
	for name, want := range cases {
		meta := parseBackupFilename(name)
		item := BackupListItem{FromVersion: meta.FromVersion, ToVersion: meta.ToVersion}
		if got := item.HasUpgradeVersions(); got != want {
			t.Errorf("%s: HasUpgradeVersions = %v, want %v", name, got, want)
		}
	}
}

// ========== Restore ==========

const restoreContainer = "payram-test"

// newRestoreManager is a manager whose PayRam container runs the bundled
// database (credentials from docker inspect) with a fake runner for every
// docker exec.
func newRestoreManager(t *testing.T, override func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (bool, []byte, error)) (*Manager, *fakeRunner, string) {
	t.Helper()
	mgr, tmpDir := newTestManager(t, mockDockerInspectExecutor(nil))
	mgr.Config.TargetContainerName = restoreContainer
	runner := &fakeRunner{handle: postgresTools(override)}
	mgr.Runner = runner
	backupPath := filepath.Join(tmpDir, "backups", "payram-backup-20260101-100000-1.7.0-to-1.8.0.dump")
	if err := os.WriteFile(backupPath, []byte("PGDMP-backup"), 0600); err != nil {
		t.Fatal(err)
	}
	return mgr, runner, backupPath
}

func indexOf(steps []string, step string) int {
	for i, s := range steps {
		if s == step {
			return i
		}
	}
	return -1
}

func TestRestoreBackup_RequiresConfirmation(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, nil)
	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: false})
	if err == nil || !strings.Contains(err.Error(), "requires explicit confirmation") {
		t.Fatalf("expected confirmation error, got: %v", err)
	}
	if len(runner.steps()) != 0 {
		t.Error("nothing may run without confirmation")
	}
}

func TestRestoreBackup_StopsWritersTakesSafetyBackupRestoresAndStartsWriters(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, nil)

	result, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true})
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if !result.DBRestored || !result.NeedsRecovery || result.FromVersion != "1.7.0" || result.ToVersion != "1.8.0" {
		t.Errorf("unexpected result: %+v", result)
	}
	if result.Database.Tables != 7 || !result.Database.HasMigrations || result.Database.MigrationRows != 12 {
		t.Errorf("unexpected counts: %+v", result.Database)
	}

	steps := runner.steps()
	stop, dump, restore, start := indexOf(steps, "supervisorctl stop"), indexOf(steps, "pg_dump"), indexOf(steps, "psql restore"), indexOf(steps, "supervisorctl start")
	if stop < 0 || dump < 0 || restore < 0 || start < 0 || !(stop < dump && dump < restore && restore < start) {
		t.Fatalf("expected stop < safety dump < restore < start, got %v", steps)
	}
	if steps[len(steps)-1] != "supervisorctl start" {
		t.Errorf("programs must be started again last, got %v", steps)
	}

	stopped := runner.callsOf("supervisorctl stop")[0].Cmd.Args
	started := runner.callsOf("supervisorctl start")[0].Cmd.Args
	wantPrograms := "payram-core payram-eth-listener"
	if !strings.HasSuffix(strings.Join(stopped, " "), "stop "+wantPrograms) {
		t.Errorf("must stop only running non-postgres programs, got %v", stopped)
	}
	if !strings.HasSuffix(strings.Join(started, " "), "start "+wantPrograms) {
		t.Errorf("must start exactly the stopped programs, got %v", started)
	}

	if !strings.HasSuffix(result.SafetyBackupPath, "-pre-restore-safety.dump") {
		t.Errorf("safety backup name = %s", result.SafetyBackupPath)
	}
	if info, err := os.Stat(result.SafetyBackupPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("safety backup missing or not 0600: %v", err)
	}

	psql := runner.callsOf("psql restore")[0]
	if !cmdHasArg(psql.Cmd, restoreContainer) || !cmdHasArg(psql.Cmd, "ON_ERROR_STOP=1") {
		t.Errorf("psql restore args: %v", psql.Cmd.Args)
	}
	stream := string(psql.Stdin)
	if !strings.HasPrefix(stream, "BEGIN;") || !strings.Contains(stream, "DROP SCHEMA IF EXISTS public CASCADE;") ||
		!strings.HasSuffix(strings.TrimSpace(stream), "COMMIT;") {
		t.Errorf("restore stream:\n%s", stream)
	}
	runner.assertNoSecretInArgs(t, "testpass")
}

func TestRestoreBackup_FailedRestoreStartsWritersAgainAndNamesSafetyBackup(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (bool, []byte, error) {
		if step == "psql restore" {
			return true, []byte("psql:<stdin>:40: ERROR:  syntax error at or near \"garbage\"\n"), &mockError{msg: "exit status 3"}
		}
		return false, nil, nil
	})

	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"Restore failed and was rolled back: the database was not changed.", "syntax error", "pre-restore-safety.dump"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	var restoreErr *dbexec.RestoreError
	if !errors.As(err, &restoreErr) || !restoreErr.Unchanged {
		t.Errorf("error must stay a dbexec.RestoreError: %v", err)
	}
	if len(runner.callsOf("supervisorctl start")) != 1 {
		t.Fatalf("programs must be started again after a failed restore: %v", runner.steps())
	}
}

func TestRestoreBackup_SafetyBackupFailureAbortsBeforeRestoring(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (bool, []byte, error) {
		if step == "pg_dump" {
			return true, []byte("pg_dump: error: could not write: No space left on device"), &mockError{msg: "exit status 1"}
		}
		return false, nil, nil
	})

	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "safety backup") || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("expected safety-backup failure, got %v", err)
	}
	if len(runner.callsOf("psql restore")) != 0 {
		t.Fatal("the database must not be touched when the safety backup fails")
	}
	if len(runner.callsOf("supervisorctl start")) != 1 {
		t.Fatal("programs must be started again")
	}
}

func TestRestoreBackup_SkipSafetyBackup(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, nil)
	result, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true, SkipSafetyBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.SafetyBackupPath != "" || len(runner.callsOf("pg_dump")) != 0 {
		t.Fatalf("no safety backup expected, steps %v", runner.steps())
	}
}

func TestRestoreBackup_RefusedWhileAnUpdateHoldsTheLock(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, nil)
	release, err := jobs.AcquireContainerLock(mgr.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true})
	if !errors.Is(err, ErrRestoreLocked) {
		t.Fatalf("expected ErrRestoreLocked, got %v", err)
	}
	if len(runner.steps()) != 0 {
		t.Fatalf("nothing may run while locked: %v", runner.steps())
	}
}

func TestRestoreBackup_DamagedBackupRefusedBeforeStoppingAnything(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (bool, []byte, error) {
		if step == "pg_restore --list" {
			return true, []byte("pg_restore: error: input file is too short (read 12, expected 5)"), &mockError{msg: "exit status 1"}
		}
		return false, nil, nil
	})

	_, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "damaged or incomplete, so nothing was changed") {
		t.Fatalf("expected damaged-backup error, got %v", err)
	}
	for _, step := range runner.steps() {
		if step != "pg_restore --list" {
			t.Fatalf("only the file check may run, got %v", runner.steps())
		}
	}
}

func TestRestoreBackup_WithoutSupervisorctlWarnsAndContinues(t *testing.T) {
	mgr, runner, backupPath := newRestoreManager(t, func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (bool, []byte, error) {
		if strings.HasPrefix(step, "supervisorctl") {
			return true, []byte(`OCI runtime exec failed: exec: "supervisorctl": executable file not found in $PATH`), &mockError{msg: "exit status 126"}
		}
		return false, nil, nil
	})

	if _, err := mgr.RestoreBackup(context.Background(), backupPath, RestoreOptions{Confirmed: true}); err != nil {
		t.Fatalf("restore must go on without supervisorctl: %v", err)
	}
	if len(runner.callsOf("supervisorctl stop")) != 0 || len(runner.callsOf("supervisorctl start")) != 0 {
		t.Fatalf("nothing to stop or start without supervisorctl: %v", runner.steps())
	}
}

func TestRestoreBackup_ManualBackupNeedsNoContainerRollback(t *testing.T) {
	mgr, _, _ := newRestoreManager(t, nil)
	manual := filepath.Join(mgr.Config.Dir, "payram-backup-20260101-100000-manual-to-manual.dump")
	os.WriteFile(manual, []byte("PGDMP"), 0600)
	result, err := mgr.RestoreBackup(context.Background(), manual, RestoreOptions{Confirmed: true, SkipSafetyBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.NeedsRecovery {
		t.Error("a manual backup records no versions to roll back to")
	}
}

func TestRestoreBackup_UnknownFormat(t *testing.T) {
	mgr, runner, _ := newRestoreManager(t, nil)
	path := filepath.Join(mgr.Config.Dir, "backup.txt")
	os.WriteFile(path, []byte("x"), 0600)
	_, err := mgr.RestoreBackup(context.Background(), path, RestoreOptions{Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "INVALID_BACKUP_FORMAT") {
		t.Fatalf("expected INVALID_BACKUP_FORMAT, got %v", err)
	}
	if len(runner.steps()) != 0 {
		t.Fatal("nothing may run for an unknown format")
	}
}

func TestRestoreBackup_FileNotFound(t *testing.T) {
	mgr, _, _ := newRestoreManager(t, nil)
	_, err := mgr.RestoreBackup(context.Background(), "/nonexistent/backup.dump", RestoreOptions{Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected 'does not exist' error, got: %v", err)
	}
}

func TestRestoreBackup_EmptyFile(t *testing.T) {
	mgr, _, _ := newRestoreManager(t, nil)
	path := filepath.Join(mgr.Config.Dir, "payram-backup-20260101-100000-1.0.0-to-1.1.0.dump")
	os.WriteFile(path, nil, 0600)
	_, err := mgr.RestoreBackup(context.Background(), path, RestoreOptions{Confirmed: true})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected 'empty' error, got: %v", err)
	}
}

func TestWaitForDatabase_PollsPgIsReadyInsideContainer(t *testing.T) {
	mgr, runner, _ := newRestoreManager(t, nil)
	if err := mgr.WaitForDatabase(context.Background(), restoreContainer, time.Minute); err != nil {
		t.Fatal(err)
	}
	calls := runner.callsOf("pg_isready")
	if len(calls) != 1 || calls[0].Cmd.Name != "docker" || !cmdHasArg(calls[0].Cmd, restoreContainer) {
		t.Fatalf("expected docker exec pg_isready, got %v", runner.steps())
	}
}
