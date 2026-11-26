package syncer_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v93/github"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

const testInstallationID int64 = 7

var testPEM = func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}()

type stubAppProvider struct {
	app *ghapp.App
	err error
}

func (p stubAppProvider) App(context.Context) (*ghapp.App, error) {
	return p.app, p.err
}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, events.Event) error { return nil }

type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *recordingPublisher) Publish(_ context.Context, ev events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, ev)
	return nil
}

func (p *recordingPublisher) snapshot() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.events...)
}

type stubChecksRefresher struct {
	changed bool
	err     error
	source  string
	calls   int
}

func (r *stubChecksRefresher) RefreshPRWith(_ context.Context, _ *gh.Client, _ store.PullRequest, source string) (store.PRValidations, bool, error) {
	r.calls++
	r.source = source
	return store.PRValidations{Passed: 1, Total: 1}, r.changed, r.err
}

type recordingCIGate struct {
	mu  sync.Mutex
	ids []uint
}

func (g *recordingCIGate) OnChecksUpdated(_ context.Context, prID uint) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ids = append(g.ids, prID)
	return nil
}

func (g *recordingCIGate) snapshot() []uint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]uint(nil), g.ids...)
}

// fakeGitHub serves the installation token endpoint plus the REST endpoints
// exercised by the syncer. The latter are supplied per test so that pagination
// and call counts are observable.
type fakeGitHub struct {
	srv *httptest.Server

	mu             sync.Mutex
	reposCalls     int
	pullsCalls     int
	installations  []int64
	failTokenForID int64

	repos            func(http.ResponseWriter, *http.Request)
	pulls            func(http.ResponseWriter, *http.Request)
	pullGet          func(http.ResponseWriter, *http.Request)
	appInstallations func(http.ResponseWriter, *http.Request)
	getInstallation  func(http.ResponseWriter, *http.Request)
	reviewReqs       []syncerReviewRequest
}

type syncerReviewRequest struct {
	Repo          string
	Number        int
	Reviewers     []string
	TeamReviewers []string
}

func newFakeGitHub(t *testing.T, repos, pulls, appInstallations func(http.ResponseWriter, *http.Request)) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{repos: repos, pulls: pulls, appInstallations: appInstallations}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/app/installations" {
		if f.appInstallations != nil {
			f.appInstallations(w, r)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "bad installation id", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.installations = append(f.installations, id)
		fail := f.failTokenForID == id
		f.mu.Unlock()
		if fail {
			http.Error(w, "token service unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":"ghs_test_%d","expires_at":%q}`, id, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return
	}
	if raw, ok := strings.CutPrefix(r.URL.Path, "/app/installations/"); ok {
		if _, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if f.getInstallation != nil {
				f.getInstallation(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
	}

	switch {
	case r.URL.Path == "/installation/repositories":
		f.mu.Lock()
		f.reposCalls++
		f.mu.Unlock()
		f.repos(w, r)
	case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/requested_reviewers"):
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Reviewers     []string `json:"reviewers"`
			TeamReviewers []string `json:"team_reviewers"`
		}
		_ = json.Unmarshal(raw, &in)
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		number := 0
		repo := ""
		if len(parts) == 6 {
			number, _ = strconv.Atoi(parts[4])
			repo = parts[1] + "/" + parts[2]
		}
		f.mu.Lock()
		f.reviewReqs = append(f.reviewReqs, syncerReviewRequest{
			Repo: repo, Number: number,
			Reviewers: in.Reviewers, TeamReviewers: in.TeamReviewers,
		})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":1}`))
	case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/pulls"):
		f.mu.Lock()
		f.pullsCalls++
		f.mu.Unlock()
		f.pulls(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") && strings.Contains(r.URL.Path, "/pulls/"):
		if f.pullGet != nil {
			f.pullGet(w, r)
		} else {
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) failTokenFor(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failTokenForID = id
}

func (f *fakeGitHub) repoCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reposCalls
}

func (f *fakeGitHub) pullCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pullsCalls
}

func (f *fakeGitHub) seenInstallations() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.installations...)
}

func (f *fakeGitHub) reviewRequests() []syncerReviewRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syncerReviewRequest(nil), f.reviewReqs...)
}

func writeRepos(t *testing.T, w http.ResponseWriter, r *http.Request, repos []*gh.Repository, nextPage string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if nextPage != "" {
		q := r.URL.Query()
		q.Set("page", nextPage)
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
	}
	if err := json.NewEncoder(w).Encode(gh.ListRepositories{
		TotalCount:   gh.Int(len(repos)),
		Repositories: repos,
	}); err != nil {
		t.Errorf("encode repositories: %v", err)
	}
}

func writePulls(t *testing.T, w http.ResponseWriter, r *http.Request, numbers []int, updated time.Time, nextPage string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if nextPage != "" {
		q := r.URL.Query()
		q.Set("page", nextPage)
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
	}
	pulls := make([]*gh.PullRequest, 0, len(numbers))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	baseRepo := ""
	if len(parts) >= 3 {
		baseRepo = parts[1] + "/" + parts[2]
	}
	for _, number := range numbers {
		pulls = append(pulls, &gh.PullRequest{
			Number: gh.Int(number),
			Title:  gh.String(fmt.Sprintf("PR %d", number)),
			User:   &gh.User{Login: gh.String("alice")},
			Head: &gh.PullRequestBranch{
				Ref:  gh.String("feature"),
				SHA:  gh.String(fmt.Sprintf("sha-%d", number)),
				Repo: &gh.Repository{FullName: gh.String(baseRepo)},
			},
			Base:      &gh.PullRequestBranch{Ref: gh.String("main")},
			HTMLURL:   gh.String(fmt.Sprintf("https://github.com/acme/repo/pull/%d", number)),
			State:     gh.String("open"),
			UpdatedAt: &gh.Timestamp{Time: updated},
		})
	}
	if err := json.NewEncoder(w).Encode(pulls); err != nil {
		t.Errorf("encode pull requests: %v", err)
	}
}

