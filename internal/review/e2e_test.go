package review

// TestE2EFlow is the automated version of docs/install.md "Verificación manual
// de extremo a extremo": trigger, review pipeline, publication and browser
// events run against fakes (a local bare repository, fake ocr/opencode doubles
// and a fake GitHub API), so a regression fails a command instead of a manual
// session with a real repository and a real LLM key.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/events"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/publish"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const e2eCheckID int64 = 555

// e2eTriggerJobs adapts runTestJobs to the trigger enqueue contract without
// making the runner fake implement a debounce path it never uses.
type e2eTriggerJobs struct{ *runTestJobs }

func (e2eTriggerJobs) EnqueueReviewTrigger(context.Context, uint, string, time.Duration) error {
	return nil
}

func TestE2EFlow(t *testing.T) {
	t.Run("success publishes summary, inline comments, check and status", testE2ESuccess)
	t.Run("mixed coverage reports pending files and keeps the finding open", testE2EMixedCoverage)
	t.Run("timeout ends inconclusive without resolving findings", testE2ETimeout)
	t.Run("invalid config fails and the retry publishes", testE2EFailedThenRetry)
	t.Run("push before checkout supersedes and the new head is reviewed", testE2ESuperseded)
	t.Run("only a successful run resolves an open finding", testE2EFindingsLifecycle)
	t.Run("auto-triage withdraws the refuted finding and publishes a curated summary", testE2EAutoTriagePublishesCuratedSummary)
}

// --- fake GitHub API -------------------------------------------------------

type e2eComment struct {
	ID     int64  `json:"id"`
	NodeID string `json:"node_id"`
	Body   string `json:"body"`
}

type e2eReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

type e2eReview struct {
	ID       int64              `json:"id"`
	Event    string             `json:"event"`
	Comments []e2eReviewComment `json:"comments"`
	Posted   []e2ePostedComment `json:"-"`
}

