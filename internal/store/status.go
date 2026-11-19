package store

// QA workflow states of a pull request.
const (
	StatusDraft            = "draft"
	StatusAwaitingApproval = "awaiting_approval"
	StatusPending          = "pending"
	StatusReviewing        = "reviewing"
	StatusChangesRequested = "changes_requested"
	StatusWaitingCI        = "waiting_ci"
	StatusReadyForHuman    = "ready_for_human"
	StatusInconclusive     = "inconclusive"
	StatusPaused           = "paused"
	StatusMerged           = "merged"
	StatusClosed           = "closed"
)

// Dashboard groups a QA status belongs to: "in_review", "blocked", "ready", or
// "" when the status is not part of any filter group.
func GroupOf(status string) string {
	switch status {
	case StatusPending, StatusReviewing, StatusWaitingCI:
		return "in_review"
	case StatusChangesRequested, StatusInconclusive, StatusAwaitingApproval, StatusPaused:
		return "blocked"
	case StatusReadyForHuman:
		return "ready"
	default:
		return ""
	}
}

// OnSync computes the QA status a pull request must have after observing the
// GitHub state `in`, given the stored pull request `prev` (nil when new).
//
// Phase 1 rule: merged/closed win, drafts stay drafts, and any new commit (or a
// PR that was draft/closed/merged) starts over at pending. Otherwise the
// current status is preserved.
func OnSync(prev *PullRequest, in PullRequest) string {
	switch in.State {
	case StatusMerged:
		return StatusMerged
	case StatusClosed:
		return StatusClosed
	}

	if in.Draft {
		return StatusDraft
	}

	if prev == nil {
		return StatusPending
	}
	if prev.HeadSHA != in.HeadSHA {
		return StatusPending
	}

	switch prev.QAStatus {
	case StatusDraft, StatusClosed, StatusMerged:
		return StatusPending
	default:
		return prev.QAStatus
	}
}