func repo(id int64, fullName string) *gh.Repository {
	return &gh.Repository{
		ID:            gh.Int64(id),
		Name:          gh.String(strings.TrimPrefix(fullName, "acme/")),
		FullName:      gh.String(fullName),
		Private:       gh.Bool(false),
		DefaultBranch: gh.String("main"),
	}
}

func numberedRepos(first, last int) []*gh.Repository {
	repos := make([]*gh.Repository, 0, last-first+1)
	for id := first; id <= last; id++ {
		repos = append(repos, repo(int64(id), fmt.Sprintf("acme/repo-%d", id)))
	}
	return repos
}

func numberedPulls(first, last int) []int {
	numbers := make([]int, 0, last-first+1)
	for n := first; n <= last; n++ {
		numbers = append(numbers, n)
	}
	return numbers
}

func newSyncer(s *store.Store, apiURL string, pub events.Publisher) *syncer.Syncer {
	return &syncer.Syncer{
		Store: s,
		Apps: stubAppProvider{app: &ghapp.App{
			Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testPEM},
			APIURL: apiURL,
		}},
		Events: pub,
	}
}

func seedInstallation(t *testing.T, s *store.Store, id int64) {
	t.Helper()
	if err := s.UpsertInstallation(context.Background(), store.Installation{
		ID: id, AccountLogin: "acme", AccountType: "Organization",
	}); err != nil {
		t.Fatalf("seed installation %d: %v", id, err)
	}
}

func seedRepo(t *testing.T, s *store.Store, id int64, installationID int64, fullName string, enabled bool) {
	t.Helper()
	// UpsertRepository deliberately preserves Enabled, so seed local settings
	// directly to emulate an administrator enabling the repository.
	err := s.DB.WithContext(context.Background()).Create(&store.Repository{
		ID: id, InstallationID: installationID, FullName: fullName,
		DefaultBranch: "main", Enabled: enabled,
	}).Error
	if err != nil {
		t.Fatalf("seed repository %d: %v", id, err)
	}
}

func seedPR(t *testing.T, s *store.Store, repoID int64, number int, updated time.Time) store.PullRequest {
	t.Helper()
	var repository store.Repository
	if err := s.DB.WithContext(context.Background()).Take(&repository, repoID).Error; err != nil {
		t.Fatalf("load repository %d: %v", repoID, err)
	}
	pr, changed, err := s.SyncPullRequest(context.Background(), store.PullRequest{
		RepositoryID: repoID, Number: number, Title: fmt.Sprintf("PR %d", number),
		AuthorLogin: "alice", HeadSHA: fmt.Sprintf("sha-%d", number),
		BaseRef: "main", HeadRef: "feature", HTMLURL: fmt.Sprintf("https://github.com/acme/repo/pull/%d", number),
		State: "open", GitHubUpdatedAt: updated, Origin: store.OriginInternal,
		HeadRepoFullName: repository.FullName,
	})
	if err != nil {
		t.Fatalf("seed pull request #%d: %v", number, err)
	}
	if !changed {
		t.Fatalf("seed pull request #%d: unexpectedly unchanged", number)
	}
	return pr
}

func seedAdmin(t *testing.T, s *store.Store) store.User {
	t.Helper()
	u := store.User{GitHubID: 999, Login: "admin", IsAdmin: true}
	if err := s.DB.WithContext(context.Background()).Create(&u).Error; err != nil {
		t.Fatalf("seed admin user: %v", err)
	}
	return u
}

