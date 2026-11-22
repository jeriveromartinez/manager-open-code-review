package monitor_test

import (
	"context"
	"errors"
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

func newPoller(t *testing.T, url string, notifier monitor.Notifier) *monitor.Poller {
	t.Helper()
	return &monitor.Poller{
		Source:   monitor.NewFetcher(url, &http.Client{Timeout: 2 * time.Second}),
		Notifier: notifier,
		Tracker:  monitor.NewTracker(30 * time.Minute),
		Logger:   discardingLogger(),
		Now:      func() time.Time { return baseTime },
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

	poller := &monitor.Poller{
		Source:   monitor.NewFetcher(worker.URL, worker.Client()),
		Notifier: notifier,
		Tracker:  monitor.NewTracker(30 * time.Minute),
		Logger:   logger,
		Now:      func() time.Time { return baseTime },
	}

	// Tick one fails to deliver but must not abort the loop.
	if err := poller.Tick(context.Background()); err == nil {
		t.Error("Tick() error = nil, want the webhook failure to be reported")
	}
	if !strings.Contains(logs.String(), "alert delivery failed") {
		t.Errorf("logs = %s, want a delivery failure line", logs.String())
	}

	// A later, different problem still gets delivered.
	stub.healthCode = http.StatusOK
	stub.healthBody = okHealth
	stub.metricsBody = stalledMetric
	if err := poller.Tick(context.Background()); err != nil {
		t.Fatalf("Tick() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("webhook calls = %d, want 3 (failed fire, recovery, stalled fire)", calls)
	}
}
