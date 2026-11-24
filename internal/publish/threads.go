package publish

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const reviewThreadsQuery = `query ReviewThreads($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $after) {
        nodes {
          id
          isResolved
          comments(first: 100) { nodes { id } }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

const resolveThreadMutation = `mutation ResolveReviewThread($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

const unresolveThreadMutation = `mutation UnresolveReviewThread($threadId: ID!) {
  unresolveReviewThread(input: {threadId: $threadId}) {
    thread { id isResolved }
  }
}`

type graphQLThread struct {
	ID       string `json:"id"`
	Resolved bool   `json:"isResolved"`
	Comments struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	} `json:"comments"`
}

// publishResolutions replies to and resolves threads of findings fixed by a
// successful run. Inconclusive and failed runs never resolve anything.
func (p *publishContext) publishResolutions(ctx context.Context, findings []store.Finding) error {
	if p.run.Status != store.RunSucceeded {
		return nil
	}
	for _, f := range findings {
		if f.Status != store.FindingResolved || f.GithubCommentID == 0 ||
			f.GithubCommentNodeID == "" || f.GithubThreadID != "" {
			continue
		}

		// Resolve first: if lookup or mutation fails, no reply has been posted
		// yet and a retry cannot duplicate it.
		threadID, err := p.lookupThread(ctx, f.GithubCommentNodeID)
		if err != nil {
			return err
		}
		if threadID == "" {
			continue
		}
		var resolved struct {
			ResolveReviewThread struct {
				Thread graphQLThread `json:"thread"`
			} `json:"resolveReviewThread"`
		}
		vars := map[string]any{"threadId": threadID}
		if err := p.app.GraphQL(ctx, p.inst.ID, resolveThreadMutation, vars, &resolved); err != nil {
			return fmt.Errorf("resolve review thread %s: %w", threadID, err)
		}

		body := p.sanitize(fmt.Sprintf(
			"Este hallazgo se marca como resuelto tras la revisión #%d.", p.runNumber))
		replyPath := p.repoPath(fmt.Sprintf("pulls/%d/comments/%d/replies",
			p.pr.Number, f.GithubCommentID))
		if err := p.request(ctx, http.MethodPost, replyPath,
			map[string]string{"body": body}, nil); err != nil {
			return fmt.Errorf("reply to finding %s: %w", f.ID, err)
		}
		// Record the state before the thread coordinates: if this write fails,
		// the publish job retries the whole step because GithubThreadID is still
		// empty. The App's echo would also reconcile it, but it may be lost.
		if err := p.w.Store.SetFindingThreadResolved(ctx, f.ID, true); err != nil {
			return err
		}
		if err := p.w.Store.SetFindingPublished(ctx, f.ID,
			f.GithubCommentID, f.GithubCommentNodeID, threadID); err != nil {
			return err
		}
	}
	return nil
}

// lookupThread finds the review thread containing a comment node id.
func (p *publishContext) lookupThread(ctx context.Context, commentNodeID string) (string, error) {
	return p.lookupThreadIn(ctx, commentNodeID, false)
}

// lookupThreadAny finds the review thread containing a comment node id whether
// it is currently resolved or not. Reopening a thread needs the resolved one.
func (p *publishContext) lookupThreadAny(ctx context.Context, commentNodeID string) (string, error) {
	return p.lookupThreadIn(ctx, commentNodeID, true)
}

func (p *publishContext) lookupThreadIn(ctx context.Context, commentNodeID string, includeResolved bool) (string, error) {
	var after *string
	for {
		var response struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						Nodes    []graphQLThread `json:"nodes"`
						PageInfo struct {
							HasNextPage bool    `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{
			"owner":  p.owner,
			"name":   p.name,
			"number": p.pr.Number,
			"after":  after,
		}
		if err := p.app.GraphQL(ctx, p.inst.ID, reviewThreadsQuery, vars, &response); err != nil {
			return "", fmt.Errorf("list review threads: %w", err)
		}
		threads := response.Repository.PullRequest.ReviewThreads
		for _, thread := range threads.Nodes {
			if thread.Resolved && !includeResolved {
				continue
			}
			for _, comment := range thread.Comments.Nodes {
				if comment.ID == commentNodeID {
					return thread.ID, nil
				}
			}
		}
		if !threads.PageInfo.HasNextPage || threads.PageInfo.EndCursor == nil {
			return "", nil
		}
		after = threads.PageInfo.EndCursor
	}
}
