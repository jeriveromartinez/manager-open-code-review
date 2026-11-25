package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v93/github"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// AppCredentials is the result of converting a GitHub App manifest. The three
// secrets are never persisted in the clear; SaveCredentials encrypts them.
type AppCredentials struct {
	AppID         int64  `json:"id"`
	Slug          string `json:"slug"`
	HTMLURL       string `json:"html_url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
}

// ConvertManifest exchanges a short-lived manifest code for the App credentials
// by POSTing to {apiURL}/app-manifests/{code}/conversions.
func ConvertManifest(ctx context.Context, httpc *http.Client, apiURL, code string) (AppCredentials, error) {
	endpoint := fmt.Sprintf("%s/app-manifests/%s/conversions",
		strings.TrimRight(apiURL, "/"), url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("build manifest conversion request: %w", err)
	}

	resp, err := httpc.Do(req)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("convert manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return AppCredentials{}, fmt.Errorf("convert manifest: unexpected HTTP status %s", resp.Status)
	}

	var creds AppCredentials
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		return AppCredentials{}, fmt.Errorf("decode manifest conversion response: %w", err)
	}
	return creds, nil
}

// PRFromGitHub maps a GitHub pull request to the QA Control Center model. The
// QA workflow status is intentionally left empty: SyncPullRequest computes it.
func PRFromGitHub(repoID int64, p *gh.PullRequest) store.PullRequest {
	state := "open"
	switch {
	case p.MergedAt != nil:
		state = "merged"
	case p.GetState() == "closed":
		state = "closed"
	}

	return store.PullRequest{
		RepositoryID:      repoID,
		Number:            p.GetNumber(),
		Title:             p.GetTitle(),
		Body:              p.GetBody(),
		AuthorLogin:       p.GetUser().GetLogin(),
		AuthorAssociation: p.GetAuthorAssociation(),
		HeadSHA:           p.GetHead().GetSHA(),
		BaseRef:           p.GetBase().GetRef(),
		BaseSHA:           p.GetBase().GetSHA(),
		HeadRef:           p.GetHead().GetRef(),
		HTMLURL:           p.GetHTMLURL(),
		Draft:             p.GetDraft(),
		State:             state,
		GitHubUpdatedAt:   p.GetUpdatedAt().Time,
		IsFork:            p.GetHead().GetRepo().GetFork(),
		HeadRepoFullName:  p.GetHead().GetRepo().GetFullName(),
	}
}
