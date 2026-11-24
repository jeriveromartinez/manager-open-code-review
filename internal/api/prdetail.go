package api

import (
	"encoding/json"
	"net/http"
	"time"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// reviewRunDTO deliberately omits CloneTokenEnc: a clone token is a credential
// valid for the duration of a run and must never reach the browser.
type reviewRunDTO struct {
	ID           uint            `json:"id"`
	ProfileID    uint            `json:"profile_id"`
	HeadSHA      string          `json:"head_sha"`
	BaseSHA      string          `json:"base_sha"`
	Trigger      string          `json:"trigger"`
	Status       string          `json:"status"`
	Attempt      int             `json:"attempt"`
	Summary      string          `json:"summary"`
	Coverage     json.RawMessage `json:"coverage"`
	Usage        json.RawMessage `json:"usage"`
	ErrorCode    string          `json:"error_code"`
	ErrorMessage string          `json:"error_message"`
	// Token columns are null on runs that predate the accounting (#73).
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
	MaxContextTokens *int64 `json:"max_context_tokens"`
	// Phase is empty on queued, terminal and pre-#123 runs; the panel renders
	// "—" instead of inventing a step.
	Phase          string  `json:"phase"`
	PhaseStartedAt *string `json:"phase_started_at"`
	// PolicyRevision is the effective policy the run executed under; empty on
	// runs that predate the recording (#153).
	PolicyRevision    string  `json:"policy_revision"`
	PolicyObservation bool    `json:"policy_observation"`
	StartedAt         *string `json:"started_at"`
	FinishedAt        *string `json:"finished_at"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

func jsonObject(raw datatypes.JSON) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

func reviewRunDTOOf(run store.ReviewRun) reviewRunDTO {
	return reviewRunDTO{
		ID: run.ID, ProfileID: run.ProfileID, HeadSHA: run.HeadSHA, BaseSHA: run.BaseSHA,
		Trigger: run.Trigger, Status: run.Status, Attempt: run.Attempt, Summary: run.Summary,
		Coverage: jsonObject(run.Coverage), Usage: jsonObject(run.Usage),
		ErrorCode: run.ErrorCode, ErrorMessage: run.ErrorMessage,
		InputTokens: run.InputTokens, OutputTokens: run.OutputTokens,
		ReasoningTokens: run.ReasoningTokens,
		CacheReadTokens: run.CacheReadTokens, CacheWriteTokens: run.CacheWriteTokens,
		TotalTokens: run.TotalTokens, MaxContextTokens: run.MaxContextTokens,
		Phase: run.Phase, PhaseStartedAt: formatTimePtr(run.PhaseStartedAt),
		PolicyRevision: run.PolicyRevision, PolicyObservation: run.PolicyObservation,
		StartedAt: formatTimePtr(run.StartedAt), FinishedAt: formatTimePtr(run.FinishedAt),
		CreatedAt: run.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: run.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

type findingDTO struct {
	ID             string  `json:"id"`
	Fingerprint    string  `json:"fingerprint"`
	Path           string  `json:"path"`
	StartLine      int     `json:"start_line"`
	EndLine        int     `json:"end_line"`
	Severity       string  `json:"severity"`
	Category       string  `json:"category"`
	Title          string  `json:"title"`
	Body           string  `json:"body"`
	Suggestion     string  `json:"suggestion"`
	Confidence     string  `json:"confidence"`
	Status         string  `json:"status"`
	FirstSeenRunID uint    `json:"first_seen_run_id"`
	LastSeenRunID  uint    `json:"last_seen_run_id"`
	TriageVerdict  string  `json:"triage_verdict"`
	TriagedBy      string  `json:"triaged_by"`
	TriagedAt      *string `json:"triaged_at"`
	// TriageSource is who produced that verdict; Verification is the verifier's
	// answer with its cited evidence, empty on findings it never looked at.
	TriageSource string          `json:"triage_source"`
	Verification json.RawMessage `json:"verification"`
	// GithubThreadResolved is GitHub's thread state, reported apart from the QA
	// verdict and the finding status.
	GithubThreadResolved bool   `json:"github_thread_resolved"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

func findingDTOOf(f store.Finding) findingDTO {
	return findingDTO{
		ID: f.ID, Fingerprint: f.Fingerprint, Path: f.Path,
		StartLine: f.StartLine, EndLine: f.EndLine, Severity: f.Severity,
		Category: f.Category, Title: f.Title, Body: f.Body,
		Suggestion: f.Suggestion, Confidence: f.Confidence, Status: f.Status,
		FirstSeenRunID: f.FirstSeenRunID, LastSeenRunID: f.LastSeenRunID,
		TriageVerdict: f.TriageVerdict, TriagedBy: f.TriagedBy, TriagedAt: formatTimePtr(f.TriagedAt),
		TriageSource: f.TriageSource, Verification: jsonObject(f.Verification),
		GithubThreadResolved: f.GithubThreadResolved,
		CreatedAt:            f.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:            f.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// prDetailResponse embeds the dashboard DTO so the detail view starts with the
// exact same fields as the list and adds the review history.
type prDetailResponse struct {
	prResponse
	Runs     []reviewRunDTO `json:"runs"`
	Findings []findingDTO   `json:"findings"`
}

// visiblePRView returns the caller's view of a pull request, or writes the
// response and returns false when it is outside their visibility scope. The
// detail and the run-log endpoints share it so the rule cannot drift; a PR
// outside the scope is reported as missing so its existence is not disclosed.
func (s *server) visiblePRView(w http.ResponseWriter, r *http.Request, id uint) (*store.PRView, bool) {
	user, _ := auth.UserFrom(r.Context())
	views, err := s.Store.ListVisiblePullRequests(r.Context(), user, store.PRFilter{})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list pull requests")
		return nil, false
	}
	for i := range views {
		if views[i].ID == id {
			return &views[i], true
		}
	}
	writeAPIError(w, http.StatusNotFound, "pull request not found")
	return nil, false
}

func (s *server) prDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUintID(w, r)
	if !ok {
		return
	}
	view, ok := s.visiblePRView(w, r, id)
	if !ok {
		return
	}

	runs, err := s.Store.ListReviewRuns(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list review runs")
		return
	}
	findings, err := s.Store.FindingsForPR(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list findings")
		return
	}
	runDTOs := make([]reviewRunDTO, 0, len(runs))
	for _, run := range runs {
		runDTOs = append(runDTOs, reviewRunDTOOf(run))
	}
	findingDTOs := make([]findingDTO, 0, len(findings))
	for _, finding := range findings {
		findingDTOs = append(findingDTOs, findingDTOOf(finding))
	}
	writeJSON(w, http.StatusOK, prDetailResponse{
		prResponse: prDTO(*view),
		Runs:       runDTOs,
		Findings:   findingDTOs,
	})
}

// retryPR authorizes the request and delegates the actual run creation to the
// injected review trigger. A nil trigger means the wiring is not configured.
func (s *server) retryPR(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUintID(w, r)
	if !ok {
		return
	}
	user, _ := auth.UserFrom(r.Context())

	var pr store.PullRequest
	if err := s.Store.DB.WithContext(r.Context()).Take(&pr, id).Error; err != nil {
		writeAPIError(w, http.StatusNotFound, "pull request not found")
		return
	}
	if !s.canActOnPR(w, r, user, pr) {
		return
	}
	if s.Reviews == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "review retrier unavailable")
		return
	}
	if err := s.Reviews.Retry(r.Context(), id); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "retry review")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}
