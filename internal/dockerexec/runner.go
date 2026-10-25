package dockerexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Logger defines the interface for logging.
type Logger interface {
	Printf(format string, v ...interface{})
}

// Runner executes Docker commands.
type Runner struct {
	DockerBin string
	Logger    Logger
	// StopTimeoutSeconds is how long `docker stop` waits before killing the
	// container. Zero keeps Docker's default (10s), which is too short for the
	// bundled Postgres to shut down cleanly.
	StopTimeoutSeconds int
}

// ContainerState is what an update needs to know about one container.
type ContainerState struct {
	Exists       bool
	Running      bool
	Status       string // created, running, restarting, exited, ...
	RestartCount int
	Image        string // image reference the container was created from
	ExitCode     int
}

// Pull pulls a Docker image.
func (r *Runner) Pull(ctx context.Context, image string) error {
	args := []string{"pull", image}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker pull failed: %w: %s", err, string(output))
	}

	r.logf("Successfully pulled image: %s", image)
	return nil
}

// Stop stops a running Docker container.
// Idempotent: returns no error if the container is not running.
func (r *Runner) Stop(ctx context.Context, container string) error {
	args := []string{"stop"}
	if r.StopTimeoutSeconds > 0 {
		args = append(args, "-t", fmt.Sprintf("%d", r.StopTimeoutSeconds))
	}
	args = append(args, container)
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if error is because container doesn't exist or isn't running
		outputStr := string(output)
		if IsNotFound(outputStr) ||
			strings.Contains(outputStr, "is not running") ||
			strings.Contains(outputStr, "already stopped") {
			r.logf("Container %s not running (idempotent operation)", container)
			return nil
		}
		return fmt.Errorf("docker stop failed: %w: %s", err, outputStr)
	}

	r.logf("Successfully stopped container: %s", container)
	return nil
}

// Start starts a stopped Docker container.
// Idempotent: returns no error if the container is already running.
func (r *Runner) Start(ctx context.Context, container string) error {
	args := []string{"start", container}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		if strings.Contains(outputStr, "is already running") {
			r.logf("Container %s already running (idempotent operation)", container)
			return nil
		}
		return fmt.Errorf("docker start failed: %w: %s", err, outputStr)
	}

	r.logf("Successfully started container: %s", container)
	return nil
}

// Restart restarts a Docker container.
func (r *Runner) Restart(ctx context.Context, container string) error {
	args := []string{"restart", container}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker restart failed: %w: %s", err, strings.TrimSpace(string(output)))
	}

	r.logf("Successfully restarted container: %s", container)
	return nil
}

// Remove removes a Docker container.
// Idempotent: returns no error if the container does not exist.
func (r *Runner) Remove(ctx context.Context, container string) error {
	args := []string{"rm", "-f", container}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if error is because container doesn't exist
		outputStr := string(output)
		if IsNotFound(outputStr) {
			r.logf("Container %s does not exist (idempotent operation)", container)
			return nil
		}
		return fmt.Errorf("docker rm failed: %w: %s", err, outputStr)
	}

	r.logf("Successfully removed container: %s", container)
	return nil
}

// Run executes a docker command with the provided arguments.
func (r *Runner) Run(ctx context.Context, args []string) error {
	return r.RunWithEnv(ctx, args, nil)
}

// RunWithEnv executes a docker command with env (KEY=VALUE) added to the
// updater's own environment. A container's secrets go this way, named on the
// command line only (`-e AES_KEY`): docker reads each value from its own
// environment, which, unlike its command line, other users cannot read.
func (r *Runner) RunWithEnv(ctx context.Context, args []string, env []string) error {
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		subcommand := "run"
		if len(args) > 0 {
			subcommand = args[0]
		}
		return fmt.Errorf("docker %s failed: %w: %s", subcommand, err, strings.TrimSpace(string(output)))
	}

	r.logf("Successfully executed docker command")
	return nil
}

// Rename renames a container.
func (r *Runner) Rename(ctx context.Context, from, to string) error {
	args := []string{"rename", from, to}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker rename %s %s failed: %w: %s", from, to, err, strings.TrimSpace(string(output)))
	}
	r.logf("Renamed container %s to %s", from, to)
	return nil
}

