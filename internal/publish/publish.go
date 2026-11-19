package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	appgithub "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const defaultBlockingThreshold = "major"

// publishContext carries the resolved configuration and GitHub clients for one
// Publish call. It also accumulates whether any text needed redaction.
type publishContext struct {
	w               *Worker
	run             store.ReviewRun
	pr              store.PullRequest
	repo            store.Repository
	policy          store.QAPolicy
	inst            store.Installation
	app             *appgithub.App
	client          *gh.Client
	owner           string
	name            string
	redactor        *Redactor
	redacted        bool
	runNumber       int
	findingSnapshot []store.Finding
}

// Publish applies the phase-2 publication matrix to a terminal review run. It
// is idempotent: comments are edited by marker, inline reviews only contain
// findings without a GitHub comment id, resolutions are attempted once, and
// artifacts are upserted.
func (w *Worker) Publish(ctx context.Context, runID uint) (err error) {
	p, err := w.loadPublishContext(ctx, runID)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil && p.redacted && w.Events != nil {
			if evErr := w.Events.Publish(ctx, events.Event{
				Type: "run.updated",
				ID:   strconv.FormatUint(uint64(p.run.ID), 10),
			}); evErr != nil {
				err = fmt.Errorf("publish redaction alert: %w", evErr)
			}
		}
	}()

	runs, err := w.Store.ListReviewRuns(ctx, p.pr.ID)
	if err != nil {
		return err
	}
	p.runNumber = len(runs)
	for i, candidate := range runs {
		if candidate.ID == p.run.ID {
			p.runNumber = i + 1
			break
		}
	}

	findings, err := w.Store.FindingsForPR(ctx, p.pr.ID)
	if err != nil {
		return err
	}
	p.findingSnapshot = findings
	open := make([]store.Finding, 0, len(findings))
	for _, f := range findings {
		if f.Status == store.FindingOpen {
			open = append(open, f)
		}
	}

	switch p.run.Status {
	case store.RunSuperseded:
		return w.CancelCheck(ctx, runID)
	case store.RunFailed:
		if err := p.completeCheck(ctx, "action_required", open); err != nil {
			return err
		}
		return p.setPRStatus(ctx, store.StatusInconclusive)
	case store.RunSucceeded, store.RunInconclusive:
		// Continue with the full publication below.
	default:
		return nil
	}

	added, err := p.fetchAddedLines(ctx)
	if err != nil {
		return err
	}
	conclusion := "action_required"
	if p.run.Status == store.RunSucceeded {
		conclusion = "success"
		if hasBlocking(open, p.policy.BlockingThreshold) {
			conclusion = "failure"
		}
	}

	resolvedSincePrevious := 0
	withoutDiffLine := make([]store.Finding, 0)
	for _, f := range findings {
		if f.Status == store.FindingResolved && f.LastSeenRunID == p.run.ID {
			resolvedSincePrevious++
		}
		if f.Status == store.FindingWithdrawn {
			continue
		}
		if f.StartLine <= 0 || !added.has(f.Path, f.StartLine) {
			withoutDiffLine = append(withoutDiffLine, f)
		}
	}

	if err := p.publishSummary(ctx, withoutDiffLine, resolvedSincePrevious); err != nil {
		return err
	}
	if err := p.publishInlineComments(ctx, added, findings); err != nil {
		return err
	}
	if p.run.Status == store.RunSucceeded {
		if err := p.publishResolutions(ctx, findings); err != nil {
			return err
		}
	}
	if err := p.completeCheck(ctx, conclusion, open); err != nil {
		return err
	}

	status := store.StatusInconclusive
	if p.run.Status == store.RunSucceeded && conclusion == "failure" {
		status = store.StatusChangesRequested
	} else if p.run.Status == store.RunSucceeded {
		status = store.StatusReadyForHuman
	}
	return p.setPRStatus(ctx, status)
}

