package reviewbench

import (
	"fmt"
	"sort"
	"strings"
)

// Adjudication verdicts for a novel finding.
const (
	AdjudicationConfirmed = "confirmed"
	AdjudicationRejected  = "rejected"
	AdjudicationDuplicate = "duplicate"
)

// AdjudicationDecision is one human decision about a blinded finding. Evidence
// is mandatory: a disagreement is resolved with evidence, not by preference.
type AdjudicationDecision struct {
	Token    string `json:"token"`
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
	Notes    string `json:"notes,omitempty"`
}

// AdjudicatedFinding is a decision reproduced in the blind report.
type AdjudicatedFinding struct {
	Token     string   `json:"token"`
	CaseID    string   `json:"case_id"`
	Reviewers []string `json:"reviewers"`
	Verdict   string   `json:"verdict"`
	Evidence  string   `json:"evidence"`
	Notes     string   `json:"notes,omitempty"`
}

// ReviewerAdjudication counts how many novel findings each blind reviewer
// contributed to each adjudication outcome.
type ReviewerAdjudication struct {
	Reviewer  string `json:"reviewer"`
	Confirmed int    `json:"confirmed"`
	Rejected  int    `json:"rejected"`
	Duplicate int    `json:"duplicate"`
	Pending   int    `json:"pending"`
}

// AdjudicationReport is the blinded adjudication section of the report. It only
// contains blind labels and finding tokens.
type AdjudicationReport struct {
	Candidates int                    `json:"candidates"`
	Confirmed  int                    `json:"confirmed"`
	Rejected   int                    `json:"rejected"`
	Duplicate  int                    `json:"duplicate"`
	Pending    []string               `json:"pending"`
	ByReviewer []ReviewerAdjudication `json:"by_reviewer"`
	Decisions  []AdjudicatedFinding   `json:"decisions"`
}

// Adjudicate applies decisions to the blinded novel findings. Unknown tokens,
// duplicate decisions, unknown verdicts and decisions without evidence are
// rejected; no reviewer name is read or written here.
func Adjudicate(findings []BlindFinding, decisions []AdjudicationDecision) (AdjudicationReport, error) {
	byToken := make(map[string]BlindFinding, len(findings))
	for _, finding := range findings {
		byToken[finding.Token] = finding
	}
	report := AdjudicationReport{
		Candidates: len(findings),
		Pending:    []string{},
		Decisions:  []AdjudicatedFinding{},
	}
	decided := make(map[string]bool, len(decisions))
	reviewerCounts := make(map[string]*ReviewerAdjudication)
	for _, finding := range findings {
		for _, label := range finding.Reviewers {
			entry := reviewerCounts[label]
			if entry == nil {
				entry = &ReviewerAdjudication{Reviewer: label}
				reviewerCounts[label] = entry
			}
		}
	}
	for _, decision := range decisions {
		finding, ok := byToken[decision.Token]
		if !ok {
			return AdjudicationReport{}, fmt.Errorf("%w: unknown finding token %q", ErrAdjudication, decision.Token)
		}
		if decided[decision.Token] {
			return AdjudicationReport{}, fmt.Errorf("%w: duplicate decision for token %q", ErrAdjudication, decision.Token)
		}
		switch decision.Verdict {
		case AdjudicationConfirmed, AdjudicationRejected, AdjudicationDuplicate:
		default:
			return AdjudicationReport{}, fmt.Errorf("%w: unknown verdict %q", ErrAdjudication, decision.Verdict)
		}
		if strings.TrimSpace(decision.Evidence) == "" {
			return AdjudicationReport{}, fmt.Errorf("%w: token %q has no evidence", ErrAdjudication, decision.Token)
		}
		decided[decision.Token] = true
		report.Decisions = append(report.Decisions, AdjudicatedFinding{
			Token: decision.Token, CaseID: finding.CaseID, Reviewers: append([]string(nil), finding.Reviewers...),
			Verdict: decision.Verdict, Evidence: decision.Evidence, Notes: decision.Notes,
		})
		switch decision.Verdict {
		case AdjudicationConfirmed:
			report.Confirmed++
		case AdjudicationRejected:
			report.Rejected++
		case AdjudicationDuplicate:
			report.Duplicate++
		}
		for _, label := range finding.Reviewers {
			entry := reviewerCounts[label]
			switch decision.Verdict {
			case AdjudicationConfirmed:
				entry.Confirmed++
			case AdjudicationRejected:
				entry.Rejected++
			case AdjudicationDuplicate:
				entry.Duplicate++
			}
		}
	}
	for _, finding := range findings {
		if decided[finding.Token] {
			continue
		}
		report.Pending = append(report.Pending, finding.Token)
		for _, label := range finding.Reviewers {
			reviewerCounts[label].Pending++
		}
	}
	sort.Strings(report.Pending)
	sort.SliceStable(report.Decisions, func(i, j int) bool { return report.Decisions[i].Token < report.Decisions[j].Token })
	labels := make([]string, 0, len(reviewerCounts))
	for label := range reviewerCounts {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		report.ByReviewer = append(report.ByReviewer, *reviewerCounts[label])
	}
	return report, nil
}
