package monitor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
)

type workerStub struct {
	healthCode  int
	healthBody  string
	metricsCode int
	metricsBody string
}

func (s workerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/healthz":
		w.WriteHeader(s.healthCode)
		_, _ = w.Write([]byte(s.healthBody))
	case "/metrics":
		w.WriteHeader(s.metricsCode)
		_, _ = w.Write([]byte(s.metricsBody))
	default:
		http.NotFound(w, r)
	}
}

const (
	okHealth = `{"status":"ok","uptime_seconds":42,"database":{"name":"postgres","ok":true},` +
		`"dependencies":[{"name":"ocr","ok":true,"version":"1.14.0"},{"name":"opencode","ok":true,"version":"1.19.0"}]}`
	degradedHealth = `{"status":"degraded","uptime_seconds":42,"database":{"name":"postgres","ok":true},` +
		`"dependencies":[{"name":"ocr","ok":false,"error":"gone"}]}`
	idleMetrics   = `{"queue":{"available":0,"running":0,"scheduled":0,"discarded":0,"oldest_available_seconds":0},"runs":{"succeeded":0,"failed":0,"inconclusive":0,"duration_p95_seconds":0},"stalled":false}`
	stalledMetric = `{"queue":{"available":5,"running":0,"scheduled":0,"discarded":0,"oldest_available_seconds":3600},"runs":{"succeeded":0,"failed":0,"inconclusive":0,"duration_p95_seconds":0},"stalled":true}`
)

func TestFetcherObservation(t *testing.T) {
	tests := []struct {
		name       string
		stub       workerStub
		wantReach  bool
		wantHealth int
		wantProbs  []string
	}{
		{
			name:       "healthy worker",
			stub:       workerStub{healthCode: http.StatusOK, healthBody: okHealth, metricsCode: http.StatusOK, metricsBody: idleMetrics},
			wantHealth: http.StatusOK,
		},
		{
			name:       "degraded worker",
			stub:       workerStub{healthCode: http.StatusServiceUnavailable, healthBody: degradedHealth, metricsCode: http.StatusOK, metricsBody: idleMetrics},
			wantHealth: http.StatusServiceUnavailable,
			wantProbs:  []string{monitor.CodeDependencyDown + ":ocr"},
		},
		{
			name:       "stalled queue",
			stub:       workerStub{healthCode: http.StatusOK, healthBody: okHealth, metricsCode: http.StatusOK, metricsBody: stalledMetric},
			wantHealth: http.StatusOK,
			wantProbs:  []string{monitor.CodeQueueStalled},
		},
		{
			name:      "malformed health body",
			stub:      workerStub{healthCode: http.StatusOK, healthBody: "not json", metricsCode: http.StatusOK, metricsBody: idleMetrics},
			wantReach: true,
			wantProbs: []string{monitor.CodeWorkerUnreachable},
		},
		{
			// A 200 with HTML (a proxy or the SPA) is not a health report.
			name:      "health endpoint replaced by something else",
			stub:      workerStub{healthCode: http.StatusOK, healthBody: "<html>hello</html>", metricsCode: http.StatusOK, metricsBody: idleMetrics},
			wantReach: true,
			wantProbs: []string{monitor.CodeWorkerUnreachable},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.stub)
			defer srv.Close()

			obs := monitor.NewFetcher(srv.URL, srv.Client()).Observe(context.Background())
			if tt.wantReach != (obs.ReachError != nil) {
				t.Fatalf("ReachError = %v, want reach error = %v", obs.ReachError, tt.wantReach)
			}
			if tt.wantHealth != 0 && obs.HealthCode != tt.wantHealth {
				t.Errorf("HealthCode = %d, want %d", obs.HealthCode, tt.wantHealth)
			}
			got := keys(monitor.Evaluate(obs))
			if len(got) != len(tt.wantProbs) {
				t.Fatalf("problems = %v, want %v", got, tt.wantProbs)
			}
			for i := range got {
				if got[i] != tt.wantProbs[i] {
					t.Errorf("problem %d = %q, want %q", i, got[i], tt.wantProbs[i])
				}
			}
		})
	}
}

