package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Compare resolves the identity of a base–head comparison through GitHub's
// Compare API. The merge base it returns is the real base of the three-dot diff
// (`base...head`), not the base branch tip.
type Compare struct {
	Apps AppProvider
}

// MergeBase asks GitHub for the merge base of head against base. Any failure is
// returned to the caller, which must enqueue a conservative review instead of
// reusing a result of a comparison it could not resolve.
func (c *Compare) MergeBase(ctx context.Context, installationID int64, repoFullName, base, head string) (string, error) {
	if c == nil || c.Apps == nil {
		return "", errors.New("github: compare has no app provider")
	}
	owner, name, ok := strings.Cut(repoFullName, "/")
	if !ok || owner == "" || name == "" {
		return "", fmt.Errorf("invalid repository full name %q", repoFullName)
	}

	app, err := c.Apps.App(ctx)
	if err != nil {
		return "", fmt.Errorf("load GitHub App: %w", err)
	}
	client, err := app.InstallationClient(installationID)
	if err != nil {
		return "", fmt.Errorf("create installation %d client: %w", installationID, err)
	}

	comparison, _, err := client.Repositories.CompareCommits(ctx, owner, name, base, head, nil)
	if err != nil {
		return "", fmt.Errorf("compare %s...%s in %s: %w", base, head, repoFullName, err)
	}
	mergeBase := comparison.GetMergeBaseCommit().GetSHA()
	if mergeBase == "" {
		return "", fmt.Errorf("compare %s...%s in %s: GitHub returned no merge base", base, head, repoFullName)
	}
	return mergeBase, nil
}
