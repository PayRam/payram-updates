package switchover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/container"
	"github.com/payram/payram-updater/internal/coreclient"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
)

const (
	oldImage = "payramapp/payram:3.9.0"
	newImage = "payramapp/payram:3.10.0"
)

// fakeContainer is one container in fakeDocker.
type fakeContainer struct {
	image    string
	running  bool
	status   string
	restart  string
	restarts int
}

// fakeDocker keeps containers in a map and applies the same rules Docker
// does for the calls a switch makes (a name can't be taken twice, a missing
// container can't be started, ...). fail injects errors: "op:name" -> how
// many calls fail.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	fail       map[string]int
	// exitOnStart makes a container with this image stop right after start.
	exitOnStart string
	calls       []string
	// createEnv is the environment each create got, by container name.
	createEnv map[string][]string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containers: map[string]*fakeContainer{
			"payram": {image: oldImage, running: true, status: "running", restart: "unless-stopped"},
		},
		fail:      map[string]int{},
		createEnv: map[string][]string{},
	}
}

func (d *fakeDocker) failing(op, name string) error {
	key := op + ":" + name
	if d.fail[key] > 0 {
		d.fail[key]--
		return fmt.Errorf("injected %s failure", key)
	}
	return nil
}

func (d *fakeDocker) RunWithEnv(_ context.Context, args []string, env []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(args) < 3 || args[0] != "create" || args[1] != "--name" {
		return fmt.Errorf("fake docker only creates: %v", args)
	}
	for i, arg := range args {
		if i > 0 && args[i-1] == "-e" && strings.Contains(arg, "=") {
			return fmt.Errorf("an environment value is on the docker command line, where every user can read it: %s", arg)
		}
	}
	name := args[2]
	d.calls = append(d.calls, "create "+name)
	if err := d.failing("create", name); err != nil {
		return err
	}
	if _, ok := d.containers[name]; ok {
		return fmt.Errorf("Conflict. The container name %q is already in use", name)
	}
	restart := "no"
	for i, arg := range args {
		if arg == "--restart" && i+1 < len(args) {
			restart = args[i+1]
		}
	}
	d.containers[name] = &fakeContainer{image: args[len(args)-1], status: "created", restart: restart}
	d.createEnv[name] = env
	return nil
}

func (d *fakeDocker) Start(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "start "+name)
	c, ok := d.containers[name]
	if !ok {
		return fmt.Errorf("No such container: %s", name)
	}
	if err := d.failing("start", name); err != nil {
		return err
	}
	for other, o := range d.containers {
		if other != name && o.running {
			return fmt.Errorf("Bind for 0.0.0.0:443 failed: port is already allocated (%s holds it)", other)
		}
	}
	c.running, c.status = true, "running"
	if d.exitOnStart != "" && c.image == d.exitOnStart {
		c.running, c.status = false, "exited"
	}
	return nil
}

func (d *fakeDocker) Stop(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "stop "+name)
	if err := d.failing("stop", name); err != nil {
		return err
	}
	if c, ok := d.containers[name]; ok {
		c.running, c.status = false, "exited"
	}
	return nil
}

func (d *fakeDocker) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "rm "+name)
	if err := d.failing("rm", name); err != nil {
		return err
	}
	delete(d.containers, name)
	return nil
}

func (d *fakeDocker) Rename(_ context.Context, from, to string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "rename "+from+" "+to)
	if err := d.failing("rename", from); err != nil {
		return err
	}
	c, ok := d.containers[from]
	if !ok {
		return fmt.Errorf("No such container: %s", from)
	}
	if _, taken := d.containers[to]; taken {
		return fmt.Errorf("Conflict. The container name %q is already in use", to)
	}
	delete(d.containers, from)
	d.containers[to] = c
	return nil
}

func (d *fakeDocker) UpdateRestartPolicy(_ context.Context, name, policy string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "update "+name+" "+policy)
	if err := d.failing("update", name); err != nil {
		return err
	}
	if err := d.failing("update", name+":"+policy); err != nil {
		return err
	}
	c, ok := d.containers[name]
	if !ok {
		return fmt.Errorf("No such container: %s", name)
	}
	c.restart = policy
	return nil
}

