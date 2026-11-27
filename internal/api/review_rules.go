package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	maxRuleExamples = 100
	maxRuleReason   = 4000
)

// ruleExampleBody is one item of the sample a rule is observed against.
type ruleExampleBody struct {
	FindingID string `json:"finding_id"`
	Path      string `json:"path"`
	Category  string `json:"category"`
	Title     string `json:"title"`
	Anchor    string `json:"anchor"`
	Severity  string `json:"severity"`
}

// createRuleBody converts a human decision into a proposed rule. finding_id is
// optional: with it the API derives the scope and the positive example from the
// decision the person already recorded on that finding.
type createRuleBody struct {
	FindingID        string            `json:"finding_id"`
	Decision         string            `json:"decision"`
	Reason           string            `json:"reason"`
	PathScope        string            `json:"path_scope"`
	Category         string            `json:"category"`
	PositiveExamples []ruleExampleBody `json:"positive_examples"`
	NegativeExamples []ruleExampleBody `json:"negative_examples"`
}

// ruleTransitionBody promotes, disables or reverts a rule. Every transition
// records the reason and the author.
type ruleTransitionBody struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// reviewRuleResponse is one version of a rule plus its measured sample and
// approval. expired is derived so the panel can show that the evidence lost
// validity without reading timestamps.
type reviewRuleResponse struct {
	ID                uint                  `json:"id"`
	RepositoryID      int64                 `json:"repository_id"`
	RuleKey           string                `json:"rule_key"`
	Version           int                   `json:"version"`
	Status            string                `json:"status"`
	Decision          string                `json:"decision"`
	Reason            string                `json:"reason"`
	PathScope         string                `json:"path_scope"`
	Category          string                `json:"category"`
	PositiveExamples  []store.RuleExample   `json:"positive_examples"`
	NegativeExamples  []store.RuleExample   `json:"negative_examples"`
	Evaluation        *store.RuleEvaluation `json:"evaluation"`
	SourceFindingID   string                `json:"source_finding_id"`
	SourceHeadSHA     string                `json:"source_head_sha"`
	ProposedBy        string                `json:"proposed_by"`
	ApprovedBy        string                `json:"approved_by"`
	ApprovedAt        *string               `json:"approved_at"`
	EvidenceExpiresAt *string               `json:"evidence_expires_at"`
	SupersededBy      *uint                 `json:"superseded_by"`
	Expired           bool                  `json:"expired"`
	CreatedAt         string                `json:"created_at"`
	UpdatedAt         string                `json:"updated_at"`
}

