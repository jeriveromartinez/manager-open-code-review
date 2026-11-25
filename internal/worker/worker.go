// Package worker exposes the review worker's health and queue metrics over
// HTTP. The worker has no public interface today, so this is the only way an
// operator can tell whether it is alive and whether the queue is moving
// without opening a psql session.
package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// Dependency reports whether an external binary the worker needs still runs.
// It is checked on each health request instead of once at startup so a binary
// that disappears from the image after a redeploy is caught.
type Dependency struct {
	Name    string
	Version func(context.Context) (string, error)
}

// StatsReader is the store surface the endpoint needs, kept narrow so the
// package does not depend on the whole store.
type StatsReader interface {
	QueueStats(ctx context.Context) (store.QueueStats, error)
	StageStats(ctx context.Context) ([]store.StageStats, error)
	RunStatsSince(ctx context.Context, cutoff time.Time) (store.RunStats, error)
	Ping(ctx context.Context) error
}

// Config configures the health server.
type Config struct {
	Addr         string
	Dependencies []Dependency
	Stats        StatsReader
	// StatsWindow is how far back the run aggregate looks.
	StatsWindow time.Duration
	// StaleQueueAfter is how long a job may wait with nothing running before
	// the queue counts as stalled. It is what turns a wedged worker into an
	// alert instead of a silent backlog.
	StaleQueueAfter time.Duration
}

// Server is the worker's health and metrics endpoint.
type Server struct {
	cfg Config
	now func() time.Time
	// startedAt is what distinguishes "the process is alive" from "the process
	// just restarted", which matters when a wedged worker is recycled.
	startedAt time.Time

	mu   sync.RWMutex
	last error
}

// New builds a health server. A nil Stats leaves the metrics endpoints empty
// instead of failing, so the worker still starts with a degraded database.
func New(cfg Config) *Server {
	return &Server{cfg: cfg, now: time.Now, startedAt: time.Now()}
}

// Handler returns the worker routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /metrics", s.metrics)
	return mux
}

type dependencyStatus struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

type healthResponse struct {
	Status       string             `json:"status"`
	UptimeSecond int                `json:"uptime_seconds"`
	Database     dependencyStatus   `json:"database"`
	Dependencies []dependencyStatus `json:"dependencies"`
}

// health reports ready only when every dependency works. The queue backlog is
// deliberately not part of it: a busy queue is not a broken worker, and a
// healthcheck that fails on load makes an orchestrator kill a healthy process.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	resp := healthResponse{
		Status:       "ok",
		UptimeSecond: int(s.now().Sub(s.startedAt).Seconds()),
		Database:     dependencyStatus{Name: "postgres", OK: true},
	}

	if s.cfg.Stats != nil {
		if err := s.cfg.Stats.Ping(r.Context()); err != nil {
			resp.Database = dependencyStatus{Name: "postgres", OK: false, Error: err.Error()}
		}
	}

	for _, dep := range s.cfg.Dependencies {
		status := dependencyStatus{Name: dep.Name, OK: true}
		version, err := dep.Version(r.Context())
		if err != nil {
			status.OK = false
			status.Error = err.Error()
		} else {
			status.Version = version
		}
		resp.Dependencies = append(resp.Dependencies, status)
		if !status.OK {
			resp.Status = "degraded"
		}
	}
	if !resp.Database.OK {
		resp.Status = "degraded"
	}

	code := http.StatusOK
	if resp.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	s.setLast(nil)
	writeJSON(w, code, resp)
}

type stageStatus struct {
	store.StageStats
	// Stalled is decided here because this process owns the threshold. A queue
	// with no consumer and an old backlog is stalled; a run phase with active
	// runs but no recent activity is stalled.
	Stalled bool `json:"stalled"`
}

type metricsResponse struct {
	Queue  store.QueueStats `json:"queue"`
	Stages []stageStatus    `json:"stages"`
	Runs   store.RunStats   `json:"runs"`
	// Stalled is kept for older monitors: true when any stage is stalled or the
	// aggregate backlog matches the old rule.
	Stalled bool `json:"stalled"`
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Stats == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "stats unavailable"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	queue, err := s.cfg.Stats.QueueStats(ctx)
	if err != nil {
		s.setLast(err)
		slog.Error("read queue stats", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "queue stats unavailable"})
		return
	}
	stages, err := s.cfg.Stats.StageStats(ctx)
	if err != nil {
		s.setLast(err)
		slog.Error("read stage stats", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "stage stats unavailable"})
		return
	}
	runs, err := s.cfg.Stats.RunStatsSince(ctx, s.now().Add(-s.cfg.StatsWindow))
	if err != nil {
		s.setLast(err)
		slog.Error("read run stats", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "run stats unavailable"})
		return
	}

	statuses := make([]stageStatus, 0, len(stages))
	stalled := queue.Available > 0 &&
		queue.Running == 0 &&
		time.Duration(queue.OldestAvailableSeconds)*time.Second >= s.cfg.StaleQueueAfter
	for _, stage := range stages {
		stageStalled := stageIsStalled(stage, s.cfg.StaleQueueAfter)
		stalled = stalled || stageStalled
		if stageStalled {
			slog.Warn("review stage stalled",
				"stage", stage.Name,
				"available", stage.Available,
				"running", stage.Running,
				"retryable", stage.Retryable,
				"discarded", stage.Discarded,
				"oldest_pending_seconds", stage.OldestPendingSeconds,
				"last_progress_seconds", stage.LastProgressSeconds,
				"stale_after", s.cfg.StaleQueueAfter)
		}
		statuses = append(statuses, stageStatus{StageStats: stage, Stalled: stageStalled})
	}

	s.setLast(nil)
	writeJSON(w, http.StatusOK, metricsResponse{Queue: queue, Stages: statuses, Runs: runs, Stalled: stalled})
}

// stageIsStalled tells a stopped stage from work in flight. A queue is stalled
// only when nobody consumes it; a run phase is stalled only when runs are
// active but none has produced activity, so an old run that keeps logging
// stays quiet while one that only heartbeats does not.
func stageIsStalled(stage store.StageStats, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		return false
	}
	if stage.Kind == store.StageKindRun {
		return stage.Running > 0 &&
			time.Duration(stage.LastProgressSeconds)*time.Second >= staleAfter
	}
	return stage.Available > 0 &&
		stage.Running == 0 &&
		time.Duration(stage.OldestPendingSeconds)*time.Second >= staleAfter
}

func (s *Server) setLast(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = err
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
