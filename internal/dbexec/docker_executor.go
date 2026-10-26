package dbexec

import (
	"context"
	"fmt"
	"time"
)

// DockerPGExecutor runs PostgreSQL client tools inside the PayRam container
// (docker exec), for the database bundled in that container. No host
// PostgreSQL tools are used.
type DockerPGExecutor struct {
	// Runner runs docker; nil means OSRunner.
	Runner    Runner
	Logger    Logger
	DockerBin string // defaults to "docker"
}

// NewDockerPGExecutor creates a new DockerPGExecutor.
func NewDockerPGExecutor(runner Runner, logger Logger) *DockerPGExecutor {
	if logger == nil {
		logger = &noopLogger{}
	}
	if runner == nil {
		runner = OSRunner{}
	}
	return &DockerPGExecutor{Runner: runner, Logger: logger, DockerBin: "docker"}
}

func (e *DockerPGExecutor) checkTarget(db DBContext) error {
	if db.Mode != DBModeInContainer {
		return &DBError{
			Code:    ErrCodeInvalidConfig,
			Message: "DockerPGExecutor can only be used with in-container databases",
		}
	}
	if db.ContainerName == "" {
		return &DBError{
			Code:    ErrCodeContainerNotFound,
			Message: "container name is required for in-container database operations",
		}
	}
	return nil
}

// toolCommand builds `docker exec [-i] [-e PGPASSWORD] <container> <tool> ...`.
// The password travels in the docker client's environment, never in argv.
func (e *DockerPGExecutor) toolCommand(db DBContext, tool string, withStdin bool, args ...string) Command {
	argv := []string{"exec"}
	if withStdin {
		argv = append(argv, "-i")
	}
	if db.Creds.Password != "" {
		argv = append(argv, "-e", "PGPASSWORD")
	}
	argv = append(argv, db.ContainerName, tool)
	argv = append(argv, args...)
	return Command{Name: e.dockerBin(), Args: argv, Env: ClientEnv(db.Creds.Password, "")}
}

func (e *DockerPGExecutor) where(db DBContext) string {
	return fmt.Sprintf("inside container %s", db.ContainerName)
}

func (e *DockerPGExecutor) runner() Runner {
	if e.Runner == nil {
		return OSRunner{}
	}
	return e.Runner
}

func (e *DockerPGExecutor) logger() Logger {
	if e.Logger == nil {
		return &noopLogger{}
	}
	return e.Logger
}

func (e *DockerPGExecutor) dockerBin() string {
	if e.DockerBin == "" {
		return "docker"
	}
	return e.DockerBin
}

// Dump implements PGExecutor.
func (e *DockerPGExecutor) Dump(ctx context.Context, db DBContext, outFile string, format string) error {
	return dumpToFile(ctx, e, db, outFile, format)
}

// VerifyDumpFile implements PGExecutor.
func (e *DockerPGExecutor) VerifyDumpFile(ctx context.Context, db DBContext, file string, format string) error {
	if err := e.checkTarget(db); err != nil {
		return err
	}
	return verifyDumpFile(ctx, e, db, file, format)
}

// Restore implements PGExecutor.
func (e *DockerPGExecutor) Restore(ctx context.Context, db DBContext, inFile string, format string) error {
	return restoreFromFile(ctx, e, db, inFile, format)
}

// DescribeDatabase implements PGExecutor.
func (e *DockerPGExecutor) DescribeDatabase(ctx context.Context, db DBContext) (DatabaseSummary, error) {
	return describeDatabase(ctx, e, db)
}

// CheckTools implements PGExecutor.
func (e *DockerPGExecutor) CheckTools(ctx context.Context, db DBContext) error {
	return checkTools(ctx, e, db)
}

// Query implements PGExecutor.
func (e *DockerPGExecutor) Query(ctx context.Context, db DBContext, queries ...string) (string, error) {
	return readOnlyQuery(ctx, e, db, queries...)
}

// Ping implements PGExecutor.
func (e *DockerPGExecutor) Ping(ctx context.Context, db DBContext) error {
	return ping(ctx, e, db)
}

// WaitReady implements PGExecutor.
func (e *DockerPGExecutor) WaitReady(ctx context.Context, db DBContext, timeout time.Duration) error {
	return waitReady(ctx, e, db, timeout)
}
