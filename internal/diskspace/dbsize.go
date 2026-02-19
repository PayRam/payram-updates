package diskspace

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
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

	return 0, fmt.Errorf("could not parse database size from output: %s", outputStr)
}

// executeQueryInContainer runs psql inside the container.
func (c *DBSizeChecker) executeQueryInContainer(ctx context.Context, containerName string, dbConfig *DBConfig, query string) ([]byte, error) {
	// Build psql command
	psqlCmd := fmt.Sprintf(
		"psql -h %s -p %s -U %s -d %s -t -A -c %q",
		dbConfig.Host, dbConfig.Port, dbConfig.Username, dbConfig.Database, query,
	)

	// Build docker exec command
	args := []string{"exec"}

	// Set password via environment if provided
	if dbConfig.Password != "" {
		args = append(args, "-e", fmt.Sprintf("PGPASSWORD=%s", dbConfig.Password))
	}

	args = append(args, containerName, "sh", "-c", psqlCmd)

	cmd := exec.CommandContext(ctx, c.DockerBin, args...)
	return cmd.CombinedOutput()
}

// executeQueryFromHost runs psql on the host machine.
func (c *DBSizeChecker) executeQueryFromHost(ctx context.Context, dbConfig *DBConfig, query string) ([]byte, error) {
	// Build psql command
	args := []string{
		"-h", dbConfig.Host,
		"-p", dbConfig.Port,
		"-U", dbConfig.Username,
		"-d", dbConfig.Database,
		"-t", // tuples only (no headers)
		"-A", // unaligned output
		"-c", query,
	}

	cmd := exec.CommandContext(ctx, "psql", args...)

	// Set password via environment if provided
	if dbConfig.Password != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("PGPASSWORD=%s", dbConfig.Password))
	}

	return cmd.CombinedOutput()
}
