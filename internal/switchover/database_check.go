package switchover

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/dbexec"
	"github.com/payram/payram-updater/internal/dockerexec"
	"github.com/payram/payram-updater/internal/jobs"
)

// Database reads and restores PayRam's database through one of PayRam's
// containers: the bundled database lives inside the container, an external
// one is reached from the host with the container's settings. Both are the
// rules the pre-update backup used, so a check or a restore works on the
// database that backup came from.
type Database interface {
	// IsBundled reports whether the database runs inside container, which
	// then has to be running for the database to be read.
	IsBundled(ctx context.Context, container string) (bool, error)
	// ReadState reads what decides whether the database still suits the
	// version that ran before the update (see jobs.DatabaseState).
	ReadState(ctx context.Context, container string) (*jobs.DatabaseState, error)
	// RestoreBackup replaces the database with the backup at backupPath,
	// all or nothing, after dumping the database as it is to a safety
	// backup, whose path it returns.
	RestoreBackup(ctx context.Context, container, backupPath string) (string, error)
}

// DatabaseVerdictKind is what CheckDatabase decided.
type DatabaseVerdictKind int

const (
	// DatabaseFitsPrevious: switch back; the previous version runs on the
	// database as it is.
	DatabaseFitsPrevious DatabaseVerdictKind = iota
	// DatabaseNeedsRestore: the new version changed the database and sent
	// no payout or sweep since the backup: restore the backup, then switch
	// back.
	DatabaseNeedsRestore
	// DatabaseMoneyMoved: the new version changed the database and a payout
	// or sweep was signed or sent since the backup. Restoring would forget
	// it (and could send it again); the previous version cannot run on the
	// changed database. Neither may happen: PayRam stays on the new version.
	DatabaseMoneyMoved
	// DatabaseUnreadable: whether the database changed, or money moved,
	// cannot be told. Neither restoring nor switching back is safe.
	DatabaseUnreadable
)

// DatabaseVerdict is the outcome of CheckDatabase.
type DatabaseVerdict struct {
	Kind DatabaseVerdictKind
	// Container is the new version's container, "" when there is none.
	Container string
	// Reason says what was found, for the log and the job's message.
	Reason string
	// Changes says how the new version changed the database ("" when it
	// did not, or when that is not known).
	Changes string
	// NeverStarted: the new version's container never started, so it ran
	// nothing on the database.
	NeverStarted bool
	// MoneyTables are the money tables that changed since the backup
	// (DatabaseMoneyMoved).
	MoneyTables []string
}

// JudgeDatabase decides from what the database held before the switch
// (beforeSwitch), what the pre-update backup holds (atBackup) and what it
// holds now:
//   - no migration added or removed and no table or column changed: the
//     previous version runs on it as it is;
//   - changed, and the payouts and sweeps that were signed or sent are
//     exactly those the backup has: restore the backup first;
//   - changed, and they differ (one was signed, sent or replaced since the
//     backup): never restore, never switch back;
//   - anything unknown: neither.
func JudgeDatabase(beforeSwitch, atBackup, now *jobs.DatabaseState) DatabaseVerdict {
	if beforeSwitch == nil {
		return DatabaseVerdict{Kind: DatabaseUnreadable, Reason: "the update has no record of the database from before the switch"}
	}
	if now == nil {
		return DatabaseVerdict{Kind: DatabaseUnreadable, Reason: "the database could not be read"}
	}
	added, removed := compareMigrations(beforeSwitch.MigrationIDs, now.MigrationIDs)
	seedsAdded, _ := compareMigrations(beforeSwitch.SeedNames, now.SeedNames)
	schemaChanged := beforeSwitch.SchemaDigest != now.SchemaDigest
	if len(added) == 0 && len(removed) == 0 && len(seedsAdded) == 0 && !schemaChanged {
		return DatabaseVerdict{Kind: DatabaseFitsPrevious, Reason: "the new version did not change the database's structure (no migration, no seed data, no table or column changed)"}
	}
	changes := describeChanges(added, removed, seedsAdded, schemaChanged)
	if atBackup == nil || atBackup.MoneyRecords == nil || now.MoneyRecords == nil {
		return DatabaseVerdict{Kind: DatabaseUnreadable, Changes: changes, Reason: "the new version changed the database (" + changes + "), and whether it moved money since the backup could not be told"}
	}
	if moved := ChangedMoneyRecords(atBackup, now); len(moved) > 0 {
		return DatabaseVerdict{Kind: DatabaseMoneyMoved, Changes: changes, MoneyTables: moved,
			Reason: "the new version changed the database (" + changes + "), and after the backup was taken it " + strings.Join(backup.DescribeMoneyRecords(moved), ", ")}
	}
	return DatabaseVerdict{Kind: DatabaseNeedsRestore, Changes: changes, Reason: "the new version changed the database (" + changes + "); it moved no money after the backup was taken (no payout, sweep, gas transfer, deposit wallet or payment address)"}
}

