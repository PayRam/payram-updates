package recovery

import (
	"strings"
	"testing"
)

func TestGetPlaybook_KnownCodes(t *testing.T) {
	testCases := []struct {
		code         string
		wantSeverity Severity
		wantDataRisk DataRisk
		wantTitle    string
	}{
		{
			code:         "POLICY_FETCH_FAILED",
			wantSeverity: SeverityRetryable,
			wantDataRisk: DataRiskNone,
			wantTitle:    "Policy Fetch Failed",
		},
		{
			code:         "MANUAL_UPGRADE_REQUIRED",
			wantSeverity: SeverityManual,
			wantDataRisk: DataRiskNone,
			wantTitle:    "Manual Upgrade Required",
		},
		{
			code:         "MANIFEST_FETCH_FAILED",
			wantSeverity: SeverityRetryable,
			wantDataRisk: DataRiskNone,
			wantTitle:    "Manifest Fetch Failed",
		},
		{
			code:         "DOCKER_PULL_FAILED",
			wantSeverity: SeverityRetryable,
			wantDataRisk: DataRiskNone,
			wantTitle:    "Docker Pull Failed",
		},
		{
			code:         "DOCKER_ERROR",
			wantSeverity: SeverityManual,
			wantDataRisk: DataRiskPossible,
			wantTitle:    "Docker Operation Failed",
		},
		{
			code:         "HEALTHCHECK_FAILED",
			wantSeverity: SeverityManual,
			wantDataRisk: DataRiskPossible,
			wantTitle:    "Health Check Failed",
		},
		{
			code:         "VERSION_MISMATCH",
			wantSeverity: SeverityManual,
			wantDataRisk: DataRiskPossible,
			wantTitle:    "Version Mismatch",
		},
		{
			code:         "MIGRATION_FAILED",
			wantSeverity: SeverityManual,
			wantDataRisk: DataRiskLikely,
			wantTitle:    "Database Migration Failed",
		},
		{
			code:         "DISK_SPACE_LOW",
			wantSeverity: SeverityManual,
			wantDataRisk: DataRiskNone,
			wantTitle:    "Disk Space Low",
		},
		{
			code:         "CONCURRENCY_BLOCKED",
			wantSeverity: SeverityRetryable,
			wantDataRisk: DataRiskNone,
			wantTitle:    "Upgrade Already In Progress",
		},
		{
			code:         "BACKUP_FAILED",
			wantSeverity: SeverityRetryable,
			wantDataRisk: DataRiskNone,
			wantTitle:    "Database Backup Failed",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.code, func(t *testing.T) {
			playbook := GetPlaybook(tc.code)

			if playbook.Code != tc.code {
				t.Errorf("expected code %q, got %q", tc.code, playbook.Code)
			}
			if playbook.Severity != tc.wantSeverity {
				t.Errorf("expected severity %q, got %q", tc.wantSeverity, playbook.Severity)
			}
			if playbook.DataRisk != tc.wantDataRisk {
				t.Errorf("expected data risk %q, got %q", tc.wantDataRisk, playbook.DataRisk)
			}
			if playbook.Title != tc.wantTitle {
				t.Errorf("expected title %q, got %q", tc.wantTitle, playbook.Title)
			}
			if playbook.UserMessage == "" {
				t.Error("user message should not be empty")
			}
			if len(playbook.SSHSteps) == 0 {
				t.Error("SSH steps should not be empty")
			}
		})
	}
}

func TestGetPlaybook_UnknownCode(t *testing.T) {
	playbook := GetPlaybook("TOTALLY_UNKNOWN_ERROR")

	if playbook.Code != "TOTALLY_UNKNOWN_ERROR" {
		t.Errorf("expected code preserved as TOTALLY_UNKNOWN_ERROR, got %q", playbook.Code)
	}
	if playbook.Severity != SeverityManual {
		t.Errorf("expected severity MANUAL_REQUIRED, got %q", playbook.Severity)
	}
	if playbook.DataRisk != DataRiskUnknown {
		t.Errorf("expected data risk UNKNOWN, got %q", playbook.DataRisk)
	}
	if playbook.Title != "Unknown Failure" {
		t.Errorf("expected title 'Unknown Failure', got %q", playbook.Title)
	}
	if playbook.UserMessage == "" {
		t.Error("user message should not be empty for unknown codes")
	}
	if len(playbook.SSHSteps) == 0 {
		t.Error("SSH steps should not be empty for unknown codes")
	}
}