func reviewRuleDTO(rule store.ReviewRule, now time.Time) reviewRuleResponse {
	positive := store.DecodeRuleExamples(rule.PositiveExamples)
	negative := store.DecodeRuleExamples(rule.NegativeExamples)
	if positive == nil {
		positive = []store.RuleExample{}
	}
	if negative == nil {
		negative = []store.RuleExample{}
	}
	var evaluation *store.RuleEvaluation
	if stored := store.DecodeRuleEvaluation(rule.Evaluation); !stored.EvaluatedAt.IsZero() {
		evaluation = &stored
	}
	expired := false
	if rule.Status == store.RuleStatusObservation || rule.Status == store.RuleStatusActive {
		expired = rule.EvidenceExpiresAt == nil || !now.Before(*rule.EvidenceExpiresAt)
	}
	return reviewRuleResponse{
		ID: rule.ID, RepositoryID: rule.RepositoryID, RuleKey: rule.RuleKey,
		Version: rule.Version, Status: rule.Status, Decision: rule.Decision,
		Reason: rule.Reason, PathScope: rule.PathScope, Category: rule.Category,
		PositiveExamples: positive, NegativeExamples: negative, Evaluation: evaluation,
		SourceFindingID: rule.SourceFindingID, SourceHeadSHA: rule.SourceHeadSHA,
		ProposedBy: rule.ProposedBy, ApprovedBy: rule.ApprovedBy,
		ApprovedAt: formatTimePtr(rule.ApprovedAt), EvidenceExpiresAt: formatTimePtr(rule.EvidenceExpiresAt),
		SupersededBy: rule.SupersededBy, Expired: expired,
		CreatedAt: rule.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: rule.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// listRepoRules returns every version of every rule of a repository, oldest
// first. The panel reads the same rows to show what was approved and what
// replaced what.
func (s *server) listRepoRules(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repoForPolicy(w, r)
	if !ok {
		return
	}
	rules, err := s.Store.ReviewRulesForRepo(r.Context(), repo.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load review rules")
		return
	}
	now := time.Now().UTC()
	out := make([]reviewRuleResponse, 0, len(rules))
	for _, rule := range rules {
		out = append(out, reviewRuleDTO(rule, now))
	}
	writeJSON(w, http.StatusOK, out)
}

// createRepoRule converts a human decision with a reason into a proposed rule.
// A rule is never active on creation: it starts proposed and must be evaluated
// against false positives and real defects before it can be observed or applied.
func (s *server) createRepoRule(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repoForPolicy(w, r)
	if !ok {
		return
	}
	var body createRuleBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.FindingID = strings.TrimSpace(body.FindingID)
	body.Decision = strings.TrimSpace(body.Decision)
	body.Reason = strings.TrimSpace(body.Reason)
	body.PathScope = strings.TrimSpace(body.PathScope)
	body.Category = strings.TrimSpace(body.Category)
	if body.Reason == "" {
		writeAPIError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if len(body.Reason) > maxRuleReason {
		writeAPIError(w, http.StatusBadRequest, "reason is too long")
		return
	}
	if len(body.PositiveExamples) > maxRuleExamples || len(body.NegativeExamples) > maxRuleExamples {
		writeAPIError(w, http.StatusBadRequest, "too many examples")
		return
	}

	in := store.ReviewRule{
		RepositoryID:     repo.ID,
		Decision:         body.Decision,
		Reason:           body.Reason,
		PathScope:        body.PathScope,
		Category:         body.Category,
		PositiveExamples: store.EncodeRuleExamples(ruleExamples(body.PositiveExamples)),
		NegativeExamples: store.EncodeRuleExamples(ruleExamples(body.NegativeExamples)),
	}
	if body.FindingID != "" {
		finding, pr, ok := s.ruleSourceFinding(w, r, repo, body.FindingID)
		if !ok {
			return
		}
		if finding.TriageSource != store.TriageSourceHuman || finding.TriageVerdict == "" {
			writeAPIError(w, http.StatusConflict, "finding has no recorded human decision")
			return
		}
		decision := store.RuleDecisionConfirm
		if finding.TriageVerdict == store.VerdictFalsePositive {
			decision = store.RuleDecisionWithdraw
		}
		if body.Decision == "" {
			body.Decision = decision
			in.Decision = decision
		}
		if body.Decision != decision {
			writeAPIError(w, http.StatusBadRequest, "decision does not match the finding verdict")
			return
		}
		if in.PathScope == "" {
			in.PathScope = finding.Path
		}
		if in.Category == "" {
			in.Category = finding.Category
		}
		if len(body.PositiveExamples) == 0 {
			in.PositiveExamples = store.EncodeRuleExamples([]store.RuleExample{{
				FindingID: finding.ID, Path: finding.Path, Category: finding.Category,
				Title: finding.Title, Anchor: finding.ExistingCode, Severity: finding.Severity,
			}})
		}
		in.SourceFindingID = finding.ID
		in.SourceHeadSHA = pr.HeadSHA
	}

	user, _ := auth.UserFrom(r.Context())
	in.ProposedBy, in.ProposedByID = user.Login, &user.ID
	rule, err := s.Store.ProposeReviewRule(r.Context(), in)
	if errors.Is(err, store.ErrInvalidRule) {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "propose review rule")
		return
	}
	if err := s.Store.Audit(r.Context(), &user.ID, "review_rule.proposed", "review_rule",
		strconv.FormatUint(uint64(rule.ID), 10), ruleAuditData(rule)); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	s.notify(r.Context(), events.Event{Type: "repo.updated", ID: strconv.FormatInt(repo.ID, 10)})
	writeJSON(w, http.StatusOK, reviewRuleDTO(rule, time.Now().UTC()))
}

// transitionRepoRule evaluates a rule or promotes, disables and reverts it.
// Promotion requires the measured sample to have no severe omission, and every
// transition records the author and the reason.
func (s *server) transitionRepoRule(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repoForPolicy(w, r)
	if !ok {
		return
	}
	rule, ok := s.repoRule(w, r, repo)
	if !ok {
		return
	}
	var body ruleTransitionBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.Action = strings.TrimSpace(body.Action)
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Action != "evaluate" && body.Reason == "" {
		writeAPIError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if len(body.Reason) > maxRuleReason {
		writeAPIError(w, http.StatusBadRequest, "reason is too long")
		return
	}

	user, _ := auth.UserFrom(r.Context())
	now := time.Now().UTC()
	var saved store.ReviewRule
	var err error
	action := "review_rule." + body.Action
	switch body.Action {
	case "evaluate":
		evaluation, evalErr := store.EvaluateReviewRule(rule, now)
		if evalErr != nil {
			writeRuleError(w, evalErr)
			return
		}
		saved, err = s.Store.RecordReviewRuleEvaluation(r.Context(), rule.ID, evaluation, now)
	case "observe":
		saved, err = s.Store.ObserveReviewRule(r.Context(), rule.ID, user.Login, &user.ID, now)
	case "activate":
		saved, err = s.Store.ActivateReviewRule(r.Context(), rule.ID, user.Login, &user.ID, now)
	case "disable":
		saved, err = s.Store.DisableReviewRule(r.Context(), rule.ID, now)
	case "revert":
		saved, err = s.Store.RevertReviewRule(r.Context(), rule.ID, user.Login, &user.ID, now)
	default:
		writeAPIError(w, http.StatusBadRequest, "action must be evaluate, observe, activate, disable or revert")
		return
	}
	if errors.Is(err, store.ErrInvalidRule) {
		writeRuleError(w, err)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "review rule not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "update review rule")
		return
	}
	data := ruleAuditData(saved)
	data["action"], data["reason"] = body.Action, body.Reason
	if err := s.Store.Audit(r.Context(), &user.ID, action, "review_rule",
		strconv.FormatUint(uint64(saved.ID), 10), data); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "write audit log")
		return
	}
	s.notify(r.Context(), events.Event{Type: "repo.updated", ID: strconv.FormatInt(repo.ID, 10)})
	writeJSON(w, http.StatusOK, reviewRuleDTO(saved, time.Now().UTC()))
}

// repoRule loads the {ruleID} route parameter and verifies it belongs to the
// repository in the URL, so a rule cannot be promoted through another repo.
func (s *server) repoRule(w http.ResponseWriter, r *http.Request, repo store.Repository) (store.ReviewRule, bool) {
	id, err := strconv.ParseUint(chi.URLParam(r, "ruleID"), 10, 64)
	if err != nil || id == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid rule id")
		return store.ReviewRule{}, false
	}
	rule, err := s.Store.ReviewRule(r.Context(), uint(id))
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "review rule not found")
		return store.ReviewRule{}, false
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load review rule")
		return store.ReviewRule{}, false
	}
	if rule.RepositoryID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "review rule not found")
		return store.ReviewRule{}, false
	}
	return rule, true
}

