package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/payram/payram-updater/internal/backup"
)

var (
	upgradeBackup = backup.BackupListItem{File: "/b/payram-backup-20260101-100000-1.7.0-to-1.8.0.dump", FromVersion: "1.7.0", ToVersion: "1.8.0"}
	manualBackup  = backup.BackupListItem{File: "/b/payram-backup-20260101-100000-manual-to-manual.dump", FromVersion: "manual", ToVersion: "manual"}
)

func prompt(item backup.BackupListItem, mode restoreMode, yes, interactive bool, input string) (restoreDecision, string) {
	var out bytes.Buffer
	d := decideRestore(restorePrompt{
		Backup:       item,
		Mode:         mode,
		Yes:          yes,
		Interactive:  interactive,
		In:           bufio.NewReader(strings.NewReader(input)),
		Out:          &out,
		SafetyBackup: true,
	})
	return d, out.String()
}

func TestDecideRestore_EverythingButAnExplicitChoiceCancels(t *testing.T) {
	cases := map[string]string{
		"end of input":         "",
		"enter":                "\n",
		"n":                    "n\n",
		"no":                   "no\n",
		"q":                    "q\n",
		"unrecognised":         "3\n",
		"choice then eof":      "1\n",
		"choice then enter":    "1\n\n",
		"choice then y":        "2\ny\n",
		"choice then YES typo": "2\nyess\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			d, _ := prompt(upgradeBackup, "", false, true, input)
			if d.Proceed {
				t.Fatalf("input %q must cancel, got mode %s", input, d.Mode)
			}
			if d.ExitCode != 0 || !strings.Contains(d.Message, "nothing was changed") {
				t.Errorf("cancel should exit 0 and say nothing changed: %+v", d)
			}
		})
	}
}

func TestDecideRestore_ExplicitChoices(t *testing.T) {
	d, out := prompt(upgradeBackup, "", false, true, "1\nyes\n")
	if !d.Proceed || d.Mode != restoreModeDBOnly {
		t.Fatalf("1 + yes: got %+v", d)
	}
	if !strings.Contains(out, "[c] Cancel (default)") {
		t.Errorf("the prompt must show cancel as the default:\n%s", out)
	}
	d, _ = prompt(upgradeBackup, "", false, true, "2\nYES\n")
	if !d.Proceed || d.Mode != restoreModeFull {
		t.Fatalf("2 + YES: got %+v", d)
	}
}

func TestDecideRestore_YesNeedsAnExplicitMode(t *testing.T) {
	// --yes with --mode never reads stdin.
	d, _ := prompt(upgradeBackup, restoreModeDBOnly, true, false, "")
	if !d.Proceed || d.Mode != restoreModeDBOnly {
		t.Fatalf("--yes --mode db-only: got %+v", d)
	}
	d, _ = prompt(upgradeBackup, restoreModeFull, true, false, "")
	if !d.Proceed || d.Mode != restoreModeFull {
		t.Fatalf("--yes --mode full: got %+v", d)
	}

	// --yes alone without a terminal cancels with instructions.
	d, _ = prompt(upgradeBackup, "", true, false, "2\n")
	if d.Proceed || d.ExitCode != 2 || !strings.Contains(d.Message, "--mode db-only") {
		t.Fatalf("--yes without --mode and without a terminal must cancel: %+v", d)
	}

	// --yes alone on a terminal still asks for the mode and the typed yes.
	d, _ = prompt(upgradeBackup, "", true, true, "1\n")
	if d.Proceed {
		t.Fatalf("--yes without --mode must still require typing yes: %+v", d)
	}

	// A mode without --yes on a terminal still needs the typed yes.
	d, _ = prompt(upgradeBackup, restoreModeFull, false, true, "\n")
	if d.Proceed {
		t.Fatalf("--mode full without --yes must require typing yes: %+v", d)
	}
	d, _ = prompt(upgradeBackup, restoreModeFull, false, true, "yes\n")
	if !d.Proceed || d.Mode != restoreModeFull {
		t.Fatalf("--mode full + typed yes: got %+v", d)
	}

	// No terminal and no flags: cancel.
	d, _ = prompt(upgradeBackup, "", false, false, "")
	if d.Proceed || d.ExitCode != 2 {
		t.Fatalf("closed stdin must cancel: %+v", d)
	}
}

func TestDecideRestore_FullRecoveryRefusedWithoutRecordedVersions(t *testing.T) {
	for _, item := range []backup.BackupListItem{
		manualBackup,
		{FromVersion: "unknown", ToVersion: "1.8.0"},
		{FromVersion: "unknown", ToVersion: "unknown"},
	} {
		d, _ := prompt(item, restoreModeFull, true, false, "")
		if d.Proceed || d.ExitCode != 1 || !strings.Contains(d.Message, "--mode db-only") {
			t.Fatalf("%s->%s: full recovery must be refused, got %+v", item.FromVersion, item.ToVersion, d)
		}
	}
}

func TestDecideRestore_ManualBackupOnlyOffersDatabaseRestore(t *testing.T) {
	d, out := prompt(manualBackup, "", false, true, "yes\n")
	if !d.Proceed || d.Mode != restoreModeDBOnly {
		t.Fatalf("manual backup + yes: got %+v", d)
	}
	if strings.Contains(out, "[2]") {
		t.Errorf("a manual backup must not offer a container rollback:\n%s", out)
	}
	d, _ = prompt(manualBackup, "", false, true, "")
	if d.Proceed {
		t.Fatal("end of input must cancel")
	}
}

func TestParseRestoreMode(t *testing.T) {
	cases := []struct {
		flag    string
		full    bool
		want    restoreMode
		wantErr bool
	}{
		{"", false, "", false},
		{"db-only", false, restoreModeDBOnly, false},
		{"FULL", false, restoreModeFull, false},
		{"", true, restoreModeFull, false},
		{"full", true, restoreModeFull, false},
		{"db-only", true, "", true},
		{"everything", false, "", true},
	}
	for _, tc := range cases {
		got, err := parseRestoreMode(tc.flag, tc.full)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("parseRestoreMode(%q, %v) = %q, %v", tc.flag, tc.full, got, err)
		}
	}
}

func TestPrintRestoreSummary(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
	item := upgradeBackup
	item.CreatedAt = "2026-01-01T10:00:00Z"
	printRestoreSummary(&out, item, now)
	for _, want := range []string{item.File, "(3h ago)", "from 1.7.0 to 1.8.0", "every payment, deposit and payout recorded after this backup"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary missing %q:\n%s", want, out.String())
		}
	}
}

func TestFormatAge(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "less than a minute ago",
		42 * time.Minute: "42m ago",
		3 * time.Hour:    "3h ago",
		72 * time.Hour:   "3d ago",
	}
	for d, want := range cases {
		if got := formatAge(d); got != want {
			t.Errorf("formatAge(%s) = %q, want %q", d, got, want)
		}
	}
}
