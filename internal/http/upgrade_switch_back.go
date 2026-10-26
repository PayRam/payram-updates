package http

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/payram/payram-updater/internal/backup"
	"github.com/payram/payram-updater/internal/dbexec"
	"github.com/payram/payram-updater/internal/jobs"
	"github.com/payram/payram-updater/internal/recovery"
	"github.com/payram/payram-updater/internal/switchover"
)

// restoreQuestionWait is how long the person following an attended update
// has to answer before the backup is restored without them. PayRam is down
// while the question waits, so it is short.
const restoreQuestionWait = 3 * time.Minute

// dataRemovedByRestore says what restoring the pre-update backup removes and
// what comes back. Checked against payram-core: each chain's listener keeps
// its block cursor in the database (blockchains.height) and re-reads every
// block after it, with no look-back limit, watching the payment addresses in
// deposit_addresses. A restore is refused when the new version handed out a
// payment address after the backup, so every address a customer can pay to
// is watched again.
const dataRemovedByRestore = "Anything PayRam recorded after that time is removed from the database, such as invoices created and deposits detected since then." +
	" PayRam then reads the blockchain again from where it stood at that time, so those deposits are found again (and their webhooks are sent again)."

// makeDatabaseFitForSwitchBack runs before PayRam is switched back to the
// previous version after the new one failed. The new version migrates the
// database when it starts, and the previous version may not work on a
// migrated database (it rejects API keys an encrypting migration rewrote,
// for one), so Switch.PrepareSwitchBack decides first:
//   - the database fits the previous version: switch back as it is;
//   - the new version changed it and moved no money since the backup: ask
//     the person following the update at root's command line (anyone else
//     gets the default, yes), restore the backup, then switch back;
//   - it changed it and moved money, or it cannot be told: neither restore
//     nor switch back. PayRam stays on the new version and support looks.
//
// It returns true when the switch back may go ahead (and whether the new
// version never started). When it returns false it has ended the job.
func (s *Server) makeDatabaseFitForSwitchBack(ctx context.Context, sw *switchover.Switch, job *jobs.Job, causeCode, reason string) (proceed, neverStarted bool) {
	from, target := versionNames(job)
	takenAt := formatBackupTime(job.BackupTakenAt)
	s.setProgress(job, jobs.JobStateVerifying, "Checking whether the new version changed the database before switching back")
	outcome, verdict, err := sw.PrepareSwitchBack(ctx, job, func(verdict switchover.DatabaseVerdict) bool {
		return s.askToRestore(job, fmt.Sprintf(
			"PayRam %s failed its checks (%s) after it had changed the database (%s). To switch back to %s, the updater restores the backup taken at %s first. %s It moved no money after the backup (no payout, sweep or new payment address), so nothing can be paid twice or missed. Restore the backup and switch back to %s? Answer no to leave both versions stopped and get the recovery steps.",
			target, reason, verdict.Changes, from, takenAt, dataRemovedByRestore, from))
	})
	left := describeContainer(ctx, sw, verdict.Container)
	backupKept := backupPathOrUnknown(job.BackupPath)

	switch outcome {
	case switchover.SwitchBackAsIs:
		return true, verdict.NeverStarted
	case switchover.SwitchBackAfterRestore:
		return true, false
	case switchover.StayMoneyMoved:
		s.finishFailed(job, jobs.FailureUpgradeMoneyMoved, causeCode, fmt.Sprintf(
			"The update to %s did not complete (%s). Before it failed, the new version had changed the database (%s), and after the backup was taken it %s. Restoring the backup would make PayRam forget that (a payout could be sent twice, a customer's payment go unseen), and %s cannot run on the changed database, so the updater did neither: PayRam was left on %s%s. Do not restore the backup. Contact PayRam support with the updater log; the backup is kept at %s.",
			target, reason, verdict.Changes, strings.Join(backup.DescribeMoneyRecords(verdict.MoneyTables), ", "), from, target, left, backupKept))
	case switchover.StayUnchecked:
		s.finishFailed(job, jobs.FailureUpgradeDatabaseUnchecked, causeCode, fmt.Sprintf(
			"The update to %s did not complete (%s), and the updater could not check the database: %s. Without knowing whether the new version changed the database or moved money, neither restoring the backup nor switching back to %s is safe, so PayRam was left on %s%s. Do not restore the backup on your own. Contact PayRam support with the updater log; the backup is kept at %s.",
			target, reason, verdict.Reason, from, target, left, backupKept))
	case switchover.StayDeclined:
		s.finishFailed(job, jobs.FailureUpgradeRestoreDeclined, causeCode, fmt.Sprintf(
			"The update to %s did not complete (%s). The new version had changed the database, and the restore of the backup taken at %s was declined, so PayRam is stopped: the new version's container %s and the previous version's container %s are both stopped, and the database is as the new version left it. The backup is kept at %s. Follow the recovery steps.",
			target, reason, takenAt, recovery.ShellQuote(verdict.Container), recovery.ShellQuote(switchover.PreviousName(job.ContainerName)), backupKept))
	case switchover.StopRestoreUncertain:
		s.finishFailed(job, jobs.FailureUpgradeRestoreFailed, causeCode, fmt.Sprintf(
			"The update to %s did not complete (%s). The new version had changed the database, so the backup taken at %s had to be restored before switching back to %s, but the restore was cut off while committing and it is not certain whether the database was replaced (%v). Both versions were left stopped: neither may run before the database is checked. Contact PayRam support with the updater log now; do not start PayRam or restore anything on your own.",
			target, reason, takenAt, from, err))
	default: // switchover.StayRestoreFailed
		s.finishFailed(job, jobs.FailureUpgradeRestoreFailed, causeCode, fmt.Sprintf(
			"The update to %s did not complete (%s). The new version had changed the database, so the backup taken at %s had to be restored before switching back to %s, but the restore failed and nothing was changed (%v). The database is as the new version left it, so PayRam was left on %s%s. Contact PayRam support with the updater log; do not restore the backup on your own.",
			target, reason, takenAt, from, err, target, left))
	}
	return false, false
}

