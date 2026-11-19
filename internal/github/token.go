package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// errorBodyLimit bounds how much of an unexpected response body is included in
// an error. Tokens must never be logged, so only GitHub's error text is kept.
const errorBodyLimit = 300

type installationAccessToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// RepositoryReadToken returns a short-lived, read-only token scoped to a single
// repository. Only the repository name is sent: the permissions block keeps the
// token from carrying write scopes.
func (a *App) RepositoryReadToken(ctx context.Context, installationID int64, repoFullName string) (token string, expires time.Time, err error) {
	owner, name, ok := strings.Cut(repoFullName, "/")
	if !ok || owner == "" || name == "" {
		return "", time.Time{}, fmt.Errorf("invalid repository full name %q", repoFullName)
	}

	body := struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}{
		Repositories: []string{name},
		Permissions:  map[string]string{"contents": "read", "metadata": "read"},
	}

	access, err := a.postInstallationAccessToken(ctx, installationID, body)
	if err != nil {
		return "", time.Time{}, err
	}
	return access.Token, access.ExpiresAt, nil
}

// installationToken obtains an unscoped installation token, used for GraphQL.
func (a *App) installationToken(ctx context.Context, installationID int64) (string, error) {
	access, err := a.postInstallationAccessToken(ctx, installationID, struct{}{})
	if err != nil {
		return "", err
	}
	return access.Token, nil
}

// postInstallationAccessToken posts to the App-JWT installation token endpoint.
// It uses AppClient's HTTP client so the request is signed with the App JWT.
func (a *App) postInstallationAccessToken(ctx context.Context, installationID int64, body any) (installationAccessToken, error) {
	var access installationAccessToken

	client, err := a.AppClient()
	if err != nil {
		return access, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return access, fmt.Errorf("encode installation token request: %w", err)
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens",
		strings.TrimRight(a.APIURL, "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return access, fmt.Errorf("build installation token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Client().Do(req)
	if err != nil {
		return access, fmt.Errorf("request installation token for installation %d: %w", installationID, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return access, fmt.Errorf("read installation token response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return access, httpStatusError("request installation token", resp.StatusCode, payload)
	}
	if err := json.Unmarshal(payload, &access); err != nil {
		return access, fmt.Errorf("decode installation token response: %w", err)
	}
	if access.Token == "" {
		return access, fmt.Errorf("installation %d token response has no token", installationID)
	}
	return access, nil
}

// httpStatusError reports the status and a bounded body excerpt.
func httpStatusError(action string, statusCode int, body []byte) error {
	excerpt := string(body)
	if len(excerpt) > errorBodyLimit {
		excerpt = excerpt[:errorBodyLimit] + "..."
	}
	return fmt.Errorf("%s: GitHub API returned status %d: %s", action, statusCode, excerpt)
}
