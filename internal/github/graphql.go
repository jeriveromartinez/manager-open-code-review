package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GraphQL executes a query with an installation token and decodes the "data"
// object into out. GraphQL-level errors are returned as a Go error even when the
// HTTP status is 200.
func (a *App) GraphQL(ctx context.Context, installationID int64, query string, vars map[string]any, out any) error {
	token, err := a.installationToken(ctx, installationID)
	if err != nil {
		return fmt.Errorf("graphql installation token: %w", err)
	}

	body := struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{Query: query, Variables: vars}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode graphql request: %w", err)
	}

	endpoint := strings.TrimRight(a.APIURL, "/") + "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("graphql request: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read graphql response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return httpStatusError("graphql request", resp.StatusCode, payload)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode graphql response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, gqlErr := range envelope.Errors {
			messages = append(messages, gqlErr.Message)
		}
		return fmt.Errorf("graphql errors: %s", strings.Join(messages, "; "))
	}
	if out != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode graphql data: %w", err)
		}
	}
	return nil
}
