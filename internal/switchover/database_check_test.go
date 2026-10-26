package switchover

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/dbexec"
	"github.com/payram/payram-updater/internal/jobs"
)

// The database before the update, as the backup holds it, and as a failed
// new version can leave it.
func dbState(migrations []string, schema string, money map[string]string) *jobs.DatabaseState {
	return &jobs.DatabaseState{MigrationIDs: migrations, SeedNames: []string{"configurations"}, SchemaDigest: schema, MoneyRecords: money}
}

var (
	atBackup = dbState([]string{"202601010000", "202605071600"}, "schema-3.1.3", map[string]string{"withdrawals": "w-1", "deposit_addresses": "d-1"})
	// 3.1.4's EncryptAPIKeys ran: the previous version would reject every API key.
	migrated = dbState([]string{"202601010000", "202605071600", "202606131200"}, "schema-3.1.4", map[string]string{"withdrawals": "w-1", "deposit_addresses": "d-1"})
	// ... and it sent a payout after the backup.
	migratedAndPaid = dbState([]string{"202601010000", "202605071600", "202606131200"}, "schema-3.1.4", map[string]string{"withdrawals": "w-2", "deposit_addresses": "d-1"})
)

// fakeDatabase is PayRam's database as seen through a container.
type fakeDatabase struct {
	bundled    bool
	now        *jobs.DatabaseState
	readErr    error
	restoreErr error
	programs   *fakePrograms
	reads      int
	restores   []string
	// runningAtRead records, per read, which programs still ran.
	runningAtRead [][]string
}

func (f *fakeDatabase) IsBundled(context.Context, string) (bool, error) { return f.bundled, nil }

func (f *fakeDatabase) ReadState(_ context.Context, container string) (*jobs.DatabaseState, error) {
	f.reads++
	if f.programs != nil {
		running, _ := f.programs.Running(context.Background(), container)
		f.runningAtRead = append(f.runningAtRead, running)
	}
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.now, nil
}

func (f *fakeDatabase) RestoreBackup(_ context.Context, container, backupPath string) (string, error) {
	f.restores = append(f.restores, container+":"+backupPath)
	if f.restoreErr != nil {
		return "", f.restoreErr
	}
	f.now = atBackup
	return "/backups/payram-backup-pre-restore-safety.dump", nil
}

// failedUpdate is the moment the new version (running as payram) failed
// its check: the old one is set aside as payram-previous, stopped.
func failedUpdate(t *testing.T, now *jobs.DatabaseState) (*Switch, *fakeDocker, *fakeDatabase, *fakePrograms, *jobs.Job) {
	t.Helper()
	d := newFakeDocker()
	d.containers = map[string]*fakeContainer{
		"payram-previous": {image: oldImage, status: "exited", restart: "no"},
		"payram":          {image: newImage, running: true, status: "running", restart: "unless-stopped"},
	}
	w, _, _ := newSwitch(d)
	programs := w.Programs.(*fakePrograms)
	programs.running = map[string][]string{"payram": {"account-processor", "payram-core"}}
	db := &fakeDatabase{bundled: true, now: now, programs: programs}
	w.Database = db

	job := newJob()
	job.PreviousImage = oldImage
	job.PreviousRestartPolicy = "unless-stopped"
	job.SwitchStage = jobs.SwitchStageSwitched
	job.SwitchedAt = time.Now().Add(-10 * time.Minute)
	job.BackupPath = "/backups/payram-backup-3.1.3-to-3.10.0.dump"
	job.DatabaseAtBackup = atBackup
	job.DatabaseBeforeSwitch = atBackup
	return w, d, db, programs, job
}

func neverAsked(t *testing.T) func(DatabaseVerdict) bool {
	return func(v DatabaseVerdict) bool {
		t.Fatalf("asked to restore although the verdict was %d (%s)", v.Kind, v.Reason)
		return false
	}
}

