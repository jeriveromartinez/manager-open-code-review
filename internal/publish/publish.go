package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	// progress records the delivery steps already settled for this run, so a
	// retry only performs the missing ones.
	progress publishProgress
}

// Publish applies the phase-2 publication matrix to a terminal review run. It
// is idempotent and resumable: comments are edited by marker, inline reviews
// are found by their run marker, findings without a GitHub comment id are the
// only ones posted again, resolutions are attempted once, and artifacts are
// upserted. The steps already delivered are recorded so a retry resumes only
// the missing ones, and every shared write re-checks the run is still current.
func (w *Worker) Publish(ctx context.Context, runID uint) (err error) {
	// The queue normally delivers one job per run, but reconciliation, redelivery
	// and two runs of the same pull request can overlap. The lock serializes them;
	// the recorded steps make the second one a no-op.
	run, err := w.Store.ReviewRun(ctx, runID)
	if err != nil {
		return err
	}
	unlock, err := w.Store.LockPublishPR(ctx, run.PullRequestID)
	if err != nil {
		return err
	}
	defer unlock()

	p, err := w.loadPublishContext(ctx, runID)
	if err != nil {
		return err
	}
	p.loadProgress()
	if p.progress.Completed {
		// A redelivery of a settled publication owes nothing.
		return nil
	}
	// A run of another comparison must not publish: the QA check is shared by
	// every comparison of the same head, so cancelling or completing it would
	// overwrite the state of the run that actually belongs to the current one.
	// A superseded run of a previous head owns a different check, so cancelling
	// it is still safe.
	if !p.pr.CoveredBy(p.run) {
		if p.run.Status == store.RunSuperseded && !strings.EqualFold(p.run.HeadSHA, p.pr.HeadSHA) {
			// Fall through: cancel the old head's check.
		} else if p.comparisonStale() {
			return p.settle(ctx)
		} else {
			// The head and base branch still match but the merge base is not
			// resolved yet. The trigger can adopt this run once Compare resolves
			// it, so the delivery must stay pending instead of settling.
			return nil
		}
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
	open := openFindings(findings)

	switch p.run.Status {
	case store.RunSuperseded:
		if !p.stepDone(store.PublishStepCheck) {
			// A run superseded before StartCheck never created one, so GitHub
			// has nothing to cancel. Settling it instead of failing keeps the
			// reconciler from retrying that run forever.
			if err := w.CancelCheck(ctx, runID); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := p.markStep(ctx, store.PublishStepCheck); err != nil {
				return err
			}
		}
		return p.settle(ctx)
	case store.RunFailed:
		if !p.stepDone(store.PublishStepCheck) {
			if ok, err := p.guard(ctx); err != nil {
				return err
			} else if !ok {
				return nil
			}
			// A human verdict recorded after the failure may open or keep the
			// gate closed; either way the automated result is not rewritten.
			conclusion, _ := p.gateOutcome(ctx, open)
			// ErrNotFound means the failure happened before StartCheck, so there
			// is no check run to close; the QA status is still updated.
			if err := p.completeCheck(ctx, conclusion, open); err != nil &&
				!errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err := p.markStep(ctx, store.PublishStepCheck); err != nil {
				return err
			}
		}
		if !p.stepDone(store.PublishStepStatus) {
			if ok, err := p.guard(ctx); err != nil {
				return err
			} else if !ok {
				return nil
			}
			_, status := p.gateOutcome(ctx, open)
			if err := p.setPRStatus(ctx, status); err != nil {
				return err
			}
			if err := p.markStep(ctx, store.PublishStepStatus); err != nil {
				return err
			}
		}
		if err := p.requestHumanReview(ctx, store.ReviewRequestCauseFailed); err != nil {
			return err
		}
		return p.settle(ctx)
	case store.RunSucceeded, store.RunInconclusive:
		// Continue with the full publication below.
	default:
		return nil
	}

	// Inline comments are published first so the summary lists only findings
	// that are genuinely left without one; reloading the snapshot afterwards
	// keeps a retried publish from re-listing findings that already have a
	// comment, and from posting a second review for the same run.
	if !p.stepDone(store.PublishStepInline) {
		if ok, err := p.guard(ctx); err != nil {
			return err
		} else if !ok {
			return nil
		}
		added, err := p.fetchAddedLines(ctx)
		if err != nil {
			return err
		}
		if err := p.publishInlineComments(ctx, added, findings); err != nil {
			if !errors.Is(err, errInlineRejected) {
				return err
			}
			// GitHub refused the whole review: no finding was lost. Settle the
			// step so a retry does not resend a review that will be refused
			// again, and let the summary carry them once.
			slog.Warn("inline review rejected; findings move to the summary",
				"pull_request_id", p.pr.ID, "run_id", p.run.ID, "error", err)
		}
		if err := p.markStep(ctx, store.PublishStepInline); err != nil {
			return err
		}
	}
	findings, err = w.Store.FindingsForPR(ctx, p.pr.ID)
	if err != nil {
		return err
	}
	p.findingSnapshot = findings
	open = openFindings(findings)

	conclusion, qaStatus := p.gateOutcome(ctx, open)
	resolvedSincePrevious := 0
	for _, f := range findings {
		if f.Status == store.FindingResolved && f.LastSeenRunID == p.run.ID {
			resolvedSincePrevious++
		}
	}

	if !p.stepDone(store.PublishStepSummary) {
		if ok, err := p.guard(ctx); err != nil {
			return err
		} else if !ok {
			return nil
		}
		if err := p.publishSummary(ctx, findings, resolvedSincePrevious); err != nil {
			return err
		}
		if err := p.markStep(ctx, store.PublishStepSummary); err != nil {
			return err
		}
	}
	if p.run.Status == store.RunSucceeded && !p.stepDone(store.PublishStepResolutions) {
		if ok, err := p.guard(ctx); err != nil {
			return err
		} else if !ok {
			return nil
		}
		if err := p.publishResolutions(ctx, findings); err != nil {
			return err
		}
		if err := p.markStep(ctx, store.PublishStepResolutions); err != nil {
			return err
		}
	}
	if !p.stepDone(store.PublishStepCheck) {
		if ok, err := p.guard(ctx); err != nil {
			return err
		} else if !ok {
			return nil
		}
		// A head without a check run has nothing to close; the status below is
		// still written so the panel and GitHub agree.
		if err := p.completeCheck(ctx, conclusion, open); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := p.markStep(ctx, store.PublishStepCheck); err != nil {
			return err
		}
	}

	if !p.stepDone(store.PublishStepStatus) {
		if ok, err := p.guard(ctx); err != nil {
			return err
		} else if !ok {
			return nil
		}
		if err := p.setPRStatus(ctx, qaStatus); err != nil {
			return err
		}
		if err := p.markStep(ctx, store.PublishStepStatus); err != nil {
			return err
		}
	}
	if err := p.requestHumanReview(ctx, store.ReviewRequestCauseInconclusive); err != nil {
		return err
	}
	return p.settle(ctx)
}

func openFindings(findings []store.Finding) []store.Finding {
	open := make([]store.Finding, 0, len(findings))
	for _, f := range findings {
		if f.Status == store.FindingOpen {
			open = append(open, f)
		}
	}
	return open
}

// requestHumanReview asks GitHub for a person when the engine could not
// conclude. The run must still cover the pull request's current comparison
// (#141): a result of another base–head pair must not assign reviewers to code
// it never saw. The request is deduplicated by pull request and head, so a
// retried publish is harmless.
func (p *publishContext) requestHumanReview(ctx context.Context, cause string) error {
	// A conclusive run normally owes no human request. A material uncertainty
	// does: the run succeeded, but it did not settle a finding that can change
	// the gate, so the human derivation path must still run.
	if p.run.Status == store.RunSucceeded && !p.hasMaterialUncertainty(p.findingSnapshot) {
		return nil
	}
	pr, err := loadPullRequest(ctx, p.w.Store, p.run.PullRequestID)
	if err != nil {
		return err
	}
	if !pr.CoveredBy(p.run) {
		return nil
	}
	service := &appgithub.ReviewRequestService{
		Store: p.w.Store, Apps: p.w.Apps, RetryDelay: appgithub.ReviewRequestRetryDelay,
	}
	_, err = service.Request(ctx, store.ReviewRequestConfig{
		PR: pr, Repo: p.repo, Policy: p.policy, Installation: p.inst,
	}, cause)
	return err
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
	// decrypted in memory and never stored in an error or an event. A finished
	// run has already discarded its clone token by design, so this entry is
	// empty for new runs and only covers runs completed before that cleanup.
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

// storedVerification is the subset of the verifier's JSON the publisher needs
// to decide whether a finding may be presented as validated or block a check.
type storedVerification struct {
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
	HeadSHA  string `json:"head_sha"`
	Reason   string `json:"reason"`
}

func parseVerification(f store.Finding) storedVerification {
	var v storedVerification
	if len(f.Verification) > 0 {
		_ = json.Unmarshal(f.Verification, &v)
	}
	return v
}

// verificationAtHead reports whether the stored answer still describes the
// reviewed head and carries the evidence a verdict needs. A confirmation
// without a cited line is not trusted, even when it predates #136.
func (p *publishContext) verificationAtHead(f store.Finding) (storedVerification, bool) {
	v := parseVerification(f)
	if v.HeadSHA == "" || p.run.HeadSHA == "" ||
		!strings.EqualFold(v.HeadSHA, p.run.HeadSHA) {
		return v, false
	}
	switch v.Verdict {
	case "confirm":
		if strings.TrimSpace(v.Evidence) == "" {
			return v, false
		}
	case "refute", "uncertain":
	default:
		return v, false
	}
	return v, true
}

// hasBlocking counts the open findings the check may fail on. A finding the
// verifier could not validate at this head never blocks: its uncertainty is the
// human's call. A finding the verifier never answered keeps the pre-verifier
// behaviour and still blocks.
func (p *publishContext) hasBlocking(findings []store.Finding) bool {
	minimum := severityRank[p.policy.BlockingThreshold]
	if minimum == 0 {
		minimum = severityRank[defaultBlockingThreshold]
	}
	for _, f := range findings {
		// Advisory evidence never blocks the gate: the architectural verdict it
		// comes from is observational (#157).
		if f.Advisory || severityRank[f.Severity] < minimum || !p.blocksOnVerification(f) {
			continue
		}
		return true
	}
	return false
}

func (p *publishContext) blocksOnVerification(f store.Finding) bool {
	// A human decision wins over the verifier's stored answer.
	if f.TriageSource == store.TriageSourceHuman {
		return true
	}
	// Only apply mode may change the gate. Off and dry run keep exactly the
	// gate the pull request would have without verification, so an observation
	// can never unblock a finding off would block.
	if p.policy.AutoTriageMode != store.AutoTriageApply {
		return true
	}
	if len(f.Verification) == 0 {
		return true
	}
	v, trusted := p.verificationAtHead(f)
	if v.Verdict == "" {
		return true
	}
	return trusted && v.Verdict != "uncertain"
}

// hasMaterialUncertainty reports whether an open finding at or above the
// blocking threshold has a verdict that cannot settle it: an uncertainty, or
// any answer recorded for another head. That uncertainty can change the
// conclusion, so the review must stay inconclusive instead of passing, and a
// human must be asked.
func (p *publishContext) hasMaterialUncertainty(findings []store.Finding) bool {
	if p.policy.AutoTriageMode != store.AutoTriageApply {
		return false
	}
	minimum := severityRank[p.policy.BlockingThreshold]
	if minimum == 0 {
		minimum = severityRank[defaultBlockingThreshold]
	}
	for _, f := range findings {
		if f.Advisory || f.Status != store.FindingOpen || severityRank[f.Severity] < minimum {
			continue
		}
		if f.TriageSource == store.TriageSourceHuman {
			continue
		}
		v := parseVerification(f)
		if v.Verdict == "" {
			// No stored answer: blocksOnVerification keeps the pre-verifier gate.
			continue
		}
		atHead, trusted := p.verificationAtHead(f)
		if !trusted {
			// A verdict from another head cannot settle or unblock this one.
			return true
		}
		if atHead.Verdict == "uncertain" {
			return true
		}
		// A trusted confirmation blocks, and a trusted refutation of a blocking
		// finding keeps the block; neither is an uncertainty.
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

// githubStatusError is a non-2xx GitHub response. The publisher needs the
// status to tell a rejected inline comment, which must fall back to the
// summary, from a transient failure that a retry can resolve.
type githubStatusError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *githubStatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s %s: GitHub API returned status %d", e.Method, e.Path, e.StatusCode)
	}
	return fmt.Sprintf("%s %s: GitHub API returned status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// isRejectedResponse reports a client error that a retry cannot fix. GitHub
// also uses 403 for secondary rate limits, so only validation failures are
// permanent.
func isRejectedResponse(err error) bool {
	var statusErr *githubStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
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
		statusErr := &githubStatusError{Method: method, Path: path, StatusCode: resp.StatusCode}
		if readErr == nil {
			statusErr.Body = string(payload)
		}
		return statusErr
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
		// An annotation belongs to one file; a design finding that spans files
		// or only cites removed code is represented in the summary instead of
		// being pinned to an arbitrary path (#162).
		path, start, end, ok := singleHeadAnchor(f)
		if !ok || f.Advisory {
			continue
		}
		level := "warning"
		if p.hasBlocking([]store.Finding{f}) {
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
			Path: path, StartLine: start, EndLine: end,
			AnnotationLevel: level, Title: title, Message: message,
		})
	}

	title := "QA Control Center"
	summary := fmt.Sprintf("Revisión #%d · %s", p.runNumber, shortSHA(p.run.HeadSHA))
	verdict, hasHuman := p.humanVerdict(ctx)
	if hasHuman {
		summary += " · humano: " + humanDecisionLabel(verdict.Decision)
	}
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
	if pending := p.pendingCoverageLine(); pending != "" {
		text = strings.TrimSpace(text)
		if text != "" {
			text += "\n\n"
		}
		text += pending
	}
	if hasHuman {
		// The automated result above and the human result below stay visibly
		// separate: a human decision never means the engine completed coverage.
		text = strings.TrimSpace(text)
		if text != "" {
			text += "\n\n"
		}
		text += p.renderHumanVerdict(verdict)
	}
	if analysis := p.architectureCheckLine(); analysis != "" {
		text = strings.TrimSpace(text)
		if text != "" {
			text += "\n\n"
		}
		text += analysis
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

	// GitHub appends annotations on every PATCH instead of replacing them, and
	// the open-finding set can change between deliveries that reuse this head's
	// check. Reconcile by content: resend only the expected annotations GitHub
	// does not already hold, so neither an accepted page nor a stale annotation
	// from an earlier publication can duplicate or suppress one.
	existing, err := p.fetchCheckAnnotations(ctx, checkID)
	if err != nil {
		return err
	}
	missing := missingAnnotations(annotations, existing)

	// Every PATCH carries the full output, not just the first one: a reused
	// check must not keep the text of a previous delivery after the gate, the
	// pending coverage or the human verdict changed.
	confirmed := len(annotations) - len(missing)
	for start := 0; start < len(missing); start += pageSize {
		end := start + pageSize
		if end > len(missing) {
			end = len(missing)
		}
		page := map[string]any{
			"title": title, "summary": summary, "text": output["text"],
			"annotations": missing[start:end],
		}
		payload := map[string]any{"status": "completed", "conclusion": conclusion, "output": page}
		if err := p.request(ctx, http.MethodPatch, path, payload, nil); err != nil {
			return fmt.Errorf("complete check run %d: %w", checkID, err)
		}
		confirmed += end - start
		if err := p.advanceCheckCursor(ctx, confirmed); err != nil {
			return err
		}
	}
	if len(missing) == 0 {
		// Nothing to annotate, but the shared output and conclusion still have
		// to be asserted, both for a retry that resumed after the last page and
		// for a run without annotations.
		page := map[string]any{
			"title": title, "summary": summary, "text": output["text"],
			"annotations": []checkAnnotation{},
		}
		payload := map[string]any{"status": "completed", "conclusion": conclusion, "output": page}
		if err := p.request(ctx, http.MethodPatch, path, payload, nil); err != nil {
			return fmt.Errorf("complete check run %d: %w", checkID, err)
		}
	}

	// The step is recorded only after GitHub confirms the effect, not after a
	// 2xx: an accepted-then-lost page must not make completion an assumption.
	return p.verifyCheckEffect(ctx, path, checkID, conclusion, annotations)
}

// checkRunState is the subset of a check run the publisher reconciles against.
type checkRunState struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Output     struct {
		AnnotationsCount int `json:"annotations_count"`
	} `json:"output"`
}

func (p *publishContext) fetchCheckRun(ctx context.Context, path string) (checkRunState, error) {
	var state checkRunState
	if err := p.request(ctx, http.MethodGet, path, nil, &state); err != nil {
		return state, fmt.Errorf("read check run: %w", err)
	}
	return state, nil
}

// fetchCheckAnnotations lists the annotations GitHub already holds for a check.
// Content, not position, is what reconciliation needs.
func (p *publishContext) fetchCheckAnnotations(ctx context.Context, checkID int64) ([]checkAnnotation, error) {
	base := p.repoPath(fmt.Sprintf("check-runs/%d/annotations", checkID))
	var all []checkAnnotation
	for page := 1; ; page++ {
		var batch []checkAnnotation
		path := fmt.Sprintf("%s?per_page=100&page=%d", base, page)
		if err := p.request(ctx, http.MethodGet, path, nil, &batch); err != nil {
			return nil, fmt.Errorf("list check annotations: %w", err)
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

// missingAnnotations returns the expected annotations absent from existing,
// comparing content as a multiset. Positional comparison would skip a finding
// added between two deliveries of the same head and duplicate one removed.
func missingAnnotations(expected, existing []checkAnnotation) []checkAnnotation {
	counts := make(map[checkAnnotation]int, len(existing))
	for _, annotation := range existing {
		counts[annotation]++
	}
	missing := make([]checkAnnotation, 0, len(expected))
	for _, want := range expected {
		if counts[want] > 0 {
			counts[want]--
			continue
		}
		missing = append(missing, want)
	}
	return missing
}

// verifyCheckEffect confirms that GitHub actually stored the completion and
// every expected annotation. Stale annotations from an earlier publication are
// tolerated because the API cannot remove them, but a missing expected one
// keeps the step unsettled.
func (p *publishContext) verifyCheckEffect(ctx context.Context, path string, checkID int64, conclusion string, expected []checkAnnotation) error {
	state, err := p.fetchCheckRun(ctx, path)
	if err != nil {
		return err
	}
	if state.Status != "completed" {
		return fmt.Errorf("complete check run %d: status is %q, want completed", checkID, state.Status)
	}
	if !strings.EqualFold(state.Conclusion, conclusion) {
		return fmt.Errorf("complete check run %d: conclusion is %q, want %q",
			checkID, state.Conclusion, conclusion)
	}
	if state.Output.AnnotationsCount < len(expected) {
		return fmt.Errorf("complete check run %d: GitHub holds %d annotations, want %d",
			checkID, state.Output.AnnotationsCount, len(expected))
	}
	remote, err := p.fetchCheckAnnotations(ctx, checkID)
	if err != nil {
		return err
	}
	if missing := missingAnnotations(expected, remote); len(missing) > 0 {
		return fmt.Errorf("complete check run %d: GitHub is missing %d of %d expected annotations",
			checkID, len(missing), len(expected))
	}
	return nil
}
