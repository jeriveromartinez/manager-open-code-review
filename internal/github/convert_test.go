package github_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v93/github"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestConvertManifest(t *testing.T) {
	const conversionJSON = `{
		"id": 12345,
		"slug": "qa-control-center",
		"html_url": "https://github.com/apps/qa-control-center",
		"client_id": "Iv1.abcdef",
		"client_secret": "client-secret-value",
		"webhook_secret": "webhook-secret-value",
		"pem": "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----\n"
	}`

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(conversionJSON))
	}))
	defer srv.Close()

	creds, err := ghapp.ConvertManifest(context.Background(), srv.Client(), srv.URL, "abc123")
	if err != nil {
		t.Fatalf("ConvertManifest() error = %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want POST", gotMethod)
	}
	if want := "/app-manifests/abc123/conversions"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}

	want := ghapp.AppCredentials{
		AppID:         12345,
		Slug:          "qa-control-center",
		HTMLURL:       "https://github.com/apps/qa-control-center",
		ClientID:      "Iv1.abcdef",
		ClientSecret:  "client-secret-value",
		WebhookSecret: "webhook-secret-value",
		PEM:           "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA...\n-----END RSA PRIVATE KEY-----\n",
	}
	if creds != want {
		t.Errorf("ConvertManifest() = %+v, want %+v", creds, want)
	}
}

func TestConvertManifestNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := ghapp.ConvertManifest(context.Background(), srv.Client(), srv.URL, "bad-code"); err == nil {
		t.Fatal("ConvertManifest() error = nil, want error for 404 response")
	}
}

func prString(s string) *string { return &s }
func prInt(i int) *int          { return &i }
func prBool(b bool) *bool       { return &b }

func newTestPR(state string, mergedAt, updatedAt time.Time, draft bool) *gh.PullRequest {
	p := &gh.PullRequest{
		Number:            prInt(42),
		Title:             prString("Ajustar pipeline de QA"),
		State:             prString(state),
		User:              &gh.User{Login: prString("octocat")},
		AuthorAssociation: prString("CONTRIBUTOR"),
		Head:              &gh.PullRequestBranch{SHA: prString("headsha"), Ref: prString("feature/qa")},
		Base:              &gh.PullRequestBranch{Ref: prString("main")},
		HTMLURL:           prString("https://github.com/acme/repo/pull/42"),
		Draft:             prBool(draft),
		UpdatedAt:         &gh.Timestamp{Time: updatedAt},
	}
	if !mergedAt.IsZero() {
		p.MergedAt = &gh.Timestamp{Time: mergedAt}
	}
	return p
}

func TestPRFromGitHub(t *testing.T) {
	updated := time.Date(2026, 11, 18, 10, 30, 0, 0, time.UTC)
	merged := updated.Add(time.Hour)
	const repoID int64 = 987654

	tests := []struct {
		name      string
		pr        *gh.PullRequest
		wantState string
		wantDraft bool
	}{
		{
			name:      "merged",
			pr:        newTestPR("closed", merged, updated, false),
			wantState: "merged",
		},
		{
			name:      "closed",
			pr:        newTestPR("closed", time.Time{}, updated, false),
			wantState: "closed",
		},
		{
			name:      "open",
			pr:        newTestPR("open", time.Time{}, updated, false),
			wantState: "open",
		},
		{
			name:      "draft",
			pr:        newTestPR("open", time.Time{}, updated, true),
			wantState: "open",
			wantDraft: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ghapp.PRFromGitHub(repoID, tt.pr)
			want := store.PullRequest{
				RepositoryID:      repoID,
				Number:            42,
				Title:             "Ajustar pipeline de QA",
				AuthorLogin:       "octocat",
				AuthorAssociation: "CONTRIBUTOR",
				HeadSHA:           "headsha",
				BaseRef:           "main",
				HeadRef:           "feature/qa",
				HTMLURL:           "https://github.com/acme/repo/pull/42",
				Draft:             tt.wantDraft,
				State:             tt.wantState,
				QAStatus:          "",
				GitHubUpdatedAt:   updated,
			}
			if got != want {
				t.Errorf("PRFromGitHub() = %+v, want %+v", got, want)
			}
		})
	}
}
