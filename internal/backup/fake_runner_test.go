package backup

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/payram/payram-updater/internal/dbexec"
)

// fakeRunner is a dbexec.Runner that records every command and answers with
// handle. It reads stdin to the end before calling handle.
type fakeRunner struct {
	mu     sync.Mutex
	calls  []fakeCall
	handle func(cmd dbexec.Command, stdin []byte, stdout io.Writer) (stderr []byte, err error)
}

type fakeCall struct {
	Cmd   dbexec.Command
	Stdin []byte
}

func (f *fakeRunner) Run(ctx context.Context, cmd dbexec.Command, stdin io.Reader, stdout io.Writer) ([]byte, error) {
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Cmd: cmd, Stdin: in})
	f.mu.Unlock()
	if stdout == nil {
		stdout = io.Discard
	}
	if f.handle == nil {
		return nil, nil
	}
	return f.handle(cmd, in, stdout)
}

// steps lists the calls as short labels ("supervisorctl stop", "pg_dump",
// "psql restore", ...) in the order they ran.
func (f *fakeRunner) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, stepOf(c.Cmd))
	}
	return out
}

func (f *fakeRunner) callsOf(step string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if stepOf(c.Cmd) == step {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeRunner) assertNoSecretInArgs(t *testing.T, secret string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		for _, a := range c.Cmd.Args {
			if strings.Contains(a, secret) {
				t.Fatalf("password visible in argv: %s %v", c.Cmd.Name, c.Cmd.Args)
			}
		}
	}
}

func cmdHasArg(cmd dbexec.Command, arg string) bool {
	for _, a := range cmd.Args {
		if a == arg {
			return true
		}
	}
	return false
}

func cmdHasEnv(cmd dbexec.Command, kv string) bool {
	for _, e := range cmd.Env {
		if e == kv {
			return true
		}
	}
	return false
}

// stepOf labels a command for assertions.
func stepOf(cmd dbexec.Command) string {
	args := strings.Join(cmd.Args, " ")
	tool := filepath.Base(cmd.Name)
	for _, t := range []string{"supervisorctl", "pg_dump", "pg_restore", "psql", "pg_isready"} {
		if cmdHasArg(cmd, t) {
			tool = t
		}
	}
	if cmdHasArg(cmd, "--version") {
		return tool + " --version"
	}
	switch tool {
	case "supervisorctl":
		for _, verb := range []string{"status", "stop", "start"} {
			if cmdHasArg(cmd, verb) {
				return "supervisorctl " + verb
			}
		}
	case "pg_restore":
		if cmdHasArg(cmd, "--list") {
			return "pg_restore --list"
		}
		return "pg_restore -f -"
	case "psql":
		if cmdHasArg(cmd, "-f") {
			return "psql restore"
		}
		if strings.Contains(args, "SELECT 1") && !strings.Contains(args, "pg_tables") {
			return "psql ping"
		}
		return "psql query"
	}
	return tool
}

// postgresTools answers pg_dump, pg_restore and psql the way a healthy
// database would; override picks off steps first (return handled=false to
// fall through).
func postgresTools(override func(step string, cmd dbexec.Command, stdin []byte, stdout io.Writer) (handled bool, stderr []byte, err error)) func(dbexec.Command, []byte, io.Writer) ([]byte, error) {
	return func(cmd dbexec.Command, stdin []byte, stdout io.Writer) ([]byte, error) {
		step := stepOf(cmd)
		if override != nil {
			if handled, stderr, err := override(step, cmd, stdin, stdout); handled {
				return stderr, err
			}
		}
		switch step {
		case "supervisorctl status":
			io.WriteString(stdout, "payram-core                      RUNNING   pid 10, uptime 1:00:00\n"+
				"payram-eth-listener              RUNNING   pid 11, uptime 1:00:00\n"+
				"postgres                         RUNNING   pid 9, uptime 1:00:00\n"+
				"payram-webhook                   STOPPED   Not started\n")
		case "pg_dump":
			io.WriteString(stdout, "PGDMP-current-database")
		case "pg_restore --list":
			io.WriteString(stdout, ";\n218; 1259 16385 TABLE public payments payram\n")
		case "pg_restore -f -":
			io.WriteString(stdout, "CREATE TABLE public.payments (id bigint);\n")
		case "psql ping":
			io.WriteString(stdout, "1\n")
		case "psql query":
			if strings.Contains(strings.Join(cmd.Args, " "), "FROM public.migrations") {
				io.WriteString(stdout, "12\n")
			} else {
				io.WriteString(stdout, "7\nt\n")
			}
		}
		return nil, nil
	}
}
