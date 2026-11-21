package jobs

import (
	"context"
	"log/slog"
	"time"
)

// Schedule runs a task immediately and then every interval until ctx is done.
//
// River executes periodic jobs only on the single database leader. The control
// ("app") and review ("worker") processes both register their own workers, so
// whichever process lost leadership silently stopped its periodic work (#35).
// Running the cadence in each process removes that dependency; leader election
// stays enabled because River still uses it for queue maintenance.
func Schedule(ctx context.Context, task string, interval time.Duration, run func(context.Context) error) {
	if interval <= 0 {
		slog.Warn("periodic task not scheduled: non-positive interval", "task", task, "interval", interval)
		return
	}

	slog.Info("periodic task scheduled", "task", task, "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if ctx.Err() != nil {
			return
		}

		if err := run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("periodic task failed", "task", task, "error", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
