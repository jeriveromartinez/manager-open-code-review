package monitor

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Source produces one observation per tick. Fetcher is the production one; a
// test can supply a stub.
type Source interface {
	Observe(ctx context.Context) Observation
}

// Poller wires transport, evaluation, state and delivery together. Each Tick
// is one complete cycle, so the loop is a plain ticker and a test drives it
// without sleeping.
type Poller struct {
	Source   Source
	Notifier Notifier
	Tracker  *Tracker
	Logger   *slog.Logger
	Now      func() time.Time
}

// Tick polls once and delivers every resulting alert. It returns the delivery
// error, if any, so Run can log it without leaving the loop.
func (p *Poller) Tick(ctx context.Context) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}

	obs := p.Source.Observe(ctx)
	alerts := p.Tracker.Sync(now(), Evaluate(obs))

	var firstErr error
	for _, alert := range alerts {
		if err := p.Notifier.Notify(ctx, alert); err != nil {
			p.Logger.Error("alert delivery failed", "code", alert.Code, "subject", alert.Subject, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Run ticks until the context is cancelled. The first tick runs immediately so
// a monitor that starts next to a dead worker alerts at once instead of after
// one interval.
func (p *Poller) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("monitor interval must be greater than zero")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := p.Tick(ctx); err != nil {
			// Tick already logged the cause; the loop keeps going because a
			// transient webhook failure must not stop monitoring.
			p.Logger.Debug("tick finished with an error", "error", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
