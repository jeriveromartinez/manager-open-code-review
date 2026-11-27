package reviewbench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// BlindResult is a result with reviewer identity removed. Only the blind label
// survives into the report; the version, configuration and display name stay in
// the sealed mapping.
type BlindResult struct {
	CaseID     string            `json:"case_id"`
	Reviewer   string            `json:"reviewer"`
	Date       string            `json:"date"`
	Result     string            `json:"result"`
	LatencyMS  int64             `json:"latency_ms"`
	CostUSD    *float64          `json:"cost_usd,omitempty"`
	Coverage   Coverage          `json:"coverage"`
	Findings   []ObservedFinding `json:"findings"`
	Duplicates int               `json:"duplicates"`
	Superseded bool              `json:"superseded,omitempty"`
}

// SealedDataset is what the report is computed from. It deliberately carries no
// reviewer names or configuration.
type SealedDataset struct {
	Manifest       Manifest      `json:"manifest"`
	TargetReviewer string        `json:"target_reviewer,omitempty"`
	Results        []BlindResult `json:"results"`
}

// SealedMapping is the separate, deliberately non-serializable mapping from
// blind labels and finding tokens to reviewer identities. JSON encodes it as an
// empty object, so it can never leak through a report dump.
type SealedMapping struct {
	labels map[string]Reviewer
	ids    map[string]string
	tokens map[string][]string
}

// BlindFinding is a novel finding (one that matches no ground-truth finding)
// presented for adjudication with blind reviewer labels only.
type BlindFinding struct {
	Token        string   `json:"token"`
	CaseID       string   `json:"case_id"`
	Reviewers    []string `json:"reviewers"`
	Path         string   `json:"path"`
	StartLine    int      `json:"start_line"`
	EndLine      int      `json:"end_line"`
	Severity     string   `json:"severity"`
	Category     string   `json:"category"`
	Title        string   `json:"title"`
	Body         string   `json:"body,omitempty"`
	ExistingCode string   `json:"existing_code,omitempty"`
	Blocking     bool     `json:"blocking"`
}

// Seal replaces reviewer identity with per-manifest blind labels. Labels are
// assigned by the SHA-256 of the reviewer ID and manifest lock, so their order
// does not reveal which reviewer is the project's own.
func Seal(dataset Dataset) (SealedDataset, SealedMapping) {
	reviewers := dataset.Reviewers()
	hashes := make(map[string]string, len(reviewers))
	for _, reviewer := range reviewers {
		sum := sha256.Sum256([]byte(dataset.Manifest.Lock + "\x00" + reviewer.ID))
		hashes[reviewer.ID] = hex.EncodeToString(sum[:])
	}
	sort.SliceStable(reviewers, func(i, j int) bool {
		if hashes[reviewers[i].ID] != hashes[reviewers[j].ID] {
			return hashes[reviewers[i].ID] < hashes[reviewers[j].ID]
		}
		return reviewers[i].ID < reviewers[j].ID
	})
	mapping := SealedMapping{
		labels: make(map[string]Reviewer, len(reviewers)),
		ids:    make(map[string]string, len(reviewers)),
		tokens: make(map[string][]string),
	}
	labelByID := make(map[string]string, len(reviewers))
	for index, reviewer := range reviewers {
		label := fmt.Sprintf("reviewer-%02d", index+1)
		mapping.labels[label] = reviewer
		mapping.ids[label] = reviewer.ID
		labelByID[reviewer.ID] = label
	}

	sealed := SealedDataset{Manifest: dataset.Manifest}
	if dataset.Manifest.TargetReviewer != "" {
		sealed.TargetReviewer = labelByID[dataset.Manifest.TargetReviewer]
	}
	for _, result := range dataset.Results {
		sealed.Results = append(sealed.Results, BlindResult{
			CaseID:     result.Case.ID,
			Reviewer:   labelByID[result.Observation.Reviewer.ID],
			Date:       result.Observation.Date,
			Result:     result.Observation.Result,
			LatencyMS:  result.Observation.LatencyMS,
			CostUSD:    result.Observation.CostUSD,
			Coverage:   result.Observation.Coverage,
			Findings:   result.Observation.Findings,
			Duplicates: result.Duplicates,
			Superseded: result.Superseded,
		})
	}

	for _, finding := range NovelFindings(sealed) {
		ids := make([]string, 0, len(finding.Reviewers))
		for _, label := range finding.Reviewers {
			ids = append(ids, mapping.ids[label])
		}
		sort.Strings(ids)
		mapping.tokens[finding.Token] = ids
	}
	return sealed, mapping
}

// MarshalJSON guarantees the sealed mapping stays sealed even if a caller
// serializes it by mistake.
func (s SealedMapping) MarshalJSON() ([]byte, error) {
	return []byte("{}"), nil
}

// Reviewer returns the identity behind a blind label. It is the explicit reveal
// step; nothing calls it while building the report.
func (s SealedMapping) Reviewer(label string) (Reviewer, bool) {
	reviewer, ok := s.labels[label]
	return reviewer, ok
}

// ReviewerID returns the stable id behind a blind label.
func (s SealedMapping) ReviewerID(label string) (string, bool) {
	id, ok := s.ids[label]
	return id, ok
}

