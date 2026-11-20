package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// findingActionBody carries the optional reason a panel action records in the
// audit log.
type findingActionBody struct {
	Reason string `json:"reason"`
}

// withdrawFinding marks a finding dismissed from the panel. The panel is the
// documented path in polling mode, so it must produce exactly the same state a
// /qa dismiss does: the finding is withdrawn, the audit row records who did it,
// the GitHub thread is resolved and the gate is recomputed.
func (s *server) withdrawFinding(w http.ResponseWriter, r *http.Request) {
	s.findingTransition(w, r, store.FindingWithdrawn)
}

// confirmFinding undoes a dismissal, bringing the finding back to open.
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
	changed, err := s.Store.SetFindingStatusAudited(r.Context(), finding.ID, status, store.FindingAudit{
		Action: action,
		Actor:  user.Login,
		Reason: body.Reason,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "update finding")
		return
	}

	if s.Findings != nil {
		if changed {
			_, _ = s.Findings.SetFindingThread(r.Context(), pr.ID, finding, status == store.FindingWithdrawn)
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
// admin, or the lead of the repository's team.
func (s *server) authorizedPR(w http.ResponseWriter, r *http.Request, user store.User, prID uint) (store.PullRequest, bool) {
	var pr store.PullRequest
	if err := s.Store.DB.WithContext(r.Context()).Take(&pr, prID).Error; err != nil {
		writeAPIError(w, http.StatusNotFound, "pull request not found")
		return store.PullRequest{}, false
	}
	if !user.IsAdmin {
		role, err := s.Store.RepoRole(r.Context(), user.ID, pr.RepositoryID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusInternalServerError, "load repository role")
			return store.PullRequest{}, false
		}
		if role != "lead" {
			writeAPIError(w, http.StatusForbidden, "forbidden")
			return store.PullRequest{}, false
		}
	}
	return pr, true
}