func countRows(t *testing.T, s *store.Store, model any, query string, args ...any) int64 {
	t.Helper()
	var n int64
	q := s.DB.WithContext(context.Background()).Model(model)
	if query != "" {
		q = q.Where(query, args...)
	}
	if err := q.Count(&n).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

func TestSyncPaginates(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	pub := &recordingPublisher{}
	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Errorf("repos per_page = %q, want 100", got)
			}
			switch r.URL.Query().Get("page") {
			case "", "1":
				writeRepos(t, w, r, numberedRepos(1, 100), "2")
			case "2":
				writeRepos(t, w, r, numberedRepos(101, 150), "")
			default:
				t.Errorf("unexpected repos page %q", r.URL.Query().Get("page"))
				http.NotFound(w, r)
			}
		},
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Errorf("pulls per_page = %q, want 100", got)
			}
			if got := r.URL.Query().Get("state"); got != "open" {
				t.Errorf("pulls state = %q, want open", got)
			}
			if got := r.URL.Path; got != "/repos/acme/repo-1/pulls" {
				t.Errorf("pulls path = %q, want /repos/acme/repo-1/pulls", got)
			}
			switch r.URL.Query().Get("page") {
			case "", "1":
				writePulls(t, w, r, numberedPulls(1, 60), updated, "2")
			case "2":
				writePulls(t, w, r, numberedPulls(61, 120), updated, "")
			default:
				t.Errorf("unexpected pulls page %q", r.URL.Query().Get("page"))
				http.NotFound(w, r)
			}
		}, nil)

	if err := newSyncer(s, f.srv.URL, pub).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	if got := countRows(t, s, &store.Repository{}, ""); got != 150 {
		t.Errorf("repositories in DB = %d, want 150", got)
	}
	if got := countRows(t, s, &store.PullRequest{}, "repository_id = ?", 1); got != 120 {
		t.Errorf("pull requests in repo 1 = %d, want 120", got)
	}
	if got := f.pullCalls(); got != 2 {
		t.Errorf("/pulls calls = %d, want 2 (one enabled repo, two pages)", got)
	}
	// Repository 1 was preseeded; the other 149 are new and must announce themselves.
	repoEvents := 0
	seenLast := false
	for _, ev := range pub.snapshot() {
		if ev.Type != "repo.updated" {
			continue
		}
		repoEvents++
		if ev.ID == "150" {
			seenLast = true
		}
		if ev.ID == "1" {
			t.Error("repo.updated published for an already-known repository")
		}
	}
	if repoEvents != 149 || !seenLast {
		t.Errorf("repo.updated events = %d (contains 150: %t), want 149 including repository 150", repoEvents, seenLast)
	}
}

func TestSyncRemovesRepo(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 100, testInstallationID, "acme/a", true)
	seedRepo(t, s, 200, testInstallationID, "acme/b", true)
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	seedPR(t, s, 200, 5, updated)
	admin := seedAdmin(t, s)

	var onlyA atomic.Bool
	pub := &recordingPublisher{}
	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			repos := []*gh.Repository{repo(100, "acme/a"), repo(200, "acme/b")}
			if onlyA.Load() {
				repos = repos[:1]
			}
			writeRepos(t, w, r, repos, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			numbers := []int{}
			if r.URL.Path == "/repos/acme/b/pulls" {
				numbers = []int{5}
			}
			writePulls(t, w, r, numbers, updated, "")
		}, nil)
	sy := newSyncer(s, f.srv.URL, pub)

	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("first SyncInstallation() error = %v", err)
	}
	views, err := s.ListVisiblePullRequests(ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests() error = %v", err)
	}
	if len(views) != 1 || views[0].Repo != "acme/b" || views[0].Number != 5 {
		t.Fatalf("visible PRs before removal = %+v, want acme/b #5", views)
	}

	onlyA.Store(true)
	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("second SyncInstallation() error = %v", err)
	}
	if got := countRows(t, s, &store.Repository{}, "id = ?", 200); got != 0 {
		t.Errorf("repository B rows = %d, want 0", got)
	}
	views, err = s.ListVisiblePullRequests(ctx, admin, store.PRFilter{})
	if err != nil {
		t.Fatalf("ListVisiblePullRequests() after removal error = %v", err)
	}
	for _, v := range views {
		if v.Repo == "acme/b" || v.Number == 5 {
			t.Errorf("PR of removed repository still visible: %+v", v)
		}
	}
	wantEvent := events.Event{Type: "repo.updated", ID: "200"}
	got := pub.snapshot()
	if len(got) != 1 || got[0] != wantEvent {
		t.Errorf("events after second sync = %+v, want [%+v]", got, wantEvent)
	}
}

// Polling must announce a base move so the reconciler can resolve the new
// comparison, and it must drop the merge base resolved for the old one.
func TestSyncClearsMergeBaseWhenTheBaseMoves(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	pr := seedPR(t, s, 1, 1, updated)
	if err := s.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{"base_sha": "tip-1", "merge_base_sha": "mb-1"}).Error; err != nil {
		t.Fatalf("seed comparison: %v", err)
	}

	pub := &recordingPublisher{}
	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]*gh.PullRequest{{
				Number:    gh.Int(1),
				Title:     gh.String("PR 1"),
				User:      &gh.User{Login: gh.String("alice")},
				Head:      &gh.PullRequestBranch{Ref: gh.String("feature"), SHA: gh.String("sha-1")},
				Base:      &gh.PullRequestBranch{Ref: gh.String("main"), SHA: gh.String("tip-2")},
				State:     gh.String("open"),
				UpdatedAt: &gh.Timestamp{Time: updated.Add(time.Hour)},
			}})
		}, nil)

	if err := newSyncer(s, f.srv.URL, pub).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}

	var saved store.PullRequest
	if err := s.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatalf("reload pull request: %v", err)
	}
	if saved.BaseSHA != "tip-2" {
		t.Errorf("base_sha = %q, want tip-2", saved.BaseSHA)
	}
	if saved.MergeBaseSHA != "" {
		t.Errorf("merge_base_sha = %q, want the old comparison cleared", saved.MergeBaseSHA)
	}
	want := events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(pr.ID), 10)}
	if got := pub.snapshot(); len(got) != 1 || got[0] != want {
		t.Errorf("events = %+v, want [%+v]", got, want)
	}
}

func TestSyncSkipsDisabledRepos(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", false)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, numberedPulls(1, 3), time.Now(), "")
		}, nil)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	if got := f.pullCalls(); got != 0 {
		t.Errorf("/pulls calls for disabled repository = %d, want 0", got)
	}
	if got := countRows(t, s, &store.PullRequest{}, ""); got != 0 {
		t.Errorf("pull requests for disabled repository = %d, want 0", got)
	}
}

