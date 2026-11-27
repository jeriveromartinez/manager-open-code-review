package reviewbench

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnknownSeverity rejects severities outside the panel's vocabulary. The
// benchmark reuses that vocabulary instead of inventing a second scale.
var ErrUnknownSeverity = errors.New("unknown severity")

// NormalizeSeverity maps the aliases the panel already understands
// (severityRank in internal/review) onto its canonical four levels. Unknown
// values are rejected instead of silently defaulting to major, so an import
// cannot hide a vocabulary mismatch.
func NormalizeSeverity(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "blocker", "critical":
		return "blocker", nil
	case "major", "high":
		return "major", nil
	case "minor", "medium":
		return "minor", nil
	case "nit", "low":
		return "nit", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownSeverity, raw)
	}
}

// IsCanonicalSeverity reports whether raw is exactly one of the panel's four
// stored severities. Ground truth uses only canonical values.
func IsCanonicalSeverity(raw string) bool {
	switch raw {
	case "blocker", "major", "minor", "nit":
		return true
	}
	return false
}

func severityRank(severity string) int {
	switch severity {
	case "blocker":
		return 4
	case "major":
		return 3
	case "minor":
		return 2
	case "nit":
		return 1
	}
	return 0
}

// IsSevere reports whether a canonical severity is major or blocker, the class
// used by the severe-defect detection metric and by the false-block rule.
func IsSevere(severity string) bool {
	return severityRank(severity) >= severityRank("major")
}
