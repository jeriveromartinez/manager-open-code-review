package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// objectionMaxTimeout bounds one objection reevaluation. The pass reads one
// cited file, so it is deliberately smaller than a full review.
const objectionMaxTimeout = 3 * time.Minute

// maxObjectionFileBytes bounds the cited file the worker fetches.
const maxObjectionFileBytes = 1 << 20

// ObjectionVerifier answers whether an objection refutes a finding at a head.
// The default implementation runs the same focused OCR pass as verification.
type ObjectionVerifier interface {
	VerifyObjection(ctx context.Context, f store.Finding, objection, workDir, headSHA string) (Verification, error)
}

// ObjectionEnqueuer schedules the publication step of an evaluated objection.
// The control process owns it because only it holds the App credentials.
type ObjectionEnqueuer interface {
	EnqueueObjectionPublish(ctx context.Context, objectionID uint) error
}

// ReevaluateObjection is the worker step of an objection: it runs the bounded
// verifier with OCR, records the decision and evidence atomically on the
// finding, and schedules the publication step. It is idempotent: a retried job
// sees the evaluated intent and only re-asserts the publication.
func (r *Runner) ReevaluateObjection(ctx context.Context, objectionID uint) error {
	intent, err := r.Store.FindingObjection(ctx, objectionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch intent.State {
	case store.ObjectionPublished:
		return nil
	case store.ObjectionEvaluated:
		return r.enqueueObjectionPublish(ctx, intent.ID)
	}

	finding, err := r.Store.Finding(ctx, intent.FindingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	escalate := func(reason string) error {
		return r.recordObjectionDecision(ctx, intent, ObjectionDecision{
			Action: store.ObjectionEscalate,
			Status: finding.Status,
			Reason: reason,
		})
	}
	if !intent.Authorized {
		return r.recordObjectionDecision(ctx, intent, ObjectionDecision{
			Action: store.ObjectionIgnored,
			Status: finding.Status,
			Reason: "el autor de la respuesta no puede cambiar el gate",
		})
	}
	// A human decision or a fixed finding that landed after the intent was
	// queued is final; do not spend the verifier on it.
	if finding.Status != store.FindingOpen || finding.TriageSource == store.TriageSourceHuman {
		return r.recordObjectionDecision(ctx, intent, ObjectionDecision{
			Action: store.ObjectionIgnored,
			Status: finding.Status,
			Reason: "la decisión humana no se reevalúa",
		})
	}

	cfg, err := r.Store.ReviewConfigForPR(ctx, intent.PullRequestID)
	if errors.Is(err, store.ErrNoReviewConfig) {
		return escalate("no hay una configuración de revisión vigente")
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(cfg.PR.HeadSHA, intent.HeadSHA) {
		return escalate("el head del pull request cambió desde la respuesta")
	}
	content, err := r.objectionFile(ctx, intent, cfg)
	if err != nil {
		// A network or OCR transient must be retried by River; only the final
		// attempt turns it into a recorded escalation.
		return fmt.Errorf("fetch cited file for objection %d: %w", intent.ID, err)
	}
	workDir, err := writeObjectionCheckout(intent.FilePath, content)
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	policy := verifyPolicy(cfg.Policy)
	verifier := r.ObjectionVerifier
	if verifier == nil {
		verifier = &nativeObjectionVerifier{
			Bin:         r.Cfg.OCRBin,
			Profile:     cfg.Profile,
			APIKey:      r.objectionAPIKey(cfg.Profile),
			MaxTokens:   r.Cfg.ContextCeiling,
			TokenBudget: policy.TokenMax,
			Timeout:     objectionMaxTimeout,
		}
	}
	verification, err := verifier.VerifyObjection(ctx, finding, intent.Body, workDir, intent.HeadSHA)
	if err != nil {
		return fmt.Errorf("verify objection %d: %w", intent.ID, err)
	}
	if verification.Verdict == "" {
		verification.Verdict = VerdictUncertain
	}
	if verification.Reason == "" && verification.Verdict == VerdictUncertain {
		verification.Reason = "la respuesta no es concluyente"
	}
	verification.HeadSHA = intent.HeadSHA
	if verification.VerifiedAt.IsZero() {
		verification.VerifiedAt = time.Now().UTC()
	}

	decision := DecideObjection(finding, verification, map[string][]byte{intent.FilePath: content}, intent.HeadSHA, policy)
	return r.recordObjectionDecision(ctx, intent, decision)
}

// AbandonObjection records the escalation the worker owes a human when the
// retry budget is exhausted. It only acts on a still-queued intent, so a late
// call cannot overwrite a decision a retry already stored.
func (r *Runner) AbandonObjection(ctx context.Context, objectionID uint, cause string) error {
	intent, err := r.Store.FindingObjection(ctx, objectionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.State != store.ObjectionQueued {
		return nil
	}
	finding, err := r.Store.Finding(ctx, intent.FindingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	reason := "no se pudo reevaluar la objeción tras agotar los reintentos"
	if strings.TrimSpace(cause) != "" {
		reason += ": " + cause
	}
	return r.recordObjectionDecision(ctx, intent, ObjectionDecision{
		Action: store.ObjectionEscalate,
		Status: finding.Status,
		Reason: reason,
	})
}

func (r *Runner) recordObjectionDecision(ctx context.Context, intent store.FindingObjection, decision ObjectionDecision) error {
	var raw []byte
	if decision.Verification.Verdict != "" || !decision.Verification.VerifiedAt.IsZero() {
		var err error
		raw, err = json.Marshal(decision.Verification)
		if err != nil {
			return fmt.Errorf("marshal objection verification: %w", err)
		}
	}
	if _, err := r.Store.RecordFindingObjectionEvaluation(ctx, intent.ID, store.ObjectionEvaluation{
		Action:       decision.Action,
		Status:       decision.Status,
		Apply:        decision.Apply,
		Reason:       decision.Reason,
		Evidence:     decision.Verification.Evidence,
		Verification: raw,
	}); err != nil {
		return err
	}
	return r.enqueueObjectionPublish(ctx, intent.ID)
}

func (r *Runner) enqueueObjectionPublish(ctx context.Context, objectionID uint) error {
	if r.Jobs == nil {
		return nil
	}
	enqueuer, ok := r.Jobs.(ObjectionEnqueuer)
	if !ok {
		return nil
	}
	return enqueuer.EnqueueObjectionPublish(ctx, objectionID)
}

func (r *Runner) objectionAPIKey(profile store.LLMProfile) string {
	if r.Box == nil || profile.APIKeyEnc == "" {
		return ""
	}
	key, err := r.Box.Open(profile.APIKeyEnc)
	if err != nil {
		return ""
	}
	return string(key)
}

// objectionFile returns the cited file. The app may have copied it into the
// intent; otherwise the worker uses the sealed read token to fetch it at the
// reviewed head.
func (r *Runner) objectionFile(ctx context.Context, intent store.FindingObjection, cfg store.ReviewConfig) ([]byte, error) {
	if len(intent.FileContent) > 0 {
		return intent.FileContent, nil
	}
	if r.Box == nil || intent.ReadTokenEnc == "" || r.Cfg.GitHubAPIURL == "" {
		return nil, errors.New("objection has no cited file or read token")
	}
	token, err := r.Box.Open(intent.ReadTokenEnc)
	if err != nil {
		return nil, fmt.Errorf("open objection read token: %w", err)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return fetchObjectionFile(fetchCtx, r.Cfg.GitHubAPIURL, cfg.Repo.FullName, intent.FilePath, intent.HeadSHA, string(token))
}

// fetchObjectionFile downloads one file with a read-only installation token.
func fetchObjectionFile(ctx context.Context, apiURL, repoFullName, path, ref, token string) ([]byte, error) {
	owner, name, ok := strings.Cut(repoFullName, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("invalid repository full name %q", repoFullName)
	}
	endpoint := strings.TrimRight(apiURL, "/") + "/repos/" + url.PathEscape(owner) + "/" +
		url.PathEscape(name) + "/contents/" + escapeRepoPath(path) + "?ref=" + url.QueryEscape(ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.raw")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("download %s at %s: GitHub API returned status %d", path, ref, resp.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, maxObjectionFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxObjectionFileBytes {
		return nil, fmt.Errorf("finding file is too large: %q", path)
	}
	return content, nil
}

func escapeRepoPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

// writeObjectionCheckout materializes the cited file in a temporary tree so the
// verifier can scan it. It applies the same path containment as readRepoFile.
func writeObjectionCheckout(path string, content []byte) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", fmt.Errorf("invalid finding path %q", path)
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("finding path escapes the checkout: %q", path)
	}
	dir, err := os.MkdirTemp("", "qa-objection-")
	if err != nil {
		return "", fmt.Errorf("create objection checkout: %w", err)
	}
	full := filepath.Join(dir, clean)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("create objection checkout directory: %w", err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("write objection checkout file: %w", err)
	}
	return dir, nil
}

// buildObjectionContext is the verifier's background for one disputed finding:
// the finding itself plus the reply that disputes it, both fenced.
func buildObjectionContext(f store.Finding, objection string) string {
	var b strings.Builder
	b.WriteString(buildVerifyContext([]store.Finding{f}))
	b.WriteString("\nThe author replied in the review thread with the objection below. ")
	b.WriteString("Decide whether that objection refutes the finding using the code at this revision.\n")
	b.WriteString("## objection\n")
	b.WriteString(fence(strings.TrimSpace(objection)))
	b.WriteString("\n")
	return b.String()
}

// nativeObjectionVerifier runs one focused `ocr scan` over the cited file. It
// reuses the verifier contract, so the evidence rules are exactly the same as
// in a review run.
type nativeObjectionVerifier struct {
	Bin         string
	Profile     store.LLMProfile
	APIKey      string
	MaxTokens   int
	TokenBudget int64
	Timeout     time.Duration
}

func (v *nativeObjectionVerifier) VerifyObjection(ctx context.Context, f store.Finding, objection, workDir, headSHA string) (Verification, error) {
	ctx, cancel := context.WithTimeout(ctx, v.Timeout)
	defer cancel()

	verifier := &NativeVerifier{
		Bin:         v.Bin,
		RepoDir:     workDir,
		Home:        filepath.Join(workDir, "home-ocr-objection"),
		Profile:     v.Profile,
		APIKey:      v.APIKey,
		Paths:       []string{f.Path},
		Background:  buildObjectionContext(f, objection),
		MaxTokens:   v.MaxTokens,
		TokenBudget: v.TokenBudget,
		Timeout:     v.Timeout,
	}
	res, _, err := verifier.Review(ctx)
	if err != nil {
		return Verification{}, err
	}
	verdicts := ParseVerdicts(res.Comments)
	verification, ok := verdicts[f.ID]
	if !ok {
		return Verification{
			Verdict: VerdictUncertain,
			HeadSHA: headSHA,
			Reason:  "OCR no respondió por la objeción",
		}, nil
	}
	verification.HeadSHA = headSHA
	verification.Model = v.Profile.Model
	verification.SessionID = res.SessionID
	verification.VerifiedAt = time.Now().UTC()
	return verification, nil
}

// ObjectionQueue is the enqueuer the control-side sweeper needs. It lives here
// so the review package does not import jobs.
type ObjectionQueue interface {
	EnqueueObjection(ctx context.Context, objectionID uint) error
	EnqueueObjectionPublish(ctx context.Context, objectionID uint) error
}

// objectionStaleAfter leaves the webhook's own enqueue time to succeed before
// the reconciler repeats it. River's uniqueness makes the repeat harmless.
const objectionStaleAfter = time.Minute

// ObjectionSweeper heals objection intents whose worker or publish job was
// lost. It runs in the control process, which may enqueue both queues.
type ObjectionSweeper struct {
	Store *store.Store
	Jobs  ObjectionQueue
}

// EnqueuePendingObjections re-asserts the missing job of stale queued intents
// and of evaluated intents whose reply was never published.
func (s *ObjectionSweeper) EnqueuePendingObjections(ctx context.Context, limit int) (int, error) {
	if s == nil || s.Store == nil || s.Jobs == nil {
		return 0, nil
	}
	count := 0
	queued, err := s.Store.PendingObjectionEvaluations(ctx, time.Now().Add(-objectionStaleAfter), limit)
	if err != nil {
		return 0, err
	}
	for _, intent := range queued {
		if err := s.Jobs.EnqueueObjection(ctx, intent.ID); err != nil {
			return count, err
		}
		count++
	}
	evaluated, err := s.Store.PendingObjectionPublications(ctx, limit)
	if err != nil {
		return count, err
	}
	for _, intent := range evaluated {
		if err := s.Jobs.EnqueueObjectionPublish(ctx, intent.ID); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}
