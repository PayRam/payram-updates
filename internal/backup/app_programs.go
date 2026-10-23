package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
)

// supervisorStates are the states supervisorctl status prints.
var supervisorStates = map[string]bool{
	"RUNNING": true, "STARTING": true, "STOPPED": true, "STOPPING": true,
	"BACKOFF": true, "EXITED": true, "FATAL": true, "UNKNOWN": true,
}

// programsStartTimeout bounds starting PayRam's programs again. It does not
// use the restore's context, which may already be cancelled.
const programsStartTimeout = 2 * time.Minute

// stopAppPrograms stops PayRam's own programs inside container (everything
// supervisord runs except PostgreSQL and the configured exclusions) so
// nothing writes to the database while it is replaced. Only RUNNING and
// STARTING programs are stopped. The returned function starts exactly those
// again; call it once, always, whatever happened in between.
//
// When the container is unknown or has no supervisorctl, the restore goes
// on with a warning: PostgreSQL's locks then keep the replacement consistent,
// but a writer may see errors while it runs.
func (m *Manager) stopAppPrograms(ctx context.Context, container string) (func() error, error) {
	nothing := func() error { return nil }
	if container == "" {
		m.Logger.Printf("Warning: the PayRam container is not known, so its programs were not stopped; make sure nothing writes to the database during the restore")
		return nothing, nil
	}

	status, err := m.runSupervisorctl(ctx, container, "status")
	programs, parsed := parseSupervisorStatus(status)
	if err != nil && !parsed {
		// supervisorctl exits 3 when some program is not RUNNING; that output
		// still parses. Anything else means it is not usable here.
		m.Logger.Printf("Warning: supervisorctl is not available in %s (%s); PayRam's programs were not stopped for the restore", container, dbexec.TruncateOutput([]byte(err.Error())))
		return nothing, nil
	}

	var toStop []string
	for name, state := range programs {
		if m.keepsRunning(name) {
			continue
		}
		if state == "RUNNING" || state == "STARTING" {
			toStop = append(toStop, name)
		}
	}
	sort.Strings(toStop)
	if len(toStop) == 0 {
		m.Logger.Printf("No PayRam programs are running in %s; nothing to stop", container)
		return nothing, nil
	}

	startAgain := func() error {
		startCtx, cancel := context.WithTimeout(context.Background(), programsStartTimeout)
		defer cancel()
		m.Logger.Printf("Starting PayRam's programs again in %s: %s", container, strings.Join(toStop, ", "))
		if _, err := m.runSupervisorctl(startCtx, container, append([]string{"start"}, toStop...)...); err != nil {
			return fmt.Errorf("PayRam's programs (%s) could not be started again: %v. Start them with: docker exec %s supervisorctl start %s",
				strings.Join(toStop, ", "), err, container, strings.Join(toStop, " "))
		}
		return nil
	}

	m.Logger.Printf("Stopping PayRam's programs in %s so nothing writes to the database: %s", container, strings.Join(toStop, ", "))
	if _, err := m.runSupervisorctl(ctx, container, append([]string{"stop"}, toStop...)...); err != nil {
		if startErr := startAgain(); startErr != nil {
			m.Logger.Printf("Warning: %v", startErr)
		}
		return nil, fmt.Errorf("could not stop PayRam's programs in %s before the restore, so nothing was changed: %v", container, err)
	}
	return startAgain, nil
}

// keepsRunning reports whether a supervisord program stays up during a
// restore: PostgreSQL itself, and anything in Config.SupervisorExclude.
func (m *Manager) keepsRunning(program string) bool {
	if strings.Contains(strings.ToLower(program), "postgres") {
		return true
	}
	for _, name := range m.Config.SupervisorExclude {
		if name == program {
			return true
		}
	}
	return false
}

// runSupervisorctl runs supervisorctl inside the container and returns its
// output (stdout and stderr together).
func (m *Manager) runSupervisorctl(ctx context.Context, container string, args ...string) (string, error) {
	cmd := dbexec.Command{
		Name: m.Config.DockerBin,
		Args: append([]string{"exec", container, "supervisorctl"}, args...),
	}
	if cmd.Name == "" {
		cmd.Name = "docker"
	}
	var out strings.Builder
	stderr, err := m.runner().Run(ctx, cmd, nil, &out)
	output := out.String() + string(stderr)
	if err != nil {
		return output, fmt.Errorf("%v: %s", err, dbexec.TruncateOutput([]byte(output)))
	}
	return output, nil
}

// parseSupervisorStatus reads `supervisorctl status` lines ("name STATE
// ..."). parsed is false when no line looks like a program status.
func parseSupervisorStatus(output string) (programs map[string]string, parsed bool) {
	programs = make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !supervisorStates[fields[1]] {
			continue
		}
		programs[fields[0]] = fields[1]
	}
	return programs, len(programs) > 0
}
