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
	ReviewedFiles int `json:"reviewed_files"`
	SkippedFiles  int `json:"skipped_files"`
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

	var coverage reviewCoverage
	if len(p.run.Coverage) > 0 {
		_ = json.Unmarshal(p.run.Coverage, &coverage)
	}
	fmt.Fprintf(&b, "**Cobertura:** %d/%d (revisados/omitidos)\n\n",
		coverage.ReviewedFiles, coverage.SkippedFiles)

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

	if p.w.PublicURL != "" {
		link := strings.TrimRight(p.w.PublicURL, "/") + "/prs/" + strconv.FormatUint(uint64(p.pr.ID), 10)
		fmt.Fprintf(&b, "\n[Ver en el panel](%s)\n", link)
	}
	return b.String()
}

// findingsWithoutInlineComment selects the open findings that have no inline
// comment yet. It re-checks both conditions even though the caller supplies a
// snapshot, so a retried publish can never list an already published finding.
func (p *publishContext) findingsWithoutInlineComment(findings []store.Finding) []store.Finding {
	missing := make([]store.Finding, 0)
	for _, f := range findings {
		if f.Status != store.FindingOpen || f.GithubCommentID != 0 {
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
