package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	// reviewLogDefaultLimit is the page size when the client does not ask for
	// one. The panel tails the log with after=next_after, so small pages refetch
	// often and a big one would only delay the first paint.
	reviewLogDefaultLimit = 500
	reviewLogMaxLimit     = 2000
)

type reviewRunLogDTO struct {
	Seq        int64  `json:"seq"`
	TS         string `json:"ts"`
	Source     string `json:"source"`
	Invocation string `json:"invocation"`
	Level      string `json:"level"`
	Phase      string `json:"phase"`
	Line       string `json:"line"`
}

// reviewRunLogsResponse is one page of a run log. next_after is the seq to send
// as after= on the next poll; has_more says whether the page was full.
type reviewRunLogsResponse struct {
	RunID     uint              `json:"run_id"`
	Entries   []reviewRunLogDTO `json:"entries"`
	NextAfter int64             `json:"next_after"`
	HasMore   bool              `json:"has_more"`
}

func reviewRunLogDTOOf(entry store.ReviewRunLog) reviewRunLogDTO {
	return reviewRunLogDTO{
		Seq: entry.Seq, TS: entry.TS.UTC().Format(time.RFC3339Nano),
		Source: entry.Source, Invocation: entry.Invocation,
		Level: entry.Level, Phase: entry.Phase, Line: entry.Line,
	}
}

func parseReviewLogAfter(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("after")
	if raw == "" {
		return 0, true
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid after")
		return 0, false
	}
	return after, true
}

func parseReviewLogLimit(w http.ResponseWriter, r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return reviewLogDefaultLimit
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid limit")
		return 0
	}
	if limit > reviewLogMaxLimit {
		limit = reviewLogMaxLimit
	}
	return limit
}

// reviewRunLogs returns a page of a run's log. The run is resolved to its pull
// request first so the regular visibility rule applies: knowing a run ID must
// not expose the log of a PR the caller cannot see.
func (s *server) reviewRunLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUintID(w, r)
	if !ok {
		return
	}
	prID, err := s.Store.ReviewRunPRID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, "review run not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "look up review run")
		return
	}
	if _, ok := s.visiblePRView(w, r, prID); !ok {
		return
	}
	after, ok := parseReviewLogAfter(w, r)
	if !ok {
		return
	}
	limit := parseReviewLogLimit(w, r)
	if limit == 0 {
		return
	}

	entries, err := s.Store.ReviewRunLogs(r.Context(), id, after, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list review run logs")
		return
	}
	dtos := make([]reviewRunLogDTO, 0, len(entries))
	nextAfter := after
	for _, entry := range entries {
		dtos = append(dtos, reviewRunLogDTOOf(entry))
		nextAfter = entry.Seq
	}
	writeJSON(w, http.StatusOK, reviewRunLogsResponse{
		RunID: id, Entries: dtos, NextAfter: nextAfter, HasMore: len(entries) == limit,
	})
}
