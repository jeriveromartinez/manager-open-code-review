package github_test

import (
	"fmt"
	"net/http"
	"testing"
)

// A GitHub approval is generic: it carries no reviewed scope and may belong to
// another SHA, so it never records a human verdict nor closes an inconclusive
// review. Only the panel's scoped decision can do that.
func TestPullRequestReviewApprovalClosesNothing(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())

	for _, commit := range []string{"aaa111", "another-sha"} {
		t.Run(commit, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{
				"action": "submitted",
				"review": {"id": 1, "state": "approved", "commit_id": %q, "body": "LGTM"},
				"pull_request": {"number": 142, "state": "open"},
				"repository": {"id": 501, "full_name": "acme/backend-api"},
				"sender": {"login": "qa-lead", "id": 8001}
			}`, commit))
			rec := f.deliverBody("pull_request_review", body, signWebhook(body))
			assertStatus(t, rec, http.StatusNoContent)
		})
	}

	if got := f.countRows("human_verdicts"); got != 0 {
		t.Errorf("human verdicts = %d, want none from a generic approval", got)
	}
	if got := f.pullRequest(); got.QAStatus != pr.QAStatus {
		t.Errorf("qa_status = %q, want %q unchanged", got.QAStatus, pr.QAStatus)
	}
}