func TestJudgeDatabase(t *testing.T) {
	seeded := dbState(atBackup.MigrationIDs, atBackup.SchemaDigest, atBackup.MoneyRecords)
	seeded.SeedNames = []string{"configurations", "usdt-on-base"}
	halfMigrated := dbState(atBackup.MigrationIDs, "schema-half-way", atBackup.MoneyRecords)
	addressGiven := dbState(migrated.MigrationIDs, migrated.SchemaDigest, map[string]string{"withdrawals": "w-1", "deposit_addresses": "d-2"})
	newEmptyTable := dbState(migrated.MigrationIDs, migrated.SchemaDigest, map[string]string{"withdrawals": "w-1", "deposit_addresses": "d-1", "sweep_transactions": "d41d8cd98f00b204e9800998ecf8427e"})
	noMoney := dbState(migrated.MigrationIDs, migrated.SchemaDigest, nil)

	cases := []struct {
		name       string
		before     *jobs.DatabaseState
		now        *jobs.DatabaseState
		want       DatabaseVerdictKind
		wantReason string
	}{
		{"no migration, nothing changed", atBackup, atBackup, DatabaseFitsPrevious, "did not change"},
		{"migrations, no money moved", atBackup, migrated, DatabaseNeedsRestore, "1 migration(s) added (202606131200)"},
		{"migrations and a payout", atBackup, migratedAndPaid, DatabaseMoneyMoved, "requested, signed or sent a payout"},
		{"migrations and a payment address handed out", atBackup, addressGiven, DatabaseMoneyMoved, "handed a payment address to a customer"},
		{"a new money table with no rows is no money", atBackup, newEmptyTable, DatabaseNeedsRestore, "moved no money"},
		{"seed data only", atBackup, seeded, DatabaseNeedsRestore, "seed data added (usdt-on-base)"},
		{"a migration that failed half way", atBackup, halfMigrated, DatabaseNeedsRestore, "failed half way"},
		{"money records not readable", atBackup, noMoney, DatabaseUnreadable, "could not be told"},
		{"no record from before the switch", nil, migrated, DatabaseUnreadable, "no record"},
		{"nothing read now", atBackup, nil, DatabaseUnreadable, "could not be read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JudgeDatabase(tc.before, atBackup, tc.now)
			if got.Kind != tc.want || !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("got %d %q, want %d containing %q", got.Kind, got.Reason, tc.want, tc.wantReason)
			}
		})
	}
}

func TestPrepareSwitchBack_NoMigrationSwitchesBackAsItIs(t *testing.T) {
	w, d, db, _, job := failedUpdate(t, atBackup)
	ctx := context.Background()

	outcome, verdict, err := w.PrepareSwitchBack(ctx, job, neverAsked(t))
	if err != nil || outcome != SwitchBackAsIs || verdict.NeverStarted {
		t.Fatalf("got %d %+v %v, want a switch back as it is", outcome, verdict, err)
	}
	if len(db.restores) != 0 {
		t.Fatalf("restored %v although nothing changed", db.restores)
	}
	if len(db.runningAtRead) != 1 || len(db.runningAtRead[0]) != 0 {
		t.Fatalf("programs running while the database was read: %v", db.runningAtRead)
	}
	if job.DatabaseCheck != jobs.DatabaseCheckUnchanged || job.SwitchStage != jobs.SwitchStageReverting {
		t.Fatalf("check %q stage %q", job.DatabaseCheck, job.SwitchStage)
	}
	if err := w.Restore(ctx, job); err != nil {
		t.Fatal(err)
	}
	assertPayramRuns(t, d, oldImage, "unless-stopped")
}

