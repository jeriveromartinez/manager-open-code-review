package syncer_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v93/github"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// checksGitHub extends the syncer's fake with the two commit endpoints the
// checks refresher reads.
type checksGitHub struct {
	*fakeGitHub
	runs     []map[string]any
	statuses []map[string]any
}

func newChecksGitHub(t *testing.T, repos, pulls func(http.ResponseWriter, *http.Request)) *checksGitHub {
	t.Helper()

	base := newFakeGitHub(t, repos, pulls, nil)
	c := &checksGitHub{fakeGitHub: base}
	base.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": len(c.runs),
				"check_runs":  c.runs,
			})
		case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/status"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state":       "success",
				"total_count": len(c.statuses),
				"statuses":    c.statuses,
			})
		default:
			base.serve(w, r)
		}
	})
	return c
}

func (c *checksGitHub) setGreen() {
	c.runs = []map[string]any{
		{"id": 1, "name": "build", "head_sha": "sha-1", "status": "completed", "conclusion": "success"},
		{"id": 2, "name": "test", "head_sha": "sha-1", "status": "completed", "conclusion": "success"},
	}
}

func TestSyncRefreshesValidations(t *testing.T) {
	ctx := context.Background()
	instal := testutil.NewStore(t)
	seedInstallation(t, instal, testInstallationID)
	seedRepo(t, instal, 1, testInstallationID, "acme/repo-1", true)

	f := newChecksGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{1}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "")
		})
	f.setGreen()

	sy := newSyncer(instal, f.srv.URL, noopPublisher{})
	sy.Checks = &ghapp.Checks{Store: instal, Apps: sy.Apps}
	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}

	var pr store.PullRequest
	if err := instal.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 1).Take(&pr).Error; err != nil {
		t.Fatalf("load PR #1: %v", err)
	}
	got, err := instal.PRValidationsFor(ctx, pr.ID, pr.HeadSHA)
	if err != nil {
		t.Fatalf("PRValidationsFor: %v", err)
	}
	if want := (store.PRValidations{Passed: 2, Total: 2}); got != want {
		t.Errorf("validations = %+v, want %+v", got, want)
	}
}

// A failing refresh must not abort the reconciliation: a transient GitHub error
// on one pull request cannot stop the remaining ones from syncing.
func TestSyncSurvivesValidationFailure(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{1, 2}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "")
		}, nil)
	// The real checks refresher hits endpoints the fake does not serve, so every
	// refresh fails with a 404.
	f.srv.Config.Handler = http.HandlerFunc(f.serve)

	sy := newSyncer(s, f.srv.URL, noopPublisher{})
	sy.Checks = &ghapp.Checks{Store: s, Apps: sy.Apps}
	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v, want nil", err)
	}
	if got := countRows(t, s, &store.PullRequest{}, ""); got != 2 {
		t.Errorf("pull requests = %d, want 2", got)
	}
}

// A refresher that panics the test would hide a nil-dependency bug, so the gate
// is also checked with the dependency absent.
func TestSyncWithoutRefresherKeepsCounters(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{1}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "")
		}, nil)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	if got := countRows(t, s, &store.PullRequest{}, ""); got != 1 {
		t.Errorf("pull requests = %d, want 1", got)
	}
}
