package monitor

import (
	"fmt"
	"net/http"
	"strings"
	"time"
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
	if obs.MetricsError == nil {
		if len(obs.Metrics.Stages) > 0 {
			// Each stage is observed separately, so a publish queue stopped
			// behind a healthy sync or review still alerts (#154).
			for _, stage := range obs.Metrics.Stages {
				if stage.Stalled {
					problems = append(problems, stageProblem(stage))
				}
			}
		} else if obs.Metrics.Stalled {
			// Older workers only report the aggregate.
			problems = append(problems, queueStalled(obs.Metrics.Queue))
		}
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

func queueStalled(q Queue) Problem {
	return Problem{
		Code: CodeQueueStalled,
		Message: fmt.Sprintf(
			"review queue stalled: %d waiting jobs, %d running, oldest waiting %ds",
			q.Available, q.Running, q.OldestAvailableSeconds),
		Values: map[string]any{
			"available":                q.Available,
			"running":                  q.Running,
			"scheduled":                q.Scheduled,
			"retryable":                q.Retryable,
			"discarded":                q.Discarded,
			"oldest_available_seconds": q.OldestAvailableSeconds,
		},
	}
}

// stageProblem states the observed evidence and, when it is known, why the
// stage is not moving: nothing consuming a queue, retries waiting, or attempts
// already discarded.
func stageProblem(stage Stage) Problem {
	var causes []string
	if stage.Kind == "run" {
		causes = append(causes, fmt.Sprintf(
			"%d run(s) active, last progress %s ago",
			stage.Running, seconds(stage.LastProgressSeconds)))
	} else {
		if stage.Running == 0 {
			causes = append(causes, "nothing is consuming the queue")
		}
		causes = append(causes, fmt.Sprintf(
			"%d job(s) waiting, oldest pending %s",
			stage.Available, seconds(stage.OldestPendingSeconds)))
	}
	if stage.Retryable > 0 {
		causes = append(causes, fmt.Sprintf("%d job(s) waiting for a retry", stage.Retryable))
	}
	if stage.Discarded > 0 {
		causes = append(causes, fmt.Sprintf("%d job(s) discarded after exhausting attempts", stage.Discarded))
	}

	return Problem{
		Code:    CodeStageStalled,
		Subject: stage.Name,
		Message: fmt.Sprintf("%s stage stalled: %s", stage.Name, strings.Join(causes, "; ")),
		Values: map[string]any{
			"stage":                  stage.Name,
			"kind":                   stage.Kind,
			"available":              stage.Available,
			"running":                stage.Running,
			"scheduled":              stage.Scheduled,
			"retryable":              stage.Retryable,
			"discarded":              stage.Discarded,
			"oldest_pending_seconds": stage.OldestPendingSeconds,
			"last_progress_seconds":  stage.LastProgressSeconds,
		},
	}
}

func seconds(value int) string {
	return (time.Duration(value) * time.Second).String()
}

func describeDependency(summary string, dep DependencyStatus) string {
	if dep.Error == "" {
		return summary
	}
	return summary + ": " + dep.Error
}