func TestFetcherRefusedConnection(t *testing.T) {
	srv := httptest.NewServer(workerStub{})
	base := srv.URL
	srv.Close()

	obs := monitor.NewFetcher(base, &http.Client{Timeout: 2 * time.Second}).Observe(context.Background())
	if obs.ReachError == nil {
		t.Fatal("ReachError = nil, want a connection error")
	}
	problems := keys(monitor.Evaluate(obs))
	if len(problems) != 1 || problems[0] != monitor.CodeWorkerUnreachable {
		t.Errorf("problems = %v, want [%s]", problems, monitor.CodeWorkerUnreachable)
	}
}

type recordedNotifier struct {
	mu     sync.Mutex
	alerts []monitor.Alert
	fail   bool
}

func (n *recordedNotifier) Notify(_ context.Context, a monitor.Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alerts = append(n.alerts, a)
	if n.fail {
		return errors.New("webhook answered 500")
	}
	return nil
}

func (n *recordedNotifier) snapshot() []monitor.Alert {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]monitor.Alert(nil), n.alerts...)
}

type testClock struct{ at time.Time }

func (c *testClock) Now() time.Time           { return c.at }
func (c *testClock) Advance(by time.Duration) { c.at = c.at.Add(by) }

func newPoller(t *testing.T, url string, notifier monitor.Notifier) *monitor.Poller {
	t.Helper()
	return &monitor.Poller{
		Source:     monitor.NewFetcher(url, &http.Client{Timeout: 2 * time.Second}),
		Notifier:   notifier,
		Tracker:    monitor.NewTracker(30 * time.Minute),
		Deliveries: monitor.NewMemoryDeliveryStore(),
		Logger:     discardingLogger(),
		Now:        func() time.Time { return baseTime },
	}
}

func TestPollerTicks(t *testing.T) {
	stub := &workerStub{healthCode: http.StatusOK, healthBody: okHealth, metricsCode: http.StatusOK, metricsBody: idleMetrics}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	notifier := &recordedNotifier{}
	poller := newPoller(t, srv.URL, notifier)

	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	if got := notifier.snapshot(); len(got) != 0 {
		t.Fatalf("alerts for a healthy worker = %d, want 0", len(got))
	}

	stub.healthCode = http.StatusServiceUnavailable
	stub.healthBody = degradedHealth
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	alerts := notifier.snapshot()
	if len(alerts) != 1 {
		t.Fatalf("alerts after the worker degraded = %d, want 1", len(alerts))
	}
	if alerts[0].Code != monitor.CodeDependencyDown || alerts[0].Subject != "ocr" {
		t.Errorf("alert = %+v, want dependency_down:ocr", alerts[0])
	}

	stub.healthCode = http.StatusOK
	stub.healthBody = okHealth
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}
	alerts = notifier.snapshot()
	if len(alerts) != 2 || !alerts[1].Recovered {
		t.Fatalf("alerts after recovery = %+v, want one recovery", alerts)
	}
}

// TestPollerSurvivesWebhookFailure keeps a broken webhook from silencing the
// monitor: the alert that follows must still reach the notifier.
func TestPollerSurvivesWebhookFailure(t *testing.T) {
	stub := &workerStub{healthCode: http.StatusServiceUnavailable, healthBody: degradedHealth, metricsCode: http.StatusOK, metricsBody: idleMetrics}
	worker := httptest.NewServer(stub)
	defer worker.Close()

	var (
		mu       sync.Mutex
		statuses = []int{http.StatusInternalServerError}
		calls    int
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		code := http.StatusOK
		if calls < len(statuses) {
			code = statuses[calls]
		}
		calls++
		w.WriteHeader(code)
	}))
	defer hook.Close()

	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	notifier := monitor.NewWebhookNotifier(hook.URL, hook.Client(), logger)

	clock := &testClock{at: baseTime}
	poller := &monitor.Poller{
		Source:     monitor.NewFetcher(worker.URL, worker.Client()),
		Notifier:   notifier,
		Tracker:    monitor.NewTracker(30 * time.Minute),
		Deliveries: monitor.NewMemoryDeliveryStore(),
		Logger:     logger,
		Now:        clock.Now,
	}

	// Tick one fails to deliver but must not abort the loop.
	if err := poller.Tick(context.Background()); err == nil {
		t.Error("Tick() error = nil, want the webhook failure to be reported")
	}
	if !strings.Contains(logs.String(), "alert delivery failed") {
		t.Errorf("logs = %s, want a delivery failure line", logs.String())
	}

	// A later, different problem still gets delivered, and the failed alert is
	// retried once its backoff expires.
	clock.Advance(time.Minute)
	stub.healthCode = http.StatusOK
	stub.healthBody = okHealth
	stub.metricsBody = stalledMetric
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 4 {
		t.Fatalf("webhook calls = %d, want 4 (failed fire, retried fire, recovery, stalled fire)", calls)
	}
}

