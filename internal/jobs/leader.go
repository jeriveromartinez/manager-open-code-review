package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WatchLeadership reports the River leader as seen from this process.
//
// River only runs periodic jobs on the leader, which is why the previous
// periodic reconcile stopped whenever the worker held leadership (#35). The
// periodic work now runs locally in every process; this watcher makes the
// leader visible so operators can still tell which component won the election.
func WatchLeadership(ctx context.Context, pool *pgxpool.Pool, component string, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var lastLeader string
	check := func() {
		var leaderID string
		err := pool.QueryRow(ctx, `SELECT leader_id FROM river_leader WHERE name = 'default'`).Scan(&leaderID)
		if errors.Is(err, pgx.ErrNoRows) {
			// River has not elected a leader yet; the table is also absent until
			// its migrations run, which is not an error for this observer.
			slog.Debug("river leader not elected yet", "component", component)
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				slog.Debug("read river leader", "component", component, "error", err)
			}
			return
		}
		if leaderID == lastLeader {
			return
		}

		if lastLeader == "" {
			slog.Info("river leader observed", "component", component, "leader_id", leaderID)
		} else {
			slog.Info("river leader changed", "component", component,
				"previous_leader_id", lastLeader, "leader_id", leaderID)
		}
		lastLeader = leaderID
	}

	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
