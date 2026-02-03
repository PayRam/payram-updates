// Package backup provides database backup and restore functionality.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/container"
)

// BackupInfo contains metadata about a backup.
type BackupInfo struct {
	ID            string    `json:"id"`
	Path          string    `json:"path"`
	Filename      string    `json:"filename"`
	Size          int64     `json:"size"`
	Checksum      string    `json:"checksum,omitempty"` // SHA256, optional
	CreatedAt     time.Time `json:"created_at"`
	FromVersion   string    `json:"from_version,omitempty"`
	TargetVersion string    `json:"target_version,omitempty"`
	JobID         string    `json:"job_id,omitempty"`
	Database      string    `json:"database"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
}

// BackupListItem contains metadata for a backup file discovered from filesystem.
type BackupListItem struct {
	File        string `json:"file"`         // Full path
	Filename    string `json:"filename"`     // Basename
	Format      string `json:"format"`       // "sql" or "dump"
	FromVersion string `json:"from_version"` // Parsed or "unknown"
	ToVersion   string `json:"to_version"`   // Parsed or "unknown"
	CreatedAt   string `json:"created_at"`   // RFC3339 if parseable, else empty
	SizeBytes   int64  `json:"size_bytes"`
}

// BackupMeta contains metadata to pass when creating a backup.
type BackupMeta struct {
	FromVersion   string
	TargetVersion string
	JobID         string
}

// BackupIndex stores the list of all backups.
type BackupIndex struct {
	Backups   []BackupInfo `json:"backups"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// CommandExecutor defines the interface for executing system commands.
// This allows mocking in tests.
type CommandExecutor interface {
	Execute(ctx context.Context, name string, args []string, env []string) ([]byte, error)
}

// Logger defines the interface for logging.
type Logger interface {
	Printf(format string, v ...interface{})
}

// Config holds backup configuration.
// Backups are always enabled.
type Config struct {
	Dir          string
	Retention    int
	PGHost       string
	PGPort       int
	PGDB         string
	PGUser       string
	PGPassword   string
	PGDumpBin    string // Path to pg_dump binary, default "pg_dump"
	ImagePattern string // Image pattern for container discovery, default "payramapp/payram:"
}

// Manager handles backup operations.
type Manager struct {
	Config   Config
	Executor CommandExecutor
	Logger   Logger
}

// NewManager creates a new backup manager.
func NewManager(cfg Config, executor CommandExecutor, logger Logger) *Manager {
	if cfg.PGDumpBin == "" {
		cfg.PGDumpBin = "pg_dump"
	}
	return &Manager{
		Config:   cfg,
		Executor: executor,
		Logger:   logger,
	}
}

// CreateBackup creates a new database backup using pg_dump.
// Returns BackupInfo with metadata, or an error.
// Backups are always enabled.
func (m *Manager) CreateBackup(ctx context.Context, meta BackupMeta) (*BackupInfo, error) {
	// Ensure backup directory exists
	if err := os.MkdirAll(m.Config.Dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Generate filename: payram-backup-<timestamp>-<fromVersion>-to-<toVersion>.dump
	timestamp := time.Now().UTC().Format("20060102-150405")
	fromVer := sanitizeVersion(meta.FromVersion)
	toVer := sanitizeVersion(meta.TargetVersion)

	filename := fmt.Sprintf("payram-backup-%s-%s-to-%s.dump", timestamp, fromVer, toVer)
	backupPath := filepath.Join(m.Config.Dir, filename)

	m.Logger.Printf("Creating backup: %s", backupPath)

	// Build pg_dump command
	args := []string{
		"-Fc", // Custom format
		"-h", m.Config.PGHost,
		"-p", fmt.Sprintf("%d", m.Config.PGPort),
		"-U", m.Config.PGUser,
		"-d", m.Config.PGDB,
		"-f", backupPath,
	}

	// Set environment for password
	env := os.Environ()
	if m.Config.PGPassword != "" {
		env = append(env, fmt.Sprintf("PGPASSWORD=%s", m.Config.PGPassword))
	}

	// Execute pg_dump
	output, err := m.Executor.Execute(ctx, m.Config.PGDumpBin, args, env)
	if err != nil {
		return nil, fmt.Errorf("pg_dump failed: %w: %s", err, string(output))
	}

	m.Logger.Printf("Backup created successfully: %s", backupPath)

	// Persist credentials if this is a local database
	// Only persist after successful backup, and only for localhost/127.0.0.1
	if IsLocalDB(m.Config.PGHost) {
		dbConfig := &ContainerDBConfig{
			Host:     m.Config.PGHost,
			Port:     fmt.Sprintf("%d", m.Config.PGPort),
			Database: m.Config.PGDB,
			Username: m.Config.PGUser,
			Password: m.Config.PGPassword,
			SSLMode:  "", // Not available in legacy config
		}
		if err := PersistDBCredentials(m.Config.Dir, dbConfig); err != nil {
			m.Logger.Printf("Warning: failed to persist DB credentials: %v", err)
			// Don't fail the backup if credential persistence fails
		} else {
			m.Logger.Printf("Persisted local DB credentials to data/state/db.env")
		}
	}

	// Get file info
	fileInfo, err := os.Stat(backupPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat backup file: %w", err)
	}

	// Calculate checksum
	checksum, err := calculateChecksum(backupPath)
	if err != nil {
		m.Logger.Printf("Warning: failed to calculate checksum: %v", err)
		checksum = ""
	}

	// Create backup info
	info := &BackupInfo{
		ID:            fmt.Sprintf("%s-%s", timestamp, fromVer),
		Path:          backupPath,
		Filename:      filename,
		Size:          fileInfo.Size(),
		Checksum:      checksum,
		CreatedAt:     time.Now().UTC(),
		FromVersion:   meta.FromVersion,
		TargetVersion: meta.TargetVersion,
		JobID:         meta.JobID,
		Database:      m.Config.PGDB,
		Host:          m.Config.PGHost,
		Port:          m.Config.PGPort,
	}

	// No index file needed - backups are discovered via filesystem scan

	return info, nil
}

// ListBackups returns all backups by scanning the filesystem.
// Scans BACKUP_DIR for payram-backup-*.sql and payram-backup-*.dump files.
// Parses metadata from filenames when possible.
// Returns sorted by timestamp DESC (parseable) or file modtime DESC (fallback).
func (m *Manager) ListBackups() ([]BackupListItem, error) {
	// Ensure directory exists
	if err := os.MkdirAll(m.Config.Dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create backup directory: %w", err)
	}

	entries, err := os.ReadDir(m.Config.Dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	var backups []BackupListItem
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		// Match payram-backup-*.sql or payram-backup-*.dump
		if !strings.HasPrefix(filename, "payram-backup-") {
			continue
		}
		if !strings.HasSuffix(filename, ".sql") && !strings.HasSuffix(filename, ".dump") {
			continue
		}

		fullPath := filepath.Join(m.Config.Dir, filename)
		info, err := os.Stat(fullPath)
		if err != nil {
			m.Logger.Printf("Warning: failed to stat backup %s: %v", filename, err)
			continue
		}

		// Determine format
		format := "unknown"
		if strings.HasSuffix(filename, ".sql") {
			format = "sql"
		} else if strings.HasSuffix(filename, ".dump") {
			format = "dump"
		}

		// Parse metadata from filename
		meta := parseBackupFilename(filename)

		backup := BackupListItem{
			File:        fullPath,
			Filename:    filename,
			Format:      format,
			FromVersion: meta.FromVersion,
			ToVersion:   meta.ToVersion,
			CreatedAt:   meta.CreatedAt,
			SizeBytes:   info.Size(),
		}

		backups = append(backups, backup)
	}

	// Sort by timestamp (parsed or modtime) descending
	sort.Slice(backups, func(i, j int) bool {
		// Try to parse timestamps
		tiI, errI := time.Parse(time.RFC3339, backups[i].CreatedAt)
		tiJ, errJ := time.Parse(time.RFC3339, backups[j].CreatedAt)

		if errI == nil && errJ == nil {
			return tiI.After(tiJ)
		}

		// Fallback: compare by modtime
		infoI, errI := os.Stat(backups[i].File)
		infoJ, errJ := os.Stat(backups[j].File)
		if errI == nil && errJ == nil {
			return infoI.ModTime().After(infoJ.ModTime())
		}

		// Last resort: lexicographic by filename (descending)
		return backups[i].Filename > backups[j].Filename
	})

	return backups, nil
}

// parseBackupFilename extracts metadata from backup filename.
// Expected format: payram-backup-YYYYMMDD-HHMMSS-fromVer-to-toVer.{sql|dump}
// Returns "unknown" for fields that cannot be parsed.
func parseBackupFilename(filename string) struct {
	FromVersion string
	ToVersion   string
	CreatedAt   string // RFC3339 or empty
} {
	result := struct {
		FromVersion string
		ToVersion   string
		CreatedAt   string
	}{
		FromVersion: "unknown",
		ToVersion:   "unknown",
		CreatedAt:   "",
	}

	// Strip prefix and extension
	name := strings.TrimPrefix(filename, "payram-backup-")
	name = strings.TrimSuffix(name, ".sql")
	name = strings.TrimSuffix(name, ".dump")

	// Split by '-'
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return result
	}

	// Parse timestamp: YYYYMMDD-HHMMSS
	if len(parts) >= 2 {
		timestampStr := parts[0] + parts[1]
		if t, err := time.Parse("20060102150405", timestampStr); err == nil {
			result.CreatedAt = t.Format(time.RFC3339)
		}
	}

	// Parse versions: fromVer-to-toVer
	// Find "to" separator
	if len(parts) >= 4 {
		for i := 2; i < len(parts)-1; i++ {
			if parts[i] == "to" {
				// Everything before "to" is fromVersion
				result.FromVersion = strings.Join(parts[2:i], "-")
				// Everything after "to" is toVersion
				result.ToVersion = strings.Join(parts[i+1:], "-")
				break
			}
		}
	}

	return result
}

// PruneBackups removes old backups, keeping only the specified retention count.
// Returns the list of pruned backups.
func (m *Manager) PruneBackups(retention int) ([]BackupListItem, error) {
	if retention < 1 {
		return nil, fmt.Errorf("retention must be at least 1")
	}

	backups, err := m.ListBackups()
	if err != nil {
		return nil, err
	}

	if len(backups) <= retention {
		m.Logger.Printf("No backups to prune (have %d, retention %d)", len(backups), retention)
		return nil, nil
	}

	// Backups are sorted newest first, so keep the first `retention` and remove the rest
	toRemove := backups[retention:]

	var pruned []BackupListItem
	for _, backup := range toRemove {
		// Remove the file
		if err := os.Remove(backup.File); err != nil {
			if !os.IsNotExist(err) {
				m.Logger.Printf("Warning: failed to remove backup file %s: %v", backup.File, err)
				continue
			}
		}
		m.Logger.Printf("Pruned backup: %s", backup.Filename)
		pruned = append(pruned, backup)
	}

	return pruned, nil
}

// indexPath returns the path to the backups.json index file.
func (m *Manager) indexPath() string {
	return filepath.Join(m.Config.Dir, "backups.json")
}

// loadIndex loads the backup index from disk.
func (m *Manager) loadIndex() (*BackupIndex, error) {
	data, err := os.ReadFile(m.indexPath())
	if err != nil {
		return nil, err
	}

	var index BackupIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to parse backup index: %w", err)
	}

	return &index, nil
}

