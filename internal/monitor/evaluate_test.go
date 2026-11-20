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
				{Name: "ocr", OK: true, Version: "1.13.13"},
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