// ChangedMoneyRecords lists, sorted, the money tables whose records differ
// between two reads. A table missing from a read has no records.
func ChangedMoneyRecords(before, now *jobs.DatabaseState) []string {
	tables := map[string]bool{}
	for table := range before.MoneyRecords {
		tables[table] = true
	}
	for table := range now.MoneyRecords {
		tables[table] = true
	}
	digest := func(state *jobs.DatabaseState, table string) string {
		if value, ok := state.MoneyRecords[table]; ok {
			return value
		}
		return backup.EmptyRecordsDigest
	}
	var changed []string
	for table := range tables {
		if digest(before, table) != digest(now, table) {
			changed = append(changed, table)
		}
	}
	sort.Strings(changed)
	return changed
}

// sameStructure reports whether two reads show the same migrations and
// schema.
func sameStructure(a, b *jobs.DatabaseState) bool {
	if a == nil || b == nil {
		return false
	}
	added, removed := compareMigrations(a.MigrationIDs, b.MigrationIDs)
	seedsAdded, seedsRemoved := compareMigrations(a.SeedNames, b.SeedNames)
	return len(added) == 0 && len(removed) == 0 && len(seedsAdded) == 0 && len(seedsRemoved) == 0 && a.SchemaDigest == b.SchemaDigest
}

func compareMigrations(before, now []string) (added, removed []string) {
	had := make(map[string]bool, len(before))
	for _, id := range before {
		had[id] = true
	}
	has := make(map[string]bool, len(now))
	for _, id := range now {
		has[id] = true
		if !had[id] {
			added = append(added, id)
		}
	}
	for _, id := range before {
		if !has[id] {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func describeChanges(added, removed, seedsAdded []string, schemaChanged bool) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, fmt.Sprintf("%d migration(s) added (%s)", len(added), listSome(added)))
	}
	if len(removed) > 0 {
		parts = append(parts, fmt.Sprintf("%d migration(s) gone (%s)", len(removed), listSome(removed)))
	}
	if len(seedsAdded) > 0 {
		parts = append(parts, fmt.Sprintf("seed data added (%s)", listSome(seedsAdded)))
	}
	if schemaChanged && len(added) == 0 && len(removed) == 0 {
		parts = append(parts, "tables or columns changed without a recorded migration, so one may have failed half way")
	}
	return strings.Join(parts, "; ")
}

func listSome(ids []string) string {
	const shown = 3
	if len(ids) <= shown {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:shown], ", ") + fmt.Sprintf(" and %d more", len(ids)-shown)
}

// findFailedVersion finds the new version's container from the containers
// that exist, whatever the recorded stage says. While the old container is
// set aside as PreviousName, the new one is the container under the name
// (or still NextName, when the switch stopped before renaming it); once the
// old one has its name back, the new one is NextName or FailedName.
func (w *Switch) findFailedVersion(ctx context.Context, name string) (string, dockerexec.ContainerState, error) {
	previous, err := w.Docker.State(ctx, PreviousName(name))
	if err != nil {
		return "", dockerexec.ContainerState{}, err
	}
	candidates := []string{NextName(name), FailedName(name)}
	if previous.Exists {
		candidates = []string{name, NextName(name)}
	}
	for _, candidate := range candidates {
		state, err := w.Docker.State(ctx, candidate)
		if err != nil {
			return "", dockerexec.ContainerState{}, err
		}
		if state.Exists {
			return candidate, state, nil
		}
	}
	return "", dockerexec.ContainerState{}, nil
}