func TestSyncClosesMissing(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
	seedPR(t, s, 1, 5, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{6}, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "")
		}, nil)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	var missing store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 5).Take(&missing).Error; err != nil {
		t.Fatalf("load PR #5: %v", err)
	}
	if missing.State != store.StatusClosed || missing.QAStatus != store.StatusClosed {
		t.Errorf("PR #5 state/qa_status = %q/%q, want %q/%q", missing.State, missing.QAStatus, store.StatusClosed, store.StatusClosed)
	}
	var present store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 6).Take(&present).Error; err != nil {
		t.Fatalf("load PR #6: %v", err)
	}
	if present.State != "open" {
		t.Errorf("PR #6 state = %q, want open", present.State)
	}
}

// A failure in the first repository of an installation must not abandon the
// remaining repositories, and the failed one must be retried once GitHub
// recovers.
func TestSyncIsolatesRepositoryFailures(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 10, testInstallationID, "acme/broken", true)
	seedRepo(t, s, 20, testInstallationID, "acme/healthy", true)
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	var broken atomic.Bool
	broken.Store(true)
	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{
				repo(10, "acme/broken"),
				repo(20, "acme/healthy"),
			}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/acme/broken/pulls" && broken.Load() {
				http.Error(w, "repository read failed", http.StatusInternalServerError)
				return
			}
			writePulls(t, w, r, []int{1}, updated, "")
		}, nil)
	sy := newSyncer(s, f.srv.URL, noopPublisher{})

	if err := sy.SyncInstallation(ctx, testInstallationID); err == nil {
		t.Fatal("SyncInstallation() error = nil, want the failed repository reported")
	}
	if got := countRows(t, s, &store.PullRequest{}, "repository_id = ?", 20); got != 1 {
		t.Fatalf("healthy repository PRs = %d, want 1 synced despite the failure", got)
	}

	broken.Store(false)
	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() after recovery error = %v", err)
	}
	for _, repoID := range []int64{10, 20} {
		if got := countRows(t, s, &store.PullRequest{}, "repository_id = ?", repoID); got != 1 {
			t.Errorf("repository %d PRs = %d, want 1", repoID, got)
		}
	}
}

// The paginated snapshot can miss a pull request opened while it was being
// read. The sweep must re-check a missing row before closing it.
func TestSyncRechecksMissingBeforeClosing(t *testing.T) {
	cases := []struct {
		name  string
		state string
		want  string
	}{
		{"opened during the sweep", "open", "open"},
		{"really closed on GitHub", "closed", store.StatusClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := testutil.NewStore(t)
			seedInstallation(t, s, testInstallationID)
			seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
			seedPR(t, s, 1, 5, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

			f := newFakeGitHub(t,
				func(w http.ResponseWriter, r *http.Request) {
					writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
				},
				func(w http.ResponseWriter, r *http.Request) {
					writePulls(t, w, r, []int{6}, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "")
				}, nil)
			f.pullGet = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/repo-1/pulls/5" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(&gh.PullRequest{
					Number: gh.Int(5),
					State:  gh.String(tc.state),
				})
			}

			if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
				t.Fatalf("SyncInstallation() error = %v", err)
			}
			var pr store.PullRequest
			if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 5).Take(&pr).Error; err != nil {
				t.Fatalf("load PR #5: %v", err)
			}
			if pr.State != tc.want {
				t.Errorf("PR #5 state = %q, want %q", pr.State, tc.want)
			}
		})
	}
}

// A webhook can persist a pull request after the revalidation snapshot but
// before the close update. That row belongs to a pull request opened during the
// sweep and must not be closed unvalidated.
func TestSyncDoesNotClosePRWrittenDuringSweep(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
	seedPR(t, s, 1, 5, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{6}, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), "")
		}, nil)
	f.pullGet = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/repo-1/pulls/5" {
			http.NotFound(w, r)
			return
		}
		// The delivery lands while the sweep is re-checking PR #5.
		raced := store.PullRequest{
			RepositoryID: 1, Number: 7, Title: "PR 7", AuthorLogin: "alice",
			HeadSHA: "sha-7", BaseRef: "main", HeadRef: "feature",
			HTMLURL: "https://github.com/acme/repo-1/pull/7",
			State:   "open", QAStatus: store.StatusPending, GitHubUpdatedAt: time.Now().UTC(),
		}
		if err := s.DB.WithContext(ctx).Create(&raced).Error; err != nil {
			t.Errorf("insert pull request during the sweep: %v", err)
			http.Error(w, "insert failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&gh.PullRequest{
			Number: gh.Int(5),
			State:  gh.String("closed"),
		})
	}

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	var closed store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 5).Take(&closed).Error; err != nil {
		t.Fatalf("load PR #5: %v", err)
	}
	if closed.State != store.StatusClosed {
		t.Errorf("PR #5 state = %q, want %q", closed.State, store.StatusClosed)
	}
	var raced store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 7).Take(&raced).Error; err != nil {
		t.Fatalf("load PR #7: %v", err)
	}
	if raced.State != "open" {
		t.Errorf("PR #7 state = %q, want open (written after the sweep began)", raced.State)
	}
}