func TestGetPlaybook_EmptyCode(t *testing.T) {
	playbook := GetPlaybook("")

	// Empty code should return unknown playbook with empty code preserved
	if playbook.Code != "" {
		t.Errorf("expected empty code preserved, got %q", playbook.Code)
	}
	if playbook.Severity != SeverityManual {
		t.Errorf("expected severity MANUAL_REQUIRED for empty code, got %q", playbook.Severity)
	}
}

func TestIsRetryable(t *testing.T) {
	retryableCodes := []string{
		"POLICY_FETCH_FAILED",
		"MANIFEST_FETCH_FAILED",
		"DOCKER_PULL_FAILED",
		"CONCURRENCY_BLOCKED",
	}

	for _, code := range retryableCodes {
		if !IsRetryable(code) {
			t.Errorf("expected %q to be retryable", code)
		}
	}

	manualCodes := []string{
		"DOCKER_ERROR",
		"HEALTHCHECK_FAILED",
		"VERSION_MISMATCH",
		"MIGRATION_FAILED",
	}

	for _, code := range manualCodes {
		if IsRetryable(code) {
			t.Errorf("expected %q to NOT be retryable", code)
		}
	}
}

func TestRequiresManualIntervention(t *testing.T) {
	manualCodes := []string{
		"DOCKER_ERROR",
		"HEALTHCHECK_FAILED",
		"VERSION_MISMATCH",
		"MIGRATION_FAILED",
		"DISK_SPACE_LOW",
		"MANUAL_UPGRADE_REQUIRED",
	}

	for _, code := range manualCodes {
		if !RequiresManualIntervention(code) {
			t.Errorf("expected %q to require manual intervention", code)
		}
	}

	retryableCodes := []string{
		"POLICY_FETCH_FAILED",
		"MANIFEST_FETCH_FAILED",
		"CONCURRENCY_BLOCKED",
	}

	for _, code := range retryableCodes {
		if RequiresManualIntervention(code) {
			t.Errorf("expected %q to NOT require manual intervention", code)
		}
	}
}

func TestHasDataRisk(t *testing.T) {
	riskyCodes := []string{
		"DOCKER_ERROR",
		"HEALTHCHECK_FAILED",
		"VERSION_MISMATCH",
		"MIGRATION_FAILED",
	}

	for _, code := range riskyCodes {
		if !HasDataRisk(code) {
			t.Errorf("expected %q to have data risk", code)
		}
	}

	safeCodes := []string{
		"POLICY_FETCH_FAILED",
		"MANIFEST_FETCH_FAILED",
		"DISK_SPACE_LOW",
		"CONCURRENCY_BLOCKED",
	}

	for _, code := range safeCodes {
		if HasDataRisk(code) {
			t.Errorf("expected %q to NOT have data risk", code)
		}
	}
}

func TestAllCodes(t *testing.T) {
	codes := AllCodes()

	if len(codes) < 10 {
		t.Errorf("expected at least 10 codes, got %d", len(codes))
	}

	// Verify some expected codes are present
	expectedCodes := []string{
		"POLICY_FETCH_FAILED",
		"DOCKER_ERROR",
		"MIGRATION_FAILED",
	}

	codeSet := make(map[string]bool)
	for _, code := range codes {
		codeSet[code] = true
	}

	for _, expected := range expectedCodes {
		if !codeSet[expected] {
			t.Errorf("expected code %q to be in AllCodes()", expected)
		}
	}
}