// UpdateRestartPolicy changes a container's restart policy in place.
func (r *Runner) UpdateRestartPolicy(ctx context.Context, container, policy string) error {
	args := []string{"update", "--restart", policy, container}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker update --restart %s %s failed: %w: %s", policy, container, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// ConnectNetwork attaches a container to an additional network.
func (r *Runner) ConnectNetwork(ctx context.Context, network, container string) error {
	args := []string{"network", "connect", network, container}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker network connect %s %s failed: %w: %s", network, container, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// State reports whether a container exists and how it is doing. A missing
// container is not an error: State returns Exists=false.
func (r *Runner) State(ctx context.Context, container string) (ContainerState, error) {
	args := []string{"inspect", "-f", "{{.State.Running}}|{{.State.Status}}|{{.RestartCount}}|{{.Config.Image}}|{{.State.ExitCode}}", container}
	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		if IsNotFound(outputStr) {
			return ContainerState{}, nil
		}
		return ContainerState{}, fmt.Errorf("docker inspect %s failed: %w: %s", container, err, strings.TrimSpace(outputStr))
	}
	return parseContainerState(strings.TrimSpace(string(output)))
}

func parseContainerState(line string) (ContainerState, error) {
	fields := strings.Split(line, "|")
	if len(fields) != 5 {
		return ContainerState{}, fmt.Errorf("unexpected docker inspect output: %q", line)
	}
	state := ContainerState{
		Exists:  true,
		Running: fields[0] == "true",
		Status:  fields[1],
		Image:   fields[3],
	}
	fmt.Sscanf(fields[2], "%d", &state.RestartCount)
	fmt.Sscanf(fields[4], "%d", &state.ExitCode)
	return state, nil
}

// InspectRunning checks if a container is currently running.
// Returns true if running, false if not running or doesn't exist.
func (r *Runner) InspectRunning(ctx context.Context, container string) (bool, error) {
	args := []string{"inspect", "-f", "{{.State.Running}}", container}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Container doesn't exist
		outputStr := string(output)
		if IsNotFound(outputStr) {
			r.logf("Container %s does not exist", container)
			return false, nil
		}
		return false, fmt.Errorf("docker inspect failed: %w: %s", err, outputStr)
	}

	outputStr := strings.TrimSpace(string(output))
	isRunning := outputStr == "true"

	r.logf("Container %s running status: %v", container, isRunning)
	return isRunning, nil
}

// PrunePayramImages removes old Payram images for the given repo.
// It keeps every tag in keepTags (the running version and the one an update
// would switch back to) and any image used by an existing container.
// Best-effort: returns error only if listing images or containers fails.
func (r *Runner) PrunePayramImages(ctx context.Context, imageRepo string, keepTags ...string) error {
	if strings.TrimSpace(imageRepo) == "" {
		return fmt.Errorf("image repo is required for pruning")
	}

	// Collect images used by any container, stopped ones included
	psArgs := []string{"ps", "-a", "--format", "{{.Image}}"}
	r.logCommand(psArgs)
	psCmd := exec.CommandContext(ctx, r.DockerBin, psArgs...)
	psOutput, err := psCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker ps failed: %w: %s", err, string(psOutput))
	}
	runningImages := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(string(psOutput)), "\n") {
		if line == "" {
			continue
		}
		runningImages[strings.TrimSpace(line)] = struct{}{}
	}

	// List all images for the repo
	listArgs := []string{"images", "--format", "{{.Repository}}:{{.Tag}}", "--filter", fmt.Sprintf("reference=%s:*", imageRepo)}
	r.logCommand(listArgs)
	listCmd := exec.CommandContext(ctx, r.DockerBin, listArgs...)
	listOutput, err := listCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker images failed: %w: %s", err, string(listOutput))
	}

	keepRefs := map[string]struct{}{}
	for _, tag := range keepTags {
		if tag != "" {
			keepRefs[fmt.Sprintf("%s:%s", imageRepo, tag)] = struct{}{}
		}
	}
	for _, ref := range strings.Split(strings.TrimSpace(string(listOutput)), "\n") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		// Skip kept tags and any image a container still uses
		if _, ok := keepRefs[ref]; ok {
			continue
		}
		if _, ok := runningImages[ref]; ok {
			continue
		}
		// Skip invalid tags
		if strings.HasSuffix(ref, ":<none>") {
			continue
		}

		rmiArgs := []string{"rmi", ref}
		r.logCommand(rmiArgs)
		rmiCmd := exec.CommandContext(ctx, r.DockerBin, rmiArgs...)
		rmiOutput, rmiErr := rmiCmd.CombinedOutput()
		if rmiErr != nil {
			r.logf("Warning: failed to remove image %s: %v: %s", ref, rmiErr, strings.TrimSpace(string(rmiOutput)))
			continue
		}
		r.logf("Removed old image: %s", ref)
	}

	return nil
}