// versionNames are the versions a job's messages name.
func versionNames(job *jobs.Job) (from, target string) {
	from = job.FromVersion
	if from == "" {
		from = "the previous version"
	}
	return from, baseVersionTag(job.ResolvedTarget)
}

// formatBackupTime is how a message names the backup's time.
func formatBackupTime(t time.Time) string {
	if t.IsZero() {
		return "the start of the update"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// describeContainer names the container PayRam was left on, and whether it
// runs, when there is one.
func describeContainer(ctx context.Context, sw *switchover.Switch, name string) string {
	if name == "" {
		return ""
	}
	running := "stopped"
	if state, err := sw.Docker.State(ctx, name); err == nil && state.Running {
		running = "running"
	}
	return fmt.Sprintf(" (container %s, %s)", recovery.ShellQuote(name), running)
}

// askToRestore asks the person following an attended update whether to
// restore the backup and switch back, and returns the answer. An unattended
// update (dashboard, auto-update, a repair after a restart), a question no
// one answers within restoreQuestionWait, and an updater that is stopping
// all take the default: restore. No person can be asked while PayRam is down
// for anyone but the one at the command line.
func (s *Server) askToRestore(job *jobs.Job, text string) bool {
	if !job.Attended || s.repairing.Load() {
		// A question an updater that stopped had asked is answered by no one.
		job.Question = nil
		return true
	}
	wait := s.questionWait
	if wait <= 0 {
		wait = restoreQuestionWait
	}
	question := &jobs.Question{
		ID:       fmt.Sprintf("%s-restore-%d", job.JobID, time.Now().UnixNano()),
		Text:     text,
		Deadline: time.Now().Add(wait).UTC(),
	}
	answers := s.answers.open(question.ID)
	defer s.answers.close()

	job.Question = question
	s.setProgress(job, jobs.JobStateVerifying, "Waiting for an answer at the command line: restore the backup and switch back?")
	s.jobStore.AppendLog("Asking at the command line before restoring the backup: " + text)
	defer func() {
		job.Question = nil
		s.setProgress(job, jobs.JobStateVerifying, job.Message)
	}()

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case restore := <-answers:
			if restore {
				s.jobStore.AppendLog("Answered yes: restoring the backup")
			} else {
				s.jobStore.AppendLog("Answered no: the backup is not restored")
			}
			return restore
		case <-deadline.C:
			s.jobStore.AppendLog(fmt.Sprintf("No answer within %s: restoring the backup (the default)", wait))
			return true
		case <-tick.C:
			if s.shuttingDown.Load() {
				s.jobStore.AppendLog("The updater is stopping: restoring the backup (the default) instead of waiting for an answer")
				return true
			}
		}
	}
}