func TestPrepareSwitchBack_MigratedAndNoMoneyMovedRestoresThenSwitchesBack(t *testing.T) {
	w, d, db, _, job := failedUpdate(t, migrated)
	ctx := context.Background()
	asked := 0

	outcome, verdict, err := w.PrepareSwitchBack(ctx, job, func(v DatabaseVerdict) bool {
		asked++
		if v.Kind != DatabaseNeedsRestore || !strings.Contains(v.Changes, "202606131200") {
			t.Fatalf("asked with %+v", v)
		}
		return true
	})
	if err != nil || outcome != SwitchBackAfterRestore {
		t.Fatalf("got %d %v, want a restore then a switch back", outcome, err)
	}
	if asked != 1 {
		t.Fatalf("asked %d times", asked)
	}
	if len(db.restores) != 1 || db.restores[0] != "payram:"+job.BackupPath {
		t.Fatalf("restores = %v, want the pre-update backup into the new version's database", db.restores)
	}
	if len(db.runningAtRead[0]) != 0 {
		t.Fatalf("the new version's programs ran while its database was checked: %v", db.runningAtRead)
	}
	if got := strings.Join(job.ProgramsStoppedForCheck, ","); got != "account-processor,payram-core" {
		t.Fatalf("stopped programs not recorded: %q", got)
	}
	if job.DatabaseCheck != jobs.DatabaseCheckRestored || job.SafetyBackupPath == "" {
		t.Fatalf("check %q safety %q", job.DatabaseCheck, job.SafetyBackupPath)
	}
	if verdict.Container != "payram" {
		t.Fatalf("container = %q", verdict.Container)
	}
	if err := w.Restore(ctx, job); err != nil {
		t.Fatal(err)
	}
	assertPayramRuns(t, d, oldImage, "unless-stopped")
}

func TestPrepareSwitchBack_MoneyMovedNeitherRestoresNorSwitchesBack(t *testing.T) {
	w, d, db, programs, job := failedUpdate(t, migratedAndPaid)

	outcome, verdict, err := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
	if err != nil || outcome != StayMoneyMoved {
		t.Fatalf("got %d %v, want PayRam left on the new version", outcome, err)
	}
	if len(db.restores) != 0 {
		t.Fatalf("restored %v past a sent payout", db.restores)
	}
	if len(verdict.MoneyTables) != 1 || verdict.MoneyTables[0] != "withdrawals" {
		t.Fatalf("money tables = %v", verdict.MoneyTables)
	}
	// The new version stays as it was: running, its restart policy on, its
	// programs started again. The old one stays aside and cannot start.
	if c := d.containers["payram"]; c == nil || c.image != newImage || !c.running || c.restart != "unless-stopped" {
		t.Fatalf("payram = %+v, want the new version left running: %s", c, d.describe())
	}
	if c := d.containers["payram-previous"]; c == nil || c.running || c.restart != "no" {
		t.Fatalf("payram-previous = %+v, want it kept stopped", c)
	}
	if got := strings.Join(programs.started, " "); got != "payram:account-processor,payram-core" {
		t.Fatalf("programs started again: %q", got)
	}
}

func TestPrepareSwitchBack_DeclinedLeavesBothVersionsStopped(t *testing.T) {
	w, d, db, _, job := failedUpdate(t, migrated)

	outcome, _, err := w.PrepareSwitchBack(context.Background(), job, func(DatabaseVerdict) bool { return false })
	if err != nil || outcome != StayDeclined {
		t.Fatalf("got %d %v", outcome, err)
	}
	if len(db.restores) != 0 {
		t.Fatalf("restored %v after a no", db.restores)
	}
	for _, name := range []string{"payram", "payram-previous"} {
		if c := d.containers[name]; c == nil || c.running || c.restart != "no" {
			t.Fatalf("%s = %+v, want stopped and unable to start by itself: %s", name, c, d.describe())
		}
	}
}

func TestPrepareSwitchBack_RestoreFailure(t *testing.T) {
	t.Run("rolled back: the new version is left as it was", func(t *testing.T) {
		w, d, db, programs, job := failedUpdate(t, migrated)
		db.restoreErr = &dbexec.RestoreError{Unchanged: true, Reason: "lock timeout"}

		outcome, _, err := w.PrepareSwitchBack(context.Background(), job, func(DatabaseVerdict) bool { return true })
		if outcome != StayRestoreFailed || err == nil {
			t.Fatalf("got %d %v", outcome, err)
		}
		if job.DatabaseCheck != jobs.DatabaseCheckNone {
			t.Fatalf("check = %q, want none: the database is the new version's", job.DatabaseCheck)
		}
		if c := d.containers["payram"]; !c.running || c.image != newImage {
			t.Fatalf("payram = %+v, want the new version left running", c)
		}
		if len(programs.started) != 1 {
			t.Fatalf("programs not started again: %v", programs.started)
		}
	})
	t.Run("not certain whether it committed: both stopped", func(t *testing.T) {
		w, d, db, _, job := failedUpdate(t, migrated)
		db.restoreErr = &dbexec.RestoreError{Unchanged: false, Reason: "psql stopped after the final COMMIT was sent"}

		outcome, _, err := w.PrepareSwitchBack(context.Background(), job, func(DatabaseVerdict) bool { return true })
		if outcome != StopRestoreUncertain || err == nil {
			t.Fatalf("got %d %v", outcome, err)
		}
		for _, name := range []string{"payram", "payram-previous"} {
			if c := d.containers[name]; c.running || c.restart != "no" {
				t.Fatalf("%s = %+v, want stopped: neither version may run on an unknown database", name, c)
			}
		}
	})
}