type e2ePostedComment struct {
	ID     int64  `json:"id"`
	NodeID string `json:"node_id"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
}

type e2eCheckPatch struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Output     struct {
		Title       string `json:"title"`
		Summary     string `json:"summary"`
		Text        string `json:"text"`
		Annotations []struct {
			Path            string `json:"path"`
			StartLine       int    `json:"start_line"`
			AnnotationLevel string `json:"annotation_level"`
		} `json:"annotations"`
	} `json:"output"`
}

// e2eGitHub emulates the endpoints the publication matrix touches. It records
// every write so the assertions can check the summary body, the inline review,
// the check conclusion and the thread resolution.
type e2eGitHub struct {
	t *testing.T

	mu                 sync.Mutex
	comments           []e2eComment
	commentPatches     map[int64]string
	commentPatchCalls  int
	reviews            []e2eReview
	checkCreates       []string
	checkPatches       []e2eCheckPatch
	replies            []string
	resolves           int
	reviewCommentNodes []string
	nextCommentID      int64
	nextReviewID       int64
	nextCheckID        int64
}

func newE2EGitHub(t *testing.T) *e2eGitHub {
	return &e2eGitHub{t: t, nextCommentID: 1000, nextReviewID: 900, nextCheckID: e2eCheckID}
}

func (g *e2eGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(v any, status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") &&
		strings.HasSuffix(r.URL.Path, "/access_tokens") {
		writeJSON(map[string]any{
			"token":      "ghs_e2e",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		}, http.StatusOK)
		return
	}

	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/origin/check-runs":
		var in struct {
			HeadSHA string `json:"head_sha"`
		}
		_ = json.Unmarshal(body, &in)
		g.nextCheckID++
		g.checkCreates = append(g.checkCreates, in.HeadSHA)
		writeJSON(map[string]any{
			"id":       g.nextCheckID,
			"html_url": fmt.Sprintf("https://github.com/acme/origin/runs/%d", g.nextCheckID),
			"status":   "in_progress",
		}, http.StatusCreated)

	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/origin/check-runs/"):
		var patch e2eCheckPatch
		_ = json.Unmarshal(body, &patch)
		g.checkPatches = append(g.checkPatches, patch)
		writeJSON(map[string]any{"id": e2eCheckID}, http.StatusOK)

	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/origin/issues/1/comments":
		comments := g.comments
		if comments == nil {
			comments = []e2eComment{}
		}
		writeJSON(comments, http.StatusOK)

	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/origin/issues/1/comments":
		var in struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		g.nextCommentID++
		c := e2eComment{ID: g.nextCommentID, NodeID: fmt.Sprintf("IC_%d", g.nextCommentID), Body: in.Body}
		g.comments = append(g.comments, c)
		writeJSON(c, http.StatusCreated)

	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/origin/issues/comments/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/acme/origin/issues/comments/"), 10, 64)
		if err != nil {
			http.Error(w, "bad comment id", http.StatusBadRequest)
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		if g.commentPatches == nil {
			g.commentPatches = map[int64]string{}
		}
		g.commentPatches[id] = in.Body
		g.commentPatchCalls++
		for i := range g.comments {
			if g.comments[i].ID == id {
				g.comments[i].Body = in.Body
			}
		}
		writeJSON(e2eComment{ID: id, Body: in.Body}, http.StatusOK)

	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/origin/pulls/1/files":
		writeJSON([]map[string]string{
			{
				"filename": "auth.go", "status": "modified",
				"patch": "@@ -1,3 +1,5 @@\n package main\n \n+var insecure = true\n var next = 1\n+var last = 2\n",
			},
			{
				"filename": "main.go", "status": "modified",
				"patch": "@@ -1,4 +1,4 @@\n package main\n \n+var other = 1\n var next = 2\n",
			},
		}, http.StatusOK)

	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/origin/pulls/1/reviews":
		var review e2eReview
		_ = json.Unmarshal(body, &review)
		g.nextReviewID++
		review.ID = g.nextReviewID
		for i, c := range review.Comments {
			id := g.nextReviewID*100 + int64(i)
			review.Posted = append(review.Posted, e2ePostedComment{
				ID: id, NodeID: fmt.Sprintf("NC_%d", id), Path: c.Path, Line: c.Line,
			})
			g.reviewCommentNodes = append(g.reviewCommentNodes, fmt.Sprintf("NC_%d", id))
		}
		g.reviews = append(g.reviews, review)
		writeJSON(map[string]any{"id": review.ID}, http.StatusOK)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/origin/pulls/1/reviews/"):
		posted := []e2ePostedComment{}
		if len(g.reviews) > 0 {
			posted = g.reviews[len(g.reviews)-1].Posted
		}
		writeJSON(posted, http.StatusOK)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/replies"):
		var in struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		g.replies = append(g.replies, in.Body)
		writeJSON(map[string]any{"id": 7000}, http.StatusCreated)

	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		var in struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &in)
		switch {
		case strings.Contains(in.Query, "unresolveReviewThread"):
			writeJSON(map[string]any{"data": map[string]any{"unresolveReviewThread": map[string]any{
				"thread": map[string]any{"id": "THREAD_1", "isResolved": false},
			}}}, http.StatusOK)
		case strings.Contains(in.Query, "resolveReviewThread"):
			g.resolves++
			writeJSON(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{
				"thread": map[string]any{"id": "THREAD_1", "isResolved": true},
			}}}, http.StatusOK)
		case strings.Contains(in.Query, "reviewThreads"):
			nodes := make([]map[string]any, 0, len(g.reviewCommentNodes))
			for _, node := range g.reviewCommentNodes {
				nodes = append(nodes, map[string]any{"id": node})
			}
			threads := []map[string]any{}
			if len(nodes) > 0 {
				threads = append(threads, map[string]any{
					"id": "THREAD_1", "isResolved": false,
					"comments": map[string]any{"nodes": nodes},
				})
			}
			writeJSON(map[string]any{"data": map[string]any{"repository": map[string]any{
				"pullRequest": map[string]any{"reviewThreads": map[string]any{
					"nodes":    threads,
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				}},
			}}}, http.StatusOK)
		default:
			http.Error(w, "unexpected graphql query", http.StatusBadRequest)
		}

	default:
		g.t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func (g *e2eGitHub) summaryBodies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var bodies []string
	for _, c := range g.comments {
		if strings.Contains(c.Body, "<!-- qa-control-center:summary -->") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

func (g *e2eGitHub) commentPatchCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.commentPatchCalls
}

func (g *e2eGitHub) reviewList() []e2eReview {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]e2eReview, len(g.reviews))
	copy(out, g.reviews)
	return out
}

func (g *e2eGitHub) checkPatchList() []e2eCheckPatch {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]e2eCheckPatch(nil), g.checkPatches...)
}

func (g *e2eGitHub) createdChecks() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.checkCreates...)
}

func (g *e2eGitHub) resolveCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.resolves
}

func (g *e2eGitHub) replyCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.replies)
}

// --- event broker and SSE --------------------------------------------------

type e2eEventStream struct {
	broker *events.Broker
	ch     <-chan events.Event
}

func startE2EBroker(t *testing.T, s *store.Store, pub events.Publisher) *e2eEventStream {
	t.Helper()
	broker := events.NewBroker(s.Pool)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("broker Run() = %v, want context.Canceled", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("broker did not stop after cancellation")
		}
	})

	ch, unsubscribe := broker.Subscribe()
	t.Cleanup(unsubscribe)

	probe := events.Event{Type: "e2e.probe", ID: "ready"}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := pub.Publish(context.Background(), probe); err != nil {
			t.Fatalf("publish broker probe: %v", err)
		}
		select {
		case ev := <-ch:
			if ev == probe {
				return &e2eEventStream{broker: broker, ch: ch}
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("event broker did not receive the probe")
	return nil
}

func (s *e2eEventStream) reset() {
	for {
		select {
		case <-s.ch:
		default:
			return
		}
	}
}

// wait blocks until every wanted event arrives. Publication happens after the
// terminal transaction, so a missing event means the panel would stay stale.
func (s *e2eEventStream) wait(t *testing.T, want ...events.Event) {
	t.Helper()
	pending := make(map[events.Event]bool, len(want))
	for _, ev := range want {
		pending[ev] = true
	}
	deadline := time.After(5 * time.Second)
	for len(pending) > 0 {
		select {
		case ev := <-s.ch:
			delete(pending, ev)
		case <-deadline:
			t.Fatalf("missing events %v (panel would not refresh)", pending)
		}
	}
}

func (s *e2eEventStream) absent(t *testing.T, unwanted events.Event) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev := <-s.ch:
			if ev == unwanted {
				t.Fatalf("unexpected event %+v", unwanted)
			}
		case <-deadline:
			return
		}
	}
}

type e2eSSE struct {
	scanner *bufio.Scanner
	event   string
	data    string
}

func (f *e2eFixture) openSSE(t *testing.T) *e2eSSE {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.events.broker.ServeSSE))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		cancel()
		t.Fatalf("build SSE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open SSE stream: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE Content-Type = %q, want text/event-stream", ct)
	}

	sse := &e2eSSE{scanner: bufio.NewScanner(resp.Body)}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_ = f.publisher.Publish(context.Background(), events.Event{Type: "e2e.probe", ID: "ready"})
			select {
			case <-stop:
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	for {
		if !sse.next() {
			t.Fatal("SSE stream closed before the subscription was active")
		}
		if sse.event == "e2e.probe" {
			return sse
		}
	}
}

func (s *e2eSSE) next() bool {
	s.event, s.data = "", ""
	for s.scanner.Scan() {
		line := s.scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			s.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			s.data = strings.TrimPrefix(line, "data: ")
		}
		if s.event != "" && s.data != "" {
			return true
		}
	}
	return false
}

func (s *e2eSSE) waitFor(t *testing.T, eventType string, id uint) {
	t.Helper()
	wantID := strconv.FormatUint(uint64(id), 10)
	for {
		if !s.next() {
			t.Fatalf("SSE stream ended before %s for %s", eventType, wantID)
		}
		if s.event != eventType {
			continue
		}
		var payload events.Event
		if err := json.Unmarshal([]byte(s.data), &payload); err != nil {
			t.Fatalf("decode SSE payload %q: %v", s.data, err)
		}
		if payload.ID == wantID {
			return
		}
	}
}

// --- fixture ---------------------------------------------------------------

type e2eApps struct{ app *ghapp.App }

func (p e2eApps) App(context.Context) (*ghapp.App, error) { return p.app, nil }

type e2eFixture struct {
	*runTestFixture
	runner    *Runner
	worker    *publish.Worker
	trigger   *Trigger
	gh        *e2eGitHub
	events    *e2eEventStream
	publisher events.Publisher
	ocrArgv   string
}

func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()

	f := newRunTestFixture(t)
	if err := f.s.DB.WithContext(f.ctx).
		Where("pull_request_id = ?", f.pr.ID).Delete(&store.ReviewRun{}).Error; err != nil {
		t.Fatalf("clear seeded runs: %v", err)
	}

	gh := newE2EGitHub(t)
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	apps := e2eApps{app: &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 12345, PEM: triggerTestPEM(t)},
		APIURL: srv.URL,
	}}

	publisher := events.NewPGPublisher(f.s.Pool)
	worker := &publish.Worker{
		Store: f.s, Apps: apps, Box: f.box, Events: publisher,
		PublicURL: "https://qa.example",
	}
	runner := f.runner()
	runner.Apps = apps
	runner.Events = publisher
	runner.Jobs = f.jobs

	fixture := &e2eFixture{
		runTestFixture: f,
		runner:         runner,
		worker:         worker,
		trigger: &Trigger{
			Store: f.s, Apps: apps, Jobs: e2eTriggerJobs{f.jobs}, Checks: worker, Box: f.box,
		},
		gh:        gh,
		publisher: publisher,
		ocrArgv:   filepath.Join(f.root, "ocr-argv.txt"),
	}
	fixture.events = startE2EBroker(t, f.s, publisher)
	return fixture
}

// enqueue drives the trigger end to end and returns the run it queued, so the
// flow exercises the same entry point as a webhook or the retry button.
func (f *e2eFixture) enqueue(t *testing.T, retry bool) uint {
	t.Helper()
	before := len(f.jobs.reviews())
	var err error
	if retry {
		err = f.trigger.Retry(f.ctx, f.pr.ID)
	} else {
		err = f.trigger.OnPullRequest(f.ctx, f.pr.ID)
	}
	if err != nil {
		t.Fatalf("trigger (retry=%v): %v", retry, err)
	}
	queued := f.jobs.reviews()
	if len(queued) != before+1 {
		t.Fatalf("queued runs = %v, want one more than %v", queued, queued[:before])
	}
	return queued[len(queued)-1]
}

// execute runs the queued job and the publish job it enqueues, the same order
// the two processes follow in production.
func (f *e2eFixture) execute(t *testing.T, runID uint) {
	t.Helper()
	if err := f.runner.Run(f.ctx, runID); err != nil {
		t.Fatalf("review run %d: %v", runID, err)
	}
	if err := f.worker.Publish(f.ctx, runID); err != nil {
		t.Fatalf("publish run %d: %v", runID, err)
	}
}

func (f *e2eFixture) push(t *testing.T, name string) string {
	t.Helper()
	work := filepath.Join(f.root, "work")
	writeRunTestFile(t, filepath.Join(work, name), "package main\n")
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "cambio "+name)
	sha := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "push", "-q", f.bare, "HEAD:refs/pull/1/head")
	return sha
}

func (f *e2eFixture) setHead(t *testing.T, sha string) {
	t.Helper()
	if err := f.s.DB.WithContext(f.ctx).Model(&store.PullRequest{}).
		Where("id = ?", f.pr.ID).Update("head_sha", sha).Error; err != nil {
		t.Fatalf("set pull request head: %v", err)
	}
}

func (f *e2eFixture) loadRun(t *testing.T, id uint) store.ReviewRun {
	t.Helper()
	run, err := f.s.ReviewRun(f.ctx, id)
	if err != nil {
		t.Fatalf("reload run %d: %v", id, err)
	}
	return run
}

func (f *e2eFixture) findings(t *testing.T) []store.Finding {
	t.Helper()
	found, err := f.s.FindingsForPR(f.ctx, f.pr.ID)
	if err != nil {
		t.Fatalf("findings for pull request: %v", err)
	}
	return found
}

func assertE2ERun(t *testing.T, run store.ReviewRun, status, code string) {
	t.Helper()
	if run.Status != status {
		t.Errorf("run %d status = %q, want %q (code %q: %q)", run.ID, run.Status, status, run.ErrorCode, run.ErrorMessage)
	}
	if run.ErrorCode != code {
		t.Errorf("run %d error_code = %q, want %q", run.ID, run.ErrorCode, code)
	}
	if run.FinishedAt == nil {
		t.Error("run finished_at is nil")
	}
}

func assertE2EPRStatus(t *testing.T, f *e2eFixture, want string) {
	t.Helper()
	if pr := f.loadPR(t); pr.QAStatus != want {
		t.Errorf("qa_status = %q, want %q", pr.QAStatus, want)
	}
}

func assertE2ECheck(t *testing.T, f *e2eFixture, want string) e2eCheckPatch {
	t.Helper()
	patches := f.gh.checkPatchList()
	if len(patches) == 0 {
		t.Fatalf("no check run was completed, want conclusion %q", want)
	}
	last := patches[len(patches)-1]
	if last.Conclusion != want {
		t.Errorf("check conclusion = %q, want %q", last.Conclusion, want)
	}
	return last
}

func assertE2ESummary(t *testing.T, f *e2eFixture, wants ...string) string {
	t.Helper()
	bodies := f.gh.summaryBodies()
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want exactly one", len(bodies))
	}
	for _, want := range wants {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("summary %q does not contain %q", bodies[0], want)
		}
	}
	return bodies[0]
}

// assertShallowBase pins the OCR regression: on a shallow clone the branch name
// is not a local ref, so the pipeline must hand ocr the resolved base SHA from
// FETCH_HEAD instead of cfg.PR.BaseRef.
func (f *e2eFixture) assertShallowBase(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(f.ocrArgv)
	if err != nil {
		t.Fatalf("read ocr argv: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		args := strings.Fields(line)
		for i, arg := range args {
			if arg != "--from" || i+1 >= len(args) {
				continue
			}
			if args[i+1] != f.baseSHA {
				t.Fatalf("ocr --from = %q, want the resolved base SHA %q (branch name breaks on shallow clones)", args[i+1], f.baseSHA)
			}
			return
		}
	}
	t.Fatalf("ocr was not invoked with --from: %s", data)
}

// --- cases -----------------------------------------------------------------

func testE2ESuccess(t *testing.T) {
	f := newE2EFixture(t)
	sse := f.openSSE(t)
	runID := f.enqueue(t, false)

	run := f.loadRun(t, runID)
	if run.Status != store.RunQueued {
		t.Fatalf("new run status = %q, want %q", run.Status, store.RunQueued)
	}
	if run.Trigger != "webhook" {
		t.Errorf("trigger = %q, want webhook", run.Trigger)
	}
	if run.BaseSHA != "" {
		t.Errorf("base_sha = %q, want empty on the first run", run.BaseSHA)
	}
	if created := f.gh.createdChecks(); len(created) != 1 || created[0] != f.headSHA {
		t.Errorf("created checks = %v, want one for %q", created, f.headSHA)
	}

	f.events.reset()
	f.execute(t, runID)

	run = f.loadRun(t, runID)
	assertE2ERun(t, run, store.RunSucceeded, "")
	if run.Summary == "" {
		t.Error("run summary is empty")
	}
	if run.CloneTokenEnc != "" {
		t.Error("finished run kept its clone token")
	}

	findings := f.findings(t)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
	}
	got := findings[0]
	if got.Status != store.FindingOpen || got.Severity != "major" {
		t.Errorf("finding = %+v, want open major", got)
	}
	if got.LastSeenRunID != runID {
		t.Errorf("last_seen_run_id = %d, want %d", got.LastSeenRunID, runID)
	}
	if got.GithubCommentID == 0 || got.GithubCommentNodeID == "" {
		t.Errorf("finding was not published inline: %+v", got)
	}
	assertE2EPRStatus(t, f, store.StatusChangesRequested)

	body := assertE2ESummary(t, f, "Revisión #1", f.headSHA[:7], "major 1", "2/0",
		"Revisión completada: 2 archivos revisados, 1 hallazgo",
		"https://qa.example/prs/"+strconv.FormatUint(uint64(f.pr.ID), 10))
	if strings.Contains(body, "Revisión no concluyente") {
		t.Errorf("successful summary carries the inconclusive warning: %q", body)
	}

	reviews := f.gh.reviewList()
	if len(reviews) != 1 || len(reviews[0].Comments) != 1 {
		t.Fatalf("inline reviews = %+v, want one with one comment", reviews)
	}
	comment := reviews[0].Comments[0]
	if reviews[0].Event != "COMMENT" {
		t.Errorf("review event = %q, want COMMENT", reviews[0].Event)
	}
	if comment.Path != "auth.go" || comment.Line != 3 {
		t.Errorf("inline comment at %s:%d, want auth.go:3", comment.Path, comment.Line)
	}
	if !strings.Contains(comment.Body, "Comparación insegura") {
		t.Errorf("inline comment body = %q", comment.Body)
	}

	check := assertE2ECheck(t, f, "failure")
	if len(check.Output.Annotations) != 1 || check.Output.Annotations[0].Path != "auth.go" {
		t.Errorf("check annotations = %+v, want one on auth.go", check.Output.Annotations)
	}

	f.assertShallowBase(t)
	if got := f.ocrCalls(t); got != 1 {
		t.Errorf("ocr invocations = %d, want 1", got)
	}
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("opencode invocations = %d, want 0", got)
	}

	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(runID), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)
	sse.waitFor(t, "run.updated", runID)
	sse.waitFor(t, "pr.updated", f.pr.ID)
}

// testE2EMixedCoverage is the #135 end-to-end regression: the diff inventory
// sees a second changed file that OCR excludes internally, so the run cannot
// claim completeness even though the engine returned complete.
func testE2EMixedCoverage(t *testing.T) {
	f := newE2EFixture(t)
	head := f.push(t, "notes.md")
	f.setHead(t, head)
	f.setOCR(t, `{
	  "status": "complete",
	  "summary": {"files_reviewed": 1, "comments": 0, "total_tokens": 100,
	    "input_tokens": 80, "output_tokens": 20},
	  "comments": [],
	  "manifest": {
	    "schema_version": "ocr.run-manifest/v1", "terminal_state": "complete",
	    "coverage": {
	      "selected": [{"path": "auth.go"}, {"path": "notes.md"}],
	      "completed": [{"path": "auth.go"}],
	      "reused": [], "failed": [], "waived": [],
	      "excluded": [{"path": "notes.md", "exclude_reason": "unsupported_ext"}]
	    }
	  }
	}`)

	seed := store.Finding{
		ID: "previous-notes", PullRequestID: f.pr.ID, Fingerprint: "previous-notes-fp",
		Path: "notes.md", StartLine: 1, EndLine: 1, Severity: "major",
		Status: store.FindingOpen, GithubCommentID: 77, GithubCommentNodeID: "NC_notes",
		FirstSeenRunID: 1, LastSeenRunID: 1,
	}
	mustCreateRunTest(t, f.s, f.ctx, &seed)

	runID := f.enqueue(t, false)
	f.events.reset()
	f.execute(t, runID)

	run := f.loadRun(t, runID)
	assertE2ERun(t, run, store.RunInconclusive, "coverage_incomplete")
	assertE2EPRStatus(t, f, store.StatusInconclusive)
	check := assertE2ECheck(t, f, "action_required")
	if !strings.Contains(check.Output.Text, "notes.md") {
		t.Errorf("check output %q does not list the pending file", check.Output.Text)
	}
	body := assertE2ESummary(t, f,
		"Archivos pendientes de revisión", "`notes.md`", "unsupported_ext")
	if strings.Contains(body, "Revisión completada") {
		t.Errorf("summary claims completion with pending coverage: %q", body)
	}

	var coverage struct {
		Files []coverageFile `json:"files"`
	}
	if err := json.Unmarshal(run.Coverage, &coverage); err != nil {
		t.Fatalf("decode coverage: %v", err)
	}
	found := false
	for _, file := range coverage.Files {
		if file.Path == "notes.md" {
			found = true
			if file.Status != coveragePending || file.Reason != "unsupported_ext" {
				t.Errorf("notes.md coverage = %+v, want pending/unsupported_ext", file)
			}
		}
	}
	if !found {
		t.Errorf("coverage files = %+v, want the pending notes.md entry", coverage.Files)
	}

	var reloaded store.Finding
	if err := f.s.DB.WithContext(f.ctx).Where("id = ?", seed.ID).Take(&reloaded).Error; err != nil {
		t.Fatalf("reload seeded finding: %v", err)
	}
	if reloaded.Status != store.FindingOpen || reloaded.LastSeenRunID != seed.LastSeenRunID {
		t.Errorf("seeded finding = %+v, want it open and untouched", reloaded)
	}
}

func testE2ETimeout(t *testing.T) {
	f := newE2EFixture(t)
	// The OCR double sleeps far longer than the deadline, so the timeout always
	// fires inside the review instead of racing the local clone and fetch.
	f.runner.Cfg.Timeout = 5 * time.Second
	t.Setenv("RUN_FAKE_OCR_SLEEP", "30")

	seed := store.Finding{
		ID: "seed-finding", PullRequestID: f.pr.ID, Fingerprint: "seed-fp",
		Path: "main.go", StartLine: 3, EndLine: 3, Severity: "major",
		Status: store.FindingOpen, GithubCommentID: 77, GithubCommentNodeID: "NC_seed",
		FirstSeenRunID: 1, LastSeenRunID: 1,
	}
	mustCreateRunTest(t, f.s, f.ctx, &seed)

	runID := f.enqueue(t, false)
	f.events.reset()
	f.execute(t, runID)

	if got := f.ocrCalls(t); got != 1 {
		t.Fatalf("ocr invocations = %d, want the review to start before the deadline", got)
	}

	run := f.loadRun(t, runID)
	assertE2ERun(t, run, store.RunInconclusive, "timeout")
	if !strings.Contains(run.ErrorMessage, "deadline exceeded") {
		t.Errorf("error_message = %q, want the agent deadline", run.ErrorMessage)
	}
	assertE2EPRStatus(t, f, store.StatusInconclusive)
	assertE2ESummary(t, f, "Revisión no concluyente", "deadline exceeded")
	check := assertE2ECheck(t, f, "action_required")
	if len(check.Output.Annotations) != 1 || check.Output.Annotations[0].Path != "main.go" {
		t.Errorf("check annotations = %+v, want the open finding", check.Output.Annotations)
	}
	if reviews := f.gh.reviewList(); len(reviews) != 0 {
		t.Errorf("inline reviews = %d, want none for an already published finding", len(reviews))
	}
	if got := f.gh.resolveCount(); got != 0 {
		t.Errorf("resolved %d threads, want 0 on an inconclusive run", got)
	}
	var reloaded store.Finding
	if err := f.s.DB.WithContext(f.ctx).Where("id = ?", seed.ID).Take(&reloaded).Error; err != nil {
		t.Fatalf("reload seeded finding: %v", err)
	}
	if reloaded.Status != store.FindingOpen || reloaded.LastSeenRunID != seed.LastSeenRunID {
		t.Errorf("seeded finding = %+v, want it untouched", reloaded)
	}

	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(runID), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)
}

func testE2EFailedThenRetry(t *testing.T) {
	f := newE2EFixture(t)
	runID := f.enqueue(t, false)

	if err := f.s.DB.WithContext(f.ctx).Model(&store.LLMProfile{}).
		Where("id = ?", f.policy.LLMProfileID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable profile: %v", err)
	}
	f.events.reset()
	f.execute(t, runID)

	run := f.loadRun(t, runID)
	assertE2ERun(t, run, store.RunFailed, "config")
	assertE2EPRStatus(t, f, store.StatusInconclusive)
	if bodies := f.gh.summaryBodies(); len(bodies) != 0 {
		t.Errorf("summary comments = %d, want none for a failed run", len(bodies))
	}
	assertE2ECheck(t, f, "action_required")
	if findings := f.findings(t); len(findings) != 0 {
		t.Errorf("findings = %d, want none from a failed run", len(findings))
	}
	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(runID), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)

	if err := f.s.DB.WithContext(f.ctx).Model(&store.LLMProfile{}).
		Where("id = ?", f.policy.LLMProfileID).Update("enabled", true).Error; err != nil {
		t.Fatalf("re-enable profile: %v", err)
	}
	retryID := f.enqueue(t, true)
	retry := f.loadRun(t, retryID)
	if retry.Trigger != "retry" {
		t.Errorf("retry trigger = %q, want retry", retry.Trigger)
	}
	if retry.HeadSHA != f.headSHA {
		t.Errorf("retry head = %q, want %q", retry.HeadSHA, f.headSHA)
	}
	if created := f.gh.createdChecks(); len(created) != 1 {
		t.Errorf("check runs created = %d, want the retry to reopen the existing one", len(created))
	}

	f.events.reset()
	f.execute(t, retryID)

	assertE2ERun(t, f.loadRun(t, retryID), store.RunSucceeded, "")
	assertE2EPRStatus(t, f, store.StatusChangesRequested)
	assertE2ESummary(t, f, "Revisión #2", "major 1")
	patches := f.gh.checkPatchList()
	if len(patches) != 3 {
		t.Fatalf("check patches = %d, want action_required, in_progress and success", len(patches))
	}
	if patches[1].Status != "in_progress" {
		t.Errorf("retry patch status = %q, want in_progress", patches[1].Status)
	}
	if patches[2].Conclusion != "failure" {
		t.Errorf("retry conclusion = %q, want failure (the retried finding is blocking)", patches[2].Conclusion)
	}
	if got := f.gh.commentPatchCount(); got != 0 {
		t.Errorf("summary edits = %d, want the retry to create the summary the failed run never published", got)
	}
	if kept := f.loadRun(t, runID); kept.Status != store.RunFailed {
		t.Errorf("failed run status = %q, want it preserved", kept.Status)
	}
	if findings := f.findings(t); len(findings) != 1 {
		t.Errorf("findings = %d, want the finding of the successful retry", len(findings))
	}

	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(retryID), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)
}

func testE2ESuperseded(t *testing.T) {
	f := newE2EFixture(t)
	runID := f.enqueue(t, false)

	head2 := f.push(t, "fix.go")
	f.setHead(t, head2)

	f.events.reset()
	f.execute(t, runID)

	run := f.loadRun(t, runID)
	assertE2ERun(t, run, store.RunSuperseded, "head_moved")
	if !strings.Contains(run.ErrorMessage, head2) {
		t.Errorf("error_message = %q, want it to name the new head %q", run.ErrorMessage, head2)
	}
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("agent calls = %d, want 0 (stale head must not be reviewed)", got)
	}
	assertE2EPRStatus(t, f, store.StatusPending)
	if bodies := f.gh.summaryBodies(); len(bodies) != 0 {
		t.Errorf("summary comments = %d, want none for a superseded run", len(bodies))
	}
	if check := assertE2ECheck(t, f, "cancelled"); check.Output.Annotations != nil {
		t.Errorf("cancelled check carried annotations: %+v", check.Output.Annotations)
	}

	// The claim announces the PR before the head check; the superseded ending
	// must not announce it a second time.
	f.events.wait(t,
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(runID), 10)},
	)
	f.events.absent(t, events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)})

	f.setOCR(t, runTestNativeFindingCovering("auth.go", "main.go", "fix.go"))
	retryID := f.enqueue(t, false)
	if got := f.loadRun(t, retryID); got.HeadSHA != head2 {
		t.Fatalf("new run head = %q, want %q", got.HeadSHA, head2)
	}
	f.events.reset()
	f.execute(t, retryID)

	assertE2ERun(t, f.loadRun(t, retryID), store.RunSucceeded, "")
	assertE2EPRStatus(t, f, store.StatusChangesRequested)
	assertE2ESummary(t, f, "Revisión #2", head2[:7])
	if created := f.gh.createdChecks(); len(created) != 2 || created[1] != head2 {
		t.Errorf("created checks = %v, want a second one for %q", created, head2)
	}
	assertE2ECheck(t, f, "failure")
	if got := f.agentCalls(t); got != 0 {
		t.Errorf("opencode calls = %d, want 0", got)
	}
	if got := f.ocrCalls(t); got != 1 {
		t.Errorf("ocr calls = %d, want only the new head reviewed", got)
	}

	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(retryID), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)
}

func testE2EFindingsLifecycle(t *testing.T) {
	f := newE2EFixture(t)
	ocrBin := f.ocrBin

	run1 := f.enqueue(t, false)
	f.events.reset()
	f.execute(t, run1)
	assertE2ERun(t, f.loadRun(t, run1), store.RunSucceeded, "")

	findings := f.findings(t)
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	finding := findings[0]
	if finding.Status != store.FindingOpen || finding.GithubCommentID == 0 {
		t.Fatalf("finding = %+v, want open and published", finding)
	}
	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(run1), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)

	// An inconclusive run publishes but never resolves.
	head2 := f.push(t, "chore.go")
	f.setHead(t, head2)
	f.runner.Cfg.OCRBin = "/bin/false"
	run2 := f.enqueue(t, false)
	f.events.reset()
	f.execute(t, run2)
	assertE2ERun(t, f.loadRun(t, run2), store.RunInconclusive, "ocr_error")
	assertE2EPRStatus(t, f, store.StatusInconclusive)
	assertE2ECheck(t, f, "action_required")
	if got := f.gh.resolveCount(); got != 0 {
		t.Errorf("resolved threads = %d, want 0 after an inconclusive run", got)
	}
	if got := f.reloadFinding(t, finding.ID); got.Status != store.FindingOpen || got.LastSeenRunID != run1 {
		t.Errorf("finding after inconclusive run = %+v, want it open and last seen by run %d", got, run1)
	}
	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(run2), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)

	// A failed run must not resolve either.
	head3 := f.push(t, "chore2.go")
	f.setHead(t, head3)
	run3 := f.enqueue(t, false)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.LLMProfile{}).
		Where("id = ?", f.policy.LLMProfileID).Update("enabled", false).Error; err != nil {
		t.Fatalf("disable profile: %v", err)
	}
	f.events.reset()
	f.execute(t, run3)
	assertE2ERun(t, f.loadRun(t, run3), store.RunFailed, "config")
	assertE2EPRStatus(t, f, store.StatusInconclusive)
	assertE2ECheck(t, f, "action_required")
	if got := f.gh.resolveCount(); got != 0 {
		t.Errorf("resolved threads = %d, want 0 after a failed run", got)
	}
	if got := f.reloadFinding(t, finding.ID); got.Status != store.FindingOpen {
		t.Errorf("finding after failed run = %+v, want it open", got)
	}
	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(run3), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.LLMProfile{}).
		Where("id = ?", f.policy.LLMProfileID).Update("enabled", true).Error; err != nil {
		t.Fatalf("re-enable profile: %v", err)
	}

	// A later successful run with no findings resolves it and closes the thread.
	head4 := f.push(t, "fix2.go")
	f.setHead(t, head4)
	f.runner.Cfg.OCRBin = ocrBin
	f.setOCR(t, runTestNativeCovering("auth.go", "main.go", "chore.go", "chore2.go", "fix2.go"))
	run4 := f.enqueue(t, false)
	f.events.reset()
	f.execute(t, run4)
	assertE2ERun(t, f.loadRun(t, run4), store.RunSucceeded, "")
	assertE2EPRStatus(t, f, store.StatusReadyForHuman)
	assertE2ESummary(t, f, "Resueltos desde la revisión anterior:** 1")
	if got := f.gh.commentPatchCount(); got != 2 {
		t.Errorf("summary edits = %d, want the later runs to edit the same comment", got)
	}
	assertE2ECheck(t, f, "success")

	resolved := f.reloadFinding(t, finding.ID)
	if resolved.Status != store.FindingResolved {
		t.Errorf("finding status = %q, want resolved", resolved.Status)
	}
	if resolved.LastSeenRunID != run4 {
		t.Errorf("finding last_seen_run_id = %d, want %d", resolved.LastSeenRunID, run4)
	}
	if resolved.GithubThreadID != "THREAD_1" {
		t.Errorf("finding thread = %q, want THREAD_1", resolved.GithubThreadID)
	}
	if got := f.gh.resolveCount(); got != 1 {
		t.Errorf("resolved threads = %d, want 1", got)
	}
	if got := f.gh.replyCount(); got != 1 {
		t.Errorf("thread replies = %d, want 1", got)
	}
	f.events.wait(t,
		events.Event{Type: "run.updated", ID: strconv.FormatUint(uint64(run4), 10)},
		events.Event{Type: "pr.updated", ID: strconv.FormatUint(uint64(f.pr.ID), 10)},
	)
}

func testE2EAutoTriagePublishesCuratedSummary(t *testing.T) {
	f := newE2EFixture(t)
	// The second pass reserves a percentage of the run budget; 15 % of the
	// default 30 s is below the one-minute minimum, so the run needs a longer
	// deadline to verify at all.
	f.runner.Cfg.Timeout = 10 * time.Minute
	f.setOCR(t, runTestNativeTwoFindings())
	f.setAutoTriage(t, store.AutoTriageApply, true, nil)

	minorID := findingIDFor(f.pr.ID, "auth.go", "security", "Comparación insegura", 3, 3)
	nitID := findingIDFor(f.pr.ID, "auth.go", "style", "Nombre confuso", 5, 5)
	f.setScan(t, runTestScanAnswer(t, map[string]string{
		minorID: "REFUTE",
		nitID:   "CONFIRM",
	}))

	runID := f.enqueue(t, false)
	f.events.reset()
	f.execute(t, runID)

	run := f.loadRun(t, runID)
	assertE2ERun(t, run, store.RunSucceeded, "")
	if run.VerifySessionID != "verify-session" {
		t.Errorf("verify_session_id = %q, want the verification session", run.VerifySessionID)
	}

	byID := map[string]store.Finding{}
	for _, finding := range f.findings(t) {
		byID[finding.ID] = finding
	}
	refuted, ok := byID[minorID]
	if !ok {
		t.Fatalf("findings = %+v, want %s", byID, minorID)
	}
	if refuted.Status != store.FindingWithdrawn ||
		refuted.TriageSource != store.TriageSourceVerifier ||
		refuted.TriageVerdict != store.VerdictFalsePositive {
		t.Errorf("refuted finding = %+v, want withdrawn by the verifier", refuted)
	}
	var evidence Verification
	if err := json.Unmarshal(refuted.Verification, &evidence); err != nil {
		t.Fatalf("decode verification of %s: %v (%s)", minorID, err, refuted.Verification)
	}
	if evidence.Verdict != VerdictRefute || evidence.Evidence != "package main" ||
		evidence.HeadSHA != f.headSHA || evidence.SessionID != "verify-session" {
		t.Errorf("verification = %+v, want the refutation with the cited evidence", evidence)
	}
	if refuted.GithubCommentID != 0 {
		t.Errorf("withdrawn finding got an inline comment: %+v", refuted)
	}

	confirmed, ok := byID[nitID]
	if !ok {
		t.Fatalf("findings = %+v, want %s", byID, nitID)
	}
	if confirmed.Status != store.FindingOpen ||
		confirmed.TriageSource != store.TriageSourceVerifier ||
		confirmed.TriageVerdict != store.VerdictConfirmed {
		t.Errorf("confirmed finding = %+v, want open and confirmed by the verifier", confirmed)
	}
	if confirmed.GithubCommentID == 0 {
		t.Errorf("confirmed finding was not published inline: %+v", confirmed)
	}

	body := assertE2ESummary(t, f,
		"**Verificación automática:** 1 validados · 0 para tu criterio · 1 descartados",
		"Validado por la verificación:**",
		"Nombre confuso",
		"1 descartados automáticamente",
	)
	if strings.Contains(body, "Comparación insegura") {
		t.Errorf("summary still lists the withdrawn finding: %q", body)
	}

	reviews := f.gh.reviewList()
	if len(reviews) != 1 || len(reviews[0].Comments) != 1 {
		t.Fatalf("inline reviews = %+v, want one with only the confirmed finding", reviews)
	}
	comment := reviews[0].Comments[0]
	if !strings.Contains(comment.Body, "Nombre confuso") {
		t.Errorf("inline comment body = %q, want the confirmed finding", comment.Body)
	}
	if comment.Path != "auth.go" || comment.Line != 5 {
		t.Errorf("inline comment at %s:%d, want the confirmed finding's added line",
			comment.Path, comment.Line)
	}
}

func (f *e2eFixture) reloadFinding(t *testing.T, id string) store.Finding {
	t.Helper()
	var got store.Finding
	if err := f.s.DB.WithContext(f.ctx).Where("id = ?", id).Take(&got).Error; err != nil {
		t.Fatalf("reload finding %s: %v", id, err)
	}
	return got
}
