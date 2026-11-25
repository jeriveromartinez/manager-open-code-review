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

// maxPendingAlerts bounds one flush. The outbox keeps the rest for the next
// tick, so a black-holed receiver cannot block the monitor with one huge
// batch; the retry backoff keeps a slow endpoint from paying the timeout every
// tick.
const maxPendingAlerts = 10

// Poller wires transport, evaluation, state and delivery together. Each Tick
// is one complete cycle, so the loop is a plain ticker and a test drives it
// without sleeping.
type Poller struct {
	Source   Source
	Notifier Notifier
	Tracker  *Tracker
	// Deliveries persists an alert before the first POST and keeps failures
	// pending. A nil store falls back to an in-memory one.
	Deliveries DeliveryStore
	Logger     *slog.Logger
	Now        func() time.Time

	// seeded records that the tracker was restored from the outbox. It is not
	// safe for concurrent ticks, which the loop does not do.
	seeded bool
}

// Tick polls once and delivers every resulting alert. It returns the first
// delivery error, if any, so Run can log it without leaving the loop.
func (p *Poller) Tick(ctx context.Context) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	at := now()

	store := p.deliveries()
	if !p.seeded {
		// Restore before Sync: an incident that was already paged must not be
		// detected again as a new event after a restart.
		if active, err := store.Active(ctx); err != nil {
			p.Logger.Error("restore active alerts", "error", err)
		} else {
			p.Tracker.Restore(active)
			p.seeded = true
		}
	}

	obs := p.Source.Observe(ctx)
	alerts := p.Tracker.Sync(at, Evaluate(obs))

	var firstErr error
	for _, alert := range alerts {
		if alert.ID == "" {
			alert.ID = AlertID(alert)
		}
		if err := store.Enqueue(ctx, alert); err != nil {
			// Without an outbox the alert still must not be dropped: deliver
			// it now, even though it cannot be retried.
			p.Logger.Error("persist alert", "id", alert.ID, "code", alert.Code,
				"subject", alert.Subject, "error", err)
			if notifyErr := p.Notifier.Notify(ctx, alert); notifyErr != nil {
				p.Logger.Error("alert delivery failed", "id", alert.ID, "code", alert.Code,
					"subject", alert.Subject, "error", notifyErr)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	pending, err := store.Pending(ctx, at, maxPendingAlerts)
	if err != nil {
		p.Logger.Error("read pending alerts", "error", err)
		if firstErr == nil {
			firstErr = err
		}
		return firstErr
	}

	for _, alert := range pending {
		if err := p.Notifier.Notify(ctx, alert); err != nil {
			p.Logger.Error("alert delivery failed", "id", alert.ID, "code", alert.Code,
				"subject", alert.Subject, "attempt", alert.Attempts+1,
				"last_error", alert.LastError, "error", err)
			if markErr := store.MarkFailed(ctx, alert.ID, at, err.Error()); markErr != nil {
				p.Logger.Error("record alert failure", "id", alert.ID, "error", markErr)
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := store.MarkDelivered(ctx, alert.ID, at); err != nil {
			// The POST succeeded; a mark failure only risks a duplicate, which
			// the receiver can drop by the alert id.
			p.Logger.Error("record alert delivery", "id", alert.ID, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (p *Poller) deliveries() DeliveryStore {
	if p.Deliveries == nil {
		p.Deliveries = NewMemoryDeliveryStore()
	}
	return p.Deliveries
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
