package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const summaryMarker = "<!-- qa-control-center:summary -->"

type reviewCoverage struct {
	ReviewedFiles    int            `json:"reviewed_files"`
	SkippedFiles     int            `json:"skipped_files"`
	NotRequiredFiles int            `json:"not_required_files"`
	Files            []coverageFile `json:"files"`
}

// coverageFile mirrors the per-file coverage the review worker stores.
type coverageFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Method string `json:"method"`
	Reason string `json:"reason"`
	Human  bool   `json:"human"`
}

// pendingFiles returns the changed files the review still owes. They are not
// policy exclusions and were not covered by any pass.
func (c reviewCoverage) pendingFiles() []coverageFile {
	var pending []coverageFile
	for _, file := range c.Files {
		if file.Status == "pending" {
			pending = append(pending, file)
		}
	}
	return pending
}

// humanFiles returns the changed files no automated pass can analyse, so the
// summary names them instead of letting them pass silently.
func (c reviewCoverage) humanFiles() []coverageFile {
	var human []coverageFile
	for _, file := range c.Files {
		if file.Human {
			human = append(human, file)
		}
	}
	return human
}

func decodeReviewCoverage(raw []byte) reviewCoverage {
	var coverage reviewCoverage
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &coverage)
	}
	return coverage
}

// pendingCoverageLine renders the pending files as one line for the check run
// output. Empty when every required file was covered.
func (p *publishContext) pendingCoverageLine() string {
	pending := decodeReviewCoverage(p.run.Coverage).pendingFiles()
	if len(pending) == 0 {
		return ""
	}
	parts := make([]string, 0, len(pending))
	for _, file := range pending {
		if file.Reason != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", p.sanitize(file.Path), p.sanitize(file.Reason)))
			continue
		}
		parts = append(parts, p.sanitize(file.Path))
	}
	return "Archivos pendientes: " + strings.Join(parts, ", ")
}

// publishSummary creates the single summary comment of the pull request or
// edits the one carrying the marker. It never posts a second summary.
func (p *publishContext) publishSummary(ctx context.Context, findings []store.Finding, resolvedSincePrevious int) error {
	body := p.buildSummary(findings, resolvedSincePrevious)

	var existingID int64
	commentsPath := p.issueCommentsPath()
	for page := 1; existingID == 0; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		path := fmt.Sprintf("%s?per_page=100&page=%d", commentsPath, page)
		if err := p.request(ctx, "GET", path, nil, &comments); err != nil {
			return fmt.Errorf("list issue comments: %w", err)
		}
		for _, c := range comments {
			if strings.Contains(c.Body, summaryMarker) {
				existingID = c.ID
				break
			}
		}
		if len(comments) < 100 {
			break
		}
	}

	var commentID int64
	if existingID != 0 {
		commentID = existingID
		if err := p.request(ctx, "PATCH", p.issueCommentPath(commentID),
			map[string]string{"body": body}, nil); err != nil {
			return fmt.Errorf("edit summary comment %d: %w", commentID, err)
		}
	} else {
		var created struct {
			ID int64 `json:"id"`
		}
		if err := p.request(ctx, "POST", p.issueCommentsPath(),
			map[string]string{"body": body}, &created); err != nil {
			return fmt.Errorf("create summary comment: %w", err)
		}
		if created.ID == 0 {
			return fmt.Errorf("create summary comment: GitHub returned no comment id")
		}
		commentID = created.ID
	}

	url := p.w.PublicURL
	if url != "" {
		url = strings.TrimRight(url, "/") + "/prs/" + strconv.FormatUint(uint64(p.pr.ID), 10)
	}
	_, err := p.w.Store.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: p.pr.ID,
		Kind:          store.ArtifactSummary,
		HeadSHA:       p.run.HeadSHA,
		ExternalID:    strconv.FormatInt(commentID, 10),
		URL:           url,
	})
	if err != nil {
		return fmt.Errorf("save summary artifact: %w", err)
	}
	return nil
}

