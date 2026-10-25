package diskspace

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
)

// DBConfig holds database connection information.
type DBConfig struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
}

// IsLocalDB returns true if the database is running locally (inside the container).
func (c *DBConfig) IsLocalDB() bool {
	host := strings.ToLower(c.Host)
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// DBSizeChecker provides methods to query database size.
type DBSizeChecker struct {
	DockerBin string
}

// NewDBSizeChecker creates a new DBSizeChecker.
func NewDBSizeChecker(dockerBin string) *DBSizeChecker {
	if dockerBin == "" {
		dockerBin = "docker"
	}
	return &DBSizeChecker{
		DockerBin: dockerBin,
	}
}

// GetDatabaseSize queries the database size in bytes.
// Returns size in bytes or error.
func (c *DBSizeChecker) GetDatabaseSize(ctx context.Context, containerName string, dbConfig *DBConfig) (int64, error) {
	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// SQL query to get database size
	sqlQuery := "SELECT pg_database_size(current_database());"

	var output []byte
	var err error

	if dbConfig.IsLocalDB() {
		// Execute psql inside the container
		output, err = c.executeQueryInContainer(ctx, containerName, dbConfig, sqlQuery)
	} else {
		// Execute psql from host to external database
		output, err = c.executeQueryFromHost(ctx, dbConfig, sqlQuery)
	}

	if err != nil {
		return 0, fmt.Errorf("failed to query database size: %w", err)
	}

	// Parse the output (psql returns just the number)
	outputStr := strings.TrimSpace(string(output))
	lines := strings.Split(outputStr, "\n")

	// Look for the numeric result (skip header/footer lines)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines, headers, and separator lines
		if line == "" || strings.Contains(line, "pg_database_size") || strings.Contains(line, "---") || strings.Contains(line, "row") {
			continue
		}

		// Try to parse as integer
		size, parseErr := strconv.ParseInt(line, 10, 64)
		if parseErr == nil && size > 0 {
			return size, nil
		}
	}

	return 0, fmt.Errorf("could not parse database size from output: %s", dbexec.TruncateOutput([]byte(outputStr)))
}

// executeQueryInContainer runs psql inside the container. The password goes
// to docker by name only (-e PGPASSWORD) with the value in docker's own
// environment, so it never shows in the process list.
func (c *DBSizeChecker) executeQueryInContainer(ctx context.Context, containerName string, dbConfig *DBConfig, query string) ([]byte, error) {
	args := []string{"exec"}
	if dbConfig.Password != "" {
		args = append(args, "-e", "PGPASSWORD")
	}
	args = append(args, containerName, "psql", "-X",
		"-h", dbConfig.Host,
		"-p", dbConfig.Port,
		"-U", dbConfig.Username,
		"-d", dbConfig.Database,
		"-t", // tuples only (no headers)
		"-A", // unaligned output
		"-c", query,
	)

	cmd := exec.CommandContext(ctx, c.DockerBin, args...)
	cmd.Env = dbexec.ClientEnv(dbConfig.Password, "")
	return runQuery(cmd)
}

// executeQueryFromHost runs psql on the host machine.
func (c *DBSizeChecker) executeQueryFromHost(ctx context.Context, dbConfig *DBConfig, query string) ([]byte, error) {
	args := []string{
		"-X",
		"-h", dbConfig.Host,
		"-p", dbConfig.Port,
		"-U", dbConfig.Username,
		"-d", dbConfig.Database,
		"-t", // tuples only (no headers)
		"-A", // unaligned output
		"-c", query,
	}

	cmd := exec.CommandContext(ctx, "psql", args...)
	cmd.Env = dbexec.ClientEnv(dbConfig.Password, "")
	return runQuery(cmd)
}

// runQuery runs a psql command and returns its output; on failure the error
// carries at most ~2 KB of what psql said.
func runQuery(cmd *exec.Cmd) ([]byte, error) {
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%w: %s", err, dbexec.TruncateOutput(output))
	}
	return output, nil
}
