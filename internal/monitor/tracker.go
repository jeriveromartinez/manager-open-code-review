package monitor

import (
	"sort"
	"time"
)

// Tracker holds the previous state so a persistent problem alerts once
// instead of every tick. It is not safe for concurrent use: one poller owns
// it, and keeping it single-threaded is what makes the state machine simple.
type Tracker struct {
	renotifyAfter time.Duration
	// active holds the problems seen on the previous tick, keyed by identity,
	// with the moment they were last alerted. A key that is absent means the
	// problem is currently clear.
	active map[string]tracked
}

type tracked struct {
	problem   Problem
	alertedAt time.Time
}

// NewTracker builds a tracker that repeats an unresolved problem only after
// renotifyAfter.
func NewTracker(renotifyAfter time.Duration) *Tracker {
	return &Tracker{renotifyAfter: renotifyAfter, active: map[string]tracked{}}
}

// Restore seeds the state machine from the outbox so a restarted monitor does
// not fire a new incident (with a new event id) for a problem it already
// paged. Only the latest event of a problem is meaningful: a warning means the
// problem is still open, a recovery means it is clear.
func (t *Tracker) Restore(alerts []Alert) {
	for _, alert := range alerts {
		if alert.Recovered {
			continue
		}
		key := alert.Problem.key()
		if prev, seen := t.active[key]; seen && !alert.At.After(prev.alertedAt) {
			continue
		}
		t.active[key] = tracked{problem: alert.Problem, alertedAt: alert.At}
	}
}

// Sync compares the current problems with the previous tick and returns the
// alerts to deliver at now: new problems, problems due for a reminder, and one
// recovery per problem that disappeared.
func (t *Tracker) Sync(now time.Time, problems []Problem) []Alert {
	current := make(map[string]Problem, len(problems))
	for _, p := range problems {
		key := p.key()
		if _, dup := current[key]; dup {
			continue
		}
		current[key] = p
	}

	var alerts []Alert
	for _, key := range sortedKeys(current) {
		p := current[key]
		prev, seen := t.active[key]
		if seen && now.Sub(prev.alertedAt) < t.renotifyAfter {
			continue
		}
		alerts = append(alerts, Alert{Problem: p, At: now})
		t.active[key] = tracked{problem: p, alertedAt: now}
	}

	// A cleared problem is a recovery even if a reminder was never sent, and
	// it leaves the state right away, so a later reappearance is a new fire
	// rather than a silent resumption.
	for _, key := range sortedKeys(t.active) {
		if _, still := current[key]; still {
			continue
		}
		alerts = append(alerts, Alert{Problem: t.active[key].problem, At: now, Recovered: true})
		delete(t.active, key)
	}

	return alerts
}

func (p Problem) key() string {
	if p.Subject == "" {
		return p.Code
	}
	return p.Code + ":" + p.Subject
}

func sortedKeys[V any](set map[string]V) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