// answerBox hands an answer to the one question an update can wait on.
type answerBox struct {
	mu sync.Mutex
	id string
	ch chan bool
}

func (b *answerBox) open(id string) <-chan bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.id, b.ch = id, make(chan bool, 1)
	return b.ch
}

func (b *answerBox) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.id, b.ch = "", nil
}

// deliver passes an answer to the waiting question with this ID. It reports
// false when no such question waits (answered already, or past its deadline).
func (b *answerBox) deliver(id string, restore bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch == nil || b.id != id {
		return false
	}
	select {
	case b.ch <- restore:
		return true
	default:
		return false
	}
}

// switchDatabase reaches PayRam's database for a switch back the way the
// pre-update backup did (backup.ContainerBackupExecutor), so the check and
// the restore work on the database that backup came from.
type switchDatabase struct {
	containers *backup.ContainerBackupExecutor
	restorer   *backup.Manager
}

func (d switchDatabase) IsBundled(ctx context.Context, container string) (bool, error) {
	target, err := d.containers.ResolveDatabase(ctx, container)
	if err != nil {
		return false, err
	}
	return target.Context.Mode == dbexec.DBModeInContainer, nil
}

func (d switchDatabase) ReadState(ctx context.Context, container string) (*jobs.DatabaseState, error) {
	return d.containers.ReadDatabaseState(ctx, container)
}

func (d switchDatabase) RestoreBackup(ctx context.Context, container, backupPath string) (string, error) {
	target, err := d.containers.ResolveDatabase(ctx, container)
	if err != nil {
		return "", err
	}
	result, err := d.restorer.RestoreBackup(ctx, backupPath, backup.RestoreOptions{
		Confirmed: true,
		// The update holds the container lock already.
		ContainerLockHeld: true,
		Database:          target,
	})
	if err != nil {
		return "", err
	}
	return result.SafetyBackupPath, nil
}

// supervisorPrograms runs PayRam's programs with supervisorctl.
type supervisorPrograms struct {
	s *Server
}

// Running lists every PayRam program that runs, starts, or will be started
// again by supervisord (BACKOFF). SUPERVISOR_INCLUDE does not narrow it:
// while the database is checked and restored, nothing of the new version may
// be able to send a payout.
func (p supervisorPrograms) Running(ctx context.Context, container string) ([]string, error) {
	running, err := p.s.listAppPrograms(ctx, container, nil, "RUNNING", "STARTING", "BACKOFF")
	if errors.Is(err, errSupervisorUnavailable) {
		return nil, switchover.ErrNoSupervisor
	}
	return running, err
}

func (p supervisorPrograms) Stop(ctx context.Context, container string, programs []string) error {
	return p.s.supervisorctlStop(ctx, container, programs)
}

func (p supervisorPrograms) Start(ctx context.Context, container string, programs []string) error {
	return p.s.supervisorctlStart(ctx, container, programs)
}
