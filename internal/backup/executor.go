package backup

import (
	"context"
	"os/exec"
)

// RealExecutor implements CommandExecutor using real system commands.
type RealExecutor struct{}

// Execute runs the given command with arguments and environment.
func (e *RealExecutor) Execute(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	return cmd.CombinedOutput()
}