// flakyNotifier fails the first failures calls and records every alert id it
// was asked to deliver.
type flakyNotifier struct {
	mu       sync.Mutex
	failures int
	ids      []string
}

func (n *flakyNotifier) Notify(_ context.Context, alert monitor.Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ids = append(n.ids, alert.ID)
	if n.failures > 0 {
		n.failures--
		return errors.New("webhook answered 503")
	}
	return nil
}

func (n *flakyNotifier) deliveredIDs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.ids...)
}

// TestPollerRetriesFailedNotificationIdempotently keeps the event identity
// stable across retries and stops sending once the outbox records delivery.
func TestPollerRetriesFailedNotificationIdempotently(t *testing.T) {
	stub := &workerStub{healthCode: http.StatusServiceUnavailable, healthBody: degradedHealth,
		metricsCode: http.StatusOK, metricsBody: idleMetrics}
	worker := httptest.NewServer(stub)
	defer worker.Close()

	notifier := &flakyNotifier{failures: 2}
	deliveries := monitor.NewMemoryDeliveryStore()
	clock := &testClock{at: baseTime}
	poller := newPoller(t, worker.URL, notifier)
	poller.Deliveries = deliveries
	poller.Now = clock.Now

	for tick := 0; tick < 2; tick++ {
		if err := poller.Tick(context.Background()); err == nil {
			t.Fatalf("tick %d error = nil, want the failed delivery reported", tick+1)
		}
		clock.Advance(time.Minute)
	}

	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("recovery tick error = %v, want the retry to succeed", err)
	}
	ids := notifier.deliveredIDs()
	if len(ids) != 3 {
		t.Fatalf("delivery attempts = %d, want 3 (two failures and one success)", len(ids))
	}
	if ids[0] != ids[1] || ids[1] != ids[2] {
		t.Errorf("alert ids = %v, want the same event id on every retry", ids)
	}
	if got := deliveries.PendingCount(); got != 0 {
		t.Errorf("pending alerts after delivery = %d, want 0", got)
	}

	// A fourth tick must not resend a delivered alert.
	clock.Advance(time.Minute)
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("tick after delivery error = %v", err)
	}
	if ids := notifier.deliveredIDs(); len(ids) != 3 {
		t.Errorf("delivery attempts after success = %v, want no duplicate", ids)
	}
}

// TestPendingAlertsSurviveRestart delivers an alert recorded by a previous
// poller, which is what the persistent outbox buys across a process restart.
func TestPendingAlertsSurviveRestart(t *testing.T) {
	stub := &workerStub{healthCode: http.StatusServiceUnavailable, healthBody: degradedHealth,
		metricsCode: http.StatusOK, metricsBody: idleMetrics}
	worker := httptest.NewServer(stub)
	defer worker.Close()

	deliveries := monitor.NewMemoryDeliveryStore()
	firstNotifier := &flakyNotifier{failures: 1}
	first := newPoller(t, worker.URL, firstNotifier)
	first.Deliveries = deliveries
	if err := first.Tick(context.Background()); err == nil {
		t.Fatal("first Tick() error = nil, want the delivery to fail")
	}

	// A new poller with the same outbox and a fresh in-memory tracker: the
	// active incident is restored from the outbox, so it is not detected again
	// with a new event id, and the pending row is still delivered.
	secondNotifier := &flakyNotifier{}
	second := newPoller(t, worker.URL, secondNotifier)
	second.Deliveries = deliveries
	second.Now = func() time.Time { return baseTime.Add(2 * time.Minute) }
	if err := second.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}

	ids := secondNotifier.deliveredIDs()
	if len(ids) != 1 {
		t.Fatalf("deliveries after restart = %v, want the pending alert once", ids)
	}
	if ids[0] != firstNotifier.deliveredIDs()[0] {
		t.Errorf("delivery id after restart = %q, want the pending event id", ids[0])
	}
	if got := deliveries.PendingCount(); got != 0 {
		t.Errorf("pending alerts after restart delivery = %d, want 0", got)
	}
}

