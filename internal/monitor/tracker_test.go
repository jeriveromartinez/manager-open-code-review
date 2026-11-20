package monitor_test

import (
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
)

var baseTime = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

func stalled() monitor.Problem {
	return monitor.Problem{
		Code:    monitor.CodeQueueStalled,
		Message: "review queue stalled: 5 waiting jobs, none running, oldest waiting 3600s",
		Values:  map[string]any{"available": 5},
	}
}

func dbDown() monitor.Problem {
	return monitor.Problem{
		Code:    monitor.CodeDatabaseDown,
		Message: "worker cannot reach PostgreSQL",
	}
}

func TestTrackerFiresOnceWhileTheProblemPersists(t *testing.T) {
	tr := monitor.NewTracker(30 * time.Minute)

	first := tr.Sync(baseTime, []monitor.Problem{stalled()})
	if len(first) != 1 {
		t.Fatalf("first tick alerts = %d, want 1", len(first))
	}
	if first[0].Recovered {
		t.Error("first tick is a recovery, want a fire")
	}
	if first[0].Code != monitor.CodeQueueStalled {
		t.Errorf("code = %q, want %q", first[0].Code, monitor.CodeQueueStalled)
	}
	if !first[0].At.Equal(baseTime) {
		t.Errorf("At = %v, want the injected clock %v", first[0].At, baseTime)
	}

	for _, offset := range []time.Duration{time.Minute, 10 * time.Minute, 29 * time.Minute} {
		if got := tr.Sync(baseTime.Add(offset), []monitor.Problem{stalled()}); len(got) != 0 {
			t.Fatalf("tick at %s re-alerted %d times, want 0", offset, len(got))
		}
	}
}

func TestTrackerRenotifiesAfterTheInterval(t *testing.T) {
	tr := monitor.NewTracker(30 * time.Minute)
	tr.Sync(baseTime, []monitor.Problem{stalled()})

	if got := tr.Sync(baseTime.Add(30*time.Minute), []monitor.Problem{stalled()}); len(got) != 1 {
		t.Fatalf("alerts at the re-notify interval = %d, want 1", len(got))
	}
	if got := tr.Sync(baseTime.Add(31*time.Minute), []monitor.Problem{stalled()}); len(got) != 0 {
		t.Fatalf("alerts one minute later = %d, want 0", len(got))
	}
	if got := tr.Sync(baseTime.Add(60*time.Minute), []monitor.Problem{stalled()}); len(got) != 1 {
		t.Fatalf("alerts after the second interval = %d, want 1", len(got))
	}
}

func TestTrackerRecoversExactlyOnce(t *testing.T) {
	tr := monitor.NewTracker(30 * time.Minute)
	tr.Sync(baseTime, []monitor.Problem{stalled()})

	recovered := tr.Sync(baseTime.Add(2*time.Minute), nil)
	if len(recovered) != 1 {
		t.Fatalf("recovery alerts = %d, want 1", len(recovered))
	}
	if !recovered[0].Recovered {
		t.Error("Recovered = false, want true")
	}
	if recovered[0].Severity() != monitor.SeverityRecovered {
		t.Errorf("severity = %q, want %q", recovered[0].Severity(), monitor.SeverityRecovered)
	}
	if recovered[0].Message != stalled().Message {
		t.Errorf("recovery message = %q, want the original problem message", recovered[0].Message)
	}

	if got := tr.Sync(baseTime.Add(3*time.Minute), nil); len(got) != 0 {
		t.Fatalf("second recovery tick alerted %d times, want 0", len(got))
	}
}

func TestTrackerRefiresAfterTheProblemClears(t *testing.T) {
	tr := monitor.NewTracker(30 * time.Minute)
	tr.Sync(baseTime, []monitor.Problem{stalled()})
	tr.Sync(baseTime.Add(time.Minute), nil)

	again := tr.Sync(baseTime.Add(2*time.Minute), []monitor.Problem{stalled()})
	if len(again) != 1 {
		t.Fatalf("reappearing problem alerts = %d, want 1", len(again))
	}
	if again[0].Recovered {
		t.Error("reappearing problem reported as recovery")
	}
}

// TestTrackerSeparatesSubjects keeps one broken binary from silencing another.
func TestTrackerSeparatesSubjects(t *testing.T) {
	tr := monitor.NewTracker(30 * time.Minute)
	ocr := monitor.Problem{Code: monitor.CodeDependencyDown, Subject: "ocr", Message: "ocr down"}
	opencode := monitor.Problem{Code: monitor.CodeDependencyDown, Subject: "opencode", Message: "opencode down"}

	if got := tr.Sync(baseTime, []monitor.Problem{ocr}); len(got) != 1 {
		t.Fatalf("first dependency alerts = %d, want 1", len(got))
	}
	both := tr.Sync(baseTime.Add(time.Minute), []monitor.Problem{ocr, opencode})
	if len(both) != 1 {
		t.Fatalf("alerts when the second dependency breaks = %d, want 1", len(both))
	}
	if both[0].Subject != "opencode" {
		t.Errorf("subject = %q, want opencode", both[0].Subject)
	}

	// Sorted so two recoveries in the same tick are deterministic.
	recovered := tr.Sync(baseTime.Add(2*time.Minute), nil)
	if len(recovered) != 2 {
		t.Fatalf("recoveries = %d, want 2", len(recovered))
	}
	if recovered[0].Subject != "ocr" || recovered[1].Subject != "opencode" {
		t.Errorf("recovery order = %q,%q, want ocr,opencode", recovered[0].Subject, recovered[1].Subject)
	}
}

func TestTrackerKeepsDuplicateKeysFromDoubleAlerting(t *testing.T) {
	tr := monitor.NewTracker(30 * time.Minute)
	if got := tr.Sync(baseTime, []monitor.Problem{dbDown(), dbDown()}); len(got) != 1 {
		t.Fatalf("alerts for a duplicated problem = %d, want 1", len(got))
	}
}