// saveIndex saves the backup index to disk.
func (m *Manager) saveIndex(index *BackupIndex) error {
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal backup index: %w", err)
	}

	if err := os.WriteFile(m.indexPath(), data, 0644); err != nil {
		return fmt.Errorf("failed to write backup index: %w", err)
	}

	return nil
}

// addToIndex adds a new backup to the index.
func (m *Manager) addToIndex(info *BackupInfo) error {
	index, err := m.loadIndex()
	if err != nil {
		if os.IsNotExist(err) {
			index = &BackupIndex{Backups: []BackupInfo{}}
		} else {
			return err
		}
	}

	index.Backups = append(index.Backups, *info)
	index.UpdatedAt = time.Now().UTC()

	return m.saveIndex(index)
}

// sanitizeVersion removes characters that are unsafe for filenames.
func sanitizeVersion(v string) string {
	if v == "" {
		return "unknown"
	}
	// Remove or replace unsafe characters
	replacer := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"*", "",
		"?", "",
		"\"", "",
		"<", "",
		">", "",
		"|", "",
	)
	return replacer.Replace(v)
}

// calculateChecksum computes SHA256 checksum of a file.
func calculateChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// GetLatestBackup returns the most recent backup, or nil if none exist.
func (m *Manager) GetLatestBackup() (*BackupListItem, error) {
	backups, err := m.ListBackups()
	if err != nil {
		return nil, err
	}
	if len(backups) == 0 {
		return nil, nil
	}
	return &backups[0], nil
}