// staticSource returns the same observation every tick.
type staticSource struct{ obs monitor.Observation }

func (s staticSource) Observe(context.Context) monitor.Observation { return s.obs }

// selectiveNotifier fails the subjects in fail and records every attempt.
type selectiveNotifier struct {
	mu       sync.Mutex
	fail     map[string]bool
	attempts []monitor.Alert
}

func (n *selectiveNotifier) Notify(_ context.Context, alert monitor.Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.attempts = append(n.attempts, alert)
	if n.fail[alert.Subject] {
		return errors.New("webhook answered 503")
	}
	return nil
}

func (n *selectiveNotifier) snapshot() []monitor.Alert {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]monitor.Alert(nil), n.attempts...)
}

func (n *selectiveNotifier) setFail(subject string, fail bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fail[subject] = fail
}

// TestPollerPartialBatchKeepsOnlyTheFailedAlert checks the batch contract: a
// successful delivery is acknowledged and the failed sibling stays pending for
// a later attempt instead of being retried or lost with it.
func TestPollerPartialBatchKeepsOnlyTheFailedAlert(t *testing.T) {
	obs := monitor.Observation{
		HealthCode: http.StatusOK,
		Health: monitor.Health{
			Status:   "ok",
			Database: monitor.DependencyStatus{Name: "postgres", OK: true},
		},
		Metrics: monitor.Metrics{Stages: []monitor.Stage{
			{Name: "review", Kind: "run", Running: 1,
				OldestPendingSeconds: 7200, LastProgressSeconds: 3600, Stalled: true},
			{Name: "publish", Kind: "queue", Available: 2,
				OldestPendingSeconds: 7200, Stalled: true},
		}},
	}
	deliveries := monitor.NewMemoryDeliveryStore()
	notifier := &selectiveNotifier{fail: map[string]bool{"publish": true}}
	clock := &testClock{at: baseTime}
	poller := &monitor.Poller{
		Source:     staticSource{obs: obs},
		Notifier:   notifier,
		Tracker:    monitor.NewTracker(30 * time.Minute),
		Deliveries: deliveries,
		Logger:     discardingLogger(),
		Now:        clock.Now,
	}

	if err := poller.Tick(context.Background()); err == nil {
		t.Fatal("Tick() error = nil, want the publish failure to be reported")
	}
	if got := deliveries.PendingCount(); got != 1 {
		t.Fatalf("pending alerts = %d, want only the failed publish", got)
	}

	notifier.setFail("publish", false)
	clock.Advance(time.Minute)
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() after the webhook recovered = %v", err)
	}
	if got := deliveries.PendingCount(); got != 0 {
		t.Errorf("pending alerts after delivery = %d, want 0", got)
	}

	attempts := notifier.snapshot()
	if len(attempts) != 3 {
		t.Fatalf("delivery attempts = %+v, want publish once, review once and publish retried", attempts)
	}
	if attempts[0].Subject != "publish" || attempts[1].Subject != "review" || attempts[2].Subject != "publish" {
		t.Errorf("delivery subjects = %q,%q,%q, want publish,review,publish",
			attempts[0].Subject, attempts[1].Subject, attempts[2].Subject)
	}
	if attempts[0].ID != attempts[2].ID {
		t.Errorf("retried publish ids = %q,%q, want the same event", attempts[0].ID, attempts[2].ID)
	}
}

