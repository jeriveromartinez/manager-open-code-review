package monitor_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
)

func healthyObservation() monitor.Observation {
	return monitor.Observation{
		HealthCode: http.StatusOK,
		Health: monitor.Health{
			Status: "ok",
			Database: monitor.DependencyStatus{
				Name: "postgres",
				OK:   true,
			},
			Dependencies: []monitor.DependencyStatus{
				{Name: "ocr", OK: true, Version: "1.14.0"},
				{Name: "opencode", OK: true, Version: "1.19.0"},
			},
		},
		Metrics: monitor.Metrics{
			Queue:   monitor.Queue{Available: 0, Running: 0},
			Stalled: false,
		},
	}
}

// keys reduces the problems to their machine identity so a table can state
// what it expects without repeating whole messages.
func keys(problems []monitor.Problem) []string {
	out := make([]string, 0, len(problems))
	for _, p := range problems {
		key := p.Code
		if p.Subject != "" {
			key += ":" + p.Subject
		}
		out = append(out, key)
	}
	return out
}

func TestEvaluate(t *testing.T) {
	refused := errors.New("dial tcp 127.0.0.1:9090: connect: connection refused")

	tests := []struct {
		name string
		obs  func() monitor.Observation
		want []string
	}{
		{
			name: "reachable and healthy",
			obs:  healthyObservation,
			want: nil,
		},
		{
			name: "healthz answers 503 without a known cause",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.HealthCode = http.StatusServiceUnavailable
				obs.Health.Status = "degraded"
				return obs
			},
			want: []string{monitor.CodeWorkerDegraded},
		},
		{
			name: "healthz answers 200 but the body is not ok",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Health.Status = "starting"
				return obs
			},
			want: []string{monitor.CodeWorkerDegraded},
		},
		{
			name: "a dependency is not ok",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Health.Status = "degraded"
				obs.HealthCode = http.StatusServiceUnavailable
				obs.Health.Dependencies[0] = monitor.DependencyStatus{
					Name: "ocr", OK: false, Error: "binary \"ocr\" --version failed",
				}
				return obs
			},
			want: []string{monitor.CodeDependencyDown + ":ocr"},
		},
		{
			name: "two dependencies are not ok",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Health.Status = "degraded"
				obs.Health.Dependencies[0] = monitor.DependencyStatus{Name: "ocr", OK: false, Error: "gone"}
				obs.Health.Dependencies[1] = monitor.DependencyStatus{Name: "opencode", OK: false, Error: "gone"}
				return obs
			},
			want: []string{
				monitor.CodeDependencyDown + ":ocr",
				monitor.CodeDependencyDown + ":opencode",
			},
		},
		{
			name: "database is down",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Health.Status = "degraded"
				obs.HealthCode = http.StatusServiceUnavailable
				obs.Health.Database = monitor.DependencyStatus{
					Name: "postgres", OK: false, Error: "ping database: connection refused",
				}
				return obs
			},
			want: []string{monitor.CodeDatabaseDown},
		},
		{
			name: "queue stalled",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Metrics.Queue = monitor.Queue{Available: 5, Running: 0, OldestAvailableSeconds: 3600}
				obs.Metrics.Stalled = true
				return obs
			},
			want: []string{monitor.CodeQueueStalled},
		},
		{
			name: "a stalled stage alerts while another stage is healthy",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Metrics.Stages = []monitor.Stage{
					{Name: "queue", Kind: "queue", Running: 1, LastProgressSeconds: 5},
					{Name: "publish", Kind: "queue", Available: 3, Running: 0,
						OldestPendingSeconds: 3600, Retryable: 1, Discarded: 1, Stalled: true},
				}
				obs.Metrics.Stalled = true
				return obs
			},
			want: []string{monitor.CodeStageStalled + ":publish"},
		},
		{
			name: "an old but progressing run does not alert",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Metrics.Stages = []monitor.Stage{
					{Name: "clone", Kind: "run", Running: 1,
						OldestPendingSeconds: 7200, LastProgressSeconds: 30, Stalled: false},
					{Name: "publish", Kind: "queue"},
				}
				// The aggregate used to keep the monitor silent; per-stage
				// evaluation must trust the stage's own verdict.
				obs.Metrics.Stalled = true
				return obs
			},
			want: nil,
		},
		{
			name: "two stalled stages alert separately",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.Metrics.Stages = []monitor.Stage{
					{Name: "review", Kind: "run", Running: 2,
						OldestPendingSeconds: 3600, LastProgressSeconds: 1800, Stalled: true},
					{Name: "publish", Kind: "queue", Available: 1,
						OldestPendingSeconds: 3600, Stalled: true},
				}
				return obs
			},
			want: []string{
				monitor.CodeStageStalled + ":review",
				monitor.CodeStageStalled + ":publish",
			},
		},
		{
			name: "metrics unavailable",
			obs: func() monitor.Observation {
				obs := healthyObservation()
				obs.MetricsError = errors.New("/metrics answered 503")
				return obs
			},
			want: []string{monitor.CodeWorkerDegraded},
		},
		{
			name: "connection refused wins over everything else",
			obs: func() monitor.Observation {
				return monitor.Observation{ReachError: refused}
			},
			want: []string{monitor.CodeWorkerUnreachable},
		},
		{
			name: "malformed health body is unreachable",
			obs: func() monitor.Observation {
				return monitor.Observation{ReachError: errors.New("/healthz returned invalid JSON")}
			},
			want: []string{monitor.CodeWorkerUnreachable},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keys(monitor.Evaluate(tt.obs()))
			if len(got) != len(tt.want) {
				t.Fatalf("problems = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("problem %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEvaluateMessagesAndValues(t *testing.T) {
	obs := healthyObservation()
	obs.Metrics.Queue = monitor.Queue{Available: 5, Running: 0, OldestAvailableSeconds: 3600}
	obs.Metrics.Stalled = true

	problems := monitor.Evaluate(obs)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", problems)
	}
	p := problems[0]
	if !strings.Contains(p.Message, "5 waiting") || !strings.Contains(p.Message, "3600s") {
		t.Errorf("Message = %q, want the observed counts", p.Message)
	}
	if p.Values["available"] != 5 {
		t.Errorf("Values[available] = %v, want 5", p.Values["available"])
	}
	if p.Values["oldest_available_seconds"] != 3600 {
		t.Errorf("Values[oldest_available_seconds] = %v, want 3600", p.Values["oldest_available_seconds"])
	}
}

// TestEvaluateStageMessageExplainsTheCause carries the evidence an operator
// needs to act, including retries and discarded attempts (#154).
func TestEvaluateStageMessageExplainsTheCause(t *testing.T) {
	obs := healthyObservation()
	obs.Metrics.Stages = []monitor.Stage{{
		Name: "publish", Kind: "queue", Available: 3, Running: 0,
		OldestPendingSeconds: 7200, Retryable: 2, Discarded: 1, Stalled: true,
	}}

	problems := monitor.Evaluate(obs)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly one", problems)
	}
	p := problems[0]
	if p.Code != monitor.CodeStageStalled || p.Subject != "publish" {
		t.Errorf("problem = %s:%s, want %s:publish", p.Code, p.Subject, monitor.CodeStageStalled)
	}
	for _, want := range []string{
		"nothing is consuming the queue",
		"3 job(s) waiting",
		"2 job(s) waiting for a retry",
		"1 job(s) discarded",
	} {
		if !strings.Contains(p.Message, want) {
			t.Errorf("Message = %q, want it to contain %q", p.Message, want)
		}
	}
	if p.Values["stage"] != "publish" || p.Values["retryable"] != 2 {
		t.Errorf("Values = %v, want the stage and retry count", p.Values)
	}
}

// TestEvaluateStalledWithoutMetrics does not invent a stalled queue when the
// metrics endpoint could not be read.
func TestEvaluateStalledWithoutMetrics(t *testing.T) {
	obs := healthyObservation()
	obs.Metrics.Stalled = true
	obs.MetricsError = errors.New("/metrics answered 503")

	for _, p := range monitor.Evaluate(obs) {
		if p.Code == monitor.CodeQueueStalled {
			t.Fatal("queue_stalled fired without a readable /metrics")
		}
	}
}