// ruleSourceFinding loads the finding a rule is converted from and its pull
// request, rejecting one of another repository. The finding is the human
// decision, so it must belong to the addressed repository.
func (s *server) ruleSourceFinding(w http.ResponseWriter, r *http.Request, repo store.Repository, findingID string) (store.Finding, store.PullRequest, bool) {
	finding, err := s.Store.Finding(r.Context(), findingID)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "finding not found")
		return store.Finding{}, store.PullRequest{}, false
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "load finding")
		return store.Finding{}, store.PullRequest{}, false
	}
	var pr store.PullRequest
	if err := s.Store.DB.WithContext(r.Context()).Take(&pr, finding.PullRequestID).Error; err != nil {
		writeAPIError(w, http.StatusNotFound, "finding not found")
		return store.Finding{}, store.PullRequest{}, false
	}
	if pr.RepositoryID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "finding not found")
		return store.Finding{}, store.PullRequest{}, false
	}
	return finding, pr, true
}

func ruleExamples(body []ruleExampleBody) []store.RuleExample {
	out := make([]store.RuleExample, 0, len(body))
	for _, example := range body {
		out = append(out, store.RuleExample{
			FindingID: strings.TrimSpace(example.FindingID),
			Path:      strings.TrimSpace(example.Path),
			Category:  strings.TrimSpace(example.Category),
			Title:     strings.TrimSpace(example.Title),
			Anchor:    strings.TrimSpace(example.Anchor),
			Severity:  strings.TrimSpace(example.Severity),
		})
	}
	return out
}

func ruleAuditData(rule store.ReviewRule) map[string]any {
	return map[string]any{
		"rule_key":       rule.RuleKey,
		"version":        rule.Version,
		"status":         rule.Status,
		"decision":       rule.Decision,
		"path_scope":     rule.PathScope,
		"category":       rule.Category,
		"approved_by":    rule.ApprovedBy,
		"evidence_limit": rule.EvidenceExpiresAt,
	}
}

func writeRuleError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrInvalidRule) {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeAPIError(w, http.StatusInternalServerError, "update review rule")
}
