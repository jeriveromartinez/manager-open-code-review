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
	Discarded              int `json:"discarded"`
	OldestAvailableSeconds int `json:"oldest_available_seconds"`
}

// Metrics is the parsed /metrics body.
type Metrics struct {
	Queue   Queue `json:"queue"`
	Stalled bool  `json:"stalled"`
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
	Problem
	At        time.Time
	Recovered bool
}

// Severity is the webhook severity of this alert.
func (a Alert) Severity() string {
	if a.Recovered {
		return SeverityRecovered
	}
	return SeverityWarning
}