func TestSyncPublishesEvents(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
	pub := &recordingPublisher{}
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{7}, updated, "")
		}, nil)
	sy := newSyncer(s, f.srv.URL, pub)

	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("first SyncInstallation() error = %v", err)
	}
	var pr store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 7).Take(&pr).Error; err != nil {
		t.Fatalf("load PR #7: %v", err)
	}
	want := []events.Event{
		{Type: "pr.updated", ID: strconv.FormatUint(uint64(pr.ID), 10)},
	}
	got := pub.snapshot()
	if len(got) != len(want) {
		t.Fatalf("events after first sync = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("second SyncInstallation() error = %v", err)
	}
	if got := pub.snapshot(); len(got) != len(want) {
		t.Fatalf("events after identical second sync = %+v, want no new events", got)
	}
}

func TestPollingReevaluatesTheGateWhenChecksChange(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{7}, updated, "")
		}, nil)
	sy := newSyncer(s, f.srv.URL, &recordingPublisher{})
	refresher := &stubChecksRefresher{changed: true}
	gate := &recordingCIGate{}
	sy.Checks = refresher
	sy.CIGate = gate

	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	var pr store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 7).Take(&pr).Error; err != nil {
		t.Fatalf("load PR #7: %v", err)
	}
	if got := gate.snapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("OnChecksUpdated calls = %v, want [%d]", got, pr.ID)
	}
	if refresher.source != store.ObservationSourcePolling {
		t.Errorf("observation source = %q, want %q", refresher.source, store.ObservationSourcePolling)
	}

	// An unchanged observation must not re-drive the gate on every pass.
	refresher.changed = false
	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("second SyncInstallation() error = %v", err)
	}
	if got := gate.snapshot(); len(got) != 1 {
		t.Errorf("OnChecksUpdated calls after an unchanged pass = %v, want [%d]", got, pr.ID)
	}
}

func TestPollingWithdrawsTheCheckAfterAFailedLookup(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, []int{7}, updated, "")
		}, nil)
	sy := newSyncer(s, f.srv.URL, noopPublisher{})
	// A failed lookup clears the counters, so changed is true even though the
	// refresher returns an error.
	sy.Checks = &stubChecksRefresher{changed: true, err: errors.New("github unavailable")}
	gate := &recordingCIGate{}
	sy.CIGate = gate

	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	var pr store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 7).Take(&pr).Error; err != nil {
		t.Fatalf("load PR #7: %v", err)
	}
	if got := gate.snapshot(); len(got) != 1 || got[0] != pr.ID {
		t.Fatalf("OnChecksUpdated calls = %v, want [%d] after a failed lookup", got, pr.ID)
	}
}

func TestReconcileContinuesOnError(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, 1)
	seedInstallation(t, s, 2)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(200, "acme/two")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, nil, time.Time{}, "")
		}, nil)
	f.failTokenFor(1)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll() error = %v, want nil after logging the failed installation", err)
	}
	if got := countRows(t, s, &store.Repository{}, "id = ?", 200); got != 1 {
		t.Errorf("repository of installation 2 = %d rows, want 1", got)
	}
	seen := f.seenInstallations()
	found := map[int64]bool{}
	for _, id := range seen {
		found[id] = true
	}
	if !found[1] || !found[2] {
		t.Errorf("installations attempted = %v, want both 1 and 2", seen)
	}
}

func writeInstallation(t *testing.T, w http.ResponseWriter, inst *gh.Installation) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(inst); err != nil {
		t.Errorf("encode installation: %v", err)
	}
}

func writeAppInstallations(t *testing.T, w http.ResponseWriter, r *http.Request, installs []*gh.Installation, nextPage string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if nextPage != "" {
		q := r.URL.Query()
		q.Set("page", nextPage)
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
	}
	if err := json.NewEncoder(w).Encode(installs); err != nil {
		t.Errorf("encode installations: %v", err)
	}
}

func ghInstallation(id int64, login, accountType string) *gh.Installation {
	return &gh.Installation{
		ID: gh.Int64(id),
		Account: &gh.User{
			Login: gh.String(login),
			Type:  gh.String(accountType),
		},
	}
}

func ghSuspendedInstallation(id int64, login, accountType string, suspendedAt time.Time) *gh.Installation {
	inst := ghInstallation(id, login, accountType)
	inst.SuspendedAt = &gh.Timestamp{Time: suspendedAt}
	return inst
}

func TestReconcileDiscoversInstallations(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("Authorization")
			var repos []*gh.Repository
			switch token {
			case "token ghs_test_1":
				repos = []*gh.Repository{repo(101, "acme/repo-1")}
			case "token ghs_test_2":
				repos = []*gh.Repository{repo(102, "personal/repo-2")}
			default:
				t.Errorf("unexpected installation token %q for %s", token, r.URL.Path)
			}
			writeRepos(t, w, r, repos, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writeAppInstallations(t, w, r, []*gh.Installation{
				ghInstallation(1, "acme", "Organization"),
				ghInstallation(2, "personal", "User"),
			}, "")
		},
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll() error = %v", err)
	}

	installations, err := s.ListInstallations(ctx)
	if err != nil {
		t.Fatalf("ListInstallations() error = %v", err)
	}
	if len(installations) != 2 {
		t.Fatalf("installations = %d, want 2", len(installations))
	}
	byID := map[int64]store.Installation{}
	for _, in := range installations {
		byID[in.ID] = in
	}
	if byID[1].AccountLogin != "acme" || byID[1].AccountType != "Organization" {
		t.Errorf("installation 1 = %+v, want acme/Organization", byID[1])
	}
	if byID[2].AccountLogin != "personal" || byID[2].AccountType != "User" {
		t.Errorf("installation 2 = %+v, want personal/User", byID[2])
	}
	if got := countRows(t, s, &store.Repository{}, "id = ?", 101); got != 1 {
		t.Errorf("repo 101 rows = %d, want 1", got)
	}
	if got := countRows(t, s, &store.Repository{}, "id = ?", 102); got != 1 {
		t.Errorf("repo 102 rows = %d, want 1", got)
	}
	var r101 store.Repository
	if err := s.DB.WithContext(ctx).Where("id = ?", 101).Take(&r101).Error; err != nil {
		t.Fatalf("load repo 101: %v", err)
	}
	if r101.Enabled {
		t.Error("repo 101 Enabled = true, want false")
	}
}

