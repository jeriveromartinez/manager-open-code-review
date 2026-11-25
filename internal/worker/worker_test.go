package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

type stubStats struct {
	queue     store.QueueStats
	stages    []store.StageStats
	runs      store.RunStats
	queueErr  error
	stagesErr error
	runsErr   error
	pingErr   error
	pingCalls int
}

func (s *stubStats) Ping(context.Context) error {
	s.pingCalls++
	return s.pingErr
}

func (s *stubStats) QueueStats(context.Context) (store.QueueStats, error) {
	if s.queueErr != nil {
		return store.QueueStats{}, s.queueErr
	}
	return s.queue, nil
}

func (s *stubStats) StageStats(context.Context) ([]store.StageStats, error) {
	if s.stagesErr != nil {
		return nil, s.stagesErr
	}
	return s.stages, nil
}

func (s *stubStats) RunStatsSince(context.Context, time.Time) (store.RunStats, error) {
	if s.runsErr != nil {
		return store.RunStats{}, s.runsErr
	}
	return s.runs, nil
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthReportsDependencies(t *testing.T) {
	stats := &stubStats{}
	srv := New(Config{
		Stats: stats,
		Dependencies: []Dependency{
			{Name: "ocr", Version: func(context.Context) (string, error) { return "1.14.0", nil }},
			{Name: "opencode", Version: func(context.Context) (string, error) { return "1.19.0", nil }},
		},
	})

	rec := get(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if len(body.Dependencies) != 2 || body.Dependencies[0].Version != "1.14.0" {
		t.Errorf("dependencies = %+v, want ocr and opencode with versions", body.Dependencies)
	}
	if stats.pingCalls != 1 {
		t.Errorf("Ping calls = %d, want 1", stats.pingCalls)
	}
}

// TestHealthDegradesWhenBinaryIsGone covers the real failure an operator hits:
// the worker is up but ocr or opencode no longer runs inside the image.
func TestHealthDegradesWhenBinaryIsGone(t *testing.T) {
	srv := New(Config{
		Stats: &stubStats{},
		Dependencies: []Dependency{
			{Name: "ocr", Version: func(context.Context) (string, error) {
				return "", errors.New("exec: ocr: executable file not found")
			}},
		},
	})

	rec := get(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	if len(body.Dependencies) != 1 || body.Dependencies[0].OK {
		t.Errorf("dependency = %+v, want a failed ocr", body.Dependencies)
	}
}

func TestHealthDegradesWhenDatabaseIsDown(t *testing.T) {
	srv := New(Config{
		Stats: &stubStats{pingErr: errors.New("connection refused")},
	})

	rec := get(t, srv.Handler(), "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Database.OK {
		t.Error("database reported healthy with a failing ping")
	}
}

func TestMetricsReportQueueAndRuns(t *testing.T) {
	stats := &stubStats{
		queue: store.QueueStats{Available: 3, Running: 2, Scheduled: 1, Discarded: 0, OldestAvailableSeconds: 30},
		runs:  store.RunStats{Succeeded: 10, Failed: 1, Inconclusive: 2, DurationP95Seconds: 240},
	}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: 10 * time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}

	var body metricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Queue.Available != 3 || body.Queue.Running != 2 {
		t.Errorf("queue = %+v", body.Queue)
	}
	if body.Runs.Succeeded != 10 || body.Runs.DurationP95Seconds != 240 {
		t.Errorf("runs = %+v", body.Runs)
	}
	if body.Stalled {
		t.Error("stalled = true while jobs are running")
	}
}

// TestMetricsDetectStalledQueue is the alert the issue asks for: a backlog with
// nothing consuming it, which is exactly what a dead worker looks like.
func TestMetricsDetectStalledQueue(t *testing.T) {
	stats := &stubStats{
		queue: store.QueueStats{Available: 5, Running: 0, OldestAvailableSeconds: 3600},
	}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: 10 * time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body metricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.Stalled {
		t.Errorf("stalled = false with %d waiting for %ds", body.Queue.Available, body.Queue.OldestAvailableSeconds)
	}
}

// TestMetricsFreshBacklogIsNotStalled keeps a burst of legitimate work from
// paging anyone: only an old backlog means the worker stopped.
func TestMetricsFreshBacklogIsNotStalled(t *testing.T) {
	stats := &stubStats{
		queue: store.QueueStats{Available: 5, Running: 0, OldestAvailableSeconds: 20},
	}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: 10 * time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	var body metricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Stalled {
		t.Error("a 20s backlog was reported as stalled")
	}
}

func TestMetricsWithoutStatsIsUnavailable(t *testing.T) {
	srv := New(Config{})
	rec := get(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestMetricsSurfacesStoreFailure(t *testing.T) {
	stats := &stubStats{queueErr: errors.New("database is gone")}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestMetricsStalledPublishStageWhileSyncRuns is the regression from #154: the
// aggregate queue reports a running sync job, but the publish stage has an old
// backlog and no consumer, so it must still be flagged.
func TestMetricsStalledPublishStageWhileSyncRuns(t *testing.T) {
	stats := &stubStats{
		queue: store.QueueStats{Available: 0, Running: 1},
		stages: []store.StageStats{
			{Name: store.StageQueue, Kind: store.StageKindQueue, Running: 1, LastProgressSeconds: 5},
			{Name: store.StagePublish, Kind: store.StageKindQueue, Available: 3, Running: 0,
				OldestPendingSeconds: 3600, Retryable: 1, Discarded: 1},
		},
	}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: 10 * time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body metricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !body.Stalled {
		t.Error("aggregate stalled = false, want true while the publish stage is stopped")
	}
	stages := map[string]stageStatus{}
	for _, stage := range body.Stages {
		stages[stage.Name] = stage
	}
	if !stages[store.StagePublish].Stalled {
		t.Errorf("publish stage = %+v, want stalled", stages[store.StagePublish])
	}
	if stages[store.StageQueue].Stalled {
		t.Errorf("queue stage = %+v, want healthy", stages[store.StageQueue])
	}
}

// TestMetricsOldRunWithFreshActivityIsNotStalled keeps a legitimately long
// review from paging: age alone is not a stall when logs keep arriving.
func TestMetricsOldRunWithFreshActivityIsNotStalled(t *testing.T) {
	stats := &stubStats{
		stages: []store.StageStats{
			{Name: store.StageClone, Kind: store.StageKindRun, Running: 1,
				OldestPendingSeconds: 7200, LastProgressSeconds: 30},
		},
	}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: 10 * time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var body metricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Stalled {
		t.Error("stalled = true for an old run that heartbeated 30s ago")
	}
}

// TestMetricsStalledRunWithoutProgress is the other half: active runs whose
// newest heartbeat is older than the window are a stall.
func TestMetricsStalledRunWithoutProgress(t *testing.T) {
	stats := &stubStats{
		stages: []store.StageStats{
			{Name: store.StageVerify, Kind: store.StageKindRun, Running: 2,
				OldestPendingSeconds: 7200, LastProgressSeconds: 900},
		},
	}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: 10 * time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	var body metricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Stages) != 1 || !body.Stages[0].Stalled {
		t.Fatalf("stages = %+v, want the verify stage stalled", body.Stages)
	}
}

func TestMetricsSurfacesStageStatsFailure(t *testing.T) {
	stats := &stubStats{stagesErr: errors.New("database is gone")}
	srv := New(Config{Stats: stats, StatsWindow: time.Hour, StaleQueueAfter: time.Minute})

	rec := get(t, srv.Handler(), "/metrics")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
