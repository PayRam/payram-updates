package diskspace

import (
	"fmt"
	"os"
	"syscall"
)

// SpaceRequirement defines minimum free space needed for a directory.
type SpaceRequirement struct {
	Path          string
	MinFreeGB     float64
	PurposeDesc   string
	FailIfMissing bool // If true, fail if path doesn't exist; if false, skip check
}

// CheckResult contains the result of a disk space check.
type CheckResult struct {
	Path          string
	AvailableGB   float64
	RequiredGB    float64
	Sufficient    bool
	PurposeDesc   string
	ErrorMessage  string
	PathNotExists bool
}

// CheckAvailableSpace checks if sufficient disk space is available for the given requirements.
// Returns slice of check results and overall success boolean.
func CheckAvailableSpace(requirements []SpaceRequirement) ([]CheckResult, bool) {
	results := make([]CheckResult, 0, len(requirements))
	allSufficient := true

	for _, req := range requirements {
		result := checkSinglePath(req)
		results = append(results, result)

		// Path doesn't exist
		if result.PathNotExists {
			if req.FailIfMissing {
				allSufficient = false
			}
			continue
		}

		// Space check failed
		if !result.Sufficient {
			allSufficient = false
		}
	}

	return results, allSufficient
}

// checkSinglePath checks disk space for a single path.
func checkSinglePath(req SpaceRequirement) CheckResult {
	result := CheckResult{
		Path:        req.Path,
		RequiredGB:  req.MinFreeGB,
		PurposeDesc: req.PurposeDesc,
	}

	// Check if path exists
	if _, err := os.Stat(req.Path); os.IsNotExist(err) {
		result.PathNotExists = true
		result.ErrorMessage = fmt.Sprintf("path does not exist: %s", req.Path)
		result.Sufficient = false
		return result
	}

	// Get filesystem stats
	var stat syscall.Statfs_t
	if err := syscall.Statfs(req.Path, &stat); err != nil {
		result.ErrorMessage = fmt.Sprintf("failed to stat filesystem: %v", err)
		result.Sufficient = false
		return result
	}

	// Calculate available space in GB
	// Available blocks * block size / (1024^3)
	availableBytes := stat.Bavail * uint64(stat.Bsize)
	availableGB := float64(availableBytes) / (1024 * 1024 * 1024)

	result.AvailableGB = availableGB
	result.Sufficient = availableGB >= req.MinFreeGB

	return result
}

// FormatCheckResults formats check results into human-readable strings.
func FormatCheckResults(results []CheckResult) []string {
	formatted := make([]string, 0, len(results))

	for _, r := range results {
		if r.PathNotExists {
			formatted = append(formatted, fmt.Sprintf("  ✗ %s: path does not exist (%s)", r.PurposeDesc, r.Path))
			continue
		}

		if r.ErrorMessage != "" && !r.PathNotExists {
			formatted = append(formatted, fmt.Sprintf("  ✗ %s: %s", r.PurposeDesc, r.ErrorMessage))
			continue
		}

		if r.Sufficient {
			formatted = append(formatted, fmt.Sprintf("  ✓ %s: %.2f GB available (required: %.2f GB)", r.PurposeDesc, r.AvailableGB, r.RequiredGB))
		} else {
			formatted = append(formatted, fmt.Sprintf("  ✗ %s: %.2f GB available, need %.2f GB (%.2f GB short)",
				r.PurposeDesc, r.AvailableGB, r.RequiredGB, r.RequiredGB-r.AvailableGB))
		}
	}

	return formatted
}