// IsNotFound reports whether docker's output says the container does not
// exist. Docker's wording differs by version and command ("Error response
// from daemon: No such container: x", "error: no such object: x"), so the
// match ignores case.
func IsNotFound(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such container") || strings.Contains(lower, "no such object")
}

// logf logs a formatted message if a logger is available.
func (r *Runner) logf(format string, args ...interface{}) {
	if r.Logger != nil {
		r.Logger.Printf(format, args...)
	}
}

// logCommand logs the docker command being executed, with secret values hidden.
func (r *Runner) logCommand(args []string) {
	if r.Logger != nil {
		r.Logger.Printf("Executing: %s %s", r.DockerBin, strings.Join(RedactArgs(args), " "))
	}
}

// inlinePassword matches a password passed inside a shell string, such as
// `PGPASSWORD=secret pg_dump ...`.
var inlinePassword = regexp.MustCompile(`(?i)\b(PGPASSWORD|PASSWORD)=\S+`)

// RedactArgs returns a copy of docker arguments that is safe to log: every
// environment value (`-e KEY=VALUE`, `--env KEY=VALUE`, `--env=KEY=VALUE`) and
// every inline PGPASSWORD=... becomes `***`. The container's environment holds
// AES_KEY (the hot-wallet encryption key) and the database password, so a
// docker command line must never reach a log, the journal or the dashboard as is.
func RedactArgs(args []string) []string {
	redacted := make([]string, len(args))
	for i, arg := range args {
		switch {
		case i > 0 && (args[i-1] == "-e" || args[i-1] == "--env"):
			redacted[i] = redactEnvValue(arg)
		case strings.HasPrefix(arg, "--env="):
			redacted[i] = "--env=" + redactEnvValue(strings.TrimPrefix(arg, "--env="))
		default:
			redacted[i] = RedactLine(arg)
		}
	}
	return redacted
}

// loggedEnvValue matches an environment value on a docker command line that
// was logged as one string (`-e KEY=VALUE`, `--env KEY=VALUE`, `--env=KEY=VALUE`).
var loggedEnvValue = regexp.MustCompile(`((?:^|\s)(?:-e|--env)(?:\s+|=)[A-Za-z_][A-Za-z0-9_]*=)\S*`)

// secretAssignment matches KEY=VALUE for keys that name a secret.
var secretAssignment = regexp.MustCompile(`\b([A-Za-z0-9_]*(?:AES_KEY|PASSWORD|SECRET|TOKEN)[A-Za-z0-9_]*)=\S+`)

// RedactLine hides the secret values a log line may carry: every docker
// environment value, inline passwords and any KEY=VALUE whose key names a
// secret. Updaters up to v0.2.3 logged whole docker command lines, AES_KEY
// included; this cleans what they left in the job log.
func RedactLine(line string) string {
	line = loggedEnvValue.ReplaceAllString(line, "${1}***")
	line = secretAssignment.ReplaceAllString(line, "$1=***")
	return inlinePassword.ReplaceAllString(line, "$1=***")
}

// RedactLines applies RedactLine to every line of text.
func RedactLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = RedactLine(line)
	}
	return strings.Join(lines, "\n")
}

// redactEnvValue turns KEY=VALUE into KEY=***. A bare KEY (docker copies the
// value from its own environment) carries no secret and is kept.
func redactEnvValue(env string) string {
	if key, _, ok := strings.Cut(env, "="); ok {
		return key + "=***"
	}
	return env
}