func TestReconcilePrunesMissingInstallations(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, 1)
	seedInstallation(t, s, 2)
	seedRepo(t, s, 201, 2, "acme/old", false)
	seedPR(t, s, 201, 9, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("Authorization")
			if token != "token ghs_test_1" {
				t.Errorf("unexpected installation token %q", token)
			}
			writeRepos(t, w, r, []*gh.Repository{repo(101, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writeAppInstallations(t, w, r, []*gh.Installation{
				ghInstallation(1, "acme", "Organization"),
			}, "")
		},
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll() error = %v", err)
	}

	if got := countRows(t, s, &store.Installation{}, "id = ?", 2); got != 0 {
		t.Errorf("installation 2 rows = %d, want 0", got)
	}
	if got := countRows(t, s, &store.Repository{}, "installation_id = ?", 2); got != 0 {
		t.Errorf("repos of installation 2 = %d, want 0", got)
	}
	if got := countRows(t, s, &store.PullRequest{}, "repository_id = ?", 201); got != 0 {
		t.Errorf("PRs of repo 201 = %d, want 0", got)
	}
	if got := countRows(t, s, &store.Installation{}, "id = ?", 1); got != 1 {
		t.Errorf("installation 1 rows = %d, want 1", got)
	}
}

func TestReconcileFallsBackOnDiscoveryError(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, 1)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(101, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll() error = %v, want nil (fallback)", err)
	}
	if got := countRows(t, s, &store.Installation{}, "id = ?", 1); got != 1 {
		t.Errorf("installation 1 rows = %d, want 1", got)
	}
	if got := countRows(t, s, &store.Repository{}, "id = ?", 101); got != 1 {
		t.Errorf("repo 101 rows = %d, want 1 (synced via fallback)", got)
	}
}

func TestSyncInstallationSkipsSuspended(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	now := time.Now()
	if err := s.UpsertInstallation(ctx, store.Installation{
		ID:           1,
		AccountLogin: "acme",
		AccountType:  "Organization",
		SuspendedAt:  &now,
	}); err != nil {
		t.Fatalf("seed suspended installation: %v", err)
	}

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("installation repositories endpoint should not be called for suspended installation")
			writeRepos(t, w, r, nil, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("pulls endpoint should not be called for suspended installation")
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		nil,
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, 1); err != nil {
		t.Fatalf("SyncInstallation() error = %v, want nil", err)
	}
	if got := len(f.seenInstallations()); got != 0 {
		t.Errorf("installation token requests = %d, want 0", got)
	}
	if got := countRows(t, s, &store.Repository{}, ""); got != 0 {
		t.Errorf("repositories rows = %d, want 0", got)
	}
}

func TestSyncInstallationRecoversMissingRow(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("pulls endpoint should not be called: the recovered repository is disabled")
			writePulls(t, w, r, nil, time.Time{}, "")
		}, nil)
	f.getInstallation = func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/app/installations/7" {
			t.Errorf("installation path = %q, want /app/installations/7", got)
		}
		writeInstallation(t, w, ghInstallation(testInstallationID, "acme", "Organization"))
	}

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}

	var inst store.Installation
	if err := s.DB.WithContext(ctx).Where("id = ?", testInstallationID).Take(&inst).Error; err != nil {
		t.Fatalf("load installation %d: %v", testInstallationID, err)
	}
	if inst.AccountLogin != "acme" || inst.AccountType != "Organization" {
		t.Errorf("installation = %+v, want acme/Organization", inst)
	}
	if got := countRows(t, s, &store.Repository{}, "id = ?", 1); got != 1 {
		t.Errorf("repo 1 rows = %d, want 1", got)
	}
}

func TestSyncInstallationMissingOnGitHubIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("repositories endpoint should not be called when the installation is absent on GitHub")
			writeRepos(t, w, r, nil, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("pulls endpoint should not be called when the installation is absent on GitHub")
			writePulls(t, w, r, nil, time.Time{}, "")
		}, nil)
	f.getInstallation = func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v, want nil", err)
	}
	if got := countRows(t, s, &store.Installation{}, ""); got != 0 {
		t.Errorf("installation rows = %d, want 0", got)
	}
	if got := f.repoCalls(); got != 0 {
		t.Errorf("/installation/repositories calls = %d, want 0", got)
	}
}