func (d *fakeDocker) ConnectNetwork(_ context.Context, network, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.failing("connect", name)
}

func (d *fakeDocker) State(_ context.Context, name string) (dockerexec.ContainerState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers[name]
	if !ok {
		return dockerexec.ContainerState{}, nil
	}
	return dockerexec.ContainerState{Exists: true, Running: c.running, Status: c.status, RestartCount: c.restarts, Image: c.image}, nil
}

// fakeProbe answers like the PayRam that currently runs as "payram" would:
// healthy if its image is in healthy, and reporting the image's tag.
type fakeProbe struct {
	docker  *fakeDocker
	healthy map[string]bool
	version map[string]string // image -> version it reports (default: its tag)
}

func (p *fakeProbe) running() (*fakeContainer, bool) {
	p.docker.mu.Lock()
	defer p.docker.mu.Unlock()
	c, ok := p.docker.containers["payram"]
	return c, ok && c.running
}

func (p *fakeProbe) Health(context.Context) (*coreclient.HealthResponse, error) {
	c, ok := p.running()
	if !ok {
		return nil, errors.New("connection refused")
	}
	if !p.healthy[c.image] {
		return &coreclient.HealthResponse{Status: "degraded", DB: "error"}, nil
	}
	return &coreclient.HealthResponse{Status: "ok", DB: "ok"}, nil
}

func (p *fakeProbe) Version(context.Context) (string, error) {
	c, ok := p.running()
	if !ok {
		return "", errors.New("connection refused")
	}
	if v, ok := p.version[c.image]; ok {
		return v, nil
	}
	return c.image[strings.LastIndex(c.image, ":")+1:], nil
}

type memoryRecorder struct {
	mu    sync.Mutex
	saved []jobs.Job
	lines []string
	fail  bool
}

func (r *memoryRecorder) Save(job *jobs.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return errors.New("disk full")
	}
	r.saved = append(r.saved, *job)
	return nil
}

func (r *memoryRecorder) Log(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, line)
}

func newSwitch(d *fakeDocker) (*Switch, *memoryRecorder, *[]string) {
	rec := &memoryRecorder{}
	var started []string
	w := &Switch{
		Docker: d,
		Record: rec,
		Timing: Timing{HealthTimeout: 400 * time.Millisecond, StableFor: 60 * time.Millisecond, PollInterval: 10 * time.Millisecond, MaxRestarts: 3},
		StartPrograms: func(_ context.Context, container string, programs []string) error {
			started = append(started, container+":"+strings.Join(programs, ","))
			return nil
		},
	}
	return w, rec, &started
}

func newJob() *jobs.Job {
	job := jobs.NewJob("job-1", jobs.JobModeDashboard, "3.10.0")
	job.ContainerName = "payram"
	return job
}

// newSpec is the new version's container as the builder describes it: the
// running container's settings, the AES key named in Options and valued in Env.
func newSpec() *container.ContainerSpec {
	return &container.ContainerSpec{
		Options:       []string{"-p", "443:443/tcp", "-e", "AES_KEY", newImage},
		Env:           []string{"AES_KEY=hot-wallet-key"},
		RestartPolicy: "unless-stopped",
		ExtraNetworks: []string{"monitoring"},
		Image:         newImage,
	}
}

// assertPayramRuns checks the promise every path must keep: exactly one
// container named payram, running image, with restart policy, and no other
// container running.
func assertPayramRuns(t *testing.T, d *fakeDocker, image, restart string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.containers["payram"]
	if !ok {
		t.Fatalf("no container named payram; containers: %v", d.describe())
	}
	if !c.running || c.image != image || c.restart != restart {
		t.Fatalf("payram = %+v, want running %s with restart %s; containers: %v", *c, image, restart, d.describe())
	}
	for name, other := range d.containers {
		if name != "payram" && other.running {
			t.Fatalf("%s is running next to payram: %v", name, d.describe())
		}
		if name != "payram" && other.restart != "no" && other.status != "created" {
			t.Fatalf("%s could restart on its own (restart %s): %v", name, other.restart, d.describe())
		}
	}
	if _, ok := d.containers["payram-next"]; ok {
		t.Fatalf("payram-next left behind: %v", d.describe())
	}
}

