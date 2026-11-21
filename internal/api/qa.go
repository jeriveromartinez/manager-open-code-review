package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

type categoryCountResponse struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

type repoCountResponse struct {
	Repo  string `json:"repo"`
	Count int    `json:"count"`
}

// qaSummaryResponse carries the windows the counts were computed over, so the
// panel labels them from the server instead of repeating the constants.
type qaSummaryResponse struct {
	ReadyForHuman        int                     `json:"ready_for_human"`
	Inconclusive         int                     `json:"inconclusive"`
	RunsRunning          int                     `json:"runs_running"`
	RunsQueued           int                     `json:"runs_queued"`
	Untriaged            severityCountsResponse  `json:"untriaged"`
	Confirmed            int                     `json:"confirmed"`
	FalsePositive        int                     `json:"false_positive"`
	PrecisionWindowDays  int                     `json:"precision_window_days"`
	Categories           []categoryCountResponse `json:"categories"`
	ConfirmedByRepo      []repoCountResponse     `json:"confirmed_by_repo"`
	CategoryWindowDays   int                     `json:"category_window_days"`
	ValidationSLASeconds int64                   `json:"validation_sla_seconds"`
}

// qaSummary takes an optional team_id. It only narrows the user's own scope,
// so asking for a team the user cannot see returns an empty summary.
func (s *server) qaSummary(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())

	var filter store.QAFilter
	if raw := r.URL.Query().Get("team_id"); raw != "" {
		id, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || id == 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid team_id")
			return
		}
		teamID := uint(id)
		filter.TeamID = &teamID
	}

	summary, err := s.Store.QASummary(r.Context(), user, time.Now().UTC(), filter)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "compute QA summary")
		return
	}

	categories := make([]categoryCountResponse, 0, len(summary.Categories))
	for _, c := range summary.Categories {
		categories = append(categories, categoryCountResponse{Category: c.Category, Count: c.Count})
	}
	repos := make([]repoCountResponse, 0, len(summary.ConfirmedByRepo))
	for _, c := range summary.ConfirmedByRepo {
		repos = append(repos, repoCountResponse{Repo: c.Repo, Count: c.Count})
	}
	writeJSON(w, http.StatusOK, qaSummaryResponse{
		ReadyForHuman:        summary.ReadyForHuman,
		Inconclusive:         summary.Inconclusive,
		RunsRunning:          summary.RunsRunning,
		RunsQueued:           summary.RunsQueued,
		Untriaged:            severityCountsDTO(summary.Untriaged),
		Confirmed:            summary.Confirmed,
		FalsePositive:        summary.FalsePositive,
		PrecisionWindowDays:  int(store.PrecisionWindow / (24 * time.Hour)),
		Categories:           categories,
		ConfirmedByRepo:      repos,
		CategoryWindowDays:   int(store.CategoryWindow / (24 * time.Hour)),
		ValidationSLASeconds: int64(s.Cfg.QAValidationSLA / time.Second),
	})
}
