// Package backup provides database backup and restore functionality.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
	"github.com/payram/payram-updater/internal/jobs"
)

// BackupInfo contains metadata about a backup.
type BackupInfo struct {
	ID            string    `json:"id"`
	Path          string    `json:"path"`
	Filename      string    `json:"filename"`
	Size          int64     `json:"size"`
	Checksum      string    `json:"checksum,omitempty"` // SHA256, optional
	CreatedAt     time.Time `json:"createdAt"`
	FromVersion   string    `json:"fromVersion,omitempty"`
	TargetVersion string    `json:"targetVersion,omitempty"`
	JobID         string    `json:"jobId,omitempty"`
	Database      string    `json:"database"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
}

// BackupListItem contains metadata for a backup file discovered from filesystem.
type BackupListItem struct {
	File        string `json:"file"`        // Full path
	Filename    string `json:"filename"`    // Basename
	Format      string `json:"format"`      // "sql" or "dump"
	FromVersion string `json:"fromVersion"` // Parsed or "unknown"
	ToVersion   string `json:"toVersion"`   // Parsed or "unknown"
	CreatedAt   string `json:"createdAt"`   // RFC3339 if parseable, else empty
	SizeBytes   int64  `json:"sizeBytes"`
}

// HasUpgradeVersions reports whether the backup records the versions of an
// upgrade (taken before upgrading FromVersion to ToVersion). Only such a
// backup can be paired with a container rollback; manual and safety backups
// cannot.
func (b BackupListItem) HasUpgradeVersions() bool {
	known := func(v string) bool {
		return v != "" && v != "unknown" && v != "manual"
	}
	return known(b.FromVersion) && known(b.ToVersion)
}

// TakenAt is when the backup was taken: the timestamp in its name, else the
// file's modification time.
func (b BackupListItem) TakenAt() time.Time {
	if t, err := time.Parse(time.RFC3339, b.CreatedAt); err == nil {
		return t
	}
	if info, err := os.Stat(b.File); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// BackupMeta contains metadata to pass when creating a backup.
type BackupMeta struct {
	FromVersion   string
	TargetVersion string
	JobID         string
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
	Dir                 string
	Retention           int
	PGHost              string
	PGPort              int
	PGDB                string
	PGUser              string
	PGPassword          string
	PGDumpBin           string // Path to pg_dump binary, default "pg_dump"
	ImagePattern        string // Image pattern for container discovery, default "payramapp/payram:"
	TargetContainerName string // Optional: explicit container name, bypasses semver discovery
	// StateDir is the updater's state directory. Restores take the container
	// lock there, and pruning never deletes the backup of the latest failed
	// update recorded there. Empty disables both (restores then refuse).
	StateDir string
	// DockerBin is the docker client, default "docker".
	DockerBin string
	// SupervisorExclude names supervisord programs that keep running while
	// the database is restored. Programs whose name contains "postgres"
	// always keep running.
	SupervisorExclude []string
}

// Manager handles backup operations.
type Manager struct {
	Config   Config
	Executor CommandExecutor
	Logger   Logger
	// Runner runs pg_dump, pg_restore, psql and supervisorctl; nil means
	// dbexec.OSRunner.
	Runner dbexec.Runner
}

// NewManager creates a new backup manager.
func NewManager(cfg Config, executor CommandExecutor, logger Logger) *Manager {
	if cfg.PGDumpBin == "" {
		cfg.PGDumpBin = "pg_dump"
	}
	if cfg.DockerBin == "" {
		cfg.DockerBin = "docker"
	}
	return &Manager{
		Config:   cfg,
		Executor: executor,
		Logger:   logger,
		Runner:   dbexec.OSRunner{},
	}
}

func (m *Manager) runner() dbexec.Runner {
	if m.Runner == nil {
		return dbexec.OSRunner{}
	}
	return m.Runner
}

// ErrRestoreLocked means another update or restore holds the container lock.
var ErrRestoreLocked = errors.New("an update or restore is running; try again when it finishes")

// MessageRestoreLocked is what the operator is told when ErrRestoreLocked
// stops a restore.
const MessageRestoreLocked = "An update or restore is running; try again when it finishes."

// safetyBackupSuffix names the dump of the current database taken before a
// restore, so the restore can be undone.
const safetyBackupSuffix = "pre-restore-safety"

// staleFileAge is how old a .partial or empty backup file must be before
// pruning deletes it: younger ones may still be being written.
const staleFileAge = time.Hour

// backupFilename is payram-backup-<UTC timestamp>-<from>-to-<to>.dump.
func backupFilename(now time.Time, meta BackupMeta) string {
	return fmt.Sprintf("payram-backup-%s-%s-to-%s.dump",
		now.UTC().Format("20060102-150405"), sanitizeVersion(meta.FromVersion), sanitizeVersion(meta.TargetVersion))
}

// safetyBackupFilename is payram-backup-<UTC timestamp>-pre-restore-safety.dump.
func safetyBackupFilename(now time.Time) string {
	return fmt.Sprintf("payram-backup-%s-%s.dump", now.UTC().Format("20060102-150405"), safetyBackupSuffix)
}

// secureBackupDir creates the backup directory with mode 0700 and tightens
// an existing one: backups hold the whole payments database. Existing
// backup files are made 0600 as well. A failed chmod is reported in the log
// but does not stop a backup.
func secureBackupDir(dir string, logger Logger) error {
	if dir == "" {
		return fmt.Errorf("backup directory is not configured")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create backup directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0700); err != nil && logger != nil {
		logger.Printf("Warning: could not restrict backup directory %s to its owner (chmod 0700): %v", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "payram-backup-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode().Perm()&0077 == 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.Chmod(path, 0600); err != nil && logger != nil {
			logger.Printf("Warning: could not restrict backup %s to its owner (chmod 0600): %v", path, err)
		}
	}
	return nil
}

// resolveDatabase finds the database to work on and the executor that
// reaches it: docker exec for the database bundled in the PayRam container,
// host tools for an external one. containerOverride, when set, names the
// PayRam container to use instead of the configured or discovered one.
func (m *Manager) resolveDatabase(ctx context.Context, containerOverride string) (dbexec.DBContext, dbexec.PGExecutor, error) {
	containerName := m.Config.TargetContainerName
	if containerOverride != "" {
		containerName = containerOverride
	}
	dbCtx, err := dbexec.DiscoverDBContext(ctx, &executorWrapper{executor: m.Executor}, dbexec.DiscoverOpts{
		ContainerName: containerName,
		ImagePattern:  m.Config.ImagePattern,
		BackupDir:     m.Config.Dir,
		Logger:        m.Logger,
	})
	if err != nil {
		return dbexec.DBContext{}, nil, err
	}
	m.Logger.Printf("Credential source: %s", dbCtx.CredSource)

	if dbCtx.Mode == dbexec.DBModeInContainer {
		if containerOverride != "" {
			dbCtx.ContainerName = containerOverride
		}
		if dbCtx.ContainerName == "" {
			return dbexec.DBContext{}, nil, &dbexec.DBError{
				Code:    dbexec.ErrCodeContainerNotFound,
				Message: "the database runs inside the PayRam container, but no container name is known; start the container or set TARGET_CONTAINER_NAME",
			}
		}
		m.Logger.Printf("DB mode: in_container, Executor: docker, Container: %s", dbCtx.ContainerName)
		docker := dbexec.NewDockerPGExecutor(m.runner(), m.Logger)
		docker.DockerBin = m.Config.DockerBin
		return dbCtx, docker, nil
	}

	if containerOverride != "" {
		dbCtx.AppContainer = containerOverride
	}
	m.Logger.Printf("DB mode: external, Executor: host, Host: %s:%s", dbCtx.Creds.Host, dbCtx.Creds.Port)
	host := dbexec.NewHostPGExecutor(m.runner(), m.Logger)
	host.UseToolsNextTo(m.Config.PGDumpBin)
	return dbCtx, host, nil
}

// CreateBackup creates a new database backup using pg_dump.
// Returns BackupInfo with metadata, or an error.
// Backups are always enabled.
//
// BACKUP LOCATION MODES (matches restore logic):
// 1. If POSTGRES_HOST is localhost/127.0.0.1 → IN_CONTAINER_DB (run pg_dump via docker exec)
// 2. Else → EXTERNAL_DB (run pg_dump from host)
func (m *Manager) CreateBackup(ctx context.Context, meta BackupMeta) (*BackupInfo, error) {
	if err := secureBackupDir(m.Config.Dir, m.Logger); err != nil {
		return nil, err
	}

	dbCtx, pgExec, err := m.resolveDatabase(ctx, "")
	if err != nil {
		var dbErr *dbexec.DBError
		if errors.As(err, &dbErr) && dbErr.Code == dbexec.ErrCodeContainerNotFound {
			return nil, fmt.Errorf("BACKUP_FAILED: container not running; cannot backup in-container DB. Start the container and retry")
		}
		return nil, err
	}

	now := time.Now().UTC()
	filename := backupFilename(now, meta)
	backupPath := filepath.Join(m.Config.Dir, filename)
	m.Logger.Printf("Creating backup: %s", backupPath)

	if err := pgExec.Dump(ctx, dbCtx, backupPath, "dump"); err != nil {
		return nil, err
	}

	// Persist credentials if this is a local database
	// Only persist after successful backup, and only for localhost/127.0.0.1
	if dbCtx.Mode == dbexec.DBModeInContainer {
		dbConfig := &ContainerDBConfig{
			Host:     dbCtx.Creds.Host,
			Port:     dbCtx.Creds.Port,
			Database: dbCtx.Creds.Database,
			Username: dbCtx.Creds.Username,
			Password: dbCtx.Creds.Password,
			SSLMode:  dbCtx.Creds.SSLMode,
		}
		if err := PersistDBCredentials(m.Config.Dir, dbConfig); err != nil {
			m.Logger.Printf("Warning: failed to persist DB credentials: %v", err)
		} else {
			m.Logger.Printf("Persisted local DB credentials to data/state/db.env")
		}
	}

	fileInfo, err := os.Stat(backupPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat backup file: %w", err)
	}

	checksum, err := calculateChecksum(backupPath)
	if err != nil {
		m.Logger.Printf("Warning: failed to calculate checksum: %v", err)
		checksum = ""
	}

	return &BackupInfo{
		ID:            fmt.Sprintf("%s-%s", now.Format("20060102-150405"), sanitizeVersion(meta.FromVersion)),
		Path:          backupPath,
		Filename:      filename,
		Size:          fileInfo.Size(),
		Checksum:      checksum,
		CreatedAt:     now,
		FromVersion:   meta.FromVersion,
		TargetVersion: meta.TargetVersion,
		JobID:         meta.JobID,
		Database:      dbCtx.Creds.Database,
		Host:          dbCtx.Creds.Host,
		Port:          mustParsePort(dbCtx.Creds.Port),
	}, nil
}

// mustParsePort converts a port string to int, returns 0 if invalid.
func mustParsePort(portStr string) int {
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return port
}

// isBackupFilename reports whether name is a finished backup
// (payram-backup-*.sql or payram-backup-*.dump).
func isBackupFilename(name string) bool {
	return strings.HasPrefix(name, "payram-backup-") &&
		(strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".dump"))
}

// DescribeBackupFile reads a backup file's metadata from its name and size.
func DescribeBackupFile(path string) (BackupListItem, error) {
	info, err := os.Stat(path)
	if err != nil {
		return BackupListItem{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	filename := filepath.Base(path)
	meta := parseBackupFilename(filename)
	return BackupListItem{
		File:        abs,
		Filename:    filename,
		Format:      detectBackupFormat(filename),
		FromVersion: meta.FromVersion,
		ToVersion:   meta.ToVersion,
		CreatedAt:   meta.CreatedAt,
		SizeBytes:   info.Size(),
	}, nil
}

// ListBackups returns all backups by scanning the filesystem.
// Scans BACKUP_DIR for payram-backup-*.sql and payram-backup-*.dump files.
// Empty files (a backup that never got written) are not listed.
// Returns sorted by timestamp DESC (parseable) or file modtime DESC (fallback).
func (m *Manager) ListBackups() ([]BackupListItem, error) {
	if err := secureBackupDir(m.Config.Dir, m.Logger); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(m.Config.Dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read backup directory: %w", err)
	}

	var backups []BackupListItem
	for _, entry := range entries {
		if entry.IsDir() || !isBackupFilename(entry.Name()) {
			continue
		}
		fullPath := filepath.Join(m.Config.Dir, entry.Name())
		item, err := DescribeBackupFile(fullPath)
		if err != nil {
			m.Logger.Printf("Warning: failed to stat backup %s: %v", entry.Name(), err)
			continue
		}
		if item.SizeBytes == 0 {
			continue
		}
		item.File = fullPath
		backups = append(backups, item)
	}

	// Sort by timestamp (parsed or modtime) descending
	sort.Slice(backups, func(i, j int) bool {
		tiI, errI := time.Parse(time.RFC3339, backups[i].CreatedAt)
		tiJ, errJ := time.Parse(time.RFC3339, backups[j].CreatedAt)
		if errI == nil && errJ == nil {
			return tiI.After(tiJ)
		}

		infoI, errI := os.Stat(backups[i].File)
		infoJ, errJ := os.Stat(backups[j].File)
		if errI == nil && errJ == nil {
			return infoI.ModTime().After(infoJ.ModTime())
		}

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
	timestampStr := parts[0] + parts[1]
	if t, err := time.Parse("20060102150405", timestampStr); err == nil {
		result.CreatedAt = t.Format(time.RFC3339)
	}

	// Parse versions: fromVer-to-toVer
	if len(parts) >= 4 {
		for i := 2; i < len(parts)-1; i++ {
			if parts[i] == "to" {
				result.FromVersion = strings.Join(parts[2:i], "-")
				result.ToVersion = strings.Join(parts[i+1:], "-")
				break
			}
		}
	}

	return result
}

// FindFailedUpdateBackup returns the backup recorded by the latest update
// job when that job FAILED and the file still exists: it is the recovery
// point of the failed update, and must not be pruned or cleaned up. Empty
// when there is no such backup or stateDir is empty.
func FindFailedUpdateBackup(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	job, err := jobs.NewStore(stateDir).LoadLatest()
	if err != nil || job == nil || job.State != jobs.JobStateFailed || job.BackupPath == "" {
		return ""
	}
	if info, err := os.Stat(job.BackupPath); err != nil || info.IsDir() {
		return ""
	}
	return job.BackupPath
}

// samePath compares two paths after making them absolute and clean.
func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return absA == absB
}

// PruneBackups removes old backups, keeping the newest `retention` of them.
// It never deletes protectedPaths or the backup of the latest FAILED update
// (when Config.StateDir is set), and it clears out .partial and empty files
// left by interrupted backups instead of counting them.
// Returns the list of pruned backups.
func (m *Manager) PruneBackups(retention int, protectedPaths ...string) ([]BackupListItem, error) {
	if retention < 1 {
		return nil, fmt.Errorf("retention must be at least 1")
	}

	protected := append([]string(nil), protectedPaths...)
	if failed := FindFailedUpdateBackup(m.Config.StateDir); failed != "" {
		protected = append(protected, failed)
	}
	isProtected := func(path string) bool {
		for _, p := range protected {
			if p != "" && samePath(p, path) {
				return true
			}
		}
		return false
	}

	m.removeStaleBackupFiles()

	backups, err := m.ListBackups()
	if err != nil {
		return nil, err
	}

	if len(backups) <= retention {
		m.Logger.Printf("No backups to prune (have %d, retention %d)", len(backups), retention)
		return nil, nil
	}

	// Backups are sorted newest first, so keep the first `retention` and remove the rest
	var pruned []BackupListItem
	for _, backup := range backups[retention:] {
		if isProtected(backup.File) {
			m.Logger.Printf("Keeping backup %s: it is the recovery point of the last failed update", backup.Filename)
			continue
		}
		if err := os.Remove(backup.File); err != nil && !os.IsNotExist(err) {
			m.Logger.Printf("Warning: failed to remove backup file %s: %v", backup.File, err)
			continue
		}
		m.Logger.Printf("Pruned backup: %s", backup.Filename)
		pruned = append(pruned, backup)
	}

	return pruned, nil
}

// removeStaleBackupFiles deletes .partial files and empty backup files older
// than staleFileAge: leftovers of backups that were interrupted.
func (m *Manager) removeStaleBackupFiles() {
	entries, err := os.ReadDir(m.Config.Dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "payram-backup-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < staleFileAge {
			continue
		}
		partial := strings.HasSuffix(name, dbexec.PartialSuffix)
		empty := isBackupFilename(name) && info.Size() == 0
		if !partial && !empty {
			continue
		}
		path := filepath.Join(m.Config.Dir, name)
		if err := os.Remove(path); err != nil {
			m.Logger.Printf("Warning: failed to remove incomplete backup %s: %v", path, err)
			continue
		}
		m.Logger.Printf("Removed incomplete backup left by an interrupted run: %s", name)
	}
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

	for _, b := range backups {
		if b.File == path || samePath(b.File, path) {
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
	// ContainerLockHeld says the caller already holds jobs.AcquireContainerLock
	// (for example around a container rollback plus this restore). Otherwise
	// RestoreBackup takes the lock itself and refuses with ErrRestoreLocked
	// while an update or another restore holds it.
	ContainerLockHeld bool
	// SkipSafetyBackup skips the dump of the current database that is
	// otherwise taken right before it is replaced (--no-safety-backup, or
	// because the caller already took one with CreateSafetyBackup).
	SkipSafetyBackup bool
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
	// SafetyBackupPath is the dump of the database as it was right before
	// the restore; restoring it undoes the restore. Empty when skipped.
	SafetyBackupPath string
	// Database is what the database holds after the restore.
	Database dbexec.DatabaseSummary
	// Warnings are problems that did not stop the restore.
	Warnings []string
}

// lockContainer takes the host-wide container lock for a restore.
func (m *Manager) lockContainer() (func(), error) {
	if m.Config.StateDir == "" {
		return nil, fmt.Errorf("the updater state directory is not configured, so the restore cannot make sure no update is running")
	}
	release, err := jobs.AcquireContainerLock(m.Config.StateDir)
	if errors.Is(err, jobs.ErrUpgradeLocked) {
		return nil, ErrRestoreLocked
	}
	if err != nil {
		return nil, err
	}
	return release, nil
}

// RestoreBackup restores a database from a backup file (.sql or .dump).
// Requires explicit confirmation via opts.Confirmed = true.
//
// The restore is all-or-nothing: the public schema is emptied and rebuilt
// from the backup inside one transaction, so any error leaves the database
// exactly as it was. Before that it holds the container lock, checks the
// backup file is complete, stops PayRam's programs inside the container
// (started again afterwards, whatever happens) and dumps the current
// database to a safety backup that undoes the restore.
//
// CREDENTIAL RESOLUTION ORDER (STRICT):
// 1. If POSTGRES_HOST != localhost: require env vars, fail if missing
// 2. If container is running: extract from container
// 3. If container NOT running: load from data/state/db.env
// 4. If none available: FAIL with CREDENTIALS_UNAVAILABLE
func (m *Manager) RestoreBackup(ctx context.Context, backupPath string, opts RestoreOptions) (result *RestoreResult, err error) {
	// Safety gate: require explicit confirmation
	if !opts.Confirmed {
		return nil, fmt.Errorf("restore operation requires explicit confirmation: use --yes flag or set Confirmed=true")
	}

	if err := m.VerifyBackupFile(backupPath); err != nil {
		return nil, fmt.Errorf("backup verification failed: %w", err)
	}

	format := detectBackupFormat(backupPath)
	if format == "unknown" {
		return nil, fmt.Errorf("INVALID_BACKUP_FORMAT: unsupported file extension (must be .sql or .dump)")
	}
	item, err := DescribeBackupFile(backupPath)
	if err != nil {
		return nil, fmt.Errorf("backup verification failed: %w", err)
	}

	if !opts.ContainerLockHeld {
		release, err := m.lockContainer()
		if err != nil {
			return nil, err
		}
		defer release()
	}

	m.Logger.Printf("Restoring database from: %s (format: %s)", backupPath, format)

	dbCtx, pgExec, err := m.resolveDatabase(ctx, opts.ContainerName)
	if err != nil {
		var dbErr *dbexec.DBError
		if errors.As(err, &dbErr) && dbErr.Code == dbexec.ErrCodeContainerNotFound {
			return nil, fmt.Errorf("CREDENTIALS_UNAVAILABLE: no running container and no persisted credentials found.\n\nRecovery options:\n1. Start the Payram container and retry\n2. Ensure data/state/db.env exists with valid credentials\n3. For remote databases, set POSTGRES_* environment variables\n\nError: %w", err)
		}
		return nil, err
	}

	// Check the backup before stopping anything.
	if err := pgExec.VerifyDumpFile(ctx, dbCtx, backupPath, format); err != nil {
		return nil, fmt.Errorf("the backup file is damaged or incomplete, so nothing was changed: %w", err)
	}

	startAgain, err := m.stopAppPrograms(ctx, dbCtx.AppContainer)
	if err != nil {
		return nil, err
	}
	defer func() {
		startErr := startAgain()
		if startErr == nil {
			return
		}
		if err != nil {
			err = fmt.Errorf("%w\nAlso: %v", err, startErr)
		} else if result != nil {
			result.Warnings = append(result.Warnings, startErr.Error())
		}
	}()

	result = &RestoreResult{
		FromVersion:   item.FromVersion,
		ToVersion:     item.ToVersion,
		NeedsRecovery: item.HasUpgradeVersions(),
	}

	if opts.SkipSafetyBackup {
		m.Logger.Printf("Safety backup skipped: this restore cannot be undone with the updater")
	} else {
		safetyPath, err := m.dumpSafetyBackup(ctx, dbCtx, pgExec)
		if err != nil {
			return nil, fmt.Errorf("could not take a safety backup of the current database, so nothing was restored. Fix the cause (often disk space) and retry, or pass --no-safety-backup to restore without one: %w", err)
		}
		result.SafetyBackupPath = safetyPath
	}

	if err := pgExec.Restore(ctx, dbCtx, backupPath, format); err != nil {
		if result.SafetyBackupPath != "" {
			return nil, fmt.Errorf("%w\nThe database as it was before this attempt is saved in %s", err, result.SafetyBackupPath)
		}
		return nil, err
	}
	result.DBRestored = true

	summary, err := pgExec.DescribeDatabase(ctx, dbCtx)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("the restore was committed but the tables could not be counted: %v", err))
		m.Logger.Printf("Warning: %s", result.Warnings[len(result.Warnings)-1])
	} else {
		result.Database = summary
		if summary.HasMigrations {
			m.Logger.Printf("Database restored: %d tables, %d rows in migrations", summary.Tables, summary.MigrationRows)
		} else {
			m.Logger.Printf("Database restored: %d tables (no migrations table)", summary.Tables)
		}
	}

	return result, nil
}

// dumpSafetyBackup dumps the current database to a safety backup.
func (m *Manager) dumpSafetyBackup(ctx context.Context, dbCtx dbexec.DBContext, pgExec dbexec.PGExecutor) (string, error) {
	if err := secureBackupDir(m.Config.Dir, m.Logger); err != nil {
		return "", err
	}
	path := filepath.Join(m.Config.Dir, safetyBackupFilename(time.Now()))
	m.Logger.Printf("Taking a safety backup of the current database: %s", path)
	if err := pgExec.Dump(ctx, dbCtx, path, "dump"); err != nil {
		return "", err
	}
	return path, nil
}

// CreateSafetyBackup dumps the current database (reached through
// containerName, or the configured/discovered PayRam container when empty)
// to payram-backup-<timestamp>-pre-restore-safety.dump and returns its path.
// Use it before a step that changes the container ahead of RestoreBackup,
// then pass SkipSafetyBackup to RestoreBackup.
func (m *Manager) CreateSafetyBackup(ctx context.Context, containerName string) (string, error) {
	dbCtx, pgExec, err := m.resolveDatabase(ctx, containerName)
	if err != nil {
		return "", err
	}
	return m.dumpSafetyBackup(ctx, dbCtx, pgExec)
}

// CheckBackupFile checks that a backup file is complete (pg_restore --list
// for .dump, the dump-complete footer for .sql) using the tools of the
// database it would be restored into. It changes nothing.
func (m *Manager) CheckBackupFile(ctx context.Context, backupPath string, containerName string) error {
	if err := m.VerifyBackupFile(backupPath); err != nil {
		return err
	}
	format := detectBackupFormat(backupPath)
	if format == "unknown" {
		return fmt.Errorf("INVALID_BACKUP_FORMAT: unsupported file extension (must be .sql or .dump)")
	}
	dbCtx, pgExec, err := m.resolveDatabase(ctx, containerName)
	if err != nil {
		return err
	}
	return pgExec.VerifyDumpFile(ctx, dbCtx, backupPath, format)
}

// WaitForDatabase polls pg_isready inside containerName until the bundled
// database accepts connections or timeout passes. An external database does
// not restart with the container, so there is nothing to wait for. When
// pg_isready is missing the wait is skipped with a warning: the restore's own
// connection then fails loudly if the database is not up.
func (m *Manager) WaitForDatabase(ctx context.Context, containerName string, timeout time.Duration) error {
	dbCtx, pgExec, err := m.resolveDatabase(ctx, containerName)
	if err != nil {
		return err
	}
	if dbCtx.Mode != dbexec.DBModeInContainer {
		return nil
	}
	err = pgExec.WaitReady(ctx, dbCtx, timeout)
	var dbErr *dbexec.DBError
	if errors.As(err, &dbErr) && dbErr.Code == dbexec.ErrCodeToolMissing {
		m.Logger.Printf("Warning: %s; not waiting for the database", dbErr.Message)
		return nil
	}
	return err
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

// executorWrapper wraps a backup.CommandExecutor to satisfy dbexec.CommandExecutor
type executorWrapper struct {
	executor CommandExecutor
}

func (e *executorWrapper) Execute(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	return e.executor.Execute(ctx, name, args, env)
}