func TestSyncInstallationRecoversSuspendedInstallation(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	suspendedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("repositories endpoint should not be called for a suspended installation")
			writeRepos(t, w, r, nil, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("pulls endpoint should not be called for a suspended installation")
			writePulls(t, w, r, nil, time.Time{}, "")
		}, nil)
	f.getInstallation = func(w http.ResponseWriter, r *http.Request) {
		writeInstallation(t, w, ghSuspendedInstallation(testInstallationID, "acme", "Organization", suspendedAt))
	}

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v, want nil", err)
	}
	var inst store.Installation
	if err := s.DB.WithContext(ctx).Where("id = ?", testInstallationID).Take(&inst).Error; err != nil {
		t.Fatalf("load installation %d: %v", testInstallationID, err)
	}
	if inst.SuspendedAt == nil || !inst.SuspendedAt.Equal(suspendedAt) {
		t.Errorf("SuspendedAt = %v, want %v", inst.SuspendedAt, suspendedAt)
	}
	if got := len(f.seenInstallations()); got != 0 {
		t.Errorf("installation token requests = %d, want 0", got)
	}
	if got := f.repoCalls(); got != 0 {
		t.Errorf("/installation/repositories calls = %d, want 0", got)
	}
}

func TestReconcileDiscoversInstallationsAcrossPages(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			var repos []*gh.Repository
			switch r.Header.Get("Authorization") {
			case "token ghs_test_1":
				repos = []*gh.Repository{repo(111, "acme/repo-1")}
			case "token ghs_test_2":
				repos = []*gh.Repository{repo(112, "personal/repo-2")}
			default:
				t.Errorf("unexpected installation token %q for %s", r.Header.Get("Authorization"), r.URL.Path)
			}
			writeRepos(t, w, r, repos, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("per_page"); got != "100" {
				t.Errorf("app installations per_page = %q, want 100", got)
			}
			switch r.URL.Query().Get("page") {
			case "", "1":
				writeAppInstallations(t, w, r, []*gh.Installation{
					ghInstallation(1, "acme", "Organization"),
				}, "2")
			case "2":
				writeAppInstallations(t, w, r, []*gh.Installation{
					ghInstallation(2, "personal", "User"),
				}, "")
			default:
				t.Errorf("unexpected installations page %q", r.URL.Query().Get("page"))
				http.NotFound(w, r)
			}
		},
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll() error = %v", err)
	}

	installations, err := s.ListInstallations(ctx)
	if err != nil {
		t.Fatalf("ListInstallations() error = %v", err)
	}
	if len(installations) != 2 {
		t.Fatalf("installations = %d, want both pages (2)", len(installations))
	}
	byID := map[int64]store.Installation{}
	for _, in := range installations {
		byID[in.ID] = in
	}
	if byID[1].AccountLogin != "acme" || byID[2].AccountLogin != "personal" {
		t.Errorf("discovered installations = %+v, want acme and personal", installations)
	}
	for _, id := range []int64{111, 112} {
		if got := countRows(t, s, &store.Repository{}, "id = ?", id); got != 1 {
			t.Errorf("repo %d rows = %d, want 1", id, got)
		}
	}
	seen := f.seenInstallations()
	if len(seen) != 2 {
		t.Errorf("installations synced = %v, want 1 and 2", seen)
	}
}

func TestReconcileDoesNotPruneWhenDiscoveryPageFails(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, 1)
	seedInstallation(t, s, 2)
	seedRepo(t, s, 201, 1, "acme/repo-1", false)
	seedRepo(t, s, 202, 2, "personal/repo-2", false)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			var repos []*gh.Repository
			switch r.Header.Get("Authorization") {
			case "token ghs_test_1":
				repos = []*gh.Repository{repo(201, "acme/repo-1")}
			case "token ghs_test_2":
				repos = []*gh.Repository{repo(202, "personal/repo-2")}
			default:
				t.Errorf("unexpected installation token %q for %s", r.Header.Get("Authorization"), r.URL.Path)
			}
			writeRepos(t, w, r, repos, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("page") {
			case "", "1":
				writeAppInstallations(t, w, r, []*gh.Installation{
					ghInstallation(1, "acme", "Organization"),
				}, "2")
			case "2":
				http.Error(w, "page 2 unavailable", http.StatusInternalServerError)
			default:
				t.Errorf("unexpected installations page %q", r.URL.Query().Get("page"))
				http.NotFound(w, r)
			}
		},
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll() error = %v, want nil fallback", err)
	}

	installations, err := s.ListInstallations(ctx)
	if err != nil {
		t.Fatalf("ListInstallations() error = %v", err)
	}
	if len(installations) != 2 {
		t.Fatalf("installations after page 2 failure = %+v, want both kept", installations)
	}
	for _, id := range []int64{1, 2} {
		if got := countRows(t, s, &store.Installation{}, "id = ?", id); got != 1 {
			t.Errorf("installation %d rows = %d, want 1", id, got)
		}
	}
	for _, id := range []int64{201, 202} {
		if got := countRows(t, s, &store.Repository{}, "id = ?", id); got != 1 {
			t.Errorf("repo %d rows = %d, want 1 (no cascade pruning)", id, got)
		}
	}
}