func TestPrepareSwitchBack_NewVersionThatNeverStartedNeedsNoCheck(t *testing.T) {
	w, d, db, _, job := failedUpdate(t, migrated)
	d.containers["payram"] = &fakeContainer{image: newImage, status: "created", restart: "unless-stopped"}

	outcome, verdict, err := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
	if err != nil || outcome != SwitchBackAsIs || !verdict.NeverStarted {
		t.Fatalf("got %d %+v %v", outcome, verdict, err)
	}
	if db.reads != 0 {
		t.Fatal("a container that never started cannot have changed the database; it must not be read")
	}
}

func TestPrepareSwitchBack_StoppedNewVersion(t *testing.T) {
	t.Run("bundled database: it cannot be read, nothing is done", func(t *testing.T) {
		w, d, db, _, job := failedUpdate(t, migrated)
		d.containers["payram"] = &fakeContainer{image: newImage, status: "exited", restart: "unless-stopped"}

		outcome, verdict, _ := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
		if outcome != StayUnchecked || !strings.Contains(verdict.Reason, "not running") {
			t.Fatalf("got %d %q", outcome, verdict.Reason)
		}
		if db.reads != 0 || len(db.restores) != 0 {
			t.Fatalf("reads %d restores %v", db.reads, db.restores)
		}
		if c := d.containers["payram"]; c.running || c.restart != "no" {
			t.Fatalf("payram = %+v, want it left stopped, unable to start by itself", c)
		}
		if c := d.containers["payram-previous"]; c.running {
			t.Fatal("the previous version must not be started on an unchecked database")
		}
	})
	t.Run("external database: read from the host, restored, switched back", func(t *testing.T) {
		w, d, db, _, job := failedUpdate(t, migrated)
		db.bundled = false
		d.containers["payram"] = &fakeContainer{image: newImage, status: "restarting", restart: "unless-stopped"}

		outcome, _, err := w.PrepareSwitchBack(context.Background(), job, func(DatabaseVerdict) bool { return true })
		if err != nil || outcome != SwitchBackAfterRestore || len(db.restores) != 1 {
			t.Fatalf("got %d %v restores %v", outcome, err, db.restores)
		}
		if err := w.Restore(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		assertPayramRuns(t, d, oldImage, "unless-stopped")
	})
}

// TestPrepareSwitchBack_RestartLoopIsStopped: a container in a restart
// loop looks running between restarts, and supervisorctl then answers "is
// restarting". It is stopped, kept from starting again, and, with the
// database inside it, not judged.
func TestPrepareSwitchBack_RestartLoopIsStopped(t *testing.T) {
	w, d, db, programs, job := failedUpdate(t, migrated)
	programs.failOn = "stop"
	d.containers["payram"].restarts = 4
	looping := &loopingPrograms{fakePrograms: programs, docker: d}
	w.Programs = looping

	outcome, verdict, _ := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
	if outcome != StayUnchecked || !strings.Contains(verdict.Reason, "not running") || db.reads != 0 {
		t.Fatalf("got %d %q with %d reads", outcome, verdict.Reason, db.reads)
	}
	if c := d.containers["payram"]; c.running || c.restart != "no" {
		t.Fatalf("payram = %+v, want the restart loop stopped", c)
	}
	if c := d.containers["payram-previous"]; c.running || c.restart != "no" {
		t.Fatalf("payram-previous = %+v, want it kept stopped", c)
	}
}

// loopingPrograms is supervisorctl in a container Docker keeps restarting:
// asking it anything finds the container restarting.
type loopingPrograms struct {
	*fakePrograms
	docker *fakeDocker
}

func (p *loopingPrograms) Stop(ctx context.Context, container string, programs []string) error {
	p.docker.mu.Lock()
	p.docker.containers[container].restarts++
	p.docker.containers[container].status = "restarting"
	p.docker.mu.Unlock()
	return errors.New("Error response from daemon: Container is restarting, wait until the container is running")
}

func TestPrepareSwitchBack_UncheckedWhenTheNewVersionCannotBeStopped(t *testing.T) {
	w, _, db, programs, job := failedUpdate(t, migrated)
	programs.failOn = "stop"

	outcome, _, _ := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
	if outcome != StayUnchecked || db.reads != 0 {
		t.Fatalf("got %d with %d reads: a database that may still be written to is not judged", outcome, db.reads)
	}
}

func TestPrepareSwitchBack_UncheckedWhenTheDatabaseCannotBeRead(t *testing.T) {
	w, d, db, _, job := failedUpdate(t, nil)
	db.readErr = errors.New("psql: connection refused")

	outcome, verdict, _ := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
	if outcome != StayUnchecked || !strings.Contains(verdict.Reason, "connection refused") {
		t.Fatalf("got %d %q", outcome, verdict.Reason)
	}
	if !d.containers["payram"].running {
		t.Fatal("the new version must be left running as it was")
	}
}

// TestPrepareSwitchBack_CarriesOnAfterAnUpdaterRestart: a restarted updater
// repeats the switch back; it must not redo a decision or a restore.
func TestPrepareSwitchBack_CarriesOnAfterAnUpdaterRestart(t *testing.T) {
	t.Run("already restored", func(t *testing.T) {
		w, _, db, _, job := failedUpdate(t, migrated)
		job.DatabaseCheck = jobs.DatabaseCheckRestored
		outcome, _, _ := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
		if outcome != SwitchBackAfterRestore || db.reads != 0 {
			t.Fatalf("got %d with %d reads", outcome, db.reads)
		}
	})
	t.Run("restore committed before the restart", func(t *testing.T) {
		w, _, db, _, job := failedUpdate(t, atBackup)
		job.DatabaseCheck = jobs.DatabaseCheckRestoring
		outcome, _, _ := w.PrepareSwitchBack(context.Background(), job, neverAsked(t))
		if outcome != SwitchBackAfterRestore || len(db.restores) != 0 || job.DatabaseCheck != jobs.DatabaseCheckRestored {
			t.Fatalf("got %d restores %v check %q", outcome, db.restores, job.DatabaseCheck)
		}
	})
	t.Run("restore cut off before it committed", func(t *testing.T) {
		w, _, db, _, job := failedUpdate(t, migrated)
		job.DatabaseCheck = jobs.DatabaseCheckRestoring
		outcome, _, _ := w.PrepareSwitchBack(context.Background(), job, func(DatabaseVerdict) bool { return true })
		if outcome != SwitchBackAfterRestore || len(db.restores) != 1 {
			t.Fatalf("got %d restores %v, want the restore done again", outcome, db.restores)
		}
	})
}

// TestRestore_TurnsTheFailedVersionsRestartPolicyOffBeforeStoppingIt is the
// reverse of the cutover: stopped first, a Docker restart between the two
// calls would start the failed container again next to the old one (two
// Postgres servers on one data folder).
func TestRestore_TurnsTheFailedVersionsRestartPolicyOffBeforeStoppingIt(t *testing.T) {
	w, d, _, _, job := failedUpdate(t, atBackup)
	d.calls = nil
	if err := w.Restore(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	off, stop := -1, -1
	for i, call := range d.calls {
		switch call {
		case "update payram no":
			if off < 0 {
				off = i
			}
		case "stop payram":
			if stop < 0 {
				stop = i
			}
		}
	}
	if off < 0 || stop < 0 || off > stop {
		t.Fatalf("the failed container's restart policy must go off before it is stopped; calls: %v", d.calls)
	}
	assertPayramRuns(t, d, oldImage, "unless-stopped")
}
