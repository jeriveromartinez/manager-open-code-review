package reviewbench

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// normalizePath collapses the spellings an exporter may use for one file so
// matching, duplicate detection and citation lookups agree.
func normalizePath(value string) string {
	cleaned := path.Clean(filepath.ToSlash(strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")))
	if cleaned == "." {
		return ""
	}
	return cleaned
}

// normalizeText is the case- and whitespace-insensitive identity used for
// titles.
func normalizeText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

// normalizeAnchor collapses whitespace inside cited code.
func normalizeAnchor(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func normalizeSide(side string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(side)) {
	case "", "RIGHT":
		return "RIGHT", nil
	case "LEFT":
		return "LEFT", nil
	default:
		return "", fmt.Errorf("unknown citation side %q", side)
	}
}

// rangesNearby reports whether two ranges overlap or are separated by at most
// gap lines. It mirrors internal/review's dedupe contract.
func rangesNearby(aStart, aEnd, bStart, bEnd, gap int) bool {
	return bStart <= aEnd+gap+1 && aStart <= bEnd+gap+1
}

// matchFindings pairs each expected finding with the first unmatched observed
// finding that shares path, category and a nearby range. Severity does not
// decide the match; the caller compares it separately. It returns the observed
// index per expectation (-1 when missed) and which observed findings matched.
func matchFindings(expected []ExpectedFinding, actual []ObservedFinding, radius int) ([]int, []bool) {
	matches := make([]int, len(expected))
	for i := range matches {
		matches[i] = -1
	}
	actualMatched := make([]bool, len(actual))
	for i, want := range expected {
		wantPath := normalizePath(want.Path)
		for j, got := range actual {
			if actualMatched[j] {
				continue
			}
			if normalizePath(got.Path) != wantPath || strings.TrimSpace(got.Category) != strings.TrimSpace(want.Category) {
				continue
			}
			if !rangesNearby(want.StartLine, want.EndLine, got.StartLine, got.EndLine, radius) {
				continue
			}
			matches[i] = j
			actualMatched[j] = true
			break
		}
	}
	return matches, actualMatched
}

// citationAccurate validates a finding against the fixed head. A path outside
// allowed_context, a missing file, a base-side citation or an out-of-range span
// is inaccurate; quoted code must appear inside the cited range, mirroring the
// corpus harness in internal/review.
func citationAccurate(c Case, finding ObservedFinding) bool {
	findingPath := normalizePath(finding.Path)
	allowed := false
	for _, candidate := range c.AllowedContext {
		if normalizePath(candidate) == findingPath {
			allowed = true
			break
		}
	}
	if !allowed || normalizeSideOrHead(finding.Side) != "RIGHT" {
		return false
	}
	lines, ok := caseFileLines(c, findingPath)
	if !ok || len(lines) == 0 {
		return false
	}
	if validateRange(finding.StartLine, finding.EndLine, len(lines)) != nil {
		return false
	}
	cited := normalizeAnchor(finding.ExistingCode)
	if cited == "" {
		return true
	}
	window := normalizeAnchor(strings.Join(lines[finding.StartLine-1:finding.EndLine], "\n"))
	return strings.Contains(window, cited)
}

func normalizeSideOrHead(side string) string {
	normalized, err := normalizeSide(side)
	if err != nil {
		return "INVALID"
	}
	return normalized
}

// sameNearbyFinding reports whether two observations describe the same comment
// at the same place, following internal/review's dedupe rules.
func sameNearbyFinding(a, b ObservedFinding, radius int) bool {
	if normalizePath(a.Path) != normalizePath(b.Path) {
		return false
	}
	if strings.TrimSpace(a.Category) != strings.TrimSpace(b.Category) {
		return false
	}
	if normalizeText(a.Title) != normalizeText(b.Title) {
		return false
	}
	aCode, bCode := normalizeAnchor(a.ExistingCode), normalizeAnchor(b.ExistingCode)
	if aCode != "" && bCode != "" && aCode != bCode {
		return false
	}
	return rangesNearby(a.StartLine, a.EndLine, b.StartLine, b.EndLine, radius)
}

// dedupeFindings returns deterministic unique findings and the number of
// duplicate comments removed. Input order never decides the survivor.
func dedupeFindings(findings []ObservedFinding, radius int) ([]ObservedFinding, int) {
	sorted := append([]ObservedFinding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		if sorted[i].StartLine != sorted[j].StartLine {
			return sorted[i].StartLine < sorted[j].StartLine
		}
		if sorted[i].EndLine != sorted[j].EndLine {
			return sorted[i].EndLine < sorted[j].EndLine
		}
		if sorted[i].Category != sorted[j].Category {
			return sorted[i].Category < sorted[j].Category
		}
		if titleA, titleB := normalizeText(sorted[i].Title), normalizeText(sorted[j].Title); titleA != titleB {
			return titleA < titleB
		}
		return sorted[i].Body < sorted[j].Body
	})
	unique := make([]ObservedFinding, 0, len(sorted))
	duplicates := 0
	for _, finding := range sorted {
		duplicate := false
		for _, kept := range unique {
			if sameNearbyFinding(kept, finding, radius) {
				duplicate = true
				break
			}
		}
		if duplicate {
			duplicates++
			continue
		}
		unique = append(unique, finding)
	}
	return unique, duplicates
}