func TestReconcileStoresSuspendedAtAndSkipsSync(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	suspendedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("installation repositories endpoint should not be called for a suspended installation")
			writeRepos(t, w, r, nil, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("pulls endpoint should not be called for a suspended installation")
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			writeAppInstallations(t, w, r, []*gh.Installation{
				ghSuspendedInstallation(3, "suspended-org", "Organization", suspendedAt),
			}, "")
		},
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll() error = %v", err)
	}

	var inst store.Installation
	if err := s.DB.WithContext(ctx).Where("id = ?", 3).Take(&inst).Error; err != nil {
		t.Fatalf("load installation 3: %v", err)
	}
	if inst.SuspendedAt == nil || !inst.SuspendedAt.Equal(suspendedAt) {
		t.Errorf("SuspendedAt = %v, want %v", inst.SuspendedAt, suspendedAt)
	}
	if got := f.seenInstallations(); len(got) != 0 {
		t.Errorf("installation token requests = %v, want none", got)
	}
	if got := f.repoCalls(); got != 0 {
		t.Errorf("/installation/repositories calls = %d, want 0", got)
	}
	if got := f.pullCalls(); got != 0 {
		t.Errorf("/pulls calls = %d, want 0", got)
	}
}

func TestSyncInstallationUnknownIDIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)

	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("repositories endpoint should not be called for an unknown installation")
			writeRepos(t, w, r, nil, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("pulls endpoint should not be called for an unknown installation")
			writePulls(t, w, r, nil, time.Time{}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			t.Error("app installations endpoint should not be called by SyncInstallation")
			writeAppInstallations(t, w, r, nil, "")
		},
	)

	if err := newSyncer(s, f.srv.URL, noopPublisher{}).SyncInstallation(ctx, 999); err != nil {
		t.Fatalf("SyncInstallation(999) error = %v, want nil", err)
	}
	if got := f.seenInstallations(); len(got) != 0 {
		t.Errorf("installation token requests = %v, want none", got)
	}
	if got := f.repoCalls(); got != 0 {
		t.Errorf("/installation/repositories calls = %d, want 0", got)
	}
	if got := f.pullCalls(); got != 0 {
		t.Errorf("/pulls calls = %d, want 0", got)
	}
}

// Polling mode has no webhook delivery, so the syncer must hand a fork to a
// person just like the webhook trigger does.
func TestSyncInstallationRequestsReviewForFork(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewStore(t)
	seedInstallation(t, s, testInstallationID)
	seedRepo(t, s, 1, testInstallationID, "acme/repo-1", true)

	profile, err := s.CreateLLMProfile(ctx, store.LLMProfile{
		Name: "primary", Provider: "openai", Model: "gpt-4o-mini",
		APIKeyEnc: "sealed", Enabled: true, HealthStatus: store.HealthOK,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	team := store.Team{Name: "review-team", Slug: "review-team"}
	if err := s.DB.WithContext(ctx).Create(&team).Error; err != nil {
		t.Fatalf("create team: %v", err)
	}
	if _, err := s.SaveQAPolicy(ctx, store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID, ReviewerLogin: "qa-reviewer",
	}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}
	if err := s.AssignRepositoryTeam(ctx, 1, &team.ID); err != nil {
		t.Fatalf("AssignRepositoryTeam: %v", err)
	}

	now := time.Now().UTC()
	f := newFakeGitHub(t,
		func(w http.ResponseWriter, r *http.Request) {
			writeRepos(t, w, r, []*gh.Repository{repo(1, "acme/repo-1")}, "")
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]*gh.PullRequest{{
				Number: gh.Int(1),
				Title:  gh.String("fork PR"),
				User:   &gh.User{Login: gh.String("bob")},
				Head: &gh.PullRequestBranch{
					Ref:  gh.String("feature"),
					SHA:  gh.String("sha-fork"),
					Repo: &gh.Repository{Fork: gh.Bool(true), FullName: gh.String("ext/repo")},
				},
				Base:      &gh.PullRequestBranch{Ref: gh.String("main")},
				HTMLURL:   gh.String("https://github.com/acme/repo-1/pull/1"),
				State:     gh.String("open"),
				UpdatedAt: &gh.Timestamp{Time: now},
			}})
		}, nil)

	sy := newSyncer(s, f.srv.URL, noopPublisher{})
	sy.ReviewRequests = &ghapp.ReviewRequestService{Store: s, Apps: sy.Apps}
	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("SyncInstallation() error = %v", err)
	}
	// A second poll sees the same head; the dedupe table must absorb it.
	if err := sy.SyncInstallation(ctx, testInstallationID); err != nil {
		t.Fatalf("second SyncInstallation() error = %v", err)
	}

	calls := f.reviewRequests()
	if len(calls) != 1 {
		t.Fatalf("review requests = %+v, want one deduplicated request", calls)
	}
	if calls[0].Repo != "acme/repo-1" || calls[0].Number != 1 ||
		len(calls[0].Reviewers) != 1 || calls[0].Reviewers[0] != "qa-reviewer" {
		t.Errorf("review request = %+v, want acme/repo-1#1 to qa-reviewer", calls[0])
	}
	var pr store.PullRequest
	if err := s.DB.WithContext(ctx).Where("repository_id = ? AND number = ?", 1, 1).Take(&pr).Error; err != nil {
		t.Fatalf("load fork PR: %v", err)
	}
	saved, err := s.ReviewRequestFor(ctx, pr.ID, "sha-fork")
	if err != nil {
		t.Fatalf("ReviewRequestFor: %v", err)
	}
	if saved.Status != store.ReviewRequestDelivered || saved.Cause != store.ReviewRequestCauseFork {
		t.Errorf("request = %+v, want a delivered fork request", saved)
	}
}