// TestPollerCapsOneTickAtTenAlerts proves the flush limit: one slow receiver
// cannot make the loop pay its timeout for an unbounded batch.
func TestPollerCapsOneTickAtTenAlerts(t *testing.T) {
	deliveries := monitor.NewMemoryDeliveryStore()
	for i := 0; i < 11; i++ {
		alert := monitor.Alert{
			ID:        fmt.Sprintf("recovery-%02d", i),
			Problem:   monitor.Problem{Code: monitor.CodeStageStalled, Subject: fmt.Sprintf("stage-%02d", i)},
			At:        baseTime.Add(time.Duration(i) * time.Second),
			Recovered: true,
		}
		if err := deliveries.Enqueue(context.Background(), alert); err != nil {
			t.Fatalf("Enqueue alert %d: %v", i, err)
		}
	}
	notifier := &selectiveNotifier{fail: map[string]bool{}}
	clock := &testClock{at: baseTime.Add(time.Hour)}
	poller := &monitor.Poller{
		Source:     staticSource{obs: healthyObservation()},
		Notifier:   notifier,
		Tracker:    monitor.NewTracker(30 * time.Minute),
		Deliveries: deliveries,
		Logger:     discardingLogger(),
		Now:        clock.Now,
	}

	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("first Tick() error = %v", err)
	}
	if got := len(notifier.snapshot()); got != 10 {
		t.Fatalf("deliveries in one tick = %d, want 10", got)
	}
	if got := deliveries.PendingCount(); got != 1 {
		t.Fatalf("pending after the first tick = %d, want 1", got)
	}

	clock.Advance(time.Minute)
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("second Tick() error = %v", err)
	}
	if got := len(notifier.snapshot()); got != 11 {
		t.Errorf("deliveries after two ticks = %d, want 11", got)
	}
	if got := deliveries.PendingCount(); got != 0 {
		t.Errorf("pending after two ticks = %d, want 0", got)
	}
}

// TestPollerDeliversIncidentAndRecoveryAfter503 exercises the real webhook:
// the endpoint is down for the incident and for the recovery, then answers
// 200, and both events end up delivered exactly once.
func TestPollerDeliversIncidentAndRecoveryAfter503(t *testing.T) {
	stub := &workerStub{healthCode: http.StatusServiceUnavailable, healthBody: degradedHealth,
		metricsCode: http.StatusOK, metricsBody: idleMetrics}
	worker := httptest.NewServer(stub)
	defer worker.Close()

	var (
		mu       sync.Mutex
		down     = true
		received []string
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		}
		_ = json.Unmarshal(raw, &payload)
		mu.Lock()
		defer mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		received = append(received, payload.ID+":"+payload.Severity)
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	clock := &testClock{at: baseTime}
	poller := newPoller(t, worker.URL, monitor.NewWebhookNotifier(hook.URL, hook.Client(), discardingLogger()))
	poller.Deliveries = monitor.NewMemoryDeliveryStore()
	poller.Now = clock.Now

	if err := poller.Tick(context.Background()); err == nil {
		t.Fatal("Tick() error = nil, want the 503 incident to be reported")
	}

	// Incident recovers while the webhook is still down: the recovery is kept
	// pending too. The clock advances past the first retry backoff.
	clock.Advance(time.Minute)
	stub.healthCode = http.StatusOK
	stub.healthBody = okHealth
	if err := poller.Tick(context.Background()); err == nil {
		t.Fatal("Tick() error = nil, want the 503 recovery to be reported")
	}

	mu.Lock()
	down = false
	mu.Unlock()
	clock.Advance(time.Minute)
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() after the webhook recovered = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("delivered = %v, want one incident and one recovery", received)
	}
	if !strings.HasSuffix(received[0], ":warning") || !strings.HasSuffix(received[1], ":recovered") {
		t.Errorf("delivered = %v, want the incident then its recovery", received)
	}
	if received[0] == received[1] {
		t.Errorf("delivered = %v, want distinct event ids", received)
	}
}
