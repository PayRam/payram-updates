package dbexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mockExecutor implements container.CommandExecutor for testing.
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

type mockLogger struct {
	logs []string
}

func (l *mockLogger) Printf(format string, v ...interface{}) {
	// Don't store logs in tests, just discard
}

// TestDiscoverDBContext_FromEnv tests discovery from environment variables.
func TestDiscoverDBContext_FromEnv(t *testing.T) {
	// Set environment variables
	os.Setenv("POSTGRES_HOST", "rds.amazonaws.com")
	os.Setenv("POSTGRES_PORT", "5432")
	os.Setenv("POSTGRES_DATABASE", "mydb")
	os.Setenv("POSTGRES_USER", "admin")
	os.Setenv("POSTGRES_PASSWORD", "password123")
	defer func() {
		os.Unsetenv("POSTGRES_HOST")
		os.Unsetenv("POSTGRES_PORT")
		os.Unsetenv("POSTGRES_DATABASE")
		os.Unsetenv("POSTGRES_USER")
		os.Unsetenv("POSTGRES_PASSWORD")
	}()

	executor := &mockExecutor{}

	dbCtx, err := DiscoverDBContext(context.Background(), executor, DiscoverOpts{
		Logger: &mockLogger{},
	})

	if err != nil {
		t.Fatalf("DiscoverDBContext failed: %v", err)
	}

	if dbCtx.Mode != DBModeExternal {
		t.Errorf("expected mode %s, got %s", DBModeExternal, dbCtx.Mode)
	}

	if dbCtx.CredSource != CredFromEnv {
		t.Errorf("expected cred source %s, got %s", CredFromEnv, dbCtx.CredSource)
	}

	if dbCtx.Creds.Host != "rds.amazonaws.com" {
		t.Errorf("expected host 'rds.amazonaws.com', got '%s'", dbCtx.Creds.Host)
	}
}

// TestIsLocalDB tests the IsLocalDB helper.
func TestIsLocalDB(t *testing.T) {
	tests := []struct {
		host     string
		expected bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"db.example.com", false},
		{"192.168.1.1", false},
		{"", false},
	}

	for _, tt := range tests {
		result := IsLocalDB(tt.host)
		if result != tt.expected {
			t.Errorf("IsLocalDB(%q) = %v, expected %v", tt.host, result, tt.expected)
		}
	}
}

// TestDockerPGExecutor_Dump tests backup using DockerPGExecutor.
func TestDockerPGExecutor_Dump(t *testing.T) {
	tmpDir := t.TempDir()
	backupFile := filepath.Join(tmpDir, "test.dump")

	executor := &mockExecutor{
		executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			// Create the backup file
			os.WriteFile(backupFile, []byte("backup data"), 0644)
			return []byte("success"), nil
		},
	}

	pgExec := NewDockerPGExecutor(executor, &mockLogger{})

	dbCtx := DBContext{
		Mode:          DBModeInContainer,
		ContainerName: "payram-core",
		Creds: DBCreds{
			Host:     "localhost",
			Port:     "5432",
			Database: "payramdb",
			Username: "payram",
			Password: "secret",
		},
	}

	err := pgExec.Dump(context.Background(), dbCtx, backupFile, "dump")
	if err != nil {
		t.Fatalf("Dump failed: %v", err)
	}

	// Verify the command was called
	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(executor.calls))
	}

	call := executor.calls[0]
	if call.Name != "sh" {
		t.Errorf("expected 'sh' command, got '%s'", call.Name)
	}

	// Check that docker exec was used
	if len(call.Args) < 2 || call.Args[0] != "-c" {
		t.Errorf("expected sh -c command, got %v", call.Args)
	}

	cmd := call.Args[1]
	if !strings.Contains(cmd, "docker exec") {
		t.Errorf("expected docker exec in command, got: %s", cmd)
	}
	if !strings.Contains(cmd, "payram-core") {
		t.Errorf("expected container name in command, got: %s", cmd)
	}
	if !strings.Contains(cmd, "pg_dump") {
		t.Errorf("expected pg_dump in command, got: %s", cmd)
	}
}

// TestHostPGExecutor_Dump tests backup using HostPGExecutor.
func TestHostPGExecutor_Dump(t *testing.T) {
	tmpDir := t.TempDir()
	backupFile := filepath.Join(tmpDir, "test.dump")

	executor := &mockExecutor{
		executeFunc: func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
			// Create the backup file
			os.WriteFile(backupFile, []byte("backup data"), 0644)
			return []byte("success"), nil
		},
	}

	pgExec := NewHostPGExecutor(executor, &mockLogger{})

	dbCtx := DBContext{
		Mode: DBModeExternal,
		Creds: DBCreds{
			Host:     "db.example.com",
			Port:     "5432",
			Database: "payramdb",
			Username: "payram",
			Password: "secret",
		},
	}

	err := pgExec.Dump(context.Background(), dbCtx, backupFile, "dump")
	if err != nil {
		t.Fatalf("Dump failed: %v", err)
	}

	// Verify the command was called
	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(executor.calls))
	}

	call := executor.calls[0]
	if call.Name != "pg_dump" {
		t.Errorf("expected 'pg_dump' command, got '%s'", call.Name)
	}

	// Check that host and credentials were passed
	argsStr := strings.Join(call.Args, " ")
	if !strings.Contains(argsStr, "db.example.com") {
		t.Errorf("expected host in args, got: %v", call.Args)
	}
	if !strings.Contains(argsStr, "payram") {
		t.Errorf("expected username in args, got: %v", call.Args)
	}

	// Check that PGPASSWORD was set
	foundPassword := false
	for _, envVar := range call.Env {
		if strings.HasPrefix(envVar, "PGPASSWORD=") {
			foundPassword = true
			break
		}
	}
	if !foundPassword {
		t.Error("expected PGPASSWORD in environment")
	}
}