func (p *publishContext) buildSummary(findings []store.Finding, resolvedSincePrevious int) string {
	var b strings.Builder
	b.WriteString(summaryMarker)
	b.WriteString("\n")

	if p.run.Status == store.RunInconclusive || p.run.Status == store.RunFailed {
		reason := p.run.ErrorMessage
		if reason == "" {
			reason = p.run.ErrorCode
		}
		if reason == "" {
			reason = "el run no terminó de forma concluyente"
		}
		b.WriteString("\n> ⚠️ **Revisión no concluyente:** ")
		b.WriteString(p.sanitize(reason))
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\n### Revisión #%d · %s\n\n", p.runNumber, shortSHA(p.run.HeadSHA))

	counts := map[string]int{"blocker": 0, "major": 0, "minor": 0, "nit": 0}
	// Severity counts describe the current open findings of the PR.
	for _, f := range p.findingSnapshot {
		if f.Status != store.FindingOpen {
			continue
		}
		if _, ok := counts[f.Severity]; ok {
			counts[f.Severity]++
		}
	}
	b.WriteString("**Severidades:** ")
	b.WriteString(fmt.Sprintf("blocker %d · major %d · minor %d · nit %d",
		counts["blocker"], counts["major"], counts["minor"], counts["nit"]))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "**Resueltos desde la revisión anterior:** %d\n\n", resolvedSincePrevious)

	b.WriteString(p.buildVerificationBlock())

	coverage := decodeReviewCoverage(p.run.Coverage)
	fmt.Fprintf(&b, "**Cobertura:** %d/%d (revisados/omitidos)", coverage.ReviewedFiles, coverage.SkippedFiles)
	if coverage.NotRequiredFiles > 0 {
		fmt.Fprintf(&b, " · %d no requeridos (eliminados, binarios o submódulos)", coverage.NotRequiredFiles)
	}
	b.WriteString("\n\n")
	if pending := coverage.pendingFiles(); len(pending) > 0 {
		fmt.Fprintf(&b, "**Archivos pendientes de revisión (%d):**\n", len(pending))
		for _, file := range pending {
			reason := file.Reason
			if reason == "" {
				reason = "sin cobertura"
			}
			fmt.Fprintf(&b, "\n- `%s` — %s\n", p.sanitize(file.Path), p.sanitize(reason))
		}
		b.WriteString("\n")
	}
	if human := coverage.humanFiles(); len(human) > 0 {
		fmt.Fprintf(&b, "**Revisión humana requerida (%d):**\n", len(human))
		for _, file := range human {
			reason := file.Reason
			if reason == "" {
				reason = "sin análisis automático"
			}
			fmt.Fprintf(&b, "\n- `%s` — %s\n", p.sanitize(file.Path), p.sanitize(reason))
		}
		b.WriteString("\n")
	}

	b.WriteString(p.sanitize(p.run.Summary))
	b.WriteString("\n")

	if missing := p.findingsWithoutInlineComment(findings); len(missing) > 0 {
		b.WriteString(fmt.Sprintf("\n**Hallazgos sin comentario inline (%d):**\n", len(missing)))
		for _, f := range missing {
			severity := f.Severity
			if severity == "" {
				severity = "sin severidad"
			}
			fmt.Fprintf(&b, "\n- **%s** · `%s` — %s\n",
				p.sanitize(severity), p.findingLocation(f), p.sanitize(f.Title))
			if body := p.sanitize(f.Body); body != "" {
				b.WriteString("  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n")
			}
		}
	}

	if link := p.panelLink(); link != "" {
		fmt.Fprintf(&b, "\n[Ver en el panel](%s)\n", link)
	}
	return b.String()
}

// verificationApplied reports whether the second OCR pass changed findings.
// It ran in this run when the session is recorded, and its triage can also be
// reused from a previous run on the same head, where no new scan runs. Only
// then are inline comments limited to the confirmed ones; a dry run or a
// skipped verification keeps today's behaviour.
func (p *publishContext) verificationApplied() bool {
	return p.policy.AutoTriageMode == store.AutoTriageApply &&
		(p.run.VerifySessionID != "" || p.hasVerifierTriage())
}

// inlineAllowed is the single predicate the summary and the inline review share:
// once the verifier's decisions are applied, only a finding confirmed with
// evidence that still matches this head may be published inline.
func (p *publishContext) inlineAllowed(f store.Finding) bool {
	return !p.verificationApplied() || p.findingVerdict(f) == "confirm"
}

func (p *publishContext) hasVerifierTriage() bool {
	for _, f := range p.findingSnapshot {
		if f.TriageSource == store.TriageSourceVerifier {
			return true
		}
	}
	return false
}