// GetBackupByPath finds a backup by its file path.
func (m *Manager) GetBackupByPath(path string) (*BackupListItem, error) {
	backups, err := m.ListBackups()
	if err != nil {
		return nil, err
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	for _, b := range backups {
		bAbsPath, _ := filepath.Abs(b.File)
		if b.File == path || bAbsPath == absPath {
			return &b, nil
		}
	}

	return nil, nil
}

// RestoreOptions contains options for restore operation.
type RestoreOptions struct {
	// Confirmed indicates user has explicitly confirmed the restore.
	// If false, RestoreBackup will return an error requiring confirmation.
	Confirmed bool
	// ContainerName optionally specifies the container to extract DB credentials from.
	// If empty, will attempt to discover the Payram container automatically.
	ContainerName string
	// FullRecovery indicates whether to perform full recovery (DB restore + container rollback).
	// If true, skips the interactive recovery prompt.
	FullRecovery bool
}

// RestoreResult contains the result of a restore operation.
type RestoreResult struct {
	// DBRestored indicates database was successfully restored
	DBRestored bool
	// FromVersion is the version to which the system should be rolled back
	FromVersion string
	// ToVersion is the version that was being upgraded to when backup was created
	ToVersion string
	// NeedsRecovery indicates if the backup was taken during an upgrade
	NeedsRecovery bool
}

// RestoreBackup restores a database from a backup file.
// Detects format based on file extension:
// - .sql files use psql
// - .dump files use pg_restore
// Requires explicit confirmation via opts.Confirmed = true.
// Returns RestoreResult containing backup metadata for potential container rollback.
//
// CREDENTIAL RESOLUTION ORDER (STRICT):
// 1. If POSTGRES_HOST != localhost: require env vars, fail if missing
// 2. If container is running: extract from container
// 3. If container NOT running: load from data/state/db.env
// 4. If none available: FAIL with CREDENTIALS_UNAVAILABLE
func (m *Manager) RestoreBackup(ctx context.Context, backupPath string, opts RestoreOptions) (*RestoreResult, error) {
	// Safety gate: require explicit confirmation
	if !opts.Confirmed {
		return nil, fmt.Errorf("restore operation requires explicit confirmation: use --yes flag or set Confirmed=true")
	}

	// Verify backup file
	if err := m.VerifyBackupFile(backupPath); err != nil {
		return nil, fmt.Errorf("backup verification failed: %w", err)
	}

	// Extract backup metadata from filename
	filename := filepath.Base(backupPath)
	metadata := parseBackupFilename(filename)

	// Detect format
	format := detectBackupFormat(backupPath)
	if format == "unknown" {
		return nil, fmt.Errorf("INVALID_BACKUP_FORMAT: unsupported file extension (must be .sql or .dump)")
	}

	m.Logger.Printf("Restoring database from: %s (format: %s)", backupPath, format)

	// STRICT CREDENTIAL RESOLUTION
	var dbConfig *ContainerDBConfig
	var containerName string
	var credentialSource string

	// Check if we have a remote database via environment variables
	envHost := os.Getenv("POSTGRES_HOST")
	if envHost != "" && !IsLocalDB(envHost) {
		// RULE 1: Remote DB requires explicit credentials from environment
		m.Logger.Printf("Remote database detected: %s", envHost)
		dbConfig = &ContainerDBConfig{
			Host:     envHost,
			Port:     getEnvOrDefault("POSTGRES_PORT", "5432"),
			Database: getEnvOrDefault("POSTGRES_DATABASE", getEnvOrDefault("POSTGRES_DB", "")),
			Username: getEnvOrDefault("POSTGRES_USERNAME", getEnvOrDefault("POSTGRES_USER", "")),
			Password: os.Getenv("POSTGRES_PASSWORD"),
			SSLMode:  getEnvOrDefault("POSTGRES_SSLMODE", "disable"),
		}
		if err := dbConfig.Validate(); err != nil {
			return nil, fmt.Errorf("CREDENTIALS_REQUIRED: remote database requires valid credentials in environment variables: %w", err)
		}
		credentialSource = "environment variables (remote database)"
	} else {
		// Local database - try container first, then persisted credentials

		// RULE 2: Try to discover and extract from running container
		imagePattern := m.Config.ImagePattern
		if imagePattern == "" {
			imagePattern = "payramapp/payram:"
		}
		discoverer := container.NewDiscoverer("docker", imagePattern, m.Logger)
		discovered, err := discoverer.DiscoverPayramContainer(ctx)

		if err == nil {
			// Container is running - extract credentials
			containerName = discovered.Name
			inspector := NewDockerInspector("docker", m.Executor)
			dbConfig, err = inspector.GetDBConfig(ctx, containerName)
			if err != nil {
				return nil, fmt.Errorf("failed to extract credentials from running container: %w", err)
			}
			credentialSource = fmt.Sprintf("running container (%s)", containerName)
			m.Logger.Printf("Using credentials from running container: %s (version: %s)", containerName, discovered.ImageTag)
		} else {
			// RULE 3: Container not running - try persisted credentials
			m.Logger.Printf("No running Payram container found, attempting to load persisted credentials...")
			dbConfig, err = LoadPersistedCredentials(m.Config.Dir)
			if err != nil {
				// RULE 4: No credentials available
				return nil, fmt.Errorf("CREDENTIALS_UNAVAILABLE: no running container and no persisted credentials found.\n\nRecovery options:\n1. Start the Payram container and retry\n2. Ensure data/state/db.env exists with valid credentials\n3. For remote databases, set POSTGRES_* environment variables\n\nError: %w", err)
			}
			credentialSource = "persisted credentials (data/state/db.env)"
			m.Logger.Printf("Using stored local DB credentials from data/state/db.env")

			// Use provided container name if available (for local DB restore via docker exec)
			if opts.ContainerName != "" {
				containerName = opts.ContainerName
				m.Logger.Printf("Using provided container name: %s", containerName)
			}
		}
	}

	// Validate DB config
	if err := dbConfig.Validate(); err != nil {
		return nil, fmt.Errorf("INVALID_DB_CONFIG: %w", err)
	}

	m.Logger.Printf("Credential source: %s", credentialSource)

	// Execute restore based on database location
	isLocal := IsLocalDB(dbConfig.Host)

	var err error
	if format == "sql" {
		err = m.restoreWithPsql(ctx, backupPath, dbConfig, containerName, isLocal)
	} else if format == "dump" {
		err = m.restoreWithPgRestore(ctx, backupPath, dbConfig, containerName, isLocal)
	} else {
		return nil, fmt.Errorf("INVALID_BACKUP_FORMAT: unknown format %s", format)
	}

	if err != nil {
		return nil, err
	}

	// Build restore result with backup metadata
	result := &RestoreResult{
		DBRestored:    true,
		FromVersion:   metadata.FromVersion,
		ToVersion:     metadata.ToVersion,
		NeedsRecovery: metadata.FromVersion != "unknown" && metadata.ToVersion != "unknown",
	}

	return result, nil
}

// getEnvOrDefault returns the value of an environment variable or a default value.
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// detectBackupFormat returns "sql", "dump", or "unknown" based on file extension.
func detectBackupFormat(path string) string {
	if strings.HasSuffix(path, ".sql") {
		return "sql"
	}
	if strings.HasSuffix(path, ".dump") {
		return "dump"
	}
	return "unknown"
}

// restoreWithPsql restores a plain SQL dump using psql.
// For local databases, runs psql inside the container using docker exec.
// For remote databases, runs psql from the host.
func (m *Manager) restoreWithPsql(ctx context.Context, backupPath string, dbConfig *ContainerDBConfig, containerName string, isLocal bool) error {
	psqlBin := "psql"
	if m.Config.PGDumpBin != "pg_dump" && m.Config.PGDumpBin != "" {
		dir := filepath.Dir(m.Config.PGDumpBin)
		psqlBin = filepath.Join(dir, "psql")
	}

	var output []byte
	var err error

	if isLocal && containerName != "" {
		// LOCAL DB: Run psql inside container using docker exec
		m.Logger.Printf("Executing restore inside container: %s", containerName)

		// Make backup file absolute path for docker exec
		absBackupPath, err := filepath.Abs(backupPath)
		if err != nil {
			return fmt.Errorf("failed to get absolute path: %w", err)
		}

		// Create a command that pipes the backup content
		// We'll use sh -c to pipe content through stdin
		shellCmd := fmt.Sprintf("docker exec -i %s psql -U %s -d %s",
			containerName, dbConfig.Username, dbConfig.Database)

		output, err = m.Executor.Execute(ctx, "sh", []string{"-c", fmt.Sprintf("cat %s | %s", absBackupPath, shellCmd)}, nil)
	} else {
		// REMOTE DB: Run psql from host
		m.Logger.Printf("Executing restore from host to remote database: %s:%s", dbConfig.Host, dbConfig.Port)

		args := []string{
			"-h", dbConfig.Host,
			"-p", dbConfig.Port,
			"-U", dbConfig.Username,
			"-d", dbConfig.Database,
			"-f", backupPath,
		}

		// Build environment with PGPASSWORD (NEVER log this)
		env := os.Environ()
		if dbConfig.Password != "" {
			env = append(env, fmt.Sprintf("PGPASSWORD=%s", dbConfig.Password))
		}

		output, err = m.Executor.Execute(ctx, psqlBin, args, env)
	}

	if err != nil {
		// NEVER include env in error (contains password)
		return fmt.Errorf("psql failed: %w: %s", err, string(output))
	}

	m.Logger.Printf("Database restored successfully from: %s (using psql)", backupPath)
	return nil
}

// restoreWithPgRestore restores a custom format dump using pg_restore.
// For local databases, runs pg_restore inside the container using docker exec.
// For remote databases, runs pg_restore from the host.
func (m *Manager) restoreWithPgRestore(ctx context.Context, backupPath string, dbConfig *ContainerDBConfig, containerName string, isLocal bool) error {
	pgRestoreBin := "pg_restore"
	if m.Config.PGDumpBin != "pg_dump" && m.Config.PGDumpBin != "" {
		dir := filepath.Dir(m.Config.PGDumpBin)
		pgRestoreBin = filepath.Join(dir, "pg_restore")
	}

	var output []byte
	var err error

	if isLocal && containerName != "" {
		// LOCAL DB: Run pg_restore inside container using docker exec
		m.Logger.Printf("Executing restore inside container: %s", containerName)

		// Make backup file absolute path
		absBackupPath, err := filepath.Abs(backupPath)
		if err != nil {
			return fmt.Errorf("failed to get absolute path: %w", err)
		}

		// Build docker exec command - pg_restore can read from stdin
		shellCmd := fmt.Sprintf("docker exec -i %s pg_restore --clean --if-exists --no-owner --no-privileges -U %s -d %s",
			containerName, dbConfig.Username, dbConfig.Database)

		output, err = m.Executor.Execute(ctx, "sh", []string{"-c", fmt.Sprintf("cat %s | %s", absBackupPath, shellCmd)}, nil)
	} else {
		// REMOTE DB: Run pg_restore from host
		m.Logger.Printf("Executing restore from host to remote database: %s:%s", dbConfig.Host, dbConfig.Port)

		args := []string{
			"--clean",
			"--if-exists",
			"--no-owner",
			"--no-privileges",
			"-h", dbConfig.Host,
			"-p", dbConfig.Port,
			"-U", dbConfig.Username,
			"-d", dbConfig.Database,
			backupPath,
		}

		// Build environment with PGPASSWORD (NEVER log this)
		env := os.Environ()
		if dbConfig.Password != "" {
			env = append(env, fmt.Sprintf("PGPASSWORD=%s", dbConfig.Password))
		}

		output, err = m.Executor.Execute(ctx, pgRestoreBin, args, env)
	}

	if err != nil {
		// NEVER include env in error (contains password)
		return fmt.Errorf("pg_restore failed: %w: %s", err, string(output))
	}

	m.Logger.Printf("Database restored successfully from: %s (using pg_restore)", backupPath)
	return nil
}

// VerifyBackupFile checks that a backup file is valid for restore.
// Checks: file exists, non-zero size, readable.
func (m *Manager) VerifyBackupFile(path string) error {
	// Check file exists
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("backup file does not exist: %s", path)
		}
		return fmt.Errorf("cannot stat backup file: %w", err)
	}

	// Check it's a regular file
	if info.IsDir() {
		return fmt.Errorf("backup path is a directory, not a file: %s", path)
	}

	// Check non-zero size
	if info.Size() == 0 {
		return fmt.Errorf("backup file is empty (0 bytes): %s", path)
	}

	// Check readable
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backup file is not readable: %w", err)
	}
	f.Close()

	return nil
}
