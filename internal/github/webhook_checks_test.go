package github_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// fakeChecksGitHub serves the installation token endpoint plus the two commit
// endpoints ReadValidations reads, so a delivery can be exercised end to end.
type fakeChecksGitHub struct {
	srv *httptest.Server

	mu          sync.Mutex
	runs        map[string][]map[string]any
	statuses    map[string][]map[string]any
	checkCalls  int
	statusCalls int
}

func newFakeChecksGitHub(t *testing.T) *fakeChecksGitHub {
	t.Helper()

	f := &fakeChecksGitHub{
		runs:     map[string][]map[string]any{},
		statuses: map[string][]map[string]any{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
			sha, ok := commitPathSHA(r.URL.Path, "check-runs")
			if !ok {
				http.NotFound(w, r)
				return
			}
			f.mu.Lock()
			f.checkCalls++
			runs := f.runs[sha]
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": len(runs),
				"check_runs":  runs,
			})
		case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/status"):
			sha, ok := commitPathSHA(r.URL.Path, "status")
			if !ok {
				http.NotFound(w, r)
				return
			}
			f.mu.Lock()
			f.statusCalls++
			statuses := f.statuses[sha]
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state":       "success",
				"total_count": len(statuses),
				"statuses":    statuses,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// commitPathSHA extracts the ref of /repos/{owner}/{repo}/commits/{ref}/{tail}.
func commitPathSHA(path, tail string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
	if len(parts) != 5 || parts[2] != "commits" || parts[4] != tail {
		return "", false
	}
	return parts[3], true
}

func (f *fakeChecksGitHub) setRuns(sha string, conclusions ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runs := make([]map[string]any, 0, len(conclusions))
	for i, conclusion := range conclusions {
		runs = append(runs, map[string]any{
			"id":           i + 1,
			"name":         fmt.Sprintf("job-%d", i+1),
			"head_sha":     sha,
			"status":       "completed",
			"conclusion":   conclusion,
			"completed_at": "2026-01-01T00:10:00Z",
		})
	}
	f.runs[sha] = runs
}

// addOwnCheckRun appends the QA App's own check run, which the counters must
// ignore: it is the review's result, not an input to the gate.
func (f *fakeChecksGitHub) addOwnCheckRun(sha, conclusion string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[sha] = append(f.runs[sha], map[string]any{
		"id":         99,
		"name":       "QA Control Center",
		"head_sha":   sha,
		"status":     "completed",
		"conclusion": conclusion,
		"app":        map[string]any{"id": 12345},
	})
}

func (f *fakeChecksGitHub) setRunningRuns(sha string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runs := make([]map[string]any, 0, n)
	for i := range n {
		runs = append(runs, map[string]any{
			"id":       i + 1,
			"name":     fmt.Sprintf("job-%d", i+1),
			"head_sha": sha,
			"status":   "in_progress",
		})
	}
	f.runs[sha] = runs
}

func (f *fakeChecksGitHub) setStatuses(sha string, states ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	statuses := make([]map[string]any, 0, len(states))
	for i, state := range states {
		statuses = append(statuses, map[string]any{
			"id":      i + 1,
			"context": fmt.Sprintf("ci/context-%d", i+1),
			"state":   state,
		})
	}
	f.statuses[sha] = statuses
}

func (f *fakeChecksGitHub) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkCalls, f.statusCalls
}

// enableChecks points the webhook handler at a fake GitHub API so deliveries
// refresh the stored counters for real.
func (f *webhookFixture) enableChecks(t *testing.T, api *fakeChecksGitHub) {
	t.Helper()

	app := &ghapp.App{
		Creds: ghapp.AppCredentials{
			AppID:         12345,
			WebhookSecret: webhookTestSecret,
			PEM:           testAppPEM(t),
		},
		APIURL: api.srv.URL,
	}
	provider := fakeAppProvider{app: app}
	f.handler.Apps = provider
	f.handler.Checks = &ghapp.Checks{Store: f.st, Apps: provider}
}

func (f *webhookFixture) validationsOf(t *testing.T, prID uint, headSHA string) store.PRValidations {
	t.Helper()

	got, err := f.st.PRValidationsFor(f.ctx, prID, headSHA)
	if err != nil {
		t.Fatalf("PRValidationsFor: %v", err)
	}
	return got
}

