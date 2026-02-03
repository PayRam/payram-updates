package dbexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/payram/payram-updater/internal/container"
)

// DiscoverOpts contains options for database context discovery.
type DiscoverOpts struct {
	// BackupDir is the directory where backups are stored (used for finding persisted credentials).
	BackupDir string
	// ImagePattern is the Docker image pattern to search for (e.g., "payramapp/payram:").
	ImagePattern string
	// Logger is used for logging discovery steps.
	Logger Logger
}

// Logger interface for logging messages.
type Logger interface {
	Printf(format string, v ...interface{})
}

// DiscoverDBContext discovers database connection information and execution mode.
// It follows this precedence:
// 1. Check for remote database via POSTGRES_HOST environment variable
// 2. Try to discover running Payram container and extract credentials
// 3. Fall back to persisted credentials from backup directory
// 4. Return error if no credentials found
func DiscoverDBContext(ctx context.Context, executor CommandExecutor, opts DiscoverOpts) (DBContext, error) {
	if opts.Logger == nil {
		opts.Logger = &noopLogger{}
	}

	// STEP 1: Check for remote database via environment variables
	envHost := os.Getenv("POSTGRES_HOST")
	if envHost != "" && !isLocalDB(envHost) {
		opts.Logger.Printf("Remote database detected via environment: %s", envHost)
		dbCtx := DBContext{
			Mode:       DBModeExternal,
			CredSource: CredFromEnv,
			Creds: DBCreds{
				Host:     envHost,
				Port:     getEnvOrDefault("POSTGRES_PORT", "5432"),
				Database: getEnvOrDefault("POSTGRES_DATABASE", getEnvOrDefault("POSTGRES_DB", "")),
				Username: getEnvOrDefault("POSTGRES_USERNAME", getEnvOrDefault("POSTGRES_USER", "")),
				Password: os.Getenv("POSTGRES_PASSWORD"),
				SSLMode:  getEnvOrDefault("POSTGRES_SSLMODE", "disable"),
			},
		}
		if err := dbCtx.Creds.Validate(); err != nil {
			return DBContext{}, &DBError{
				Code:    "INVALID_DB_CONFIG",
				Message: "remote database requires valid credentials in environment variables",
				Err:     err,
			}
		}
		return dbCtx, nil
	}

	// STEP 2: Try to discover running container
	imagePattern := opts.ImagePattern
	if imagePattern == "" {
		imagePattern = "payramapp/payram:"
	}

	discoverer := container.NewDiscoverer("docker", imagePattern, opts.Logger)
	discovered, err := discoverer.DiscoverPayramContainer(ctx)

	if err == nil {
		// Container is running - extract credentials
		opts.Logger.Printf("Using credentials from running container: %s (version: %s)", discovered.Name, discovered.ImageTag)

		// Extract database credentials from container environment
		dbConfig, err := getContainerDBConfig(ctx, executor, discovered.Name)
		if err != nil {
			return DBContext{}, &DBError{
				Code:    "INVALID_DB_CONFIG",
				Message: fmt.Sprintf("failed to extract credentials from running container %s", discovered.Name),
				Err:     err,
			}
		}

		// Determine if DB is in-container or external
		mode := DBModeExternal
		containerName := ""
		if isLocalDB(dbConfig.Host) {
			mode = DBModeInContainer
			containerName = discovered.Name
			opts.Logger.Printf("Database is running inside container: %s", containerName)
		} else {
			opts.Logger.Printf("Database is external: %s", dbConfig.Host)
		}

		return DBContext{
			Mode:          mode,
			CredSource:    CredFromRunningContainer,
			ContainerName: containerName,
			Creds: DBCreds{
				Host:     dbConfig.Host,
				Port:     dbConfig.Port,
				Database: dbConfig.Database,
				Username: dbConfig.Username,
				Password: dbConfig.Password,
				SSLMode:  dbConfig.SSLMode,
			},
		}, nil
	}

	// STEP 3: Container not running - try persisted credentials
	if opts.BackupDir == "" {
		return DBContext{}, &DBError{
			Code:    "CONTAINER_NOT_FOUND",
			Message: "no running container found and no backup directory specified for persisted credentials",
			Err:     err,
		}
	}

	opts.Logger.Printf("No running Payram container found, attempting to load persisted credentials...")
	dbConfig, err := loadPersistedCredentials(opts.BackupDir)
	if err != nil {
		return DBContext{}, &DBError{
			Code: "INVALID_DB_CONFIG",
			Message: fmt.Sprintf("no running container and no persisted credentials found.\n\n"+
				"Recovery options:\n"+
				"1. Start the Payram container and retry\n"+
				"2. Ensure %s/../state/db.env exists with valid credentials\n"+
				"3. For remote databases, set POSTGRES_* environment variables", opts.BackupDir),
			Err: err,
		}
	}

	opts.Logger.Printf("Using stored credentials from persisted file")

	// Determine if DB is in-container or external
	mode := DBModeExternal
	if isLocalDB(dbConfig.Host) {
		mode = DBModeInContainer
		// Note: ContainerName is empty here - caller must provide it if they want to use docker exec
	}

	return DBContext{
		Mode:       mode,
		CredSource: CredFromPersistedFile,
		Creds: DBCreds{
			Host:     dbConfig.Host,
			Port:     dbConfig.Port,
			Database: dbConfig.Database,
			Username: dbConfig.Username,
			Password: dbConfig.Password,
			SSLMode:  dbConfig.SSLMode,
		},
	}, nil
}

