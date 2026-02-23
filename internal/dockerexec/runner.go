package dockerexec

import (
	"context"
	"fmt"
	"os/exec"
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

	r.Logger.Printf("Successfully pulled image: %s", image)
	return nil
}

// Stop stops a running Docker container.
// Idempotent: returns no error if the container is not running.
func (r *Runner) Stop(ctx context.Context, container string) error {
	args := []string{"stop", container}
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if error is because container doesn't exist or isn't running
		outputStr := string(output)
		if strings.Contains(outputStr, "No such container") ||
			strings.Contains(outputStr, "is not running") ||
			strings.Contains(outputStr, "already stopped") {
			r.Logger.Printf("Container %s not running (idempotent operation)", container)
			return nil
		}
		return fmt.Errorf("docker stop failed: %w: %s", err, outputStr)
	}

	r.Logger.Printf("Successfully stopped container: %s", container)
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
			r.Logger.Printf("Container %s already running (idempotent operation)", container)
			return nil
		}
		return fmt.Errorf("docker start failed: %w: %s", err, outputStr)
	}

	r.Logger.Printf("Successfully started container: %s", container)
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

	r.Logger.Printf("Successfully restarted container: %s", container)
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
		if strings.Contains(outputStr, "No such container") {
			r.Logger.Printf("Container %s does not exist (idempotent operation)", container)
			return nil
		}
		return fmt.Errorf("docker rm failed: %w: %s", err, outputStr)
	}

	r.Logger.Printf("Successfully removed container: %s", container)
	return nil
}

// Run executes a docker command with the provided arguments.
func (r *Runner) Run(ctx context.Context, args []string) error {
	r.logCommand(args)

	cmd := exec.CommandContext(ctx, r.DockerBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker run failed: %w: %s", err, string(output))
	}

	r.Logger.Printf("Successfully executed docker command")
	return nil
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
		if strings.Contains(outputStr, "No such object") ||
			strings.Contains(outputStr, "No such container") {
			r.Logger.Printf("Container %s does not exist", container)
			return false, nil
		}
		return false, fmt.Errorf("docker inspect failed: %w: %s", err, outputStr)
	}

	outputStr := strings.TrimSpace(string(output))
	isRunning := outputStr == "true"

	r.Logger.Printf("Container %s running status: %v", container, isRunning)
	return isRunning, nil
}

// PrunePayramImages removes old Payram images for the given repo.
// It keeps the current tag and any tags used by running containers.
// Best-effort: returns error only if listing images or containers fails.
func (r *Runner) PrunePayramImages(ctx context.Context, imageRepo string, keepTag string) error {
	if strings.TrimSpace(imageRepo) == "" {
		return fmt.Errorf("image repo is required for pruning")
	}

	// Collect images used by running containers
	psArgs := []string{"ps", "--format", "{{.Image}}"}
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

	currentRef := fmt.Sprintf("%s:%s", imageRepo, keepTag)
	for _, ref := range strings.Split(strings.TrimSpace(string(listOutput)), "\n") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		// Skip current tag and any running images
		if ref == currentRef {
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
			r.Logger.Printf("Warning: failed to remove image %s: %v: %s", ref, rmiErr, strings.TrimSpace(string(rmiOutput)))
			continue
		}
		r.Logger.Printf("Removed old image: %s", ref)
	}

	return nil
}

// logCommand logs the docker command being executed.
func (r *Runner) logCommand(args []string) {
	r.Logger.Printf("Executing: %s %s", r.DockerBin, strings.Join(args, " "))
}