func (d *fakeDocker) describe() string {
	var parts []string
	for name, c := range d.containers {
		parts = append(parts, fmt.Sprintf("%s{%s running=%v restart=%s}", name, c.image, c.running, c.restart))
	}
	return strings.Join(parts, " ")
}

// runSwitch drives a whole switch the way the engine does: create, cutover,
// verify, then finish or restore.
func runSwitch(t *testing.T, w *Switch, d *fakeDocker, probe Prober, job *jobs.Job) error {
	t.Helper()
	ctx := context.Background()
	if err := w.CreateNext(ctx, job, newSpec()); err != nil {
		return err
	}
	err := w.Cutover(ctx, job, oldImage, "unless-stopped")
	if err == nil {
		_, err = w.WaitHealthy(ctx, job.ContainerName, probe, "3.10.0")
	}
	if err != nil {
		if restoreErr := w.Restore(ctx, job); restoreErr != nil {
			t.Fatalf("restore failed: %v", restoreErr)
		}
		return err
	}
	w.Finish(ctx, job)
	return nil
}

func TestSwitch_HealthyNewVersionReplacesTheOld(t *testing.T) {
	d := newFakeDocker()
	w, _, _ := newSwitch(d)
	probe := &fakeProbe{docker: d, healthy: map[string]bool{oldImage: true, newImage: true}}
	job := newJob()

	if err := runSwitch(t, w, d, probe, job); err != nil {
		t.Fatalf("switch failed: %v", err)
	}
	assertPayramRuns(t, d, newImage, "unless-stopped")
	if _, ok := d.containers["payram-previous"]; ok {
		t.Fatal("the old container must be removed after a verified switch")
	}
	if job.SwitchStage != jobs.SwitchStageVerified {
		t.Fatalf("stage = %s, want VERIFIED", job.SwitchStage)
	}
}

func TestSwitch_FailuresAlwaysEndWithTheOldVersionRunning(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(d *fakeDocker, p *fakeProbe)
		wantCode string
		changed  bool
	}{
		{"create fails", func(d *fakeDocker, p *fakeProbe) { d.fail["create:payram-next"] = 1 }, jobs.FailureDockerCreateFailed, false},
		{"network connect fails", func(d *fakeDocker, p *fakeProbe) { d.fail["connect:payram-next"] = 1 }, jobs.FailureDockerCreateFailed, false},
		{"stop fails", func(d *fakeDocker, p *fakeProbe) { d.fail["stop:payram"] = 1 }, "DOCKER_ERROR", false},
		{"switching the old container's restart policy off fails", func(d *fakeDocker, p *fakeProbe) { d.fail["update:payram:no"] = 1 }, "DOCKER_ERROR", false},
		{"giving the new container its restart policy fails", func(d *fakeDocker, p *fakeProbe) { d.fail["update:payram:unless-stopped"] = 1 }, "DOCKER_ERROR", true},
		{"setting the old container aside fails", func(d *fakeDocker, p *fakeProbe) { d.fail["rename:payram"] = 1 }, "DOCKER_ERROR", true},
		{"putting the new container in place fails", func(d *fakeDocker, p *fakeProbe) { d.fail["rename:payram-next"] = 1 }, "DOCKER_ERROR", true},
		{"new container does not start", func(d *fakeDocker, p *fakeProbe) { d.fail["start:payram"] = 1 }, "DOCKER_ERROR", true},
		{"new version never healthy", func(d *fakeDocker, p *fakeProbe) { p.healthy[newImage] = false }, "HEALTHCHECK_FAILED", true},
		{"new container exits", func(d *fakeDocker, p *fakeProbe) { d.exitOnStart = newImage }, "DOCKER_ERROR", true},
		{"new version reports another version", func(d *fakeDocker, p *fakeProbe) { p.version = map[string]string{newImage: "3.9.9"} }, "VERSION_MISMATCH", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDocker()
			w, _, _ := newSwitch(d)
			probe := &fakeProbe{docker: d, healthy: map[string]bool{oldImage: true, newImage: true}}
			tc.setup(d, probe)
			job := newJob()

			err := runSwitch(t, w, d, probe, job)
			var stepErr *StepError
			if !errors.As(err, &stepErr) {
				t.Fatalf("got %v, want a StepError", err)
			}
			if stepErr.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s (%s)", stepErr.Code, tc.wantCode, stepErr.Message)
			}
			// Health and version failures come from WaitHealthy, which runs
			// only after the cutover; Changed is reported by create/cutover.
			fromCutover := tc.wantCode != "HEALTHCHECK_FAILED" && tc.wantCode != "VERSION_MISMATCH" && d.exitOnStart == ""
			if fromCutover && stepErr.Changed != tc.changed {
				t.Fatalf("changed = %v, want %v", stepErr.Changed, tc.changed)
			}
			assertPayramRuns(t, d, oldImage, "unless-stopped")
		})
	}
}