// CheckDatabase decides, after the new version failed, whether PayRam may be
// switched back as it is, needs the backup restored first, or must stay on
// the new version (see JudgeDatabase). Before reading, it makes sure the new
// version cannot write any more: its programs are stopped (the bundled
// database keeps running), or, when its container is not running, the
// container is kept from starting again. A check that already decided (an
// updater restarted after it) is not repeated. Every step is saved first, so
// a restarted updater can carry on or undo it (LeaveFailedVersion).
func (w *Switch) CheckDatabase(ctx context.Context, job *jobs.Job) DatabaseVerdict {
	switch job.DatabaseCheck {
	case jobs.DatabaseCheckUnchanged, jobs.DatabaseCheckRestored:
		return DatabaseVerdict{Kind: DatabaseFitsPrevious, Reason: "the database was checked before the updater restarted"}
	}
	unreadable := func(container, format string, args ...interface{}) DatabaseVerdict {
		return DatabaseVerdict{Kind: DatabaseUnreadable, Container: container, Reason: fmt.Sprintf(format, args...)}
	}

	name, state, err := w.findFailedVersion(ctx, job.ContainerName)
	if err != nil {
		return unreadable("", "the containers could not be read (%s)", shortError(err))
	}
	if name == "" && !job.SwitchedAt.IsZero() {
		return unreadable("", "the new version's container is gone, so whether it ran cannot be told")
	}
	if name == "" || !state.HasStarted {
		w.logf("The new version never started, so it did not touch the database")
		job.DatabaseCheck = jobs.DatabaseCheckUnchanged
		if err := w.save(job); err != nil {
			w.logf("Warning: %v", err)
		}
		return DatabaseVerdict{Kind: DatabaseFitsPrevious, Container: name, Reason: "the new version never started", NeverStarted: true}
	}

	// From here PayRam is being switched back: a restarted updater must not
	// take the new version for one that may still pass its check.
	job.SwitchStage = jobs.SwitchStageReverting
	if err := w.save(job); err != nil {
		w.logf("Warning: %v", err)
	}
	if w.Database == nil {
		return unreadable(name, "the updater has no access to the database")
	}
	bundled, err := w.Database.IsBundled(ctx, name)
	if err != nil {
		return unreadable(name, "the database settings could not be read (%s)", shortError(err))
	}

	running := state.Running && state.Status != "restarting"
	if running {
		if err := w.stopFailedVersionPrograms(ctx, job, name); err != nil {
			// A container in a restart loop answers "is restarting".
			now, stateErr := w.Docker.State(ctx, name)
			if stateErr != nil || (now.Running && now.Status != "restarting" && now.RestartCount == state.RestartCount) {
				return unreadable(name, "the new version's programs could not be stopped, so it may still be writing to the database (%s)", shortError(err))
			}
			running, state = false, now
		}
	}
	if !running {
		// A container that is not running, or keeps restarting, is kept
		// from starting again mid-check, and stays stopped afterwards.
		if err := w.StopFailedVersion(ctx, name); err != nil {
			return unreadable(name, "%s", shortError(err))
		}
		if bundled {
			return unreadable(name, "the database runs inside the new version's container, which is not running (%s)", state.Status)
		}
	}

	w.logf("Reading the database to see whether the new version changed it...")
	now, err := w.Database.ReadState(ctx, name)
	if err != nil {
		return unreadable(name, "the database could not be read (%s)", shortError(err))
	}
	verdict := JudgeDatabase(job.DatabaseBeforeSwitch, job.DatabaseAtBackup, now)
	if job.DatabaseCheck == jobs.DatabaseCheckRestoring && sameStructure(job.DatabaseAtBackup, now) {
		// The restore committed before the updater restarted.
		verdict = DatabaseVerdict{Kind: DatabaseFitsPrevious, Reason: "the backup had been restored before the updater restarted"}
		job.DatabaseCheck = jobs.DatabaseCheckRestored
	} else if verdict.Kind == DatabaseFitsPrevious {
		job.DatabaseCheck = jobs.DatabaseCheckUnchanged
	}
	if err := w.save(job); err != nil {
		w.logf("Warning: %v", err)
	}
	verdict.Container = name
	w.logf("Database check: %s", verdict.Reason)
	return verdict
}

// stopFailedVersionPrograms stops the new version's programs, all but the
// database, recording them in the job first.
func (w *Switch) stopFailedVersionPrograms(ctx context.Context, job *jobs.Job, name string) error {
	if w.Programs == nil {
		return ErrNoSupervisor
	}
	running, err := w.Programs.Running(ctx, name)
	if err != nil {
		return err
	}
	if len(running) == 0 {
		return nil
	}
	job.ProgramsStoppedForCheck = mergeNames(job.ProgramsStoppedForCheck, running)
	if err := w.save(job); err != nil {
		return err
	}
	w.logf("Stopping the new version's programs so nothing writes to the database while it is checked: %s", strings.Join(running, ", "))
	return w.Programs.Stop(ctx, name, running)
}

// RestoreDatabase restores the pre-update backup into the database the new
// version's container uses, all or nothing. On success the job records the
// safety backup of the database as the new version left it.
func (w *Switch) RestoreDatabase(ctx context.Context, job *jobs.Job, container string) error {
	if w.Database == nil {
		return fmt.Errorf("the updater has no access to the database")
	}
	if job.BackupPath == "" {
		return fmt.Errorf("the update has no backup to restore")
	}
	job.DatabaseCheck = jobs.DatabaseCheckRestoring
	if err := w.save(job); err != nil {
		w.logf("Warning: %v", err)
	}
	w.logf("Restoring the backup taken before the update (%s); one transaction, so any error leaves the database as it is", job.BackupPath)
	safety, err := w.Database.RestoreBackup(ctx, container, job.BackupPath)
	if err != nil {
		return err
	}
	job.DatabaseCheck = jobs.DatabaseCheckRestored
	job.SafetyBackupPath = safety
	if err := w.save(job); err != nil {
		w.logf("Warning: %v", err)
	}
	w.logf("Backup restored")
	return nil
}

