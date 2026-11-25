package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	maxHumanVerdictScopeItems = 200
	maxHumanVerdictText       = 4000
)

// humanVerdictScopeBody is the wire shape of the scope a person examined.
type humanVerdictScopeBody struct {
	Files []struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	} `json:"files"`
	PendingFindings []string `json:"pending_findings"`
	Complete        bool     `json:"complete"`
}

// humanVerdictBody is the panel payload that records the human outcome of an
// inconclusive review.
type humanVerdictBody struct {
	Decision   string                `json:"decision"`
	Conclusion string                `json:"conclusion"`
	Reason     string                `json:"reason"`
	Scope      humanVerdictScopeBody `json:"scope"`
}

// humanVerdictRangeResponse is one reviewed file range.
type humanVerdictRangeResponse struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// humanVerdictScopeResponse mirrors the stored scope.
type humanVerdictScopeResponse struct {
	Files           []humanVerdictRangeResponse `json:"files"`
	PendingFindings []string                    `json:"pending_findings"`
	Complete        bool                        `json:"complete"`
}

// humanVerdictResponse is the human decision shown separately from the
// automated run. current is false once a push or retarget invalidated it.
type humanVerdictResponse struct {
	Decision          string                    `json:"decision"`
	Conclusion        string                    `json:"conclusion"`
	Reason            string                    `json:"reason"`
	ActorLogin        string                    `json:"actor_login"`
	Scope             humanVerdictScopeResponse `json:"scope"`
	HeadSHA           string                    `json:"head_sha"`
	BaseRef           string                    `json:"base_ref"`
	MergeBaseSHA      string                    `json:"merge_base_sha"`
	Current           bool                      `json:"current"`
	InvalidatedAt     *string                   `json:"invalidated_at"`
	InvalidatedReason string                    `json:"invalidated_reason"`
	CreatedAt         string                    `json:"created_at"`
}

func humanVerdictDTO(v store.HumanVerdict, current bool) humanVerdictResponse {
	scope := store.HumanVerdictScopeOf(v)
	files := make([]humanVerdictRangeResponse, 0, len(scope.Files))
	for _, file := range scope.Files {
		files = append(files, humanVerdictRangeResponse{
			Path: file.Path, StartLine: file.StartLine, EndLine: file.EndLine,
		})
	}
	pending := scope.PendingFindings
	if pending == nil {
		pending = []string{}
	}
	return humanVerdictResponse{
		Decision:     v.Decision,
		Conclusion:   v.Conclusion,
		Reason:       v.Reason,
		ActorLogin:   v.ActorLogin,
		Scope:        humanVerdictScopeResponse{Files: files, PendingFindings: pending, Complete: scope.Complete},
		HeadSHA:      v.HeadSHA,
		BaseRef:      v.BaseRef,
		MergeBaseSHA: v.MergeBaseSHA,
		Current:      current,
		// invalidated_at is null while the decision still applies.
		InvalidatedAt:     formatTimePtr(v.InvalidatedAt),
		InvalidatedReason: v.InvalidatedReason,
		CreatedAt:         v.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// recordHumanVerdict writes the human outcome of an inconclusive review. Only an
// admin, or the lead or QA of the repository's team, may decide (the same rule
// as triage and retry). The decision is tied to the pull request's current
// comparison, contributes to the gate only while it applies, and never rewrites
// the automated run.
func (s *server) recordHumanVerdict(w http.ResponseWriter, r *http.Request) {
	prID, ok := parseUintID(w, r)
	if !ok {
		return
	}
	var body humanVerdictBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.Decision = strings.TrimSpace(body.Decision)
	body.Conclusion = strings.TrimSpace(body.Conclusion)
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Decision != store.HumanVerdictApproved && body.Decision != store.HumanVerdictChangesRequested {
		writeAPIError(w, http.StatusBadRequest, "decision must be approved or changes_requested")
		return
	}
	if body.Conclusion == "" {
		writeAPIError(w, http.StatusBadRequest, "conclusion is required")
		return
	}
	if len(body.Conclusion) > maxHumanVerdictText || len(body.Reason) > maxHumanVerdictText {
		writeAPIError(w, http.StatusBadRequest, "conclusion or reason is too long")
		return
	}
	if len(body.Scope.Files) > maxHumanVerdictScopeItems || len(body.Scope.PendingFindings) > maxHumanVerdictScopeItems {
		writeAPIError(w, http.StatusBadRequest, "scope has too many entries")
		return
	}
	scope := store.HumanVerdictScope{Complete: body.Scope.Complete, PendingFindings: body.Scope.PendingFindings}
	for _, file := range body.Scope.Files {
		scope.Files = append(scope.Files, store.HumanVerdictRange{
			Path: file.Path, StartLine: file.StartLine, EndLine: file.EndLine,
		})
	}
	encoded, err := store.EncodeHumanVerdictScope(scope)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	user, _ := auth.UserFrom(r.Context())
	pr, ok := s.authorizedPR(w, r, user, prID)
	if !ok {
		return
	}
	// This decision answers an inconclusive automation result. A successful run
	// has its own verdict, and an outstanding run will publish one.
	run, found, err := s.Store.CurrentVerdictRun(r.Context(), pr)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load current review")
		return
	}
	if !found || (run.Status != store.RunInconclusive && run.Status != store.RunFailed) {
		writeAPIError(w, http.StatusConflict, "no inconclusive review for this comparison")
		return
	}
	if err := s.validateHumanVerdictFindings(r.Context(), pr.ID, scope.PendingFindings); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	saved, err := s.Store.RecordHumanVerdict(r.Context(), store.HumanVerdict{
		PullRequestID: pr.ID,
		Decision:      body.Decision,
		Conclusion:    body.Conclusion,
		Reason:        body.Reason,
		Scope:         encoded,
		ScopeComplete: scope.Complete,
		ActorLogin:    user.Login,
		ActorUserID:   &user.ID,
		Source:        store.HumanVerdictSourcePanel,
	})
	if errors.Is(err, store.ErrHumanVerdictNotApplicable) {
		writeAPIError(w, http.StatusConflict, "pull request is not open for a human verdict")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "record human verdict")
		return
	}
	if err := s.Store.Audit(r.Context(), &user.ID, "human_verdict.recorded", "pull_request",
		strconv.FormatUint(uint64(pr.ID), 10), map[string]any{
			"decision":         saved.Decision,
			"head_sha":         saved.HeadSHA,
			"base_ref":         saved.BaseRef,
			"scope_files":      len(scope.Files),
			"pending_findings": scope.PendingFindings,
			"complete":         scope.Complete,
			"reason":           body.Reason,
		}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "record audit")
		return
	}
	s.notify(r.Context(), events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(pr.ID), 10)})

	// The human outcome changes the gate; recompute the check and the QA status
	// so GitHub and the panel show the same decision. The record is already
	// stored, so a retry replaces it instead of stacking a second verdict.
	if s.Findings != nil {
		if err := s.Findings.Republish(r.Context(), pr.ID); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "republish check")
			return
		}
	}
	writeJSON(w, http.StatusOK, humanVerdictDTO(saved, true))
}

// validateHumanVerdictFindings keeps the scope honest: a pending finding id must
// belong to this pull request.
func (s *server) validateHumanVerdictFindings(ctx context.Context, prID uint, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	findings, err := s.Store.FindingsForPR(ctx, prID)
	if err != nil {
		return errors.New("load findings")
	}
	known := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		known[finding.ID] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := known[strings.TrimSpace(id)]; !ok {
			return errors.New("pending finding does not belong to this pull request")
		}
	}
	return nil
}
