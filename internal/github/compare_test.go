package github_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
)

func TestCompareMergeBase(t *testing.T) {
	var comparePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"read-token","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		comparePath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"merge_base_commit":{"sha":"merge-base-sha"}}`)
	}))
	defer srv.Close()

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: gateTestPEM(t)}, APIURL: srv.URL}
	compare := &ghapp.Compare{Apps: fakeAppProvider{app: app}}

	got, err := compare.MergeBase(context.Background(), 7, "acme/repo", "main", "headsha")
	if err != nil {
		t.Fatalf("MergeBase() error = %v", err)
	}
	if got != "merge-base-sha" {
		t.Errorf("MergeBase() = %q, want merge-base-sha", got)
	}
	if want := "/repos/acme/repo/compare/main...headsha"; comparePath != want {
		t.Errorf("Compare path = %q, want %q", comparePath, want)
	}
}

func TestCompareMergeBaseWithoutMergeBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			_, _ = fmt.Fprintf(w, `{"token":"read-token","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	app := &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: gateTestPEM(t)}, APIURL: srv.URL}
	compare := &ghapp.Compare{Apps: fakeAppProvider{app: app}}

	if _, err := compare.MergeBase(context.Background(), 7, "acme/repo", "main", "headsha"); err == nil {
		t.Fatal("MergeBase() error = nil, want an error when GitHub reports no merge base")
	}
}