// buildVerificationBlock renders the curated summary of the second OCR pass.
// With auto-triage off since before the run and no stored verifier decision,
// the summary stays exactly as it was.
func (p *publishContext) buildVerificationBlock() string {
	if p.policy.AutoTriageMode == store.AutoTriageOff &&
		p.run.VerifySessionID == "" && !p.hasVerifierTriage() {
		return ""
	}

	var validated, uncertain []store.Finding
	dismissed := 0
	for _, f := range p.findingSnapshot {
		if f.TriageSource == store.TriageSourceHuman {
			continue
		}
		switch p.findingVerdict(f) {
		case "confirm":
			if f.Status == store.FindingOpen {
				validated = append(validated, f)
			}
		case "refute":
			// Only a withdrawal the policy applied counts as dismissed; a
			// refutation it degraded (blocking severity, auto-withdraw off or
			// evidence missing) is still the human's call.
			switch f.Status {
			case store.FindingWithdrawn:
				dismissed++
			case store.FindingOpen:
				uncertain = append(uncertain, f)
			}
		default:
			// Uncertainty is the human's call even when the row still looks
			// withdrawn: new evidence may have invalidated that dismissal.
			uncertain = append(uncertain, f)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Verificación automática:** %d validados · %d para tu criterio · %d descartados\n",
		len(validated), len(uncertain), dismissed)
	if p.policy.AutoTriageMode == store.AutoTriageDryRun {
		b.WriteString("\n_(simulación: no se cambió el estado de ningún hallazgo)_\n")
	}
	if len(validated) > 0 {
		b.WriteString("\n**Validado por la verificación:**\n")
		for _, f := range validated {
			b.WriteString(p.findingBullet(f))
		}
	}
	if len(uncertain) > 0 {
		fmt.Fprintf(&b, "\n<details><summary>Necesita tu criterio (%d)</summary>\n\n", len(uncertain))
		for _, f := range uncertain {
			b.WriteString(p.findingBullet(f))
		}
		b.WriteString("\n</details>\n")
	}
	if dismissed > 0 {
		fmt.Fprintf(&b, "\n%d descartados automáticamente", dismissed)
		if link := p.panelLink(); link != "" {
			fmt.Fprintf(&b, " · [ver en el panel](%s)", link)
		}
		b.WriteString("\n")
	}
	if p.policy.AutoTriageMode != store.AutoTriageOff &&
		p.run.VerifySessionID == "" && !p.hasVerifierTriage() {
		b.WriteString("\n> La verificación automática no se ejecutó; estos hallazgos no están confirmados.\n")
	}
	b.WriteString("\n")
	return b.String()
}

// findingVerdict is the verifier's answer for a finding. The stored record is
// authoritative because it is rewritten every run, including the uncertain
// answer that degrades a stale or invalid confirmation; a verdict retained from
// another commit is not trusted at all.
func (p *publishContext) findingVerdict(f store.Finding) string {
	v, trusted := p.verificationAtHead(f)
	if !trusted {
		return ""
	}
	return v.Verdict
}

func (p *publishContext) findingBullet(f store.Finding) string {
	severity := f.Severity
	if severity == "" {
		severity = "sin severidad"
	}
	bullet := fmt.Sprintf("- **%s** · `%s` — %s\n",
		p.sanitize(severity), p.findingLocation(f), p.sanitize(f.Title))
	if reason := strings.TrimSpace(p.findingReason(f)); reason != "" {
		bullet += fmt.Sprintf("  _%s_\n", p.sanitize(reason))
	}
	return bullet
}

// findingReason explains why the verifier's answer could not be used, so the
// summary does not leave an uncertainty without its cause.
func (p *publishContext) findingReason(f store.Finding) string {
	if v, trusted := p.verificationAtHead(f); trusted {
		return v.Reason
	}
	if v := parseVerification(f); v.Verdict != "" {
		return "la verificación corresponde a otro commit"
	}
	return ""
}

func (p *publishContext) panelLink() string {
	if p.w.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(p.w.PublicURL, "/") + "/prs/" + strconv.FormatUint(uint64(p.pr.ID), 10)
}

// findingsWithoutInlineComment selects the open findings that have no inline
// comment yet. It re-checks both conditions even though the caller supplies a
// snapshot, so a retried publish can never list an already published finding.
func (p *publishContext) findingsWithoutInlineComment(findings []store.Finding) []store.Finding {
	missing := make([]store.Finding, 0)
	for _, f := range findings {
		if f.Status != store.FindingOpen || f.GithubCommentID != 0 || !p.inlineAllowed(f) {
			continue
		}
		missing = append(missing, f)
	}
	sort.SliceStable(missing, func(i, j int) bool {
		if a, b := severityRank[missing[i].Severity], severityRank[missing[j].Severity]; a != b {
			return a > b
		}
		if missing[i].Path != missing[j].Path {
			return missing[i].Path < missing[j].Path
		}
		if missing[i].StartLine != missing[j].StartLine {
			return missing[i].StartLine < missing[j].StartLine
		}
		return missing[i].Title < missing[j].Title
	})
	return missing
}

// findingLocation renders path:start-end, path:start or the bare path.
func (p *publishContext) findingLocation(f store.Finding) string {
	path := p.sanitize(f.Path)
	if path == "" {
		return "sin ruta"
	}
	start := f.StartLine
	end := f.EndLine
	if start <= 0 {
		return path
	}
	if end < start {
		end = start
	}
	if end == start {
		return fmt.Sprintf("%s:%d", path, start)
	}
	return fmt.Sprintf("%s:%d-%d", path, start, end)
}
