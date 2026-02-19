package diskspace

import (
	"strings"
	"testing"
)

func TestCheckAvailableSpace_RootAlwaysExists(t *testing.T) {
	requirements := []SpaceRequirement{
		{
			Path:          "/",
			MinFreeGB:     0.001, // 1MB - should always pass
			PurposeDesc:   "System root",
			FailIfMissing: true,
		},
	}

	results, allSufficient := CheckAvailableSpace(requirements)

	if !allSufficient {
		t.Error("Expected root directory to have sufficient space")
	}

	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}

	if results[0].PathNotExists {
		t.Error("Root directory should exist")
	}

	if !results[0].Sufficient {
		t.Errorf("Root directory should have at least 1MB free, got %.2f GB", results[0].AvailableGB)
	}
}

func TestCheckAvailableSpace_NonexistentPath(t *testing.T) {
	requirements := []SpaceRequirement{
		{
			Path:          "/nonexistent/path/that/does/not/exist",
			MinFreeGB:     1.0,
			PurposeDesc:   "Nonexistent",
			FailIfMissing: true,
		},
	}

	results, allSufficient := CheckAvailableSpace(requirements)

	if allSufficient {
		t.Error("Expected check to fail for nonexistent path")
	}

	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}

	if !results[0].PathNotExists {
		t.Error("PathNotExists should be true for nonexistent path")
	}
}

func TestCheckAvailableSpace_FailIfMissingFalse(t *testing.T) {
	requirements := []SpaceRequirement{
		{
			Path:          "/nonexistent/path/that/does/not/exist",
			MinFreeGB:     1.0,
			PurposeDesc:   "Nonexistent",
			FailIfMissing: false, // Should not fail overall check
		},
	}

	results, allSufficient := CheckAvailableSpace(requirements)

	// Should pass because FailIfMissing is false
	if !allSufficient {
		t.Error("Expected check to pass when FailIfMissing is false")
	}

	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}

	if !results[0].PathNotExists {
		t.Error("PathNotExists should be true for nonexistent path")
	}
}

func TestFormatCheckResults_Sufficient(t *testing.T) {
	results := []CheckResult{
		{
			Path:        "/tmp",
			AvailableGB: 10.5,
			RequiredGB:  5.0,
			Sufficient:  true,
			PurposeDesc: "Temp directory",
		},
	}

	formatted := FormatCheckResults(results)

	if len(formatted) != 1 {
		t.Fatalf("Expected 1 formatted line, got %d", len(formatted))
	}

	if !strings.Contains(formatted[0], "✓") {
		t.Errorf("Expected success symbol ✓ in output, got: %s", formatted[0])
	}
}

func TestFormatCheckResults_Insufficient(t *testing.T) {
	results := []CheckResult{
		{
			Path:        "/tmp",
			AvailableGB: 2.5,
			RequiredGB:  5.0,
			Sufficient:  false,
			PurposeDesc: "Temp directory",
		},
	}

	formatted := FormatCheckResults(results)

	if len(formatted) != 1 {
		t.Fatalf("Expected 1 formatted line, got %d", len(formatted))
	}

	if !strings.Contains(formatted[0], "✗") {
		t.Errorf("Expected failure symbol ✗ in output, got: %s", formatted[0])
	}

	if !strings.Contains(formatted[0], "short") {
		t.Errorf("Expected 'short' in insufficient space message, got: %s", formatted[0])
	}
}

func TestFormatCheckResults_PathNotExists(t *testing.T) {
	results := []CheckResult{
		{
			Path:          "/nonexistent",
			PathNotExists: true,
			PurposeDesc:   "Nonexistent",
		},
	}

	formatted := FormatCheckResults(results)

	if len(formatted) != 1 {
		t.Fatalf("Expected 1 formatted line, got %d", len(formatted))
	}

	if !strings.Contains(formatted[0], "path does not exist") {
		t.Errorf("Expected 'path does not exist' message, got: %s", formatted[0])
	}
}
