package review

// AgentFinding is a review finding in the panel's closed vocabulary. OCR
// comments are converted into this shape before Reconcile.
type AgentFinding struct {
	Path       string `json:"path"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Suggestion string `json:"suggestion"`
	Confidence string `json:"confidence"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
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
