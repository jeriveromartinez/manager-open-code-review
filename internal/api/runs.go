package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/jeriveromartinez/qa-control-center/internal/auth"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
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

const (
	// runHistoryDefaultLimit is the history page size when the client does not
	// ask for one. Unlike the log tail, a larger page only delays the first
	// paint, so anything outside 1-200 is a client bug and is rejected.
	runHistoryDefaultLimit = 50
	runHistoryMaxLimit     = 200
)

// runHistoryDTO is a finished run. It keeps the active-run shape so the panel
// can merge both lists into one, and adds the fields only the history has.
type runHistoryDTO struct {
	activeRunDTO
	ErrorCode       string  `json:"error_code"`
	ErrorMessage    string  `json:"error_message"`
	FinishedAt      *string `json:"finished_at"`
	DurationSeconds *int64  `json:"duration_seconds"`
}

type runHistoryResponse struct {
	Runs       []runHistoryDTO `json:"runs"`
	NextBefore int64           `json:"next_before"`
	HasMore    bool            `json:"has_more"`
}

// knownRunStatuses are every status a run can hold, so the status filter can be
// validated before reaching the store. Queued and running are accepted but the
// history never returns them, matching the panel's status facet.
var knownRunStatuses = map[string]bool{
	store.RunQueued:       true,
	store.RunRunning:      true,
	store.RunSucceeded:    true,
	store.RunInconclusive: true,
	store.RunFailed:       true,
	store.RunSuperseded:   true,
}

func parseHistoryBefore(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("before")
	if raw == "" {
		return 0, true
	}
	before, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || before < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid before")
		return 0, false
	}
	return before, true
}

func parseHistoryLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return runHistoryDefaultLimit, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > runHistoryMaxLimit {
		writeAPIError(w, http.StatusBadRequest, "invalid limit")
		return 0, false
	}
	return limit, true
}

// parseHistoryStatuses accepts both repeated keys and CSV, the two shapes the
// panel and ad-hoc clients use, and rejects any status the store cannot know.
func parseHistoryStatuses(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var statuses []string
	for _, raw := range r.URL.Query()["status"] {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if !knownRunStatuses[value] {
				writeAPIError(w, http.StatusBadRequest, "invalid status")
				return nil, false
			}
			statuses = append(statuses, value)
		}
	}
	return statuses, true
}

// historyRepoFilter resolves the repo parameter. The panel sends the full name
// while direct API clients send the numeric id; a name that matches no
// repository yields match=false so the caller can answer with an empty page
// instead of the unfiltered list. The store still applies the team scope, so
// resolving another team's repository does not leak its runs.
func (s *server) historyRepoFilter(w http.ResponseWriter, r *http.Request) (id int64, match, ok bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("repo"))
	if raw == "" {
		return 0, true, true
	}
	if numeric, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if numeric <= 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid repo")
			return 0, false, false
		}
		return numeric, true, true
	}

	var repo store.Repository
	err := s.Store.DB.WithContext(r.Context()).Select("id").Where("full_name = ?", raw).Take(&repo).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, true
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "look up repository")
		return 0, false, false
	}
	return repo.ID, true, true
}

func durationSeconds(startedAt, finishedAt *time.Time) *int64 {
	if startedAt == nil || finishedAt == nil {
		return nil
	}
	seconds := int64(finishedAt.Sub(*startedAt) / time.Second)
	return &seconds
}

func runHistoryDTOOf(run store.RunHistoryRow) runHistoryDTO {
	return runHistoryDTO{
		activeRunDTO: activeRunDTO{
			ID: run.ID, PullRequestID: run.PullRequestID,
			PRNumber: run.PRNumber, PRTitle: run.PRTitle,
			Repo: run.Repo, Team: run.Team, Profile: run.Profile,
			Trigger: run.Trigger, Status: run.Status, Attempt: run.Attempt,
			StartedAt: formatTimePtr(run.StartedAt),
			CreatedAt: run.CreatedAt.UTC().Format(time.RFC3339),
		},
		ErrorCode:       run.ErrorCode,
		ErrorMessage:    run.ErrorMessage,
		FinishedAt:      formatTimePtr(run.FinishedAt),
		DurationSeconds: durationSeconds(run.StartedAt, run.FinishedAt),
	}
}

// runHistory lists the finished runs the caller may see, newest first. The
// store applies the same team scope as the active list, so this handler only
// validates the cursor and filters.
func (s *server) runHistory(w http.ResponseWriter, r *http.Request) {
	user, _ := auth.UserFrom(r.Context())
	before, ok := parseHistoryBefore(w, r)
	if !ok {
		return
	}
	limit, ok := parseHistoryLimit(w, r)
	if !ok {
		return
	}
	statuses, ok := parseHistoryStatuses(w, r)
	if !ok {
		return
	}
	repoID, repoMatch, ok := s.historyRepoFilter(w, r)
	if !ok {
		return
	}
	if !repoMatch {
		writeJSON(w, http.StatusOK, runHistoryResponse{Runs: []runHistoryDTO{}})
		return
	}

	page, err := s.Store.ReviewRunHistory(r.Context(), user, store.RunHistoryFilter{
		BeforeID: before,
		Limit:    limit,
		Statuses: statuses,
		RepoID:   repoID,
		Trigger:  r.URL.Query().Get("trigger"),
		Query:    r.URL.Query().Get("q"),
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list review run history")
		return
	}

	dtos := make([]runHistoryDTO, 0, len(page.Runs))
	for _, run := range page.Runs {
		dtos = append(dtos, runHistoryDTOOf(run))
	}
	writeJSON(w, http.StatusOK, runHistoryResponse{
		Runs: dtos, NextBefore: page.NextBefore, HasMore: page.HasMore,
	})
}