// LeaveFailedVersion leaves PayRam on the new version, as it was before
// CheckDatabase: a running container gets the programs it stopped started
// again; a container that was not running, or kept restarting, stays
// stopped and kept from starting by itself (with restart policy "no", as
// the previous version's container), for a person to look at.
func (w *Switch) LeaveFailedVersion(ctx context.Context, job *jobs.Job, container string) {
	if container == "" {
		return
	}
	state, err := w.Docker.State(ctx, container)
	if err != nil || !state.Running {
		return
	}
	if len(job.ProgramsStoppedForCheck) == 0 || w.Programs == nil {
		return
	}
	w.logf("Starting the new version's programs again: %s", strings.Join(job.ProgramsStoppedForCheck, ", "))
	if err := w.Programs.Start(ctx, container, job.ProgramsStoppedForCheck); err != nil {
		w.logf("Warning: the new version's programs could not be started again (%s)", shortError(err))
	}
}

func mergeNames(have, more []string) []string {
	seen := make(map[string]bool, len(have)+len(more))
	var out []string
	for _, name := range append(append([]string(nil), have...), more...) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// SwitchBackOutcome is where PrepareSwitchBack left PayRam.
type SwitchBackOutcome int

const (
	// SwitchBackAsIs: the database fits the previous version as it is;
	// switch back (Restore).
	SwitchBackAsIs SwitchBackOutcome = iota
	// SwitchBackAfterRestore: the backup was restored; switch back.
	SwitchBackAfterRestore
	// StayMoneyMoved: money moved since the backup; the new version was left
	// as it was (LeaveFailedVersion). Do not switch back.
	StayMoneyMoved
	// StayUnchecked: the database could not be judged; the new version was
	// left as it was. Do not switch back.
	StayUnchecked
	// StayDeclined: the person asked said no; the new version was stopped,
	// so both versions are stopped. Do not switch back.
	StayDeclined
	// StayRestoreFailed: the restore failed and changed nothing; the new
	// version was left as it was. Do not switch back.
	StayRestoreFailed
	// StopRestoreUncertain: the restore was cut off while committing, so the
	// database's state is unknown; the new version was stopped, so both are
	// stopped. Do not switch back.
	StopRestoreUncertain
)

// PrepareSwitchBack makes sure the previous version can run on the database
// before Restore switches back to it. It runs CheckDatabase and acts on the
// verdict: the database fits, or the backup must be restored first, which
// happens only when ask (called with the verdict) says yes; otherwise PayRam
// stays where the outcome says. The error is the restore's, when it failed.
func (w *Switch) PrepareSwitchBack(ctx context.Context, job *jobs.Job, ask func(DatabaseVerdict) bool) (SwitchBackOutcome, DatabaseVerdict, error) {
	verdict := w.CheckDatabase(ctx, job)
	switch verdict.Kind {
	case DatabaseFitsPrevious:
		if job.DatabaseCheck == jobs.DatabaseCheckRestored {
			return SwitchBackAfterRestore, verdict, nil
		}
		return SwitchBackAsIs, verdict, nil
	case DatabaseMoneyMoved:
		w.LeaveFailedVersion(ctx, job, verdict.Container)
		return StayMoneyMoved, verdict, nil
	case DatabaseUnreadable:
		w.LeaveFailedVersion(ctx, job, verdict.Container)
		return StayUnchecked, verdict, nil
	}

	if !ask(verdict) {
		if err := w.StopFailedVersion(ctx, verdict.Container); err != nil {
			w.logf("Warning: %v", err)
		}
		return StayDeclined, verdict, nil
	}
	if err := w.RestoreDatabase(ctx, job, verdict.Container); err != nil {
		w.logf("The restore failed: %s", shortError(err))
		var restoreErr *dbexec.RestoreError
		if errors.As(err, &restoreErr) && !restoreErr.Unchanged {
			// Neither version may run on a database in an unknown state.
			if stopErr := w.StopFailedVersion(ctx, verdict.Container); stopErr != nil {
				w.logf("Warning: %v", stopErr)
			}
			return StopRestoreUncertain, verdict, err
		}
		job.DatabaseCheck = jobs.DatabaseCheckNone
		if saveErr := w.save(job); saveErr != nil {
			w.logf("Warning: %v", saveErr)
		}
		w.LeaveFailedVersion(ctx, job, verdict.Container)
		return StayRestoreFailed, verdict, err
	}
	return SwitchBackAfterRestore, verdict, nil
}