// TestSwitch_NewContainerCannotStartByItselfBeforeTheSwitch: until Cutover
// starts it, the new container must not start on its own (a Docker or server
// restart during the backup would otherwise run it next to the old one), and
// its secrets reach docker through the environment only.
func TestSwitch_NewContainerCannotStartByItselfBeforeTheSwitch(t *testing.T) {
	d := newFakeDocker()
	w, _, _ := newSwitch(d)
	job := newJob()
	if err := w.CreateNext(context.Background(), job, newSpec()); err != nil {
		t.Fatalf("CreateNext: %v", err)
	}
	next := d.containers["payram-next"]
	if next == nil || next.restart != "no" {
		t.Fatalf("payram-next = %+v, want restart policy no until the switch", next)
	}
	if env := d.createEnv["payram-next"]; len(env) != 1 || env[0] != "AES_KEY=hot-wallet-key" {
		t.Fatalf("create env = %v, want the AES key passed through the environment", env)
	}

	if err := w.Cutover(context.Background(), job, oldImage, "unless-stopped"); err != nil {
		t.Fatalf("Cutover: %v", err)
	}
	if got := d.containers["payram"].restart; got != "unless-stopped" {
		t.Fatalf("new payram restart = %s, want the old container's unless-stopped", got)
	}
	if got := d.containers["payram-previous"].restart; got != "no" {
		t.Fatalf("payram-previous restart = %s, want no", got)
	}
	// The old container's policy goes off before it is stopped, so a Docker
	// restart between the two cannot start it again.
	stop, off := -1, -1
	for i, call := range d.calls {
		switch call {
		case "stop payram":
			stop = i
		case "update payram no":
			if off < 0 {
				off = i
			}
		}
	}
	if off < 0 || stop < 0 || off > stop {
		t.Fatalf("restart policy must be switched off before the stop; calls: %v", d.calls)
	}
}

// TestRestore_StartsPayramEvenIfItsRestartPolicyCannotBeSetBack: putting the
// restart policy back is retried after the start; failing at it must never
// keep PayRam down.
func TestRestore_StartsPayramEvenIfItsRestartPolicyCannotBeSetBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failures    int
		wantRestart string
	}{
		{"fails once, retried after the start", 1, "unless-stopped"},
		{"keeps failing", 2, "no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDocker()
			w, rec, _ := newSwitch(d)
			ctx := context.Background()
			job := newJob()
			if err := w.CreateNext(ctx, job, newSpec()); err != nil {
				t.Fatal(err)
			}
			if err := w.Cutover(ctx, job, oldImage, "unless-stopped"); err != nil {
				t.Fatal(err)
			}
			d.fail["update:payram:unless-stopped"] = tc.failures
			if err := w.Restore(ctx, job); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			c := d.containers["payram"]
			if c == nil || !c.running || c.image != oldImage || c.restart != tc.wantRestart {
				t.Fatalf("payram = %+v, want the old version running with restart %s; %s", c, tc.wantRestart, d.describe())
			}
			if tc.failures > 1 && !strings.Contains(strings.Join(rec.lines, "\n"), "docker update --restart unless-stopped payram") {
				t.Fatalf("no instruction to set the policy by hand: %v", rec.lines)
			}
		})
	}
}