// FindingReviewers returns the reviewers behind a novel-finding token.
func (s SealedMapping) FindingReviewers(token string) ([]Reviewer, bool) {
	ids, ok := s.tokens[token]
	if !ok {
		return nil, false
	}
	reviewers := make([]Reviewer, 0, len(ids))
	for _, id := range ids {
		for _, reviewer := range s.labels {
			if reviewer.ID == id {
				reviewers = append(reviewers, reviewer)
				break
			}
		}
	}
	sort.Slice(reviewers, func(i, j int) bool { return reviewers[i].ID < reviewers[j].ID })
	return reviewers, true
}

// NovelFindings returns the findings that match no ground truth, grouped across
// reviewers. They are sorted by token so the adjudication order does not group
// comments by reviewer. Only completed reviews contribute: a partial, timed-out
// or superseded result cannot be compared with the fixed comparison.
func NovelFindings(dataset SealedDataset) []BlindFinding {
	type candidate struct {
		resultCase string
		finding    ObservedFinding
		reviewer   string
	}
	radius := dataset.Manifest.ContextRadius
	var candidates []candidate
	for _, result := range dataset.Results {
		if result.Result != ResultCompleted {
			continue
		}
		c, ok := caseByID(dataset.Manifest, result.CaseID)
		if !ok {
			continue
		}
		for _, finding := range result.Findings {
			if findingMatchesAny(c.Expected, finding, radius) {
				continue
			}
			candidates = append(candidates, candidate{resultCase: result.CaseID, finding: finding, reviewer: result.Reviewer})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.resultCase != b.resultCase {
			return a.resultCase < b.resultCase
		}
		if a.finding.Path != b.finding.Path {
			return a.finding.Path < b.finding.Path
		}
		if a.finding.StartLine != b.finding.StartLine {
			return a.finding.StartLine < b.finding.StartLine
		}
		if a.finding.EndLine != b.finding.EndLine {
			return a.finding.EndLine < b.finding.EndLine
		}
		if a.finding.Category != b.finding.Category {
			return a.finding.Category < b.finding.Category
		}
		if titleA, titleB := normalizeText(a.finding.Title), normalizeText(b.finding.Title); titleA != titleB {
			return titleA < titleB
		}
		if a.finding.ExistingCode != b.finding.ExistingCode {
			return a.finding.ExistingCode < b.finding.ExistingCode
		}
		return a.reviewer < b.reviewer
	})

	groups := make([]BlindFinding, 0, len(candidates))
	for _, item := range candidates {
		merged := false
		for i := range groups {
			if groups[i].CaseID != item.resultCase {
				continue
			}
			representative := groups[i]
			if !sameNearbyFinding(observedFinding(representative), item.finding, radius) {
				continue
			}
			groups[i].Reviewers = appendLabel(groups[i].Reviewers, item.reviewer)
			groups[i].Blocking = groups[i].Blocking || item.finding.Blocking || item.finding.Severity == "blocker"
			merged = true
			break
		}
		if merged {
			continue
		}
		groups = append(groups, BlindFinding{
			Token:        novelToken(dataset.Manifest.Lock, item.resultCase, item.finding),
			CaseID:       item.resultCase,
			Reviewers:    []string{item.reviewer},
			Path:         item.finding.Path,
			StartLine:    item.finding.StartLine,
			EndLine:      item.finding.EndLine,
			Severity:     item.finding.Severity,
			Category:     item.finding.Category,
			Title:        item.finding.Title,
			Body:         item.finding.Body,
			ExistingCode: item.finding.ExistingCode,
			Blocking:     item.finding.Blocking || item.finding.Severity == "blocker",
		})
	}
	for i := range groups {
		sort.Strings(groups[i].Reviewers)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Token < groups[j].Token })
	return groups
}

func observedFinding(blind BlindFinding) ObservedFinding {
	return ObservedFinding{
		Path: blind.Path, StartLine: blind.StartLine, EndLine: blind.EndLine,
		Severity: blind.Severity, Category: blind.Category, Title: blind.Title,
		Body: blind.Body, ExistingCode: blind.ExistingCode, Blocking: blind.Blocking,
	}
}

func appendLabel(labels []string, label string) []string {
	for _, existing := range labels {
		if existing == label {
			return labels
		}
	}
	return append(labels, label)
}

func novelToken(lock, caseID string, finding ObservedFinding) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		lock, caseID, normalizePath(finding.Path), strings.TrimSpace(finding.Category),
		normalizeText(finding.Title), normalizeAnchor(finding.ExistingCode),
		strconv.Itoa(finding.StartLine),
	}, "\x00")))
	return "n_" + hex.EncodeToString(sum[:])[:16]
}

func findingMatchesAny(expected []ExpectedFinding, finding ObservedFinding, radius int) bool {
	for _, want := range expected {
		if normalizePath(want.Path) != normalizePath(finding.Path) ||
			strings.TrimSpace(want.Category) != strings.TrimSpace(finding.Category) {
			continue
		}
		if rangesNearby(want.StartLine, want.EndLine, finding.StartLine, finding.EndLine, radius) {
			return true
		}
	}
	return false
}

func caseByID(manifest Manifest, id string) (Case, bool) {
	for _, c := range manifest.Cases {
		if c.ID == id {
			return c, true
		}
	}
	return Case{}, false
}
