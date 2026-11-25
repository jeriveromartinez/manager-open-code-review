package review

import "github.com/jeriveromartinez/qa-control-center/internal/store"

// AgentFinding is a review finding in the panel's closed vocabulary. OCR
// comments are converted into this shape before Reconcile.
type AgentFinding struct {
	Path         string `json:"path"`
	Severity     string `json:"severity"`
	Category     string `json:"category"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	Suggestion   string `json:"suggestion"`
	Confidence   string `json:"confidence"`
	ExistingCode string `json:"existing_code"`
	StartLine    int    `json:"start_line"`
	EndLine      int    `json:"end_line"`
	// Scope and Citations describe design findings whose evidence spans files
	// or the whole pull request (#162). An empty scope means one file.
	Scope     string                  `json:"scope,omitempty"`
	Citations []store.FindingCitation `json:"citations,omitempty"`
	// Advisory findings are evidence from the observational architectural
	// analysis: the panel and the summary show them, but they never block the
	// check and are not posted inline.
	Advisory bool `json:"advisory,omitempty"`
}

// AgentOutput is the set of findings a review run produced.
type AgentOutput struct {
	Summary  string `json:"summary"`
	Coverage struct {
		ReviewedFiles int `json:"reviewed_files"`
		SkippedFiles  int `json:"skipped_files"`
	} `json:"coverage"`
	PreviousFindings []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"previous_findings"`
	Findings []AgentFinding `json:"findings"`
}