func (w *Worker) loadPublishContext(ctx context.Context, runID uint) (*publishContext, error) {
	run, err := w.Store.ReviewRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	pr, err := loadPullRequest(ctx, w.Store, run.PullRequestID)
	if err != nil {
		return nil, err
	}
	repo, err := loadRepository(ctx, w.Store, pr.RepositoryID)
	if err != nil {
		return nil, err
	}
	inst, err := loadInstallation(ctx, w.Store, repo.InstallationID)
	if err != nil {
		return nil, err
	}

	policy := store.QAPolicy{BlockingThreshold: defaultBlockingThreshold}
	if repo.TeamID != nil {
		loaded, err := w.Store.QAPolicyForTeam(ctx, *repo.TeamID)
		switch {
		case err == nil:
			policy = loaded
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}

	// API keys and clone tokens are only needed as redaction inputs. They are
	// decrypted in memory and never stored in an error or an event.
	secrets := make([]string, 0, 2)
	profileID := run.ProfileID
	if profileID == 0 && policy.LLMProfileID != 0 {
		profileID = policy.LLMProfileID
	}
	if profileID != 0 {
		profile, err := w.Store.LLMProfile(ctx, profileID)
		switch {
		case err == nil:
			secrets = append(secrets, w.decrypt(profile.APIKeyEnc))
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	secrets = append(secrets, w.decrypt(run.CloneTokenEnc))

	app, err := w.Apps.App(ctx)
	if err != nil {
		return nil, fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(inst.ID)
	if err != nil {
		return nil, fmt.Errorf("create installation client: %w", err)
	}
	owner, name, err := splitFullName(repo.FullName)
	if err != nil {
		return nil, err
	}
	return &publishContext{
		w:        w,
		run:      run,
		pr:       pr,
		repo:     repo,
		policy:   policy,
		inst:     inst,
		app:      app,
		client:   client,
		owner:    owner,
		name:     name,
		redactor: NewRedactor(secrets...),
	}, nil
}

// decrypt returns a sealed secret in plaintext, or an empty string when it is
// absent, unconfigured or cannot be opened. The recover guard keeps a zero Box
// from panicking; no error text ever includes the value itself.
func (w *Worker) decrypt(sealed string) (plain string) {
	if w == nil || w.Box == nil || sealed == "" {
		return ""
	}
	defer func() { _ = recover() }()
	raw, err := w.Box.Open(sealed)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

var severityRank = map[string]int{
	"blocker": 4,
	"major":   3,
	"minor":   2,
	"nit":     1,
}

func hasBlocking(findings []store.Finding, threshold string) bool {
	minimum := severityRank[threshold]
	if minimum == 0 {
		minimum = severityRank[defaultBlockingThreshold]
	}
	for _, f := range findings {
		if severityRank[f.Severity] >= minimum {
			return true
		}
	}
	return false
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (p *publishContext) setPRStatus(ctx context.Context, status string) error {
	if p.pr.QAStatus == status {
		return nil
	}
	if err := p.w.Store.SetPRStatus(ctx, p.pr.ID, status); err != nil {
		return err
	}
	p.pr.QAStatus = status
	if p.w.Events == nil {
		return nil
	}
	if err := p.w.Events.Publish(ctx, events.Event{
		Type: "pr.updated",
		ID:   strconv.FormatUint(uint64(p.pr.ID), 10),
	}); err != nil {
		return fmt.Errorf("publish pr.updated: %w", err)
	}
	return nil
}

// request performs a REST call against the configured API URL using the
// installation client, which supplies the Authorization header.
func (p *publishContext) request(ctx context.Context, method, path string, in, out any) error {
	var reader io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode %s %s request: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}
	endpoint := strings.TrimRight(p.app.APIURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build %s %s request: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Client().Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, cancelErrorBodyLimit))
		if readErr != nil {
			return fmt.Errorf("%s %s: GitHub API returned status %d", method, path, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: GitHub API returned status %d: %s", method, path, resp.StatusCode, payload)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return nil
}

func (p *publishContext) repoPath(suffix string) string {
	return fmt.Sprintf("/repos/%s/%s/%s",
		url.PathEscape(p.owner), url.PathEscape(p.name), suffix)
}

func (p *publishContext) issueCommentsPath() string {
	return p.repoPath(fmt.Sprintf("issues/%d/comments", p.pr.Number))
}

func (p *publishContext) issueCommentPath(id int64) string {
	return p.repoPath(fmt.Sprintf("issues/comments/%d", id))
}

type checkAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"`
	Title           string `json:"title"`
	Message         string `json:"message"`
}

// completeCheck closes the check run created by StartCheck. Annotations are
// sent in pages of at most 50, as required by the checks API.
func (p *publishContext) completeCheck(ctx context.Context, conclusion string, open []store.Finding) error {
	artifact, err := p.w.Store.ArtifactFor(ctx, p.pr.ID, store.ArtifactCheck, p.run.HeadSHA)
	if err != nil {
		return err
	}
	checkID, err := strconv.ParseInt(artifact.ExternalID, 10, 64)
	if err != nil {
		return fmt.Errorf("check artifact external id %q: %w", artifact.ExternalID, err)
	}

	annotations := make([]checkAnnotation, 0, len(open))
	for _, f := range open {
		if f.Path == "" || f.StartLine <= 0 {
			continue
		}
		end := f.EndLine
		if end < f.StartLine {
			end = f.StartLine
		}
		level := "warning"
		if hasBlocking([]store.Finding{f}, p.policy.BlockingThreshold) {
			level = "failure"
		}
		title := p.sanitize(f.Title)
		if title == "" {
			title = "Hallazgo de QA"
		}
		message := p.sanitize(f.Body)
		if message == "" {
			message = p.sanitize(f.Category)
		}
		if message == "" {
			message = "Revisar el hallazgo en el panel de QA"
		}
		annotations = append(annotations, checkAnnotation{
			Path: f.Path, StartLine: f.StartLine, EndLine: end,
			AnnotationLevel: level, Title: title, Message: message,
		})
	}

	title := "QA Control Center"
	summary := fmt.Sprintf("Revisión #%d · %s", p.runNumber, shortSHA(p.run.HeadSHA))
	if conclusion == "failure" {
		summary += " · hallazgos bloqueantes"
	}
	link := strings.TrimRight(p.w.PublicURL, "/") + "/prs/" + strconv.FormatUint(uint64(p.pr.ID), 10)
	text := p.run.Summary
	if p.run.Status == store.RunInconclusive || p.run.Status == store.RunFailed {
		if p.run.ErrorMessage != "" {
			text = p.run.ErrorMessage
		} else if p.run.ErrorCode != "" {
			text = p.run.ErrorCode
		}
	}
	output := map[string]any{
		"title":   title,
		"summary": summary,
		"text":    p.sanitize(text),
	}
	if p.w.PublicURL != "" {
		output["text"] = fmt.Sprintf("%s\n\n[Ver en el panel](%s)", output["text"], link)
	}

	path := p.repoPath(fmt.Sprintf("check-runs/%d", checkID))
	const pageSize = 50
	for i := 0; ; i += pageSize {
		end := i + pageSize
		if end > len(annotations) {
			end = len(annotations)
		}
		page := output
		if i > 0 {
			page = map[string]any{"title": title, "summary": summary}
		}
		page["annotations"] = annotations[i:end]
		payload := map[string]any{"status": "completed", "conclusion": conclusion, "output": page}
		if err := p.request(ctx, http.MethodPatch, path, payload, nil); err != nil {
			return fmt.Errorf("complete check run %d: %w", checkID, err)
		}
		if end == len(annotations) {
			break
		}
	}
	return nil
}
