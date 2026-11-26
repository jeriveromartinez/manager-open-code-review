package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
)

type requestedReview struct {
	Reviewers     []string `json:"reviewers"`
	TeamReviewers []string `json:"team_reviewers"`
}

func TestAppRequestReview(t *testing.T) {
	var gotPath string
	var got requestedReview
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/9/access_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"[SECRET]","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/repo/pulls/7/requested_reviewers" {
			http.NotFound(w, r)
			return
		}
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7}`))
	}))
	defer srv.Close()

	app := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	err := app.RequestReview(context.Background(), 9, "acme/repo", 7, []string{"qa-reviewer"}, []string{"qa-team"})
	if err != nil {
		t.Fatalf("RequestReview() error = %v", err)
	}
	if gotPath != "/repos/acme/repo/pulls/7/requested_reviewers" {
		t.Errorf("request path = %q", gotPath)
	}
	if len(got.Reviewers) != 1 || got.Reviewers[0] != "qa-reviewer" ||
		len(got.TeamReviewers) != 1 || got.TeamReviewers[0] != "qa-team" {
		t.Errorf("requested review = %+v, want qa-reviewer/qa-team", got)
	}
}

func TestAppRequestedReviewers(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/9/access_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"[SECRET]","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/repo/pulls/7/requested_reviewers" {
			http.NotFound(w, r)
			return
		}
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"login":"qa-reviewer"}],"teams":[{"slug":"qa-team"}]}`))
	}))
	defer srv.Close()

	app := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	reviewers, teams, err := app.RequestedReviewers(context.Background(), 9, "acme/repo", 7)
	if err != nil {
		t.Fatalf("RequestedReviewers() error = %v", err)
	}
	if gotPath == "" {
		t.Error("request path was not the requested_reviewers endpoint")
	}
	if len(reviewers) != 1 || reviewers[0] != "qa-reviewer" ||
		len(teams) != 1 || teams[0] != "qa-team" {
		t.Errorf("reviewers = %v teams = %v, want qa-reviewer/qa-team", reviewers, teams)
	}
}

func TestAppRequestReviewLocalErrorsAreNotRetryable(t *testing.T) {
	app := &ghapp.App{}
	for _, tc := range []struct {
		name      string
		repo      string
		reviewers []string
	}{
		{"invalid repository", "no-slash", []string{"qa"}},
		{"no reviewer", "acme/repo", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := app.RequestReview(context.Background(), 9, tc.repo, 7, tc.reviewers, nil)
			reqErr, ok := err.(*ghapp.ReviewRequestError)
			if !ok {
				t.Fatalf("error = %T, want *ReviewRequestError", err)
			}
			if !reqErr.Local || reqErr.Retryable() {
				t.Errorf("error = %+v, want a non-retryable local failure", reqErr)
			}
		})
	}
}

func TestAppRequestReviewSecondaryRateLimitIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/9/access_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"[SECRET]","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "secondary rate limit", http.StatusForbidden)
	}))
	defer srv.Close()

	app := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	err := app.RequestReview(context.Background(), 9, "acme/repo", 7, []string{"qa-reviewer"}, nil)
	reqErr, ok := err.(*ghapp.ReviewRequestError)
	if !ok {
		t.Fatalf("error = %T, want *ReviewRequestError", err)
	}
	if reqErr.StatusCode != http.StatusForbidden || !reqErr.Retryable() || reqErr.RetryAfter != 2*time.Second {
		t.Errorf("error = %+v, want a retryable 403 with a 2s Retry-After", reqErr)
	}
}

func TestAppRequestReviewHonorsRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/9/access_tokens" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"[SECRET]","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		w.Header().Set("Retry-After", "2")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	app := &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
		APIURL: srv.URL,
	}
	err := app.RequestReview(context.Background(), 9, "acme/repo", 7, []string{"qa-reviewer"}, nil)
	reqErr, ok := err.(*ghapp.ReviewRequestError)
	if !ok {
		t.Fatalf("error = %T, want *ReviewRequestError", err)
	}
	if reqErr.StatusCode != http.StatusTooManyRequests || reqErr.RetryAfter != 2*time.Second {
		t.Errorf("error = %+v, want 429 with a 2s Retry-After", reqErr)
	}
}

func TestAppRequestReviewClassifiesErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		retryable bool
	}{
		{"rate limit", http.StatusTooManyRequests, true},
		{"server error", http.StatusInternalServerError, true},
		{"unprocessable", http.StatusUnprocessableEntity, false},
		{"not found", http.StatusNotFound, false},
		{"forbidden without retry-after", http.StatusForbidden, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/app/installations/9/access_tokens" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"token":"[SECRET]","expires_at":%q}`,
						time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
					return
				}
				http.Error(w, "rejected", tc.status)
			}))
			defer srv.Close()

			app := &ghapp.App{
				Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testAppPEM(t)},
				APIURL: srv.URL,
			}
			err := app.RequestReview(context.Background(), 9, "acme/repo", 7, []string{"qa-reviewer"}, nil)
			if err == nil {
				t.Fatal("RequestReview() error = nil, want a rejection")
			}
			reqErr, ok := err.(*ghapp.ReviewRequestError)
			if !ok {
				t.Fatalf("error = %T, want *ReviewRequestError", err)
			}
			if reqErr.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", reqErr.StatusCode, tc.status)
			}
			if reqErr.Retryable() != tc.retryable {
				t.Errorf("retryable = %v, want %v", reqErr.Retryable(), tc.retryable)
			}
		})
	}
}