func TestWebhookCheckRunRefreshesValidations(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	api := newFakeChecksGitHub(t)
	api.setRuns("aaa111", "success", "success", "neutral")
	api.addOwnCheckRun("aaa111", "failure")
	api.setStatuses("aaa111", "success")
	f.enableChecks(t, api)

	body := []byte(`{
		"action": "completed",
		"repository": {"id": 501, "full_name": "acme/backend-api"},
		"check_run": {"id": 77, "head_sha": "aaa111", "status": "completed", "conclusion": "success"}
	}`)
	assertStatus(t, f.deliverBody("check_run", body, signWebhook(body)), http.StatusAccepted)

	want := store.PRValidations{Passed: 4, Total: 4}
	if got := f.validationsOf(t, pr.ID, "aaa111"); got != want {
		t.Fatalf("stored validations = %+v, want %+v", got, want)
	}
	if got := f.reviews.snapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("OnPullRequest calls = %v, want [%d]", got, pr.ID)
	}
	if got := f.pub.snapshot(); len(got) != 1 {
		t.Fatalf("published events = %v, want one pr.updated", got)
	}
}

func TestWebhookStatusRefreshesValidations(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	api := newFakeChecksGitHub(t)
	api.setStatuses("aaa111", "success", "failure", "pending")
	f.enableChecks(t, api)

	body := []byte(`{
		"sha": "aaa111",
		"state": "failure",
		"context": "ci/build",
		"repository": {"id": 501, "full_name": "acme/backend-api"}
	}`)
	assertStatus(t, f.deliverBody("status", body, signWebhook(body)), http.StatusAccepted)

	want := store.PRValidations{Passed: 1, Failed: 1, Total: 3}
	got := f.validationsOf(t, pr.ID, "aaa111")
	if got != want {
		t.Fatalf("stored validations = %+v, want %+v", got, want)
	}
	if got.Green() {
		t.Error("Green() = true, want false while a status is pending and another failed")
	}
}

func TestWebhookCheckSuiteRefreshesValidations(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	api := newFakeChecksGitHub(t)
	api.setRuns("aaa111", "success", "success")
	f.enableChecks(t, api)

	body := []byte(`{
		"action": "completed",
		"repository": {"id": 501, "full_name": "acme/backend-api"},
		"check_suite": {"id": 9, "head_sha": "aaa111", "status": "completed", "conclusion": "success"}
	}`)
	assertStatus(t, f.deliverBody("check_suite", body, signWebhook(body)), http.StatusAccepted)

	if got := f.validationsOf(t, pr.ID, "aaa111"); got != (store.PRValidations{Passed: 2, Total: 2}) {
		t.Fatalf("stored validations = %+v, want 2/2", got)
	}
}

func TestWebhookCheckEventsUnknownTargetIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	f.seedOpenPR()
	api := newFakeChecksGitHub(t)
	api.setRuns("aaa111", "success")
	f.enableChecks(t, api)

	tests := []struct {
		name string
		body string
	}{
		{
			name: "unknown repository",
			body: `{"action":"completed","repository":{"id":999},"check_run":{"head_sha":"aaa111"}}`,
		},
		{
			name: "unknown head",
			body: `{"action":"completed","repository":{"id":501},"check_run":{"head_sha":"zzz999"}}`,
		},
		{
			name: "missing head",
			body: `{"action":"completed","repository":{"id":501},"check_run":{}}`,
		},
		{
			name: "status for unknown head",
			body: `{"sha":"zzz999","state":"success","repository":{"id":501}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			rec := f.deliverBody("check_run", body, signWebhook(body))
			assertStatus(t, rec, http.StatusNoContent)
		})
	}

	if got := f.reviews.snapshot(); len(got) != 0 {
		t.Fatalf("OnPullRequest calls = %v, want none", got)
	}
	if checks, statuses := api.calls(); checks != 0 || statuses != 0 {
		t.Fatalf("GitHub calls = %d checks / %d statuses, want none", checks, statuses)
	}
}

func TestWebhookCheckRunForDisabledRepoIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	f.seedOpenPR()
	api := newFakeChecksGitHub(t)
	api.setRuns("aaa111", "success")
	f.enableChecks(t, api)

	if err := f.st.DB.WithContext(f.ctx).Model(&store.Repository{}).
		Where("id = ?", 501).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable repository: %v", err)
	}

	body := []byte(`{"action":"completed","repository":{"id":501},"check_run":{"head_sha":"aaa111"}}`)
	assertStatus(t, f.deliverBody("check_run", body, signWebhook(body)), http.StatusNoContent)
	if got := f.reviews.snapshot(); len(got) != 0 {
		t.Fatalf("OnPullRequest calls = %v, want none", got)
	}
}
