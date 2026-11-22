package jobs

import (
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/jeriveromartinez/qa-control-center/internal/review"
)

// TestReviewClientJobTimeoutOutlivesRiverDefault pins the regression that left
// runs "running" forever: River's one-minute default cancelled the job while
// the LLM was still working. The timeout must cover the whole run budget plus
// the retries the pipeline may still perform.
func TestReviewClientJobTimeoutOutlivesRiverDefault(t *testing.T) {
	tests := []struct {
		name    string
		runner  *review.Runner
		budget  time.Duration
		wantMin time.Duration
	}{
		{name: "no runner", runner: nil, wantMin: 30 * time.Minute},
		{name: "configured budget", runner: &review.Runner{Cfg: review.RunnerConfig{Timeout: 20 * time.Minute}}, budget: 20 * time.Minute, wantMin: 25 * time.Minute},
		{name: "long budget", runner: &review.Runner{Cfg: review.RunnerConfig{Timeout: time.Hour}}, budget: time.Hour, wantMin: time.Hour + 5*time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := reviewClientConfig(tt.runner, river.NewWorkers(), nil)
			if cfg.JobTimeout < tt.wantMin {
				t.Errorf("JobTimeout = %v, want at least %v", cfg.JobTimeout, tt.wantMin)
			}
			if cfg.JobTimeout <= river.JobTimeoutDefault {
				t.Errorf("JobTimeout = %v, must stay above River's default %v", cfg.JobTimeout, river.JobTimeoutDefault)
			}
			if tt.budget > 0 && cfg.JobTimeout <= tt.budget {
				t.Errorf("JobTimeout = %v, must outlive the run budget %v", cfg.JobTimeout, tt.budget)
			}
		})
	}
}

// TestReviewClientSetsSoftStopTimeout pins the drain window: without a soft
// stop River cancels a running review as soon as SIGTERM arrives, so the run
// stays "running" until the stale watchdog (#69).
func TestReviewClientSetsSoftStopTimeout(t *testing.T) {
	cfg := reviewClientConfig(nil, river.NewWorkers(), nil)
	if cfg.SoftStopTimeout != ReviewDrainTimeout {
		t.Errorf("SoftStopTimeout = %v, want %v", cfg.SoftStopTimeout, ReviewDrainTimeout)
	}
	if cfg.SoftStopTimeout <= 0 {
		t.Error("SoftStopTimeout must be positive so a stopping worker drains in-flight reviews")
	}
}
