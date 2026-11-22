package api

import (
	"net/http"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
)

// activeRunDTO is one queued or running run of the live reviews view (#123).
// It carries the repo, team and profile names so the list needs no extra
// lookups, and both timestamps the panel needs to render elapsed time.
type activeRunDTO struct {
	ID             uint    `json:"id"`
	PullRequestID  uint    `json:"pull_request_id"`
	PRNumber       int     `json:"pr_number"`
	PRTitle        string  `json:"pr_title"`
	Repo           string  `json:"repo"`
	Team           string  `json:"team"`
	Profile        string  `json:"profile"`
	Trigger        string  `json:"trigger"`
	Status         string  `json:"status"`
	Attempt        int     `json:"attempt"`
	Phase          string  `json:"phase"`
	PhaseStartedAt *string `json:"phase_started_at"`
	StartedAt      *string `json:"started_at"`
	CreatedAt      string  `json:"created_at"`
}

// activeRuns lists the queued and running runs the caller may see. The scope is
// the same as the dashboard: a run of another team's repository is not listed.
func (s *server) activeRuns(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	runs, err := s.Store.ListActiveReviewRuns(r.Context(), user)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list active review runs")
		return
	}
	dtos := make([]activeRunDTO, 0, len(runs))
	for _, run := range runs {
		dtos = append(dtos, activeRunDTO{
			ID: run.ID, PullRequestID: run.PullRequestID,
			PRNumber: run.PRNumber, PRTitle: run.PRTitle,
			Repo: run.Repo, Team: run.Team, Profile: run.Profile,
			Trigger: run.Trigger, Status: run.Status, Attempt: run.Attempt,
			Phase: run.Phase, PhaseStartedAt: formatTimePtr(run.PhaseStartedAt),
			StartedAt: formatTimePtr(run.StartedAt),
			CreatedAt: run.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": dtos})
}