// isLocalDB returns true if the host indicates a local database.
func isLocalDB(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// getEnvOrDefault returns the value of an environment variable or a default value.
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

type noopLogger struct{}

func (l *noopLogger) Printf(format string, v ...interface{}) {}

// IsLocalDB is a public helper that checks if a host is local.
func IsLocalDB(host string) bool {
	return isLocalDB(host)
}

// inferContainerNameForRestore attempts to get a container name for restore operations
// when we have persisted credentials indicating in-container DB but no running container.
func inferContainerNameForRestore(opts DiscoverOpts) string {
	// Check if user provided TARGET_CONTAINER env
	if name := os.Getenv("TARGET_CONTAINER"); name != "" {
		return strings.TrimSpace(name)
	}
	// No container available - caller should handle this
	return ""
}

// loadPersistedCredentials loads database credentials from backup directory's db.env file.
// Returns error if file doesn't exist or cannot be read.
func loadPersistedCredentials(backupDir string) (*containerDBConfig, error) {
	dbEnvPath := filepath.Join(backupDir, "../state/db.env")

	// Check file exists
	if _, err := os.Stat(dbEnvPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("no persisted credentials found at %s", dbEnvPath)
	}

	// Read file
	content, err := os.ReadFile(dbEnvPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read db.env: %w", err)
	}

	// Parse env vars
	envMap := make(map[string]string)
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	config := &containerDBConfig{
		Host:     envMap["POSTGRES_HOST"],
		Port:     envMap["POSTGRES_PORT"],
		Database: envMap["POSTGRES_DATABASE"],
		Username: envMap["POSTGRES_USERNAME"],
		Password: envMap["POSTGRES_PASSWORD"],
		SSLMode:  envMap["POSTGRES_SSLMODE"],
	}

	// Validate required fields
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid persisted credentials: %w", err)
	}

	return config, nil
}

// containerDBConfig is a local type to avoid importing backup package
type containerDBConfig struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
	SSLMode  string
}

func (c *containerDBConfig) Validate() error {
	if c.Host == "" {
		return fmt.Errorf("missing POSTGRES_HOST")
	}
	if c.Port == "" {
		return fmt.Errorf("missing POSTGRES_PORT")
	}
	if c.Database == "" {
		return fmt.Errorf("missing POSTGRES_DATABASE")
	}
	if c.Username == "" {
		return fmt.Errorf("missing POSTGRES_USERNAME")
	}
	return nil
}

// getContainerDBConfig extracts database configuration from a running container's environment.
func getContainerDBConfig(ctx context.Context, executor CommandExecutor, containerName string) (*containerDBConfig, error) {
	// Get container environment variables using docker inspect
	output, err := executor.Execute(ctx, "docker", []string{
		"inspect",
		"--format={{json .Config.Env}}",
		containerName,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container %s: %w: %s", containerName, err, string(output))
	}

	// Parse JSON array of env vars
	var envVars []string
	if err := json.Unmarshal(output, &envVars); err != nil {
		return nil, fmt.Errorf("failed to parse container environment: %w", err)
	}

	// Build env map
	envMap := make(map[string]string)
	for _, envVar := range envVars {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Support both naming conventions for database name
	database := envMap["POSTGRES_DB"]
	if database == "" {
		database = envMap["POSTGRES_DATABASE"]
	}

	// Support both naming conventions for username
	username := envMap["POSTGRES_USER"]
	if username == "" {
		username = envMap["POSTGRES_USERNAME"]
	}

	config := &containerDBConfig{
		Host:     envMap["POSTGRES_HOST"],
		Port:     envMap["POSTGRES_PORT"],
		Database: database,
		Username: username,
		Password: envMap["POSTGRES_PASSWORD"],
		SSLMode:  envMap["POSTGRES_SSLMODE"],
	}

	// Validate required fields
	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}