func TestSwitch_ExitedContainerFailsFastInsteadOfWaitingOutTheTimeout(t *testing.T) {
	d := newFakeDocker()
	d.exitOnStart = newImage
	w, _, _ := newSwitch(d)
	w.Timing.HealthTimeout = 5 * time.Second
	probe := &fakeProbe{docker: d, healthy: map[string]bool{oldImage: true, newImage: true}}

	start := time.Now()
	err := runSwitch(t, w, d, probe, newJob())
	if err == nil || !strings.Contains(err.Error(), "stopped while starting") {
		t.Fatalf("got %v, want a 'stopped while starting' failure", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %s; an exited container must not wait out the 5s health timeout", elapsed)
	}
}

func TestSwitch_NothingIsChangedWhenProgressCannotBeSaved(t *testing.T) {
	d := newFakeDocker()
	w, rec, _ := newSwitch(d)
	rec.fail = true

	err := w.CreateNext(context.Background(), newJob(), newSpec())
	var stepErr *StepError
	if !errors.As(err, &stepErr) || stepErr.Code != "STATE_SAVE_FAILED" {
		t.Fatalf("got %v, want STATE_SAVE_FAILED", err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("docker was called although progress could not be saved: %v", d.calls)
	}
}

// TestRestore_FromEveryPointAnUpdateCanBeCutOff covers a crash, restart or
// reboot at each point of the switch: Restore must bring the old PayRam back.
func TestRestore_FromEveryPointAnUpdateCanBeCutOff(t *testing.T) {
	cases := []struct {
		name         string
		containers   map[string]*fakeContainer
		quiesced     []string
		wantPrograms bool
	}{
		{
			name: "programs stopped for the backup, container still running",
			containers: map[string]*fakeContainer{
				"payram":      {image: oldImage, running: true, status: "running", restart: "unless-stopped"},
				"payram-next": {image: newImage, status: "created", restart: "unless-stopped"},
			},
			quiesced:     []string{"payram-core", "eth-listener"},
			wantPrograms: true,
		},
		{
			name: "old container stopped, not yet set aside",
			containers: map[string]*fakeContainer{
				"payram":      {image: oldImage, status: "exited", restart: "no"},
				"payram-next": {image: newImage, status: "created", restart: "unless-stopped"},
			},
		},
		{
			name: "old container set aside, new one not yet in place",
			containers: map[string]*fakeContainer{
				"payram-previous": {image: oldImage, status: "exited", restart: "no"},
				"payram-next":     {image: newImage, status: "created", restart: "unless-stopped"},
			},
		},
		{
			name: "new container in place, not started",
			containers: map[string]*fakeContainer{
				"payram-previous": {image: oldImage, status: "exited", restart: "no"},
				"payram":          {image: newImage, status: "created", restart: "unless-stopped"},
			},
		},
		{
			name: "new container running but never verified",
			containers: map[string]*fakeContainer{
				"payram-previous": {image: oldImage, status: "exited", restart: "no"},
				"payram":          {image: newImage, running: true, status: "running", restart: "unless-stopped"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDocker()
			d.containers = tc.containers
			w, _, started := newSwitch(d)
			job := newJob()
			job.PreviousRestartPolicy = "unless-stopped"
			job.QuiescedPrograms = tc.quiesced

			if err := w.Restore(context.Background(), job); err != nil {
				t.Fatalf("restore: %v", err)
			}
			assertPayramRuns(t, d, oldImage, "unless-stopped")
			if tc.wantPrograms && len(*started) != 1 {
				t.Fatalf("programs stopped for the backup were not started again: %v", *started)
			}
			if failed, ok := d.containers["payram-failed"]; ok && (failed.image != newImage || failed.restart != "no") {
				t.Fatalf("payram-failed = %+v, want the new image kept with restart no", *failed)
			}
		})
	}
}

func TestRestore_SaysSoWhenTheOldContainerCannotBeBroughtBack(t *testing.T) {
	d := newFakeDocker()
	d.containers = map[string]*fakeContainer{
		"payram-previous": {image: oldImage, status: "exited", restart: "no"},
		"payram":          {image: newImage, running: true, status: "running", restart: "unless-stopped"},
	}
	d.fail["rename:payram-previous"] = 1
	w, _, _ := newSwitch(d)
	job := newJob()
	job.PreviousRestartPolicy = "unless-stopped"

	err := w.Restore(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "name back") {
		t.Fatalf("got %v, want an error about giving the old container its name back", err)
	}
}

func TestClearLeftovers_KeepsAnOldPreviousAndRemovesTheRest(t *testing.T) {
	d := newFakeDocker()
	d.containers["payram-previous"] = &fakeContainer{image: "payramapp/payram:3.8.0", status: "exited", restart: "unless-stopped"}
	d.containers["payram-failed"] = &fakeContainer{image: "payramapp/payram:3.9.1", status: "exited", restart: "no"}
	d.containers["payram-next"] = &fakeContainer{image: newImage, status: "created"}
	w, _, _ := newSwitch(d)

	if err := w.ClearLeftovers(context.Background(), "payram"); err != nil {
		t.Fatalf("ClearLeftovers: %v", err)
	}
	var kept []string
	for name, c := range d.containers {
		switch {
		case name == "payram":
		case strings.HasPrefix(name, "payram-previous-"):
			if c.restart != "no" {
				t.Fatalf("kept %s must not restart on its own: %+v", name, *c)
			}
			kept = append(kept, name)
		default:
			t.Fatalf("leftover %s was not cleared: %s", name, d.describe())
		}
	}
	if len(kept) != 1 {
		t.Fatalf("the earlier previous container must be kept under a timestamped name: %s", d.describe())
	}
}

func TestClearLeftovers_RefusesWhenALeftoverIsRunning(t *testing.T) {
	d := newFakeDocker()
	d.containers["payram-previous"] = &fakeContainer{image: oldImage, running: true, status: "running"}
	w, _, _ := newSwitch(d)

	if err := w.ClearLeftovers(context.Background(), "payram"); err == nil {
		t.Fatal("a running leftover next to payram must stop the update")
	}
	if _, ok := d.containers["payram-previous"]; !ok {
		t.Fatal("a running container must never be removed")
	}
}

func TestWaitHealthy_ToleratesRestartsWhileBootingButNotOnceUp(t *testing.T) {
	d := newFakeDocker()
	w, _, _ := newSwitch(d)
	probe := &fakeProbe{docker: d, healthy: map[string]bool{oldImage: true}}

	// Two restarts before the first good answer are fine.
	d.containers["payram"].restarts = 2
	if _, err := w.WaitHealthy(context.Background(), "payram", probe, ""); err != nil {
		t.Fatalf("restarts before booting must be tolerated: %v", err)
	}

	// A restart after the first good answer is not.
	go func() {
		time.Sleep(25 * time.Millisecond)
		d.mu.Lock()
		d.containers["payram"].restarts++
		d.mu.Unlock()
	}()
	w.Timing.StableFor = 200 * time.Millisecond
	if _, err := w.WaitHealthy(context.Background(), "payram", probe, ""); err == nil || !strings.Contains(err.Error(), "restarting") {
		t.Fatalf("got %v, want a restart during the stability window to fail", err)
	}
}

func TestHealthWindowLeft_ARestartedCheckOnlyWaitsWhatIsLeft(t *testing.T) {
	w := &Switch{Timing: Timing{HealthTimeout: 10 * time.Minute}}
	if left := w.HealthWindowLeft(time.Now().Add(-4 * time.Minute)); left < 5*time.Minute+50*time.Second || left > 6*time.Minute {
		t.Fatalf("4 minutes in: left = %s, want about 6m", left)
	}
	if left := w.HealthWindowLeft(time.Now().Add(-30 * time.Minute)); left != time.Minute {
		t.Fatalf("past the window: left = %s, want the 1m minimum", left)
	}
	if left := w.HealthWindowLeft(time.Time{}); left != 10*time.Minute {
		t.Fatalf("unknown start: left = %s, want the whole window", left)
	}
}
