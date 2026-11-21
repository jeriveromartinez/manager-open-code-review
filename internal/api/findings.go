package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// findingActionBody carries the optional reason a panel action records in the
// audit log.
type findingActionBody struct {
	Reason string `json:"reason"`
}

// withdrawFinding marks a finding a false positive from the panel. The panel is
// the documented path in polling mode, so the finding ends in the same state a
// /qa dismiss leaves: withdrawn, audited, its GitHub thread resolved and the
// gate recomputed. On top of that it records QA's verdict, and unlike /qa
// (lead only) the team's QA may use it, because triage is QA's job.
func (s *server) withdrawFinding(w http.ResponseWriter, r *http.Request) {
	s.findingTransition(w, r, store.FindingWithdrawn)
}

// confirmFinding records that a person vouches for the finding. On a dismissed
// finding it also brings it back to open.
func (s *server) confirmFinding(w http.ResponseWriter, r *http.Request) {
	s.findingTransition(w, r, store.FindingOpen)
}

func (s *server) findingTransition(w http.ResponseWriter, r *http.Request, status string) {
	prID, ok := parseUintID(w, r)
	if !ok {
		return
	}
	findingID := chi.URLParam(r, "findingID")
	if findingID == "" {
		writeAPIError(w, http.StatusBadRequest, "missing finding id")
		return
	}

	var body findingActionBody
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}

	user, _ := auth.UserFrom(r.Context())
	pr, ok := s.authorizedPR(w, r, user, prID)
	if !ok {
		return
	}

	finding, err := s.Store.Finding(r.Context(), findingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "finding not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "load finding")
		return
	}
	// A finding from another pull request must look absent, not forbidden.
	if finding.PullRequestID != pr.ID {
		writeAPIError(w, http.StatusNotFound, "finding not found")
		return
	}

	action := "finding.withdrawn"
	if status == store.FindingOpen {
		action = "finding.confirmed"
	}
	changed, err := s.Store.TriageFinding(r.Context(), finding.ID, status, store.FindingAudit{
		Action:  action,
		Actor:   user.Login,
		ActorID: &user.ID,
		Reason:  body.Reason,
	})
	if errors.Is(err, store.ErrFindingResolved) {
		writeAPIError(w, http.StatusConflict, "finding is resolved")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "update finding")
		return
	}
	// Notify even when the status already matched: the PR verdict may change.
	s.notify(r.Context(), events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(pr.ID), 10)})

	if s.Findings != nil {
		if changed {
			// The decision is already stored; a thread that fails to follow is
			// reconciled by the next republish, so it must not fail the request.
			if _, err := s.Findings.SetFindingThread(r.Context(), pr.ID, finding, status == store.FindingWithdrawn); err != nil {
				slog.Warn("mirror finding triage on GitHub thread", "finding_id", finding.ID, "error", err)
			}
		}
		// Republish even when the status already matched: the gate may have
		// drifted while the action was repeated.
		if err := s.Findings.Republish(r.Context(), pr.ID); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "republish check")
			return
		}
	}

	updated, err := s.Store.Finding(r.Context(), finding.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload finding")
		return
	}
	writeJSON(w, http.StatusOK, findingDTOOf(updated))
}

// authorizedPR loads the pull request and enforces the same rule as retry: an
// admin, or the lead or QA of the repository's team. Triage is QA's daily job;
// developers stay out because they would be judging their own code.
func (s *server) authorizedPR(w http.ResponseWriter, r *http.Request, user store.User, prID uint) (store.PullRequest, bool) {
	var pr store.PullRequest
	if err := s.Store.DB.WithContext(r.Context()).Take(&pr, prID).Error; err != nil {
		writeAPIError(w, http.StatusNotFound, "pull request not found")
		return store.PullRequest{}, false
	}
	if !s.canActOnPR(w, r, user, pr) {
		return store.PullRequest{}, false
	}
	return pr, true
}

// canActOnPR writes the error response itself when it returns false.
func (s *server) canActOnPR(w http.ResponseWriter, r *http.Request, user store.User, pr store.PullRequest) bool {
	if user.IsAdmin {
		return true
	}
	role, err := s.Store.RepoRole(r.Context(), user.ID, pr.RepositoryID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "load repository role")
		return false
	}
	if role != "lead" && role != "qa" {
		writeAPIError(w, http.StatusForbidden, "forbidden")
		return false
	}
	return true
}
