package monitor_test

import (
	"context"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// TestAlertIDIsDeterministic pins the identity contract: the same event keeps
// one id (so a retry is deduplicated), while another time, subject or severity
// is another event.
func TestAlertIDIsDeterministic(t *testing.T) {
	first := alert(baseTime)
	second := alert(baseTime)
	if got, want := monitor.AlertID(first), monitor.AlertID(second); got != want {
		t.Errorf("AlertID() = %q and %q for the same event, want equal", got, want)
	}

	later := alert(baseTime.Add(time.Second))
	recovered := alert(baseTime)
	recovered.Recovered = true
	for name, other := range map[string]monitor.Alert{"time": later, "severity": recovered} {
		if monitor.AlertID(first) == monitor.AlertID(other) {
			t.Errorf("AlertID() identical for a different %s, want distinct", name)
		}
	}
}

// TestMemoryDeliveryBackoffWaitsBetweenAttempts keeps a black-holed receiver
// from being retried every tick.
func TestMemoryDeliveryBackoffWaitsBetweenAttempts(t *testing.T) {
	ctx := context.Background()
	store := monitor.NewMemoryDeliveryStore()
	a := alert(baseTime)
	if err := store.Enqueue(ctx, a); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := store.MarkFailed(ctx, a.ID, baseTime, "webhook answered 503"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	pending, err := store.Pending(ctx, baseTime.Add(10*time.Second), 10)
	if err != nil {
		t.Fatalf("Pending before the backoff: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending before the backoff = %+v, want none", pending)
	}

	pending, err = store.Pending(ctx, baseTime.Add(31*time.Second), 10)
	if err != nil {
		t.Fatalf("Pending after the backoff: %v", err)
	}
	if len(pending) != 1 || pending[0].Attempts != 1 || pending[0].LastError != "webhook answered 503" {
		t.Errorf("pending after the backoff = %+v, want the failed attempt exposed", pending)
	}
}

// TestPGDeliveryStorePersistsAndDeduplicates covers the durable half of the
// outbox: a repeated enqueue keeps one row, a failed delivery stays pending
// with its attempt count after a reopen, and a delivered row is never returned
// again (#154).
func TestPGDeliveryStorePersistsAndDeduplicates(t *testing.T) {
	s := testutil.NewStore(t)
	ctx := context.Background()
	store := monitor.NewPGDeliveryStore(s.Pool)
	a := alert(baseTime)

	for i := 0; i < 2; i++ {
		if err := store.Enqueue(ctx, a); err != nil {
			t.Fatalf("Enqueue() attempt %d: %v", i+1, err)
		}
	}
	pending, err := store.Pending(ctx, baseTime, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != a.ID {
		t.Fatalf("pending = %+v, want the alert once", pending)
	}
	active, err := store.Active(ctx)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if len(active) != 1 || active[0].ID != a.ID {
		t.Errorf("active = %+v, want the open incident", active)
	}

	if err := store.MarkFailed(ctx, a.ID, baseTime, "webhook answered 503"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	// A new store on the same database is what a restarted monitor sees.
	reopened := monitor.NewPGDeliveryStore(s.Pool)
	pending, err = reopened.Pending(ctx, baseTime.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("Pending after reopen: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending after reopen = %+v, want the failed alert", pending)
	}
	if pending[0].Attempts != 1 || pending[0].LastError != "webhook answered 503" {
		t.Errorf("pending after reopen = %+v, want the attempt and its cause", pending[0])
	}
	if pending[0].Message != a.Message || pending[0].Code != a.Code {
		t.Errorf("pending alert = %+v, want the original event", pending[0])
	}

	if err := reopened.MarkDelivered(ctx, a.ID, baseTime); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	pending, err = reopened.Pending(ctx, baseTime.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("Pending after delivery: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending after delivery = %+v, want none", pending)
	}
}
