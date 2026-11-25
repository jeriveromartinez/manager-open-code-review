// Package monitor turns the worker's /healthz and /metrics endpoints into
// alerts. The worker itself cannot tell anyone it died, so the alert has to
// come from outside: a separate process polls it and notifies on transitions.
package monitor

import "time"

// Problem codes are the stable machine identity of an alert. The webhook
// payload carries them, so a receiver can route or deduplicate without
// parsing the human message.
const (
	CodeWorkerUnreachable = "worker_unreachable"
	CodeWorkerDegraded    = "worker_degraded"
	CodeDependencyDown    = "dependency_down"
	CodeDatabaseDown      = "database_down"
	CodeQueueStalled      = "queue_stalled"
	// CodeStageStalled names one stalled pipeline stage. The stage travels in
	// Subject, so each stage is alerted and recovered on its own.
	CodeStageStalled = "stage_stalled"
)

// Severities sent on delivery. A recovery is its own severity so a receiver
// can close an incident instead of stacking another warning.
const (
	SeverityWarning   = "warning"
	SeverityRecovered = "recovered"
)

// DependencyStatus mirrors one entry of the worker's health report.
type DependencyStatus struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Health is the parsed /healthz body.
type Health struct {
	Status        string             `json:"status"`
	UptimeSeconds int                `json:"uptime_seconds"`
	Database      DependencyStatus   `json:"database"`
	Dependencies  []DependencyStatus `json:"dependencies"`
}

// Queue is the part of the worker's /metrics report the monitor needs.
type Queue struct {
	Available              int `json:"available"`
	Running                int `json:"running"`
	Scheduled              int `json:"scheduled"`
	Retryable              int `json:"retryable"`
	Discarded              int `json:"discarded"`
	OldestAvailableSeconds int `json:"oldest_available_seconds"`
}

// Stage is one critical pipeline step as reported by the worker. The check is
// evaluated per stage by the worker, which owns the stale threshold; the
// monitor only decides what a stalled stage means for delivery.
type Stage struct {
	Name                 string `json:"name"`
	Kind                 string `json:"kind"`
	Available            int    `json:"available"`
	Running              int    `json:"running"`
	Scheduled            int    `json:"scheduled"`
	Retryable            int    `json:"retryable"`
	Discarded            int    `json:"discarded"`
	OldestPendingSeconds int    `json:"oldest_pending_seconds"`
	LastProgressSeconds  int    `json:"last_progress_seconds"`
	Stalled              bool   `json:"stalled"`
}

// Metrics is the parsed /metrics body.
type Metrics struct {
	Queue   Queue   `json:"queue"`
	Stages  []Stage `json:"stages"`
	Stalled bool    `json:"stalled"`
}

// Observation is one round of polling, already parsed. Keeping transport out
// of Evaluate is what makes the rules testable without a server or a clock.
type Observation struct {
	// ReachError means the worker could not be read at all: refused, timed
	// out, or answered something that is not the JSON contract.
	ReachError   error
	HealthCode   int
	Health       Health
	Metrics      Metrics
	MetricsError error
}

// Problem is one detected failure.
type Problem struct {
	Code string
	// Subject names what failed when a code can apply to several things, so
	// one broken binary does not mask another.
	Subject string
	Message string
	Values  map[string]any
}

// Alert is a delivery: a problem that started or is due for a reminder, or a
// recovery that closes it.
type Alert struct {
	// ID is the deterministic identity of this event. The outbox keys on it,
	// so a retried delivery and a crash between the POST and the acknowledgement
	// can be deduplicated by the receiver.
	ID string `json:"-"`
	Problem
	At        time.Time
	Recovered bool
	// Attempts, LastAttemptAt and LastError describe the outbox state when the
	// alert is read back for a retry.
	Attempts      int        `json:"-"`
	LastAttemptAt *time.Time `json:"-"`
	LastError     string     `json:"-"`
}

// Severity is the webhook severity of this alert.
func (a Alert) Severity() string {
	if a.Recovered {
		return SeverityRecovered
	}
	return SeverityWarning
}
