package monitor

import (
	"fmt"
	"net/http"
)

// Evaluate turns one observation into the problems it proves. It is a pure
// function on purpose: the state machine above it only decides when to speak,
// never what is wrong.
//
// The order is stable so two identical observations always produce the same
// alert sequence, which is what keeps the tracker's bookkeeping honest.
func Evaluate(obs Observation) []Problem {
	if obs.ReachError != nil {
		return []Problem{{
			Code:    CodeWorkerUnreachable,
			Message: fmt.Sprintf("worker is unreachable: %v", obs.ReachError),
		}}
	}

	var problems []Problem

	if !obs.Health.Database.OK {
		problems = append(problems, Problem{
			Code:    CodeDatabaseDown,
			Message: describeDependency("worker cannot reach PostgreSQL", obs.Health.Database),
			Values:  map[string]any{"dependency": obs.Health.Database.Name, "error": obs.Health.Database.Error},
		})
	}

	for _, dep := range obs.Health.Dependencies {
		if dep.OK {
			continue
		}
		problems = append(problems, Problem{
			Code:    CodeDependencyDown,
			Subject: dep.Name,
			Message: describeDependency(fmt.Sprintf("worker dependency %q is not available", dep.Name), dep),
			Values:  map[string]any{"dependency": dep.Name, "error": dep.Error},
		})
	}

	// A stalled queue is only proven by /metrics: a failed read is not a
	// stall, it is a broken metrics endpoint, reported below.
	if obs.MetricsError == nil && obs.Metrics.Stalled {
		q := obs.Metrics.Queue
		problems = append(problems, Problem{
			Code: CodeQueueStalled,
			Message: fmt.Sprintf(
				"review queue stalled: %d waiting jobs, %d running, oldest waiting %ds",
				q.Available, q.Running, q.OldestAvailableSeconds),
			Values: map[string]any{
				"available":                q.Available,
				"running":                  q.Running,
				"scheduled":                q.Scheduled,
				"discarded":                q.Discarded,
				"oldest_available_seconds": q.OldestAvailableSeconds,
			},
		})
	}

	if len(problems) > 0 {
		return problems
	}

	// Degraded without a named cause: the worker said it is not ok, or the
	// metrics endpoint refused. Both mean "something is wrong" without
	// enough detail to say what, and neither may stay silent.
	switch {
	case obs.HealthCode != http.StatusOK:
		return []Problem{degraded(fmt.Sprintf("/healthz answered %d", obs.HealthCode))}
	case obs.Health.Status != "ok":
		return []Problem{degraded(fmt.Sprintf("/healthz reported status %q", obs.Health.Status))}
	case obs.MetricsError != nil:
		return []Problem{degraded(fmt.Sprintf("/metrics is unavailable: %v", obs.MetricsError))}
	}

	return nil
}

func degraded(reason string) Problem {
	return Problem{Code: CodeWorkerDegraded, Message: "worker is degraded: " + reason}
}

func describeDependency(summary string, dep DependencyStatus) string {
	if dep.Error == "" {
		return summary
	}
	return summary + ": " + dep.Error
}