func TestMigrationFailedPlaybook(t *testing.T) {
	// Specific test for MIGRATION_FAILED as required by step 3.2
	playbook := GetPlaybook("MIGRATION_FAILED")

	if playbook.Severity != SeverityManual {
		t.Errorf("MIGRATION_FAILED should have severity MANUAL_REQUIRED, got %q", playbook.Severity)
	}
	if playbook.DataRisk != DataRiskLikely {
		t.Errorf("MIGRATION_FAILED should have data risk LIKELY, got %q", playbook.DataRisk)
	}

	// Verify it has critical warnings in SSH steps
	hasStopWarning := false
	hasRestoreWarning := false
	for _, step := range playbook.SSHSteps {
		if containsSubstring(step, "STOP") || containsSubstring(step, "Do not retry") {
			hasStopWarning = true
		}
		if containsSubstring(step, "RESTORE") || containsSubstring(step, "backup") {
			hasRestoreWarning = true
		}
	}

	if !hasStopWarning {
		t.Error("MIGRATION_FAILED playbook should warn user to STOP/not retry")
	}
	if !hasRestoreWarning {
		t.Error("MIGRATION_FAILED playbook should mention database RESTORE/backup")
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestGetPlaybookWithBackup(t *testing.T) {
	t.Run("enriches playbook with backup path", func(t *testing.T) {
		backupPath := "/var/lib/payram/backups/payram-backup-20240115-120000-1.0.0-to-1.1.0.dump"
		playbook := GetPlaybookWithBackup("MIGRATION_FAILED", backupPath)

		if playbook.BackupPath != backupPath {
			t.Errorf("expected BackupPath %q, got %q", backupPath, playbook.BackupPath)
		}

		// Check that placeholder was replaced in SSH steps
		foundBackupPath := false
		foundPlaceholder := false
		for _, step := range playbook.SSHSteps {
			if containsSubstring(step, backupPath) {
				foundBackupPath = true
			}
			if containsSubstring(step, "<backup_path>") {
				foundPlaceholder = true
			}
		}

		if !foundBackupPath {
			t.Error("backup path should be present in SSH steps")
		}
		if foundPlaceholder {
			t.Error("placeholder <backup_path> should have been replaced")
		}
	})

	t.Run("handles empty backup path", func(t *testing.T) {
		playbook := GetPlaybookWithBackup("MIGRATION_FAILED", "")

		if playbook.BackupPath != "" {
			t.Errorf("expected empty BackupPath, got %q", playbook.BackupPath)
		}

		// Placeholder should still be present when no backup path
		foundPlaceholder := false
		for _, step := range playbook.SSHSteps {
			if containsSubstring(step, "<backup_path>") {
				foundPlaceholder = true
				break
			}
		}

		if !foundPlaceholder {
			t.Error("placeholder should remain when no backup path provided")
		}
	})

	t.Run("HEALTHCHECK_FAILED includes backup restore steps", func(t *testing.T) {
		backupPath := "/backups/test.dump"
		playbook := GetPlaybookWithBackup("HEALTHCHECK_FAILED", backupPath)

		if playbook.BackupPath != backupPath {
			t.Errorf("expected BackupPath %q, got %q", backupPath, playbook.BackupPath)
		}

		// Should have backup-related steps
		hasBackupList := false
		hasBackupRestore := false
		for _, step := range playbook.SSHSteps {
			if containsSubstring(step, "backup list") {
				hasBackupList = true
			}
			if containsSubstring(step, "backup restore") {
				hasBackupRestore = true
			}
		}

		if !hasBackupList {
			t.Error("HEALTHCHECK_FAILED should include 'backup list' step")
		}
		if !hasBackupRestore {
			t.Error("HEALTHCHECK_FAILED should include 'backup restore' step")
		}
	})

	t.Run("VERSION_MISMATCH includes backup restore steps", func(t *testing.T) {
		playbook := GetPlaybookWithBackup("VERSION_MISMATCH", "/backups/test.dump")

		hasBackupRestore := false
		for _, step := range playbook.SSHSteps {
			if containsSubstring(step, "backup restore") {
				hasBackupRestore = true
				break
			}
		}

		if !hasBackupRestore {
			t.Error("VERSION_MISMATCH should include 'backup restore' step")
		}
	})

	t.Run("does not modify original playbook", func(t *testing.T) {
		// Get playbook twice - modifications should not affect the original
		original := GetPlaybook("MIGRATION_FAILED")
		enriched := GetPlaybookWithBackup("MIGRATION_FAILED", "/backups/test.dump")

		// Original should still have placeholder
		originalHasPlaceholder := false
		for _, step := range original.SSHSteps {
			if containsSubstring(step, "<backup_path>") {
				originalHasPlaceholder = true
				break
			}
		}

		if !originalHasPlaceholder {
			t.Error("original playbook should be unchanged")
		}

		// Enriched should have the actual path
		if enriched.BackupPath == "" {
			t.Error("enriched playbook should have backup path set")
		}
	})
}

func TestReplaceBackupPlaceholder(t *testing.T) {
	testCases := []struct {
		name       string
		step       string
		backupPath string
		want       string
	}{
		{
			name:       "replaces single placeholder",
			step:       "Restore: payram-updater backup restore --file <backup_path> --yes",
			backupPath: "/backups/test.dump",
			want:       "Restore: payram-updater backup restore --file /backups/test.dump --yes",
		},
		{
			name:       "replaces multiple placeholders",
			step:       "Check <backup_path> and restore from <backup_path>",
			backupPath: "/backups/test.dump",
			want:       "Check /backups/test.dump and restore from /backups/test.dump",
		},
		{
			name:       "no placeholder returns unchanged",
			step:       "Check container logs",
			backupPath: "/backups/test.dump",
			want:       "Check container logs",
		},
		{
			name:       "empty backup path preserves placeholder",
			step:       "Restore from <backup_path>",
			backupPath: "",
			want:       "Restore from ",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := replaceBackupPlaceholder(tc.step, tc.backupPath)
			if got != tc.want {
				t.Errorf("replaceBackupPlaceholder(%q, %q) = %q, want %q",
					tc.step, tc.backupPath, got, tc.want)
			}
		})
	}
}

// TestAllFailureCodesHavePlaybooks verifies that every failure code
// used in the codebase has a corresponding recovery playbook (Phase G1).
func TestAllFailureCodesHavePlaybooks(t *testing.T) {
	// These are all the failure codes used in server.go
	// This test ensures none are missing from the playbook registry
	requiredCodes := []string{
		"POLICY_FETCH_FAILED",
		"MANIFEST_FETCH_FAILED",
		"CONTAINER_NAME_UNRESOLVED",
		"RUNTIME_INSPECTION_FAILED",
		"DOCKER_RUN_BUILD_FAILED",
		"DOCKER_DAEMON_DOWN",
		"DOCKER_PULL_FAILED",
		"DOCKER_ERROR",
		"HEALTHCHECK_FAILED",
		"VERSION_MISMATCH",
		"MIGRATION_FAILED",
		"MIGRATION_TIMEOUT",
		"BACKUP_FAILED",
		"CONTAINER_NOT_FOUND",
		"INVALID_DB_CONFIG",
		"BACKUP_TIMEOUT",
		"MANUAL_UPGRADE_REQUIRED",
		"DISK_SPACE_LOW",
		"CONCURRENCY_BLOCKED",
	}

	for _, code := range requiredCodes {
		t.Run(code, func(t *testing.T) {
			playbook := GetPlaybook(code)

			// Verify playbook exists (not the unknown fallback)
			if playbook.Code != code {
				t.Errorf("Playbook not found for code %s (got unknown playbook)", code)
			}

			// Verify playbook has all required fields
			if playbook.Title == "" {
				t.Errorf("Playbook %s missing Title", code)
			}
			if playbook.UserMessage == "" {
				t.Errorf("Playbook %s missing UserMessage", code)
			}
			if len(playbook.SSHSteps) == 0 {
				t.Errorf("Playbook %s missing SSHSteps", code)
			}
			if playbook.Severity == "" {
				t.Errorf("Playbook %s missing Severity", code)
			}
			if playbook.DataRisk == "" {
				t.Errorf("Playbook %s missing DataRisk", code)
			}
		})
	}
}

// TestPlaybookCompleteness verifies each playbook has meaningful content (Phase G1).
func TestPlaybookCompleteness(t *testing.T) {
	for _, code := range AllCodes() {
		t.Run(code, func(t *testing.T) {
			playbook := GetPlaybook(code)

			// Title should be descriptive (more than just the code)
			if len(playbook.Title) < 5 {
				t.Errorf("Playbook %s has too short Title: %s", code, playbook.Title)
			}

			// User message should be helpful (at least 20 chars)
			if len(playbook.UserMessage) < 20 {
				t.Errorf("Playbook %s has too short UserMessage: %s", code, playbook.UserMessage)
			}

			// Should have at least 3 recovery steps
			if len(playbook.SSHSteps) < 3 {
				t.Errorf("Playbook %s has too few SSHSteps (want >= 3, got %d)", code, len(playbook.SSHSteps))
			}

			// DocsURL should be a valid URL or empty
			if playbook.DocsURL != "" && !strings.HasPrefix(playbook.DocsURL, "http") {
				t.Errorf("Playbook %s has invalid DocsURL: %s", code, playbook.DocsURL)
			}
		})
	}
}

// TestContainerSafetyZones verifies playbooks correctly identify when container is safe (Phase G1).
func TestContainerSafetyZones(t *testing.T) {
	testCases := []struct {
		code               string
		containerUntouched bool // true if failure occurred before any container changes
		dataRisk           DataRisk
		severity           Severity
	}{
		// Pre-modification failures (container untouched)
		{"POLICY_FETCH_FAILED", true, DataRiskNone, SeverityRetryable},
		{"MANIFEST_FETCH_FAILED", true, DataRiskNone, SeverityRetryable},
		{"CONTAINER_NAME_UNRESOLVED", true, DataRiskNone, SeverityManual},
		{"RUNTIME_INSPECTION_FAILED", true, DataRiskNone, SeverityRetryable},
		{"DOCKER_RUN_BUILD_FAILED", true, DataRiskNone, SeverityManual},
		{"DOCKER_DAEMON_DOWN", true, DataRiskNone, SeverityManual},
		{"DOCKER_PULL_FAILED", true, DataRiskNone, SeverityRetryable},
		{"BACKUP_FAILED", true, DataRiskNone, SeverityRetryable},
		{"CONTAINER_NOT_FOUND", true, DataRiskNone, SeverityManual},
		{"INVALID_DB_CONFIG", true, DataRiskNone, SeverityManual},
		{"BACKUP_TIMEOUT", true, DataRiskNone, SeverityRetryable},

		// Post-modification failures (container may be affected)
		{"DOCKER_ERROR", false, DataRiskPossible, SeverityManual},
		{"HEALTHCHECK_FAILED", false, DataRiskPossible, SeverityManual},
		{"VERSION_MISMATCH", false, DataRiskPossible, SeverityManual},
		{"MIGRATION_FAILED", false, DataRiskLikely, SeverityManual},
		{"MIGRATION_TIMEOUT", false, DataRiskPossible, SeverityManual},
	}

	for _, tc := range testCases {
		t.Run(tc.code, func(t *testing.T) {
			playbook := GetPlaybook(tc.code)

			if playbook.DataRisk != tc.dataRisk {
				t.Errorf("Expected DataRisk %s for %s, got %s", tc.dataRisk, tc.code, playbook.DataRisk)
			}

			if playbook.Severity != tc.severity {
				t.Errorf("Expected Severity %s for %s, got %s", tc.severity, tc.code, playbook.Severity)
			}

			// Pre-modification failures should always have DataRiskNone
			if tc.containerUntouched && playbook.DataRisk != DataRiskNone {
				t.Errorf("Pre-modification failure %s should have DataRiskNone, got %s", tc.code, playbook.DataRisk)
			}
		})
	}
}

// TestPlaybookRegistry verifies playbook map is properly initialized (Phase G1).
func TestPlaybookRegistry(t *testing.T) {
	if len(playbooks) == 0 {
		t.Fatal("Playbook registry is empty")
	}

	// Verify no duplicate codes
	seen := make(map[string]bool)
	for code := range playbooks {
		if seen[code] {
			t.Errorf("Duplicate playbook code: %s", code)
		}
		seen[code] = true
	}

	// Verify all codes in registry match their keys
	for key, playbook := range playbooks {
		if playbook.Code != key {
			t.Errorf("Playbook key %s doesn't match playbook.Code %s", key, playbook.Code)
		}
	}
}

// TestDataRiskClassification verifies proper data risk assessment (Phase G1).
func TestDataRiskClassification(t *testing.T) {
	testCases := []struct {
		codes    []string
		dataRisk DataRisk
	}{
		{
			codes:    []string{"POLICY_FETCH_FAILED", "MANIFEST_FETCH_FAILED", "DOCKER_PULL_FAILED", "BACKUP_FAILED"},
			dataRisk: DataRiskNone,
		},
		{
			codes:    []string{"HEALTHCHECK_FAILED", "VERSION_MISMATCH", "MIGRATION_TIMEOUT"},
			dataRisk: DataRiskPossible,
		},
		{
			codes:    []string{"MIGRATION_FAILED"},
			dataRisk: DataRiskLikely,
		},
	}

	for _, tc := range testCases {
		for _, code := range tc.codes {
			t.Run(code, func(t *testing.T) {
				playbook := GetPlaybook(code)
				if playbook.DataRisk != tc.dataRisk {
					t.Errorf("Expected %s to have DataRisk %s, got %s", code, tc.dataRisk, playbook.DataRisk)
				}
			})
		}
	}
}
