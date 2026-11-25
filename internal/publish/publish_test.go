package publish_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	"github.com/jeriveromartinez/qa-control-center/internal/events"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/publish"
	"github.com/jeriveromartinez/qa-control-center/internal/review"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

const defaultPRFiles = `[{"filename":"src/a.go","status":"modified",` +
	`"patch":"@@ -1,3 +1,4 @@\n ctx\n-old\n+new\n+added\n tail\n"}]`

type fakeComment struct {
	ID     int64  `json:"id"`
	NodeID string `json:"node_id"`
	Body   string `json:"body"`
}

type reviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

type fakeReview struct {
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []reviewComment `json:"comments"`
}

type reviewRequestCall struct {
	Reviewers     []string `json:"reviewers"`
	TeamReviewers []string `json:"team_reviewers"`
}

type checkPatch struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Output     struct {
		Title       string `json:"title"`
		Summary     string `json:"summary"`
		Text        string `json:"text"`
		Annotations []struct {
			Path            string `json:"path"`
			StartLine       int    `json:"start_line"`
			EndLine         int    `json:"end_line"`
			AnnotationLevel string `json:"annotation_level"`
			Title           string `json:"title"`
			Message         string `json:"message"`
		} `json:"annotations"`
	} `json:"output"`
}

type fakeGitHub struct {
	t *testing.T

	mu                 sync.Mutex
	files              string
	comments           []fakeComment
	commentPatches     map[int64]string
	reviews            []fakeReview
	reviewIDs          []int64
	replies            []string
	threadCommentBody  string
	markerOnPage       int
	checkPatches       []checkPatch
	checkRuns          []map[string]any
	commitStatuses     []map[string]any
	checksFailure      bool
	gqlQueries         []string
	nextCommentID      int64
	nextReviewID       int64
	omitReviewComments bool
	// reviewCommentsWithoutLine mimics GitHub returning a just-created review's
	// comments with no line fields, only their position.
	reviewCommentsWithoutLine bool
	// reviewCreateStatus makes POST /reviews fail with that status, as GitHub
	// does when a comment cannot be anchored to the diff.
	reviewCreateStatus     int
	failResolveOnce        bool
	failCheckOnce          bool
	failReviewCommentsOnce bool
	reviewRequests         []reviewRequestCall
	reviewRequestAttempts  int
	reviewRequestStatus    int
	reviewRequestFailures  int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	return &fakeGitHub{t: t, files: defaultPRFiles, nextCommentID: 1000, nextReviewID: 899}
}

func (g *fakeGitHub) failNextResolve() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failResolveOnce = true
}

// setChecks programs the CI endpoints the publisher reads when the team gates
// reviews on green CI. Each conclusion becomes a check run named job-N.
func (g *fakeGitHub) setChecks(conclusions ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	runs := make([]map[string]any, 0, len(conclusions))
	for i, conclusion := range conclusions {
		runs = append(runs, map[string]any{
			"id": i + 1, "name": fmt.Sprintf("job-%d", i+1),
			"status": "completed", "conclusion": conclusion,
			"app": map[string]any{"id": 7},
		})
	}
	g.checkRuns = runs
}

// setCheckTasks programs named check runs with an explicit App origin.
func (g *fakeGitHub) setCheckTasks(checks ...store.CheckObservation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	runs := make([]map[string]any, 0, len(checks))
	for i, check := range checks {
		status, conclusion := "completed", "success"
		switch check.State {
		case store.CheckFailed:
			conclusion = "failure"
		case store.CheckPending:
			status, conclusion = "in_progress", ""
		}
		run := map[string]any{
			"id": i + 1, "name": check.Name, "status": status, "conclusion": conclusion,
		}
		if strings.HasPrefix(check.Origin, "app:") {
			var appID int64
			_, _ = fmt.Sscanf(check.Origin, "app:%d", &appID)
			run["app"] = map[string]any{"id": appID}
		}
		runs = append(runs, run)
	}
	g.checkRuns = runs
}

func (g *fakeGitHub) failChecks(fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checksFailure = fail
}

func (g *fakeGitHub) failNextCheck() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failCheckOnce = true
}

func (g *fakeGitHub) failNextReviewComments() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failReviewCommentsOnce = true
}

func (g *fakeGitHub) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/app/installations/9/access_tokens" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"token":"ghs_inst","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return
	}

	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()

	writeJSON := func(v any, status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}

	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commits/") &&
		strings.HasSuffix(r.URL.Path, "/check-runs"):
		if g.checksFailure {
			http.Error(w, "checks unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(map[string]any{"total_count": len(g.checkRuns), "check_runs": g.checkRuns}, http.StatusOK)

	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commits/") &&
		strings.HasSuffix(r.URL.Path, "/status"):
		if g.checksFailure {
			http.Error(w, "statuses unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(map[string]any{
			"state": "success", "total_count": len(g.commitStatuses), "statuses": g.commitStatuses,
		}, http.StatusOK)

	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/issues/7/comments":
		comments := g.comments
		if comments == nil {
			comments = []fakeComment{}
		}
		writeJSON(comments, http.StatusOK)

	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/repo/issues/7/comments":
		var in struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		g.nextCommentID++
		c := fakeComment{ID: g.nextCommentID, NodeID: fmt.Sprintf("IC_%d", g.nextCommentID), Body: in.Body}
		g.comments = append(g.comments, c)
		writeJSON(c, http.StatusCreated)

	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/repo/issues/comments/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/acme/repo/issues/comments/"), 10, 64)
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
		for i := range g.comments {
			if g.comments[i].ID == id {
				g.comments[i].Body = in.Body
			}
		}
		writeJSON(fakeComment{ID: id, Body: in.Body}, http.StatusOK)

	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/pulls/7/files":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, g.files)

	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/pulls/7/reviews":
		var out []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		for i, review := range g.reviews {
			out = append(out, struct {
				ID   int64  `json:"id"`
				Body string `json:"body"`
			}{ID: g.reviewIDs[i], Body: review.Body})
		}
		if out == nil {
			out = []struct {
				ID   int64  `json:"id"`
				Body string `json:"body"`
			}{}
		}
		writeJSON(out, http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/repo/pulls/7/requested_reviewers":
		g.reviewRequestAttempts++
		// A negative failure count fails forever; a positive one counts down.
		if g.reviewRequestStatus != 0 && g.reviewRequestFailures != 0 {
			if g.reviewRequestFailures > 0 {
				g.reviewRequestFailures--
			}
			http.Error(w, "review request rejected", g.reviewRequestStatus)
			return
		}
		var in reviewRequestCall
		_ = json.Unmarshal(body, &in)
		g.reviewRequests = append(g.reviewRequests, in)
		writeJSON(map[string]any{"number": 7}, http.StatusCreated)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/repo/pulls/7/comments":
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			page, _ = strconv.Atoi(raw)
		}
		comments := []map[string]any{}
		if page == 1 {
			for i := 0; i < 99; i++ {
				comments = append(comments, map[string]any{"id": i + 1, "body": "chatter"})
			}
			body := ""
			if g.markerOnPage <= 1 {
				body = g.threadCommentBody
			}
			comments = append(comments, map[string]any{"id": 100, "body": body})
		} else if page == 2 && g.markerOnPage == 2 {
			comments = append(comments, map[string]any{"id": 101, "body": g.threadCommentBody})
		}
		writeJSON(comments, http.StatusOK)

	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/repo/pulls/7/reviews":
		if g.reviewCreateStatus != 0 {
			http.Error(w, "review rejected", g.reviewCreateStatus)
			return
		}
		var review fakeReview
		_ = json.Unmarshal(body, &review)
		g.nextReviewID++
		g.reviews = append(g.reviews, review)
		g.reviewIDs = append(g.reviewIDs, g.nextReviewID)
		writeJSON(struct {
			ID int64 `json:"id"`
		}{ID: g.nextReviewID}, http.StatusOK)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/repo/pulls/7/reviews/"):
		if g.failReviewCommentsOnce {
			g.failReviewCommentsOnce = false
			http.Error(w, "review comments temporarily unavailable", http.StatusInternalServerError)
			return
		}
		var posted []struct {
			ID     int64  `json:"id"`
			NodeID string `json:"node_id"`
			Path   string `json:"path"`
			Line   int    `json:"line"`
		}
		if g.omitReviewComments {
			writeJSON(posted, http.StatusOK)
			return
		}
		if len(g.reviews) > 0 {
			for i, c := range g.reviews[len(g.reviews)-1].Comments {
				line := c.Line
				if g.reviewCommentsWithoutLine {
					line = 0
				}
				posted = append(posted, struct {
					ID     int64  `json:"id"`
					NodeID string `json:"node_id"`
					Path   string `json:"path"`
					Line   int    `json:"line"`
				}{ID: int64(2000 + i), NodeID: fmt.Sprintf("NC_%d", 2000+i), Path: c.Path, Line: line})
			}
		}
		writeJSON(posted, http.StatusOK)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/replies"):
		var in struct {
			Body string `json:"body"`
		}
		_ = json.Unmarshal(body, &in)
		g.replies = append(g.replies, in.Body)
		writeJSON(map[string]any{"id": 7000}, http.StatusCreated)

	case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/repo/check-runs/555":
		if g.failCheckOnce {
			g.failCheckOnce = false
			http.Error(w, "check temporarily unavailable", http.StatusInternalServerError)
			return
		}
		var patch checkPatch
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, "bad check payload", http.StatusBadRequest)
			return
		}
		g.checkPatches = append(g.checkPatches, patch)
		writeJSON(map[string]any{"id": 555}, http.StatusOK)

	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		var in struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &in)
		g.gqlQueries = append(g.gqlQueries, in.Query)
		switch {
		case strings.Contains(in.Query, "unresolveReviewThread"):
			writeJSON(map[string]any{"data": map[string]any{"unresolveReviewThread": map[string]any{
				"thread": map[string]any{"id": "THREAD_1", "isResolved": false},
			}}}, http.StatusOK)
			return
		case strings.Contains(in.Query, "resolveReviewThread"):
			if g.failResolveOnce {
				g.failResolveOnce = false
				http.Error(w, "resolution temporarily unavailable", http.StatusInternalServerError)
				return
			}
			writeJSON(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{
				"thread": map[string]any{"id": "THREAD_1", "isResolved": true},
			}}}, http.StatusOK)
			return
		case strings.Contains(in.Query, "reviewThreads"):
			writeJSON(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
				"reviewThreads": map[string]any{
					"nodes": []any{map[string]any{
						"id":         "THREAD_1",
						"isResolved": false,
						"comments":   map[string]any{"nodes": []any{map[string]any{"id": "NC_resolve"}}},
					}},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				},
			}}}}, http.StatusOK)
			return
		}
		http.Error(w, "unexpected graphql query", http.StatusBadRequest)

	default:
		g.t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

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

func (p *recordingPublisher) all() []events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]events.Event(nil), p.events...)
}

func randomName(prefix string) string {
	raw := make([]byte, 6)
	_, _ = rand.Read(raw)
	return prefix + "-" + hex.EncodeToString(raw)
}

func seedPublishContext(t *testing.T, st *store.Store, status, threshold string) (store.ReviewRun, store.PullRequest) {
	t.Helper()

	ctx := context.Background()
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	apiKey, err := box.Seal([]byte("sk-live-abcdefghijklmnopqrstuvwxyz"))
	if err != nil {
		t.Fatalf("seal api key: %v", err)
	}
	cloneToken, err := box.Seal([]byte("ghp_clonetokentokentokentokentoken"))
	if err != nil {
		t.Fatalf("seal clone token: %v", err)
	}

	team := store.Team{Name: randomName("team"), Slug: randomName("team")}
	if err := st.DB.WithContext(ctx).Create(&team).Error; err != nil {
		t.Fatalf("create team: %v", err)
	}
	profile := store.LLMProfile{
		Name: randomName("profile"), Provider: "openai", Model: "gpt-5",
		Mode: "api_key", APIKeyEnc: apiKey, Enabled: true, HealthStatus: store.HealthUnknown,
	}
	if err := st.DB.WithContext(ctx).Create(&profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}
	policy := store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID, BlockingThreshold: threshold,
		MaxFilesPerPass: 40, MaxDiffLinesPerPass: 3000, MaxPasses: 4,
		ExcludeGlobs: datatypes.JSON([]byte(`[]`)),
	}
	if err := st.DB.WithContext(ctx).Create(&policy).Error; err != nil {
		t.Fatalf("create policy: %v", err)
	}
	inst := store.Installation{ID: 9, AccountLogin: "acme", AccountType: "Organization"}
	if err := st.DB.WithContext(ctx).Create(&inst).Error; err != nil {
		t.Fatalf("create installation: %v", err)
	}
	repo := store.Repository{
		ID: 42, InstallationID: inst.ID, FullName: "acme/repo", Enabled: true, TeamID: &team.ID,
	}
	if err := st.DB.WithContext(ctx).Create(&repo).Error; err != nil {
		t.Fatalf("create repository: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: repo.ID, Number: 7, HeadSHA: "abcdef1234567890",
		Title: "Cambios", HeadRef: "feature/qa", QAStatus: store.StatusReviewing,
		GitHubUpdatedAt: time.Now().UTC(),
	}
	if err := st.DB.WithContext(ctx).Create(&pr).Error; err != nil {
		t.Fatalf("create pull request: %v", err)
	}
	run := store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: pr.HeadSHA,
		Status: status, Summary: "Revisión completada sin problemas",
		Coverage:      datatypes.JSON([]byte(`{"reviewed_files":3,"skipped_files":1}`)),
		CloneTokenEnc: cloneToken,
	}
	if err := st.DB.WithContext(ctx).Create(&run).Error; err != nil {
		t.Fatalf("create review run: %v", err)
	}
	if _, err := st.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactCheck, HeadSHA: run.HeadSHA,
		ExternalID: "555", URL: "https://github.com/acme/repo/runs/555",
	}); err != nil {
		t.Fatalf("seed check artifact: %v", err)
	}
	return run, pr
}

func newPublishWorker(t *testing.T, st *store.Store, gh *fakeGitHub) (*publish.Worker, *recordingPublisher) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(gh.handler))
	t.Cleanup(srv.Close)
	pub := &recordingPublisher{}
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &publish.Worker{
		Store:     st,
		Apps:      fakeAppProvider{app: &ghapp.App{Creds: ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)}, APIURL: srv.URL}},
		Box:       box,
		Events:    pub,
		PublicURL: "https://qa.example",
	}, pub
}

func seedFinding(t *testing.T, st *store.Store, prID, runID uint, id string, line int, severity, status string) store.Finding {
	t.Helper()
	f := store.Finding{
		ID: id, PullRequestID: prID, Fingerprint: "fp-" + id,
		Path: "src/a.go", StartLine: line, EndLine: line,
		Severity: severity, Category: "bug", Title: "Título " + id,
		Body: "Cuerpo " + id, Status: status,
		FirstSeenRunID: runID, LastSeenRunID: runID,
	}
	saved, err := st.UpsertFinding(context.Background(), f)
	if err != nil {
		t.Fatalf("upsert finding %s: %v", id, err)
	}
	return saved
}

func summaryBodies(t *testing.T, gh *fakeGitHub) []string {
	t.Helper()
	gh.mu.Lock()
	defer gh.mu.Unlock()
	var bodies []string
	for _, c := range gh.comments {
		if strings.Contains(c.Body, "<!-- qa-control-center:summary -->") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

func TestPublishSummaryIsEditedNotDuplicated(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	gh := newFakeGitHub(t)
	// An earlier delivery already left the shared summary comment; publishing
	// must edit it and a redelivery must not touch it again.
	gh.comments = []fakeComment{{
		ID: 1, NodeID: "IC_1",
		Body: "<!-- qa-control-center:summary -->\nresumen anterior",
	}}
	w, _ := newPublishWorker(t, st, gh)

	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}

	bodies := summaryBodies(t, gh)
	gh.mu.Lock()
	posts, patches := len(gh.comments), len(gh.commentPatches)
	gh.mu.Unlock()
	if posts != 1 {
		t.Errorf("summary comments = %d, want 1", posts)
	}
	if patches != 1 {
		t.Errorf("summary PATCHes = %d, want 1", patches)
	}
	if len(bodies) != 1 {
		t.Fatalf("summary comments with marker = %d, want 1", len(bodies))
	}
	body := bodies[0]
	for _, want := range []string{"#1", "abcdef1", "blocker", "major", "minor", "nit", "3/1"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary %q does not contain %q", body, want)
		}
	}
	if !strings.Contains(body, "https://qa.example/prs/"+strconv.FormatUint(uint64(pr.ID), 10)) {
		t.Errorf("summary %q has no panel link", body)
	}
}

func TestPublishOnlyNewFindingsInline(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	old := seedFinding(t, st, pr.ID, run.ID, "old", 2, "major", store.FindingOpen)
	if err := st.DB.WithContext(ctx).Model(&store.Finding{}).Where("id = ?", old.ID).
		Update("github_comment_id", 123).Error; err != nil {
		t.Fatalf("mark old finding published: %v", err)
	}
	seedFinding(t, st, pr.ID, run.ID, "new", 3, "minor", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.reviews) != 1 {
		t.Fatalf("reviews = %d, want 1", len(gh.reviews))
	}
	review := gh.reviews[0]
	if review.Event != "COMMENT" {
		t.Errorf("review event = %q, want COMMENT", review.Event)
	}
	if len(review.Comments) != 1 {
		t.Fatalf("inline comments = %d, want 1", len(review.Comments))
	}
	if !strings.Contains(review.Comments[0].Body, "Título new") || strings.Contains(review.Comments[0].Body, "Título old") {
		t.Errorf("inline comment = %q, want only the new finding", review.Comments[0].Body)
	}

	saved, err := st.FindingsForPR(ctx, pr.ID)
	if err != nil {
		t.Fatalf("FindingsForPR() error = %v", err)
	}
	for _, f := range saved {
		if f.ID == "new" && f.GithubCommentID == 0 {
			t.Error("new finding was not persisted with a GitHub comment id")
		}
		if f.ID == "old" && f.GithubCommentID != 123 {
			t.Error("already published finding was modified")
		}
	}
}

func TestPublishSuggestionBlock(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	full := seedFinding(t, st, pr.ID, run.ID, "full", 2, "major", store.FindingOpen)
	full.Suggestion = "return err"
	full.EndLine = 3
	if _, err := st.UpsertFinding(ctx, full); err != nil {
		t.Fatalf("set suggestion: %v", err)
	}
	partial := seedFinding(t, st, pr.ID, run.ID, "partial", 2, "major", store.FindingOpen)
	partial.Suggestion = "return nil"
	partial.EndLine = 4
	if _, err := st.UpsertFinding(ctx, partial); err != nil {
		t.Fatalf("set partial suggestion: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.reviews) != 1 || len(gh.reviews[0].Comments) != 2 {
		t.Fatalf("inline comments = %+v, want two", gh.reviews)
	}
	bodies := map[string]string{}
	for _, c := range gh.reviews[0].Comments {
		if strings.Contains(c.Body, "Título full") {
			bodies["full"] = c.Body
		}
		if strings.Contains(c.Body, "Título partial") {
			bodies["partial"] = c.Body
		}
	}
	if !strings.Contains(bodies["full"], "```suggestion\nreturn err\n```") {
		t.Errorf("full suggestion body = %q", bodies["full"])
	}
	if strings.Contains(bodies["partial"], "```suggestion") || !strings.Contains(bodies["partial"], "return nil") {
		t.Errorf("partial suggestion body = %q, want plain text", bodies["partial"])
	}
}

func TestPublishResolvesThread(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	f := seedFinding(t, st, pr.ID, run.ID, "resolved", 2, "major", store.FindingResolved)
	f.GithubCommentID = 123
	f.GithubCommentNodeID = "NC_resolve"
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("mark finding as published: %v", err)
	}
	// UpsertFinding preserves GitHub coordinates; set them directly.
	if err := st.DB.WithContext(ctx).Model(&store.Finding{}).Where("id = ?", f.ID).Updates(map[string]any{
		"github_comment_id": 123, "github_comment_node_id": "NC_resolve",
	}).Error; err != nil {
		t.Fatalf("set finding coordinates: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	replies := append([]string(nil), gh.replies...)
	queries := append([]string(nil), gh.gqlQueries...)
	gh.mu.Unlock()
	if len(replies) != 1 {
		t.Errorf("thread replies = %d, want 1", len(replies))
	}
	var resolveQueries int
	for _, q := range queries {
		if strings.Contains(q, "resolveReviewThread") {
			resolveQueries++
		}
	}
	if resolveQueries != 1 {
		t.Errorf("resolveReviewThread mutations = %d, want 1", resolveQueries)
	}

	saved, err := st.FindingsForPR(ctx, pr.ID)
	if err != nil {
		t.Fatalf("FindingsForPR() error = %v", err)
	}
	for _, finding := range saved {
		if finding.GithubThreadID != "THREAD_1" {
			t.Errorf("finding thread id = %q, want THREAD_1", finding.GithubThreadID)
		}
	}
}

func TestPublishInconclusiveDoesNotResolve(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).Updates(map[string]any{
		"error_message": "cobertura parcial",
	}).Error; err != nil {
		t.Fatalf("set error message: %v", err)
	}
	f := seedFinding(t, st, pr.ID, run.ID, "resolved", 2, "major", store.FindingResolved)
	if err := st.DB.WithContext(ctx).Model(&store.Finding{}).Where("id = ?", f.ID).Updates(map[string]any{
		"github_comment_id": 123, "github_comment_node_id": "NC_resolve",
	}).Error; err != nil {
		t.Fatalf("set finding coordinates: %v", err)
	}
	gh := newFakeGitHub(t)
	w, pub := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	bodies := summaryBodies(t, gh)
	gh.mu.Lock()
	replies, queries := len(gh.replies), append([]string(nil), gh.gqlQueries...)
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if replies != 0 {
		t.Errorf("thread replies = %d, want 0 for inconclusive runs", replies)
	}
	for _, q := range queries {
		if strings.Contains(q, "resolveReviewThread") {
			t.Error("resolveReviewThread called for inconclusive run")
		}
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "cobertura parcial") {
		t.Errorf("summary = %q, want the inconclusive warning", bodies)
	}
	if len(patches) != 1 || patches[0].Conclusion != "action_required" {
		t.Errorf("check conclusion = %+v, want action_required", patches)
	}
	var savedPR store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&savedPR, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got := savedPR.QAStatus; got != store.StatusInconclusive {
		t.Errorf("qa_status = %q, want inconclusive", got)
	}
	_ = pub
}

// TestPublishMixedPRSurfacesPendingCoverage pins #135 on the publishing side:
// a required changed file left out of coverage must appear in the summary and
// check output, and the check must stay action_required.
func TestPublishMixedPRSurfacesPendingCoverage(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).Updates(map[string]any{
		"error_message": "cobertura incompleta: 1 archivo sin revisar",
		"coverage": datatypes.JSON([]byte(`{"reviewed_files":1,"skipped_files":1,"files":[
			{"path":"auth.go","status":"reviewed"},
			{"path":"notes.md","status":"pending","reason":"unsupported_ext"}]}`)),
	}).Error; err != nil {
		t.Fatalf("set coverage: %v", err)
	}
	seedFinding(t, st, pr.ID, run.ID, "notes-finding", 4, "major", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	for _, want := range []string{"Archivos pendientes de revisión", "`notes.md`", "unsupported_ext"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("summary %q does not contain %q", bodies[0], want)
		}
	}

	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if len(patches) == 0 || patches[0].Conclusion != "action_required" {
		t.Fatalf("check conclusion = %+v, want action_required", patches)
	}
	if !strings.Contains(patches[0].Output.Text, "notes.md") {
		t.Errorf("check output %q does not list the pending file", patches[0].Output.Text)
	}
}

// TestPublishSummaryNamesFilesNeedingHumanReview pins #147: binaries and
// submodules have no automatic analysis, so the summary names them instead of
// letting them pass silently.
func TestPublishSummaryNamesFilesNeedingHumanReview(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, _ := seedPublishContext(t, st, store.RunSucceeded, "major")
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).Updates(map[string]any{
		"coverage": datatypes.JSON([]byte(`{"reviewed_files":1,"skipped_files":0,"not_required_files":2,"files":[
			{"path":"auth.go","status":"reviewed","method":"ocr"},
			{"path":"logo.png","status":"binary","reason":"binary","human":true},
			{"path":"vendor/lib","status":"submodule","reason":"submodule","human":true}]}`)),
	}).Error; err != nil {
		t.Fatalf("set coverage: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	for _, want := range []string{"Revisión humana requerida", "`logo.png`", "`vendor/lib`", "binary", "submodule"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("summary %q does not contain %q", bodies[0], want)
		}
	}
}

func TestPublishArchitectureVerdictIsObservational(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		wantLabel  string
		wantDetail string
	}{
		{"complete", store.ArchitectureComplete, "completo", "Condiciones de aceptación"},
		{"inconclusive", store.ArchitectureInconclusive, "inconcluso", "Análisis incompleto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			ctx := context.Background()
			run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
			verdict := store.ArchitectureVerdict{
				Status:    tc.status,
				Requested: true,
				Mode:      store.ArchitectureAnalysisObservation,
				Signals:   []string{"persistence", "public_interface"},
				Scope: store.ArchitectureScope{
					Included: []store.ArchitectureScopeFile{{Path: "src/a.go", Kind: "modified"}},
					Excluded: []store.ArchitectureScopeFile{{Path: "assets/logo.png", Kind: "binary", Reason: "binary"}},
				},
				Dimensions:           []store.ArchitectureDimension{{Name: "Behavior Changes", Content: "- changed"}},
				AcceptanceConditions: []string{"Run the migration test"},
				Controls:             store.ArchitectureControls{Observation: true, WritesDecision: false},
			}
			if tc.status == store.ArchitectureInconclusive {
				verdict.Errors = []string{"analysis failed"}
			}
			if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
				Update("architecture_analysis", datatypes.JSON(store.ArchitectureVerdictJSON(verdict))).Error; err != nil {
				t.Fatal(err)
			}
			gh := newFakeGitHub(t)
			w, _ := newPublishWorker(t, st, gh)

			if err := w.Publish(ctx, run.ID); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			bodies := summaryBodies(t, gh)
			if len(bodies) != 1 {
				t.Fatalf("summary comments = %d, want 1", len(bodies))
			}
			for _, want := range []string{"Análisis arquitectónico", tc.wantLabel, "persistence", tc.wantDetail, "Modo observación"} {
				if !strings.Contains(bodies[0], want) {
					t.Errorf("summary %q does not contain %q", bodies[0], want)
				}
			}

			gh.mu.Lock()
			patches := append([]checkPatch(nil), gh.checkPatches...)
			reviews := len(gh.reviews)
			gh.mu.Unlock()
			if len(patches) == 0 || patches[0].Conclusion != "success" {
				t.Fatalf("check conclusion = %+v, want success", patches)
			}
			if !strings.Contains(patches[0].Output.Text, "Análisis arquitectónico") {
				t.Errorf("check text %q does not carry the verdict", patches[0].Output.Text)
			}
			if reviews != 0 {
				t.Errorf("inline reviews = %d, want 0", reviews)
			}
			var saved store.PullRequest
			if err := st.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			if saved.QAStatus != store.StatusReadyForHuman {
				t.Errorf("qa_status = %q, want %q", saved.QAStatus, store.StatusReadyForHuman)
			}
		})
	}
}

func TestPublishArchitectureBaselineOnly(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, _ := seedPublishContext(t, st, store.RunSucceeded, "major")
	verdict := store.ArchitectureVerdict{
		Status:    store.ArchitectureNotRequested,
		Requested: false,
		Mode:      store.ArchitectureAnalysisObservation,
		Baseline: &store.ArchitectureBaseline{
			Status:    store.ArchitectureBaselinePass,
			RulesPath: ".architecture/baseline.json",
			Controls:  store.ArchitectureBaselineControls{BaseOnly: true},
		},
		Controls: store.ArchitectureControls{Observation: true, WritesDecision: false},
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Update("architecture_analysis", datatypes.JSON(store.ArchitectureVerdictJSON(verdict))).Error; err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	if !strings.Contains(bodies[0], "Línea base de arquitectura") ||
		!strings.Contains(bodies[0], "correcta") {
		t.Errorf("summary %q does not carry the baseline", bodies[0])
	}

	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if len(patches) == 0 || patches[0].Conclusion != "success" {
		t.Fatalf("check conclusion = %+v, want success", patches)
	}
	if !strings.Contains(patches[0].Output.Text, "Línea base de arquitectura: correcta") {
		t.Errorf("check text %q does not carry the baseline", patches[0].Output.Text)
	}
	if strings.Contains(patches[0].Output.Text, store.ArchitectureNotRequested) {
		t.Errorf("check text %q leaked the raw not_requested status", patches[0].Output.Text)
	}
}

func TestPublishOpsEvidenceIsObservational(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	verdict := store.ArchitectureVerdict{
		Status: store.ArchitectureNotRequested,
		Mode:   store.ArchitectureAnalysisObservation,
		OpsEvidence: &store.OpsEvidence{
			Status: store.OpsStatusUnknown,
			Dimensions: []store.OpsDimension{
				{
					Name:     store.OpsDimensionSchema,
					Applies:  true,
					Status:   store.OpsStatusUnknown,
					Reason:   "schema change without a migration in the change",
					Missing:  []string{"migration"},
					Evidence: []string{"artifact: migration migrations/00022_add.sql"},
				},
				{
					Name:    store.OpsDimensionTopology,
					Applies: false,
					Status:  store.OpsStatusNotApplicable,
				},
			},
			Controls: store.OpsControls{Observation: true, WritesDecision: false},
		},
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Update("architecture_analysis", datatypes.JSON(store.ArchitectureVerdictJSON(verdict))).Error; err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	for _, want := range []string{
		"Evidencia operativa", "Migraciones y datos", "desconocida",
		"falta: migration", "artifact: migration migrations/00022_add.sql", "Modo observación",
		"`abcdef1`",
	} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("summary %q does not contain %q", bodies[0], want)
		}
	}
	if strings.Contains(bodies[0], "Despliegue y topología") {
		t.Errorf("summary %q rendered a dimension that does not apply", bodies[0])
	}

	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	reviews := len(gh.reviews)
	gh.mu.Unlock()
	if len(patches) == 0 || patches[0].Conclusion != "success" {
		t.Fatalf("check conclusion = %+v, want success", patches)
	}
	if !strings.Contains(patches[0].Output.Text, "Evidencia operativa") {
		t.Errorf("check text %q does not carry the operational verdict", patches[0].Output.Text)
	}
	if reviews != 0 {
		t.Errorf("inline reviews = %d, want 0", reviews)
	}
	var saved store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.QAStatus != store.StatusReadyForHuman {
		t.Errorf("qa_status = %q, want %q", saved.QAStatus, store.StatusReadyForHuman)
	}
}

func TestPublishOpsEvidenceInventoryErrorIsNotSilent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, _ := seedPublishContext(t, st, store.RunSucceeded, "major")
	verdict := store.ArchitectureVerdict{
		Status: store.ArchitectureNotRequested,
		Mode:   store.ArchitectureAnalysisObservation,
		OpsEvidence: &store.OpsEvidence{
			Status:   store.OpsStatusUnknown,
			Error:    "diff inventory failed: git diff timed out",
			Controls: store.OpsControls{Observation: true, WritesDecision: false},
		},
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Update("architecture_analysis", datatypes.JSON(store.ArchitectureVerdictJSON(verdict))).Error; err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	for _, want := range []string{"Evidencia operativa incompleta", "diff inventory failed"} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("summary %q does not contain %q", bodies[0], want)
		}
	}
	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if len(patches) == 0 || patches[0].Conclusion != "success" {
		t.Fatalf("check conclusion = %+v, want success", patches)
	}
}

func TestPublishCheckConclusions(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		severity   string
		wantConc   string
		wantStatus string
		wantSumm   bool
	}{
		{"succeeded with blocker", store.RunSucceeded, "major", "failure", store.StatusChangesRequested, true},
		{"succeeded clean", store.RunSucceeded, "minor", "success", store.StatusReadyForHuman, true},
		{"inconclusive", store.RunInconclusive, "major", "action_required", store.StatusInconclusive, true},
		{"failed", store.RunFailed, "major", "action_required", store.StatusInconclusive, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			ctx := context.Background()
			run, pr := seedPublishContext(t, st, tc.status, "major")
			if tc.status == store.RunFailed {
				if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
					Update("error_message", "falló la revisión").Error; err != nil {
					t.Fatal(err)
				}
			}
			seedFinding(t, st, pr.ID, run.ID, "f1", 2, tc.severity, store.FindingOpen)
			gh := newFakeGitHub(t)
			w, _ := newPublishWorker(t, st, gh)

			if err := w.Publish(ctx, run.ID); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			gh.mu.Lock()
			patches := append([]checkPatch(nil), gh.checkPatches...)
			posts := len(gh.comments)
			reviews := len(gh.reviews)
			gh.mu.Unlock()
			if len(patches) == 0 || patches[0].Conclusion != tc.wantConc {
				t.Fatalf("check conclusion = %+v, want %q", patches, tc.wantConc)
			}
			if (posts > 0) != tc.wantSumm {
				t.Errorf("summary posts = %d, want presence %v", posts, tc.wantSumm)
			}
			if tc.status == store.RunFailed && reviews != 0 {
				t.Errorf("reviews = %d, want 0 for failed runs", reviews)
			}
			var saved store.PullRequest
			if err := st.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			if saved.QAStatus != tc.wantStatus {
				t.Errorf("qa_status = %q, want %q", saved.QAStatus, tc.wantStatus)
			}
		})
	}
}

func TestPublishIsIdempotent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, pub := newPublishWorker(t, st, gh)

	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}

	gh.mu.Lock()
	posts, reviews := len(gh.comments), len(gh.reviews)
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if posts != 1 {
		t.Errorf("summary posts = %d, want 1", posts)
	}
	if reviews != 1 {
		t.Errorf("reviews = %d, want 1", reviews)
	}
	if len(patches) < 1 {
		t.Fatal("no check run was completed")
	}
	artifact, err := st.ArtifactFor(ctx, pr.ID, store.ArtifactCheck, run.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.ExternalID != "555" {
		t.Errorf("check external id = %q, want 555", artifact.ExternalID)
	}
	var prEvents int
	for _, ev := range pub.all() {
		if ev.Type == "pr.updated" {
			prEvents++
		}
	}
	if prEvents != 1 {
		t.Errorf("pr.updated events = %d, want 1", prEvents)
	}
}

func TestPublishSupersededCancelsCheckOnly(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSuperseded, "major")
	gh := newFakeGitHub(t)
	w, pub := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	posts, reviews := len(gh.comments), len(gh.reviews)
	patches := len(gh.checkPatches)
	gh.mu.Unlock()
	if posts != 0 || reviews != 0 {
		t.Errorf("summary posts = %d, reviews = %d, want 0", posts, reviews)
	}
	if patches != 1 {
		t.Errorf("check patches = %d, want 1 (cancelled)", patches)
	}
	var saved store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.QAStatus != store.StatusReviewing {
		t.Errorf("qa_status = %q, want unchanged", saved.QAStatus)
	}
	if len(pub.all()) != 0 {
		t.Errorf("events = %+v, want none", pub.all())
	}
}

// A superseded run of a previous head owns a different check; cancelling it
// must still work even though it no longer covers the pull request.
func TestPublishSupersededOldHeadCancelsItsCheck(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSuperseded, "major")
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Update("head_sha", "old-head").Error; err != nil {
		t.Fatalf("move the run head: %v", err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Update("head_sha", "new-head").Error; err != nil {
		t.Fatalf("move the PR head: %v", err)
	}
	if _, err := st.UpsertArtifact(ctx, store.GitHubArtifact{
		PullRequestID: pr.ID, Kind: store.ArtifactCheck, HeadSHA: "old-head",
		ExternalID: "555", URL: "https://github.com/acme/repo/runs/555",
	}); err != nil {
		t.Fatalf("seed old check artifact: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	posts, reviews, patches := len(gh.comments), len(gh.reviews), len(gh.checkPatches)
	gh.mu.Unlock()
	if posts != 0 || reviews != 0 {
		t.Errorf("summary posts = %d, reviews = %d, want none", posts, reviews)
	}
	if patches != 1 {
		t.Errorf("check patches = %d, want the old head's check cancelled", patches)
	}
	var saved store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.QAStatus != store.StatusReviewing {
		t.Errorf("qa_status = %q, want unchanged", saved.QAStatus)
	}
}

// A terminal run of an old comparison must not touch the shared check, the
// summary or the PR status, even when it finishes after the retarget.
func TestPublishSkipsRunOfAnotherComparison(t *testing.T) {
	cases := []struct {
		name            string
		runBaseRef      string
		runBaseSHA      string
		prBaseRef       string
		prMergeBaseSHA  string
		wantPublication bool
	}{
		{
			name:       "retarget keeps the head but changes the base branch",
			runBaseRef: "main", runBaseSHA: "mb-old", prBaseRef: "release", prMergeBaseSHA: "mb-old",
		},
		{
			name:       "force-push changes the merge base of the same branch",
			runBaseRef: "main", runBaseSHA: "mb-old", prBaseRef: "main", prMergeBaseSHA: "mb-new",
		},
		{
			name:       "base moved and the comparison is unresolved",
			runBaseRef: "main", runBaseSHA: "mb-old", prBaseRef: "main", prMergeBaseSHA: "",
		},
		{
			name:       "same comparison still publishes",
			runBaseRef: "main", runBaseSHA: "mb", prBaseRef: "main", prMergeBaseSHA: "mb", wantPublication: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			ctx := context.Background()
			run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
			if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
				Updates(map[string]any{"base_ref": tc.runBaseRef, "base_sha": tc.runBaseSHA}).Error; err != nil {
				t.Fatalf("set run comparison: %v", err)
			}
			if err := st.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
				Updates(map[string]any{"base_ref": tc.prBaseRef, "merge_base_sha": tc.prMergeBaseSHA}).Error; err != nil {
				t.Fatalf("set PR comparison: %v", err)
			}
			seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
			gh := newFakeGitHub(t)
			w, pub := newPublishWorker(t, st, gh)

			if err := w.Publish(ctx, run.ID); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			gh.mu.Lock()
			posts, reviews, patches := len(gh.comments), len(gh.reviews), len(gh.checkPatches)
			gh.mu.Unlock()
			var saved store.PullRequest
			if err := st.DB.WithContext(ctx).Take(&saved, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			if tc.wantPublication {
				if posts == 0 || reviews == 0 || patches == 0 {
					t.Fatalf("publication = posts %d, reviews %d, patches %d; want a full publication",
						posts, reviews, patches)
				}
				return
			}
			if posts != 0 || reviews != 0 || patches != 0 {
				t.Errorf("publication = posts %d, reviews %d, patches %d; want none for a stale comparison",
					posts, reviews, patches)
			}
			if saved.QAStatus != store.StatusReviewing {
				t.Errorf("qa_status = %q, want unchanged", saved.QAStatus)
			}
			if len(pub.all()) != 0 {
				t.Errorf("events = %+v, want none", pub.all())
			}
		})
	}
}

func TestPublishAnnotationsArePaged(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	for i := 0; i < 60; i++ {
		seedFinding(t, st, pr.ID, run.ID, fmt.Sprintf("f%02d", i), 2, "major", store.FindingOpen)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if len(patches) == 2 {
		if got := len(patches[0].Output.Annotations); got != 50 {
			t.Errorf("first page annotations = %d, want 50", got)
		}
		if got := len(patches[1].Output.Annotations); got != 10 {
			t.Errorf("second page annotations = %d, want 10", got)
		}
	} else {
		t.Errorf("check patches = %d, want 2 pages", len(patches))
	}
}

func TestPublishRedactsEveryPublishedText(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).Updates(map[string]any{
		"summary": "Mensaje de @juan con sk-live-abcdefghijklmnopqrstuvwxyz",
	}).Error; err != nil {
		t.Fatal(err)
	}
	f := seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	if err := st.DB.WithContext(ctx).Model(&store.Finding{}).Where("id = ?", f.ID).Updates(map[string]any{
		"body": " Usa ghp_abcdefghijklmnopqrstuvwxyz0123456789 y @ana",
	}).Error; err != nil {
		t.Fatal(err)
	}
	gh := newFakeGitHub(t)
	w, pub := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	var all strings.Builder
	for _, c := range gh.comments {
		all.WriteString(c.Body)
	}
	for _, r := range gh.reviews {
		all.WriteString(r.Body)
		for _, c := range r.Comments {
			all.WriteString(c.Body)
		}
	}
	for _, p := range gh.checkPatches {
		all.WriteString(p.Output.Title + p.Output.Summary + p.Output.Text)
		for _, a := range p.Output.Annotations {
			all.WriteString(a.Title + a.Message)
		}
	}
	text := all.String()
	if strings.Contains(text, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") ||
		strings.Contains(text, "sk-live-abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("published text leaked a secret: %q", text)
	}
	if !strings.Contains(text, "[redacted]") || !strings.Contains(text, "`@`juan") || !strings.Contains(text, "`@`ana") {
		t.Errorf("published text was not sanitized: %q", text)
	}
	var redactionAlert bool
	for _, ev := range pub.all() {
		if ev.Type == "run.updated" && ev.ID == strconv.FormatUint(uint64(run.ID), 10) {
			redactionAlert = true
		}
	}
	if !redactionAlert {
		t.Errorf("no redaction alert emitted: %+v", pub.all())
	}
}

func TestPublishKeepsAddedLinesStartingWithDoublePlus(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	gh := newFakeGitHub(t)
	// A real added source line can start with "++". The file header is before
	// the first hunk and must not consume the first diff line.
	gh.files = `[{"filename":"src/a.go","status":"modified",` +
		`"patch":"@@ -0,0 +1,2 @@\n+++i;\n+otra\n"}]`
	f := seedFinding(t, st, pr.ID, run.ID, "plus", 2, "major", store.FindingOpen)
	f.EndLine = 2
	f.Suggestion = "arreglado"
	if _, err := st.UpsertFinding(ctx, f); err != nil {
		t.Fatalf("set suggestion: %v", err)
	}
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.reviews) != 1 || len(gh.reviews[0].Comments) != 1 {
		t.Fatalf("reviews = %+v, want one inline comment on added line 2", gh.reviews)
	}
	if !strings.Contains(gh.reviews[0].Comments[0].Body, "```suggestion\narreglado\n```") {
		t.Errorf("inline comment = %q, want a suggestion for the whole added range",
			gh.reviews[0].Comments[0].Body)
	}
}

func TestPublishResolutionRetryPostsReplyOnce(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	f := seedFinding(t, st, pr.ID, run.ID, "resuelta", 2, "major", store.FindingResolved)
	if err := st.DB.WithContext(ctx).Model(&store.Finding{}).Where("id = ?", f.ID).Updates(map[string]any{
		"github_comment_id": 123, "github_comment_node_id": "NC_resolve",
	}).Error; err != nil {
		t.Fatalf("set finding coordinates: %v", err)
	}
	gh := newFakeGitHub(t)
	gh.failNextResolve()
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err == nil {
		t.Fatal("first Publish() error = nil, want the injected resolution failure")
	}
	gh.mu.Lock()
	firstReplies := len(gh.replies)
	gh.mu.Unlock()
	if firstReplies != 0 {
		t.Errorf("replies after failed resolution = %d, want 0", firstReplies)
	}

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("retry Publish() error = %v", err)
	}
	gh.mu.Lock()
	replies := len(gh.replies)
	gh.mu.Unlock()
	if replies != 1 {
		t.Errorf("replies after retry = %d, want exactly 1", replies)
	}
	saved, err := st.FindingsForPR(ctx, pr.ID)
	if err != nil {
		t.Fatalf("FindingsForPR() error = %v", err)
	}
	for _, finding := range saved {
		if finding.GithubThreadID != "THREAD_1" {
			t.Errorf("thread id = %q, want THREAD_1", finding.GithubThreadID)
		}
	}
}

func TestPublishLinksReviewCommentsWithoutLine(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "sin-linea", 2, "minor", store.FindingOpen)
	gh := newFakeGitHub(t)
	gh.reviewCommentsWithoutLine = true
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	saved, err := st.FindingsForPR(ctx, pr.ID)
	if err != nil {
		t.Fatalf("FindingsForPR() error = %v", err)
	}
	for _, f := range saved {
		if f.GithubCommentID == 0 || f.GithubCommentNodeID == "" {
			t.Errorf("finding %s coordinates = %d/%q, want the review comment id",
				f.ID, f.GithubCommentID, f.GithubCommentNodeID)
		}
	}
}

func TestPublishSummaryListsFindingsWithoutInlineComment(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")

	blocker := seedFinding(t, st, pr.ID, run.ID, "bloqueante", 5, "blocker", store.FindingOpen)
	blocker.Path = "src/b.go"
	blocker.EndLine = 7
	blocker.Title = "Firewall abierto"
	blocker.Body = "El firewall queda expuesto a Internet."
	if _, err := st.UpsertFinding(ctx, blocker); err != nil {
		t.Fatalf("set blocker finding: %v", err)
	}
	seedFinding(t, st, pr.ID, run.ID, "sin-linea", 0, "minor", store.FindingOpen)

	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)
	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	section := summaryFindingsSection(t, bodies[0])
	for _, want := range []string{
		"**blocker**", "`src/b.go:5-7`", "Firewall abierto", "El firewall queda expuesto a Internet.",
		"`src/a.go`", "Título sin-linea",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("summary section %q does not contain %q", section, want)
		}
	}
	for _, unwanted := range []string{"src/a.go:", "src/a.go-"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("summary section %q renders a dangling location %q", section, unwanted)
		}
	}
	blockerAt := strings.Index(section, "Firewall abierto")
	minorAt := strings.Index(section, "Título sin-linea")
	if blockerAt < 0 || minorAt < 0 || blockerAt > minorAt {
		t.Errorf("summary section %q does not order findings by severity", section)
	}
}

func TestPublishSummarySkipsFindingsWithInlineComment(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	published := seedFinding(t, st, pr.ID, run.ID, "ya-publicado", 5, "major", store.FindingOpen)
	if err := st.DB.WithContext(ctx).Model(&store.Finding{}).Where("id = ?", published.ID).
		Update("github_comment_id", 123).Error; err != nil {
		t.Fatalf("mark finding published: %v", err)
	}
	seedFinding(t, st, pr.ID, run.ID, "inline", 2, "minor", store.FindingOpen)
	seedFinding(t, st, pr.ID, run.ID, "pendiente", 9, "nit", store.FindingOpen)
	seedFinding(t, st, pr.ID, run.ID, "resuelta", 5, "minor", store.FindingResolved)

	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)
	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}

	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	section := summaryFindingsSection(t, bodies[0])
	if !strings.Contains(section, "Título pendiente") {
		t.Fatalf("summary section %q does not list the finding without an inline comment", section)
	}
	for _, unwanted := range []string{"Título ya-publicado", "Título inline", "Título resuelta"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("summary section %q lists %q, which has no missing inline comment", section, unwanted)
		}
	}
}

func summaryFindingsSection(t *testing.T, body string) string {
	t.Helper()
	const heading = "**Hallazgos sin comentario inline"
	i := strings.Index(body, heading)
	if i < 0 {
		return ""
	}
	if end := strings.Index(body[i:], "[Ver en el panel]"); end >= 0 {
		return body[i : i+end]
	}
	return body[i:]
}

func TestPublishLeavesCommentIDUnsetWithoutCoordinates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "sin-coordenadas", 2, "major", store.FindingOpen)
	gh := newFakeGitHub(t)
	gh.omitReviewComments = true
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	reviews := len(gh.reviews)
	gh.mu.Unlock()
	if reviews != 1 {
		t.Fatalf("reviews = %d, want 1", reviews)
	}
	saved, err := st.FindingsForPR(ctx, pr.ID)
	if err != nil {
		t.Fatalf("FindingsForPR() error = %v", err)
	}
	for _, f := range saved {
		if f.GithubCommentID != 0 || f.GithubCommentNodeID != "" {
			t.Errorf("finding coordinates = %d/%q, want unset when GitHub returns none",
				f.GithubCommentID, f.GithubCommentNodeID)
		}
	}
}

// enableAutoTriage flips the only policy of the freshly seeded database.
func enableAutoTriage(t *testing.T, st *store.Store, mode string) {
	t.Helper()
	if err := st.DB.Exec("UPDATE qa_policies SET auto_triage_mode = ?", mode).Error; err != nil {
		t.Fatalf("enable auto-triage: %v", err)
	}
}

func setRunVerificationSession(t *testing.T, st *store.Store, runID uint, session string) {
	t.Helper()
	if err := st.DB.Exec("UPDATE review_runs SET verify_session_id = ? WHERE id = ?", session, runID).
		Error; err != nil {
		t.Fatalf("set verify session: %v", err)
	}
}

// markVerified stores the verifier's answer and, when applied, its triage.
func markVerified(t *testing.T, st *store.Store, id, verdict, triageVerdict string) {
	t.Helper()
	markVerifiedAtHead(t, st, id, verdict, triageVerdict, "abcdef1234567890")
}

func markVerifiedAtHead(t *testing.T, st *store.Store, id, verdict, triageVerdict, head string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"verdict": verdict, "evidence": "guard()", "head_sha": head,
	})
	if err != nil {
		t.Fatalf("marshal verification: %v", err)
	}
	updates := map[string]any{
		"verification":  datatypes.JSON(raw),
		"triage_source": store.TriageSourceVerifier,
	}
	if triageVerdict != "" {
		updates["triage_verdict"] = triageVerdict
	}
	if err := st.DB.Model(&store.Finding{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		t.Fatalf("mark finding %s verified: %v", id, err)
	}
}

func publishedSummary(t *testing.T, gh *fakeGitHub) string {
	t.Helper()
	bodies := summaryBodies(t, gh)
	if len(bodies) == 0 {
		t.Fatal("no summary comment was published")
	}
	return bodies[len(bodies)-1]
}

func TestSummaryListsValidatedAndUncertain(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "confirmed", 2, "minor", store.FindingOpen)
	markVerified(t, st, "confirmed", "confirm", store.VerdictConfirmed)
	seedFinding(t, st, pr.ID, run.ID, "uncertain", 3, "minor", store.FindingOpen)
	markVerified(t, st, "uncertain", "uncertain", "")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	body := publishedSummary(t, gh)
	for _, want := range []string{
		"**Verificación automática:** 1 validados · 1 para tu criterio · 0 descartados",
		"**Validado por la verificación:**",
		"Título confirmed",
		"<details><summary>Necesita tu criterio (1)</summary>",
		"Título uncertain",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary does not contain %q:\n%s", want, body)
		}
	}
}

func TestSummaryCountsAutoDismissed(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "dismissed", 2, "nit", store.FindingWithdrawn)
	markVerified(t, st, "dismissed", "refute", store.VerdictFalsePositive)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	body := publishedSummary(t, gh)
	if !strings.Contains(body, "**Verificación automática:** 0 validados · 0 para tu criterio · 1 descartados") {
		t.Errorf("summary does not count the dismissal:\n%s", body)
	}
	if !strings.Contains(body, "1 descartados automáticamente · [ver en el panel](https://qa.example/prs/") {
		t.Errorf("summary does not link the dismissed findings:\n%s", body)
	}
	if strings.Contains(body, "Título dismissed") {
		t.Errorf("summary lists a dismissed finding:\n%s", body)
	}
}

func TestSummaryNotesSkippedVerification(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	seedFinding(t, st, pr.ID, run.ID, "pending", 2, "minor", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	body := publishedSummary(t, gh)
	if !strings.Contains(body, "La verificación automática no se ejecutó") {
		t.Errorf("summary does not explain the skipped verification:\n%s", body)
	}
	if !strings.Contains(body, "0 validados · 1 para tu criterio") {
		t.Errorf("summary does not list the unverified finding:\n%s", body)
	}
}

func TestInlineOnlyForConfirmedWhenVerificationRan(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "confirmed", 2, "minor", store.FindingOpen)
	markVerified(t, st, "confirmed", "confirm", store.VerdictConfirmed)
	seedFinding(t, st, pr.ID, run.ID, "pending", 3, "minor", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	reviews := append([]fakeReview(nil), gh.reviews...)
	gh.mu.Unlock()
	if len(reviews) != 1 || len(reviews[0].Comments) != 1 {
		t.Fatalf("reviews = %+v, want one review with only the confirmed finding", reviews)
	}
	body := reviews[0].Comments[0].Body
	if !strings.Contains(body, "Título confirmed") || strings.Contains(body, "Título pending") {
		t.Errorf("inline comment = %q, want the confirmed finding only", body)
	}
}

func TestSummaryKeepsDegradedRefutationsForTheHuman(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "rejected", 2, "major", store.FindingOpen)
	markVerified(t, st, "rejected", "refute", "")
	// La política no aplicó el descarte (severidad bloqueante): el hallazgo
	// sigue abierto y sin triage, así que la decisión es del humano.
	if err := st.DB.Exec("UPDATE findings SET triage_source = '' WHERE id = ?", "rejected").Error; err != nil {
		t.Fatalf("clear triage source: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	body := publishedSummary(t, gh)
	if !strings.Contains(body, "**Verificación automática:** 0 validados · 1 para tu criterio · 0 descartados") {
		t.Errorf("summary counts a refutation the policy never applied:\n%s", body)
	}
	if !strings.Contains(body, "Necesita tu criterio (1)") || !strings.Contains(body, "Título rejected") {
		t.Errorf("summary hides a degraded refutation from the human:\n%s", body)
	}
	if strings.Contains(body, "descartados automáticamente") {
		t.Errorf("summary reports a dismissal that never happened:\n%s", body)
	}
}

func TestInlineFiltersReusedTriageWithoutANewScan(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	// Sin sesión nueva: el veredicto se reutilizó de una pasada anterior.
	seedFinding(t, st, pr.ID, run.ID, "confirmed", 2, "minor", store.FindingOpen)
	markVerified(t, st, "confirmed", "confirm", store.VerdictConfirmed)
	seedFinding(t, st, pr.ID, run.ID, "pending", 3, "minor", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	reviews := append([]fakeReview(nil), gh.reviews...)
	gh.mu.Unlock()
	if len(reviews) != 1 || len(reviews[0].Comments) != 1 {
		t.Fatalf("reviews = %+v, want one review with only the confirmed finding", reviews)
	}
	if body := reviews[0].Comments[0].Body; !strings.Contains(body, "Título confirmed") ||
		strings.Contains(body, "Título pending") {
		t.Errorf("inline comment = %q, want the confirmed finding only", body)
	}
}

func TestInlineFallsBackWhenVerificationDisabled(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "first", 2, "minor", store.FindingOpen)
	seedFinding(t, st, pr.ID, run.ID, "second", 3, "minor", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	reviews := append([]fakeReview(nil), gh.reviews...)
	gh.mu.Unlock()
	if len(reviews) != 1 || len(reviews[0].Comments) != 2 {
		t.Fatalf("reviews = %+v, want both findings inline when verification is off", reviews)
	}
	body := publishedSummary(t, gh)
	if strings.Contains(body, "Verificación automática") {
		t.Errorf("summary carries a verification block with the mode off:\n%s", body)
	}
}

// publishConclusion returns the conclusion and QA status of the last publish.
func publishConclusion(t *testing.T, st *store.Store, gh *fakeGitHub, prID uint) (string, string) {
	t.Helper()
	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if len(patches) == 0 {
		t.Fatal("no check run was completed")
	}
	var pr store.PullRequest
	if err := st.DB.Take(&pr, prID).Error; err != nil {
		t.Fatalf("reload pull request: %v", err)
	}
	return patches[len(patches)-1].Conclusion, pr.QAStatus
}

func TestPublishBlocksOnValidatedConfirmation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "confirmed", 2, "major", store.FindingOpen)
	markVerified(t, st, "confirmed", "confirm", store.VerdictConfirmed)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	conclusion, status := publishConclusion(t, st, gh, pr.ID)
	if conclusion != "failure" || status != store.StatusChangesRequested {
		t.Errorf("conclusion/status = %q/%q, want a validated confirmation to block", conclusion, status)
	}
}

func TestPublishBlocksOnHumanConfirmationDespiteVerifierUncertainty(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "confirmed", 2, "major", store.FindingOpen)
	markVerified(t, st, "confirmed", "uncertain", "")
	if err := st.DB.Exec(
		"UPDATE findings SET triage_source = ?, triage_verdict = ? WHERE id = ?",
		store.TriageSourceHuman, store.VerdictConfirmed, "confirmed",
	).Error; err != nil {
		t.Fatalf("set human triage: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	conclusion, status := publishConclusion(t, st, gh, pr.ID)
	if conclusion != "failure" || status != store.StatusChangesRequested {
		t.Errorf("conclusion/status = %q/%q, want a human confirmation to block", conclusion, status)
	}
}

func TestPublishDoesNotBlockOnUncertainVerification(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "uncertain", 2, "major", store.FindingOpen)
	markVerified(t, st, "uncertain", "uncertain", "")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	conclusion, status := publishConclusion(t, st, gh, pr.ID)
	if conclusion != "success" || status != store.StatusReadyForHuman {
		t.Errorf("conclusion/status = %q/%q, want an uncertainty to leave the human in charge", conclusion, status)
	}
}

func TestSummaryListsInvalidatedDismissalForTheHuman(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "reopened", 2, "nit", store.FindingWithdrawn)
	markVerified(t, st, "reopened", "uncertain", "")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	body := publishedSummary(t, gh)
	if !strings.Contains(body, "**Verificación automática:** 0 validados · 1 para tu criterio · 0 descartados") {
		t.Errorf("summary drops an invalidated dismissal:\n%s", body)
	}
	if !strings.Contains(body, "Necesita tu criterio (1)") || !strings.Contains(body, "Título reopened") {
		t.Errorf("summary hides the invalidated dismissal from the human:\n%s", body)
	}
}

func TestPublishDoesNotTrustAConfirmationWithoutEvidence(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "legacy", 2, "major", store.FindingOpen)
	raw := datatypes.JSON([]byte(`{"verdict":"confirm","head_sha":"abcdef1234567890"}`))
	if err := st.DB.Model(&store.Finding{}).Where("id = ?", "legacy").Updates(map[string]any{
		"verification":   raw,
		"triage_source":  store.TriageSourceVerifier,
		"triage_verdict": store.VerdictConfirmed,
	}).Error; err != nil {
		t.Fatalf("store pre-#136 confirmation: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	conclusion, status := publishConclusion(t, st, gh, pr.ID)
	if conclusion != "success" || status != store.StatusReadyForHuman {
		t.Errorf("conclusion/status = %q/%q, want an evidence-less confirmation not to block", conclusion, status)
	}
	body := publishedSummary(t, gh)
	if !strings.Contains(body, "0 validados · 1 para tu criterio") {
		t.Errorf("summary presents an evidence-less confirmation as validated:\n%s", body)
	}
	gh.mu.Lock()
	reviews := append([]fakeReview(nil), gh.reviews...)
	gh.mu.Unlock()
	for _, review := range reviews {
		for _, comment := range review.Comments {
			if strings.Contains(comment.Body, "Título legacy") {
				t.Errorf("an evidence-less confirmation was published inline: %q", comment.Body)
			}
		}
	}
}

func TestPublishDoesNotTrustAVerdictFromAnotherHead(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableAutoTriage(t, st, store.AutoTriageApply)
	setRunVerificationSession(t, st, run.ID, "verify-session")
	seedFinding(t, st, pr.ID, run.ID, "stale", 2, "major", store.FindingOpen)
	markVerifiedAtHead(t, st, "stale", "confirm", store.VerdictConfirmed, "oldsha111")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	conclusion, status := publishConclusion(t, st, gh, pr.ID)
	if conclusion != "success" || status != store.StatusReadyForHuman {
		t.Errorf("conclusion/status = %q/%q, want a stale verdict not to block", conclusion, status)
	}
	body := publishedSummary(t, gh)
	for _, want := range []string{
		"0 validados · 1 para tu criterio",
		"la verificación corresponde a otro commit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("summary does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Validado por la verificación") {
		t.Errorf("summary presents a verdict from another head as validated:\n%s", body)
	}
	gh.mu.Lock()
	reviews := append([]fakeReview(nil), gh.reviews...)
	gh.mu.Unlock()
	for _, review := range reviews {
		for _, comment := range review.Comments {
			if strings.Contains(comment.Body, "Título stale") {
				t.Errorf("a stale confirmation was published inline: %q", comment.Body)
			}
		}
	}
}

// enableCI turns on the CI gate for the seeded team and configures the checks
// it must see green.
func enableCI(t *testing.T, st *store.Store, pr store.PullRequest, checks ...store.RequiredCheck) {
	t.Helper()
	ctx := context.Background()
	var repo store.Repository
	if err := st.DB.WithContext(ctx).Take(&repo, pr.RepositoryID).Error; err != nil {
		t.Fatalf("load repository: %v", err)
	}
	if repo.TeamID == nil {
		t.Fatal("repository has no team")
	}
	policy, err := st.QAPolicyForTeam(ctx, *repo.TeamID)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	encoded, err := store.NormalizeRequiredChecks(checks)
	if err != nil {
		t.Fatalf("normalize required checks: %v", err)
	}
	policy.RequireGreenCI = true
	policy.RequiredChecks = encoded
	if _, err := st.SaveQAPolicy(ctx, policy); err != nil {
		t.Fatalf("save policy: %v", err)
	}
}

func publishedConclusion(t *testing.T, gh *fakeGitHub) string {
	t.Helper()
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.checkPatches) == 0 {
		t.Fatal("no check patch published")
	}
	return gh.checkPatches[len(gh.checkPatches)-1].Conclusion
}

func storedQAStatus(t *testing.T, st *store.Store, prID uint) string {
	t.Helper()
	var pr store.PullRequest
	if err := st.DB.WithContext(context.Background()).Take(&pr, prID).Error; err != nil {
		t.Fatalf("reload PR: %v", err)
	}
	return pr.QAStatus
}

// A run scheduled while CI was green must not publish a success after the same
// head turned red: publication re-reads the gate instead of trusting the
// enqueue-time snapshot.
func TestPublishDoesNotSucceedWhenCITurnedRed(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableCI(t, st, pr)
	gh := newFakeGitHub(t)
	gh.setChecks("failure", "success")
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := publishedConclusion(t, gh); got != "action_required" {
		t.Errorf("check conclusion = %q, want action_required", got)
	}
	if got := storedQAStatus(t, st, pr.ID); got != store.StatusWaitingCI {
		t.Errorf("qa_status = %q, want %q", got, store.StatusWaitingCI)
	}
}

func TestPublishWaitsForPendingCI(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableCI(t, st, pr)
	gh := newFakeGitHub(t)
	gh.setCheckTasks(store.CheckObservation{Name: "build", Origin: "app:7", State: store.CheckPending})
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := publishedConclusion(t, gh); got != "action_required" {
		t.Errorf("check conclusion = %q, want action_required while CI is pending", got)
	}
	if got := storedQAStatus(t, st, pr.ID); got != store.StatusWaitingCI {
		t.Errorf("qa_status = %q, want %q", got, store.StatusWaitingCI)
	}
}

func TestPublishGreenCIAllowsSuccess(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableCI(t, st, pr)
	gh := newFakeGitHub(t)
	gh.setChecks("success", "success")
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := publishedConclusion(t, gh); got != "success" {
		t.Errorf("check conclusion = %q, want success", got)
	}
	if got := storedQAStatus(t, st, pr.ID); got != store.StatusReadyForHuman {
		t.Errorf("qa_status = %q, want %q", got, store.StatusReadyForHuman)
	}
}

// A failed lookup is explicit: no success is published and the previous
// counters are replaced by an unknown observation.
func TestPublishCILookupFailureIsExplicit(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableCI(t, st, pr)
	gh := newFakeGitHub(t)
	gh.failChecks(true)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := publishedConclusion(t, gh); got != "action_required" {
		t.Errorf("check conclusion = %q, want action_required after a failed lookup", got)
	}
	if got := storedQAStatus(t, st, pr.ID); got != store.StatusWaitingCI {
		t.Errorf("qa_status = %q, want %q", got, store.StatusWaitingCI)
	}
	observation, err := st.PRValidationObservation(ctx, pr.ID, run.HeadSHA)
	if err != nil {
		t.Fatalf("PRValidationObservation: %v", err)
	}
	if observation == nil || observation.Result != store.ObservationLookupFailed {
		t.Errorf("stored observation = %+v, want an explicit lookup failure", observation)
	}
}

// Only the configured required checks decide the gate; a failure elsewhere
// does not block a success, and an absent required check blocks it.
func TestPublishRequiredCheckSelection(t *testing.T) {
	tests := []struct {
		name     string
		required []store.RequiredCheck
		checks   []store.CheckObservation
		want     string
	}{
		{
			name:     "required check green while another fails",
			required: []store.RequiredCheck{{Name: "build", Origin: "app:7"}},
			checks: []store.CheckObservation{
				{Name: "build", Origin: "app:7", State: store.CheckPassed},
				{Name: "optional", Origin: "app:8", State: store.CheckFailed},
			},
			want: "success",
		},
		{
			name:     "required check absent",
			required: []store.RequiredCheck{{Name: "build", Origin: "app:7"}},
			checks: []store.CheckObservation{
				{Name: "build", Origin: "app:8", State: store.CheckPassed},
			},
			want: "action_required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			ctx := context.Background()
			run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
			enableCI(t, st, pr, tt.required...)
			gh := newFakeGitHub(t)
			gh.setCheckTasks(tt.checks...)
			w, _ := newPublishWorker(t, st, gh)

			if err := w.Publish(ctx, run.ID); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}
			if got := publishedConclusion(t, gh); got != tt.want {
				t.Errorf("check conclusion = %q, want %q", got, tt.want)
			}
		})
	}
}

// Republishing applies the same gate, so red -> green -> red converges on the
// same SHA without repeating the review.
func TestRepublishConvergesRedGreenRed(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	enableCI(t, st, pr)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	steps := []struct {
		ci        string
		wantCheck string
		status    string
	}{
		{"failure", "action_required", store.StatusWaitingCI},
		{"success", "success", store.StatusReadyForHuman},
		{"failure", "action_required", store.StatusWaitingCI},
	}
	for i, step := range steps {
		gh.setChecks(step.ci)
		if err := w.Republish(ctx, pr.ID); err != nil {
			t.Fatalf("Republish() #%d error = %v", i+1, err)
		}
		if got := publishedConclusion(t, gh); got != step.wantCheck {
			t.Errorf("step %d conclusion = %q, want %q", i+1, got, step.wantCheck)
		}
		if got := storedQAStatus(t, st, pr.ID); got != step.status {
			t.Errorf("step %d qa_status = %q, want %q", i+1, got, step.status)
		}
	}
}

type recordingEnqueuer struct {
	mu  sync.Mutex
	ids []uint
}

func (e *recordingEnqueuer) EnqueuePublish(_ context.Context, id uint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, id)
	return nil
}

func (e *recordingEnqueuer) published() []uint {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]uint(nil), e.ids...)
}

func setPublishPending(t *testing.T, st *store.Store, runID uint) {
	t.Helper()
	if err := st.DB.Model(&store.ReviewRun{}).Where("id = ?", runID).
		Update("publish_pending", true).Error; err != nil {
		t.Fatalf("set publish pending: %v", err)
	}
}

func loadRun(t *testing.T, st *store.Store, runID uint) store.ReviewRun {
	t.Helper()
	run, err := st.ReviewRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ReviewRun() error = %v", err)
	}
	return run
}

// A terminal run whose publish job was lost is found by the reconciler and its
// delivery succeeds on the next publish.
func TestPublishReconcilesRunWhoseJobWasLost(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	setPublishPending(t, st, run.ID)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)
	enqueuer := &recordingEnqueuer{}
	w.Jobs = enqueuer

	enqueued, err := w.ReconcilePendingPublishes(ctx, 10)
	if err != nil {
		t.Fatalf("ReconcilePendingPublishes() error = %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", enqueued)
	}
	if got := enqueuer.published(); len(got) != 1 || got[0] != run.ID {
		t.Fatalf("enqueued runs = %v, want [%d]", got, run.ID)
	}

	// The queued job then delivers the run.
	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if got := len(summaryBodies(t, gh)); got != 1 {
		t.Errorf("summary comments = %d, want 1", got)
	}
	saved := loadRun(t, st, run.ID)
	if saved.PublishPending {
		t.Error("publish_pending is still true after a successful delivery")
	}
	state := map[string]bool{}
	if err := json.Unmarshal(saved.PublishState, &state); err != nil {
		t.Fatalf("decode publish_state %s: %v", saved.PublishState, err)
	}
	if !state["completed"] {
		t.Errorf("publish_state = %s, want completed", saved.PublishState)
	}
}

// A redelivery of a settled run writes nothing.
func TestPublishDuplicateIsNoOp(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("first Publish() error = %v", err)
	}
	gh.mu.Lock()
	before := struct {
		comments, reviews, checks, patches int
	}{len(gh.comments), len(gh.reviews), len(gh.checkPatches), len(gh.commentPatches)}
	gh.mu.Unlock()

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("duplicate Publish() error = %v", err)
	}

	gh.mu.Lock()
	after := struct {
		comments, reviews, checks, patches int
	}{len(gh.comments), len(gh.reviews), len(gh.checkPatches), len(gh.commentPatches)}
	gh.mu.Unlock()
	if after != before {
		t.Errorf("redelivery wrote again: before %+v, after %+v", before, after)
	}
}

// A late publish of an old head or an old run of the same comparison writes
// nothing shared and settles instead of retrying forever.
func TestPublishLateRunWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		set  func(t *testing.T, st *store.Store, run store.ReviewRun, pr store.PullRequest)
	}{
		{
			name: "head moved",
			set: func(t *testing.T, st *store.Store, run store.ReviewRun, pr store.PullRequest) {
				if err := st.DB.Model(&store.ReviewRun{}).Where("id = ?", run.ID).
					Update("head_sha", "old-head").Error; err != nil {
					t.Fatalf("move run head: %v", err)
				}
				if err := st.DB.Model(&store.PullRequest{}).Where("id = ?", pr.ID).
					Update("head_sha", "new-head").Error; err != nil {
					t.Fatalf("move PR head: %v", err)
				}
			},
		},
		{
			name: "newer run owns the comparison",
			set: func(t *testing.T, st *store.Store, run store.ReviewRun, pr store.PullRequest) {
				if _, err := st.CreateReviewRun(context.Background(), store.ReviewRun{
					PullRequestID: pr.ID, ProfileID: run.ProfileID, HeadSHA: run.HeadSHA,
					Status: store.RunSucceeded, Summary: "más reciente",
				}); err != nil {
					t.Fatalf("create newer run: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewStore(t)
			ctx := context.Background()
			run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
			seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
			setPublishPending(t, st, run.ID)
			tc.set(t, st, run, pr)
			gh := newFakeGitHub(t)
			w, pub := newPublishWorker(t, st, gh)

			if err := w.Publish(ctx, run.ID); err != nil {
				t.Fatalf("Publish() error = %v", err)
			}

			gh.mu.Lock()
			comments, reviews, checks := len(gh.comments), len(gh.reviews), len(gh.checkPatches)
			gh.mu.Unlock()
			if comments != 0 || reviews != 0 || checks != 0 {
				t.Errorf("writes = comments %d, reviews %d, checks %d; want none",
					comments, reviews, checks)
			}
			if events := pub.all(); len(events) != 0 {
				t.Errorf("events = %+v, want none", events)
			}
			if saved := loadRun(t, st, run.ID); saved.PublishPending {
				t.Error("a stale run stayed pending publication")
			}
		})
	}
}

// A partial delivery resumes only the step that failed: the summary and the
// inline review were already written and are not touched again.
func TestPublishRetriesOnlyMissingSteps(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	setPublishPending(t, st, run.ID)
	gh := newFakeGitHub(t)
	gh.failNextCheck()
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err == nil {
		t.Fatal("first Publish() error = nil, want the injected check failure")
	}
	gh.mu.Lock()
	firstPosts, firstReviews, firstPatches := len(gh.comments), len(gh.reviews), len(gh.commentPatches)
	gh.mu.Unlock()
	if firstPosts != 1 || firstReviews != 1 {
		t.Fatalf("first attempt wrote posts %d, reviews %d; want the summary and the inline review",
			firstPosts, firstReviews)
	}

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("retry Publish() error = %v", err)
	}
	gh.mu.Lock()
	posts, reviews, patches, checks := len(gh.comments), len(gh.reviews), len(gh.commentPatches), len(gh.checkPatches)
	gh.mu.Unlock()
	if posts != 1 || reviews != 1 {
		t.Errorf("retry duplicated a finished step: posts %d, reviews %d", posts, reviews)
	}
	if patches != firstPatches {
		t.Errorf("retry re-edited the summary: PATCHes %d, want %d", patches, firstPatches)
	}
	if checks != 1 {
		t.Errorf("check patches = %d, want the one missing step", checks)
	}
}

// A review created on GitHub before the response was stored is recovered by its
// run marker on retry instead of being posted a second time.
func TestPublishRecoversReviewAfterPartialResponse(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	setPublishPending(t, st, run.ID)
	gh := newFakeGitHub(t)
	gh.failNextReviewComments()
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err == nil {
		t.Fatal("first Publish() error = nil, want the injected comment-listing failure")
	}
	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("retry Publish() error = %v", err)
	}

	gh.mu.Lock()
	reviews := len(gh.reviews)
	gh.mu.Unlock()
	if reviews != 1 {
		t.Errorf("reviews = %d, want one recovered review", reviews)
	}
	saved, err := st.FindingsForPR(ctx, pr.ID)
	if err != nil {
		t.Fatalf("FindingsForPR() error = %v", err)
	}
	for _, f := range saved {
		if f.ID == "f1" && f.GithubCommentID == 0 {
			t.Error("the recovered review did not link its finding")
		}
	}
}

// publishProbe observes how many publishers are inside the context-load phase
// at once. With the per-PR lock only one may be there; without it two overlap.
type publishProbe struct {
	mu     sync.Mutex
	active int
	max    int
}

func (p *publishProbe) enter() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active++
	if p.active > p.max {
		p.max = p.active
	}
	// Hold the phase long enough that an unlocked second publisher must overlap.
	time.Sleep(25 * time.Millisecond)
}

func (p *publishProbe) leave() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
}

func (p *publishProbe) maxActive() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

type probingAppProvider struct {
	base  ghapp.AppProvider
	probe *publishProbe
}

func (p probingAppProvider) App(ctx context.Context) (*ghapp.App, error) {
	p.probe.enter()
	defer p.probe.leave()
	return p.base.App(ctx)
}

// A run that finishes while the pull request's merge base is unresolved must
// stay pending, so the trigger can adopt the comparison and publish it later.
func TestPublishAfterComparisonAdoption(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	setPublishPending(t, st, run.ID)
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Updates(map[string]any{"base_ref": "main", "base_sha": "mb-1"}).Error; err != nil {
		t.Fatalf("set run comparison: %v", err)
	}
	if err := st.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{"base_ref": "main", "merge_base_sha": ""}).Error; err != nil {
		t.Fatalf("set unresolved PR comparison: %v", err)
	}
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() with an unresolved comparison error = %v", err)
	}
	gh.mu.Lock()
	comments, reviews, checks := len(gh.comments), len(gh.reviews), len(gh.checkPatches)
	gh.mu.Unlock()
	if comments != 0 || reviews != 0 || checks != 0 {
		t.Fatalf("unresolved comparison wrote comments %d, reviews %d, checks %d",
			comments, reviews, checks)
	}
	pending := loadRun(t, st, run.ID)
	if !pending.PublishPending {
		t.Fatal("an unresolved comparison settled its publish intent")
	}
	state := map[string]bool{}
	if len(pending.PublishState) > 0 {
		if err := json.Unmarshal(pending.PublishState, &state); err != nil {
			t.Fatalf("decode publish_state %s: %v", pending.PublishState, err)
		}
	}
	if state["completed"] {
		t.Fatal("an unresolved comparison was marked completed")
	}

	// The trigger resolves the comparison and adopts the existing run.
	if err := st.DB.WithContext(ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Update("merge_base_sha", "mb-1").Error; err != nil {
		t.Fatalf("adopt the comparison: %v", err)
	}
	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() after adoption error = %v", err)
	}
	gh.mu.Lock()
	comments, reviews, checks = len(gh.comments), len(gh.reviews), len(gh.checkPatches)
	gh.mu.Unlock()
	if comments != 1 || reviews != 1 || checks != 1 {
		t.Errorf("adopted publication = comments %d, reviews %d, checks %d; want one of each",
			comments, reviews, checks)
	}
	var adoptedPR store.PullRequest
	if err := st.DB.WithContext(ctx).Take(&adoptedPR, pr.ID).Error; err != nil {
		t.Fatalf("load adopted pull request: %v", err)
	}
	if adoptedPR.QAStatus != store.StatusChangesRequested {
		t.Errorf("qa_status after adoption = %q, want %q", adoptedPR.QAStatus, store.StatusChangesRequested)
	}
	saved := loadRun(t, st, run.ID)
	if saved.PublishPending {
		t.Error("adopted publication left publish_pending true")
	}
}

// Two concurrent deliveries of the same run stay idempotent and never overlap
// in the shared publication phase.
func TestPublishConcurrentIsIdempotent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	seedFinding(t, st, pr.ID, run.ID, "f1", 2, "major", store.FindingOpen)
	setPublishPending(t, st, run.ID)
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)
	probe := &publishProbe{}
	w.Apps = probingAppProvider{base: w.Apps, probe: probe}

	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			errs <- w.Publish(ctx, run.ID)
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Publish() error = %v", err)
		}
	}

	if got := probe.maxActive(); got != 1 {
		t.Errorf("concurrent publishers in the shared phase = %d, want 1", got)
	}
	gh.mu.Lock()
	posts, reviews, checks := len(gh.comments), len(gh.reviews), len(gh.checkPatches)
	gh.mu.Unlock()
	if posts != 1 || reviews != 1 || checks != 1 {
		t.Errorf("concurrent delivery = posts %d, reviews %d, checks %d; want one of each",
			posts, reviews, checks)
	}
	saved := loadRun(t, st, run.ID)
	if saved.PublishPending {
		t.Error("publish_pending is still true after concurrent deliveries")
	}
}

// setReviewTarget points the single team policy of a test database at a GitHub
// user and/or team.
func setReviewTarget(t *testing.T, st *store.Store, login, team string) {
	t.Helper()
	if err := st.DB.WithContext(context.Background()).Model(&store.QAPolicy{}).
		Where("id > 0").Updates(map[string]any{
		"reviewer_login": login, "reviewer_team": team,
	}).Error; err != nil {
		t.Fatalf("set review target: %v", err)
	}
}

func reviewRequestCalls(gh *fakeGitHub) []reviewRequestCall {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return append([]reviewRequestCall(nil), gh.reviewRequests...)
}

func TestPublishRequestsHumanReviewWhenInconclusive(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	setReviewTarget(t, st, "qa-reviewer", "qa-team")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	calls := reviewRequestCalls(gh)
	if len(calls) != 1 {
		t.Fatalf("review requests = %d, want 1", len(calls))
	}
	if len(calls[0].Reviewers) != 1 || calls[0].Reviewers[0] != "qa-reviewer" ||
		len(calls[0].TeamReviewers) != 1 || calls[0].TeamReviewers[0] != "qa-team" {
		t.Errorf("review request = %+v, want qa-reviewer/qa-team", calls[0])
	}

	saved, err := st.ReviewRequestFor(ctx, pr.ID, run.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor() error = %v", err)
	}
	if saved.Status != store.ReviewRequestDelivered || saved.Attempts != 1 || saved.DeliveredAt == nil {
		t.Errorf("request = %+v, want delivered on the first attempt", saved)
	}
	if saved.Cause != store.ReviewRequestCauseInconclusive || saved.HeadSHA != run.HeadSHA ||
		saved.ReviewerLogin != "qa-reviewer" || saved.ReviewerTeam != "qa-team" {
		t.Errorf("request = %+v, want the inconclusive cause and the configured recipients", saved)
	}
}

func TestPublishRequestsHumanReviewForFailedRun(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, _ := seedPublishContext(t, st, store.RunFailed, "major")
	setReviewTarget(t, st, "qa-reviewer", "")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if calls := reviewRequestCalls(gh); len(calls) != 1 {
		t.Fatalf("review requests = %d, want 1 for a failed run", len(calls))
	}
}

// A conclusive review is owned by the engine, not by a person: no request.
func TestPublishDoesNotRequestReviewForConclusiveRun(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	setReviewTarget(t, st, "qa-reviewer", "qa-team")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if calls := reviewRequestCalls(gh); len(calls) != 0 {
		t.Fatalf("review requests = %+v, want none", calls)
	}
	if _, err := st.ReviewRequestFor(ctx, pr.ID, run.HeadSHA); err == nil {
		t.Fatal("ReviewRequestFor() found a request for a conclusive run")
	}
}

func TestPublishReviewRequestDeduplicatesPerPRAndSHA(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	setReviewTarget(t, st, "qa-reviewer", "")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}
	if calls := reviewRequestCalls(gh); len(calls) != 1 {
		t.Fatalf("review requests = %d, want 1 after a retried publish", len(calls))
	}
	saved, err := st.ReviewRequestFor(ctx, pr.ID, run.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor() error = %v", err)
	}
	if saved.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", saved.Attempts)
	}
}

func TestPublishReviewRequestRetriesThenEscalates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	setReviewTarget(t, st, "qa-reviewer", "")
	gh := newFakeGitHub(t)
	gh.reviewRequestStatus = http.StatusInternalServerError
	gh.reviewRequestFailures = -1
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	attempts := gh.reviewRequestAttempts
	gh.mu.Unlock()
	if attempts != 3 {
		t.Errorf("review-request attempts = %d, want the bounded 3", attempts)
	}
	saved, err := st.ReviewRequestFor(ctx, pr.ID, run.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor() error = %v", err)
	}
	if saved.Status != store.ReviewRequestEscalated || saved.Attempts != 3 || saved.DeliveredAt != nil {
		t.Errorf("request = %+v, want escalated after three attempts", saved)
	}
	if !strings.Contains(saved.LastError, "500") {
		t.Errorf("last error = %q, want the GitHub status", saved.LastError)
	}
}

func TestPublishReviewRequestRejectionIsRecorded(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	setReviewTarget(t, st, "qa-reviewer", "")
	gh := newFakeGitHub(t)
	gh.reviewRequestStatus = http.StatusUnprocessableEntity
	gh.reviewRequestFailures = -1
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	attempts := gh.reviewRequestAttempts
	gh.mu.Unlock()
	if attempts != 1 {
		t.Errorf("review-request attempts = %d, want 1 (a rejection is not retried)", attempts)
	}
	saved, err := st.ReviewRequestFor(ctx, pr.ID, run.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor() error = %v", err)
	}
	if saved.Status != store.ReviewRequestRejected || saved.DeliveredAt != nil {
		t.Errorf("request = %+v, want rejected", saved)
	}
	if !strings.Contains(saved.LastError, "422") {
		t.Errorf("last error = %q, want the GitHub status", saved.LastError)
	}
}

func TestPublishReviewRequestWithoutTargetIsRecorded(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunInconclusive, "major")
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if calls := reviewRequestCalls(gh); len(calls) != 0 {
		t.Fatalf("review requests = %+v, want none without a target", calls)
	}
	saved, err := st.ReviewRequestFor(ctx, pr.ID, run.HeadSHA)
	if err != nil {
		t.Fatalf("ReviewRequestFor() error = %v", err)
	}
	if saved.Status != store.ReviewRequestRejected || !strings.Contains(saved.LastError, "no reviewer") {
		t.Errorf("request = %+v, want a rejected row that names the missing target", saved)
	}
}

func setFindingEvidence(t *testing.T, st *store.Store, f store.Finding, scope string, citations []store.FindingCitation) store.Finding {
	t.Helper()
	f.Scope = scope
	f.Citations = store.EncodeFindingCitations(citations)
	saved, err := st.UpsertFinding(context.Background(), f)
	if err != nil {
		t.Fatalf("set evidence of finding %s: %v", f.ID, err)
	}
	return saved
}

func TestPublishCrossFileFindingOnceInSummary(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	design := seedFinding(t, st, pr.ID, run.ID, "diseno", 2, "major", store.FindingOpen)
	design.Title = "Límite entre módulos"
	design = setFindingEvidence(t, st, design, store.FindingScopeFiles, []store.FindingCitation{
		{Path: "src/a.go", StartLine: 2, EndLine: 2},
		{Path: "src/b.go", StartLine: 10, EndLine: 12},
	})
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}

	gh.mu.Lock()
	reviews := len(gh.reviews)
	gh.mu.Unlock()
	if reviews != 0 {
		t.Fatalf("reviews = %d, want 0: a finding that spans files has no single inline anchor", reviews)
	}
	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	section := summaryFindingsSection(t, bodies[0])
	if got := strings.Count(section, "Límite entre módulos"); got != 1 {
		t.Errorf("finding appears %d times in the summary, want once: %q", got, section)
	}
	for _, want := range []string{"src/a.go:2", "src/b.go:10-12"} {
		if !strings.Contains(section, want) {
			t.Errorf("summary section %q does not cite %q", section, want)
		}
	}
	saved, err := st.Finding(ctx, design.ID)
	if err != nil {
		t.Fatalf("Finding() error = %v", err)
	}
	if saved.GithubCommentID != 0 {
		t.Errorf("github comment id = %d, want no inline comment", saved.GithubCommentID)
	}
	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	for _, patch := range patches {
		if len(patch.Output.Annotations) != 0 {
			t.Errorf("check annotations = %+v, want none for a cross-file finding", patch.Output.Annotations)
		}
	}
}

func TestPublishDeletionOnlyFindingInSummary(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	deletion := seedFinding(t, st, pr.ID, run.ID, "eliminacion", 2, "major", store.FindingOpen)
	deletion.Title = "Se perdió la validación"
	deletion = setFindingEvidence(t, st, deletion, store.FindingScopeFile, []store.FindingCitation{
		{Path: "src/a.go", StartLine: 2, EndLine: 2, Side: store.FindingCitationBase},
	})
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	reviews := len(gh.reviews)
	gh.mu.Unlock()
	if reviews != 0 {
		t.Fatalf("reviews = %d, want 0: removed code cannot be anchored inline", reviews)
	}
	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	section := summaryFindingsSection(t, bodies[0])
	if !strings.Contains(section, "src/a.go:2 (código anterior)") {
		t.Errorf("summary section %q does not keep the previous-code citation", section)
	}
	if got := strings.Count(section, "Se perdió la validación"); got != 1 {
		t.Errorf("finding appears %d times in the summary, want once", got)
	}
	gh.mu.Lock()
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	for _, patch := range patches {
		if len(patch.Output.Annotations) != 0 {
			t.Errorf("check annotations = %+v, want none for a deletion-only finding", patch.Output.Annotations)
		}
	}
}

func TestPublishInlineRejectionFallsBackToSummary(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	finding := seedFinding(t, st, pr.ID, run.ID, "rechazado", 2, "major", store.FindingOpen)
	finding.Title = "Comentario rechazado"
	finding = setFindingEvidence(t, st, finding, store.FindingScopeFile, []store.FindingCitation{
		{Path: "src/a.go", StartLine: 2, EndLine: 2},
	})
	if err := st.DB.WithContext(ctx).Model(&store.Finding{}).Where("id = ?", finding.ID).
		Updates(map[string]any{
			"status": store.FindingOpen, "triage_verdict": store.VerdictConfirmed,
			"triaged_by": "alice", "triage_source": store.TriageSourceHuman,
		}).Error; err != nil {
		t.Fatalf("record human decision: %v", err)
	}
	gh := newFakeGitHub(t)
	gh.reviewCreateStatus = http.StatusUnprocessableEntity
	w, _ := newPublishWorker(t, st, gh)

	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}

	gh.mu.Lock()
	reviews, comments := len(gh.reviews), len(gh.comments)
	gh.mu.Unlock()
	if reviews != 0 {
		t.Fatalf("reviews = %d, want 0 after GitHub rejected the review", reviews)
	}
	if comments != 1 {
		t.Fatalf("summary comments = %d, want one edited summary", comments)
	}
	bodies := summaryBodies(t, gh)
	section := summaryFindingsSection(t, bodies[0])
	if got := strings.Count(section, "Comentario rechazado"); got != 1 {
		t.Errorf("finding appears %d times in the summary, want once: %q", got, section)
	}
	saved, err := st.Finding(ctx, finding.ID)
	if err != nil {
		t.Fatalf("Finding() error = %v", err)
	}
	if saved.GithubCommentID != 0 {
		t.Errorf("github comment id = %d, want no inline comment after rejection", saved.GithubCommentID)
	}
	if saved.Status != store.FindingOpen || saved.TriageVerdict != store.VerdictConfirmed ||
		saved.TriagedBy != "alice" {
		t.Errorf("human verdict was lost after rejection: %+v", saved)
	}
}

func TestPublishRegularFindingWithCitationInlineOnce(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")
	finding := seedFinding(t, st, pr.ID, run.ID, "cita", 2, "major", store.FindingOpen)
	finding = setFindingEvidence(t, st, finding, store.FindingScopeFile, []store.FindingCitation{
		{Path: "src/a.go", StartLine: 2, EndLine: 2, Side: store.FindingCitationHead},
	})
	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)

	if err := w.Publish(ctx, run.ID); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	gh.mu.Lock()
	reviews := len(gh.reviews)
	gh.mu.Unlock()
	if reviews != 1 || len(gh.reviews[0].Comments) != 1 {
		t.Fatalf("reviews = %+v, want one inline comment", gh.reviews)
	}
	if !strings.Contains(gh.reviews[0].Comments[0].Body, "Citas: src/a.go:2") {
		t.Errorf("inline body = %q, want the citation", gh.reviews[0].Comments[0].Body)
	}
	bodies := summaryBodies(t, gh)
	if section := summaryFindingsSection(t, bodies[0]); strings.Contains(section, "Título cita") {
		t.Errorf("summary duplicates an inline finding: %q", section)
	}
}

func TestPublishArchitectureFindingOnceInSummary(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "nit")
	verdict := store.ArchitectureVerdict{
		Requested: true, Status: store.ArchitectureComplete, Mode: store.ArchitectureAnalysisObservation,
		Signals: []string{"module_boundary"},
		Scope: store.ArchitectureScope{Included: []store.ArchitectureScopeFile{
			{Path: "src/a.go"}, {Path: "src/b.go"},
		}},
		Risks: []string{
			"El contrato cambia en `src/a.go:2` y `src/b.go:10-12`",
		},
		Recommendation: "Separar el límite entre los módulos",
	}
	if err := st.DB.WithContext(ctx).Model(&store.ReviewRun{}).Where("id = ?", run.ID).
		Update("architecture_analysis", datatypes.JSON(store.ArchitectureVerdictJSON(verdict))).Error; err != nil {
		t.Fatalf("seed architecture analysis: %v", err)
	}
	produced := review.ArchitectureFindings(verdict)
	if len(produced) != 1 {
		t.Fatalf("producer findings = %d, want 1", len(produced))
	}
	first, _ := review.Reconcile(nil, review.AgentOutput{Findings: produced}, run.ID, true, pr.ID, nil)
	if len(first) != 1 {
		t.Fatalf("reconciled findings = %d, want 1", len(first))
	}
	if _, err := st.UpsertFinding(ctx, first[0]); err != nil {
		t.Fatalf("persist produced finding: %v", err)
	}
	second, _ := review.Reconcile(first, review.AgentOutput{Findings: produced}, run.ID+1, true, pr.ID, nil)
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("identity changed across runs: %+v", second)
	}

	gh := newFakeGitHub(t)
	w, _ := newPublishWorker(t, st, gh)
	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}

	gh.mu.Lock()
	reviews := len(gh.reviews)
	patches := append([]checkPatch(nil), gh.checkPatches...)
	gh.mu.Unlock()
	if reviews != 0 {
		t.Fatalf("reviews = %d, want no inline comment for an advisory design finding", reviews)
	}
	for _, patch := range patches {
		if patch.Conclusion == "failure" {
			t.Error("advisory finding blocked the check")
		}
		if len(patch.Output.Annotations) != 0 {
			t.Errorf("annotations = %+v, want none", patch.Output.Annotations)
		}
	}
	bodies := summaryBodies(t, gh)
	if len(bodies) != 1 {
		t.Fatalf("summary comments = %d, want 1", len(bodies))
	}
	section := summaryFindingsSection(t, bodies[0])
	if got := strings.Count(section, produced[0].Title); got != 1 {
		t.Errorf("advisory finding appears %d times, want once: %q", got, section)
	}
	for _, want := range []string{"src/a.go:2", "src/b.go:10-12", "observación"} {
		if !strings.Contains(section, want) {
			t.Errorf("summary section %q does not contain %q", section, want)
		}
	}
	if got := strings.Count(section, "- "+verdict.Risks[0]); got != 1 {
		t.Errorf("risk bullet appears %d times in the advisory finding, want once", got)
	}
	if strings.Contains(bodies[0], "**Riesgos y compatibilidad:**") {
		t.Error("the architecture block repeats the risk list already carried by the advisory finding")
	}
	saved, err := st.Finding(ctx, first[0].ID)
	if err != nil {
		t.Fatalf("Finding() error = %v", err)
	}
	if !saved.Advisory || saved.GithubCommentID != 0 {
		t.Errorf("saved advisory/comment = %v/%d, want true/0", saved.Advisory, saved.GithubCommentID)
	}
	if len(saved.Verification) != 0 {
		t.Errorf("advisory verification = %s, want no automatic verdict", saved.Verification)
	}
}

func TestPublishCombinedDesignFindingInlineRejection(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	run, pr := seedPublishContext(t, st, store.RunSucceeded, "major")

	design := seedFinding(t, st, pr.ID, run.ID, "combinado", 2, "major", store.FindingOpen)
	design.Title = "Diseño combinado"
	design = setFindingEvidence(t, st, design, store.FindingScopeFiles, []store.FindingCitation{
		{Path: "src/a.go", StartLine: 2, EndLine: 2, Side: store.FindingCitationHead},
		{Path: "src/b.go", StartLine: 10, EndLine: 10, Side: store.FindingCitationBase},
	})
	seedFinding(t, st, pr.ID, run.ID, "normal", 2, "major", store.FindingOpen)

	advisory := store.Finding{
		ID: "observacion", PullRequestID: pr.ID, Fingerprint: "fp-observacion",
		Severity: "minor", Category: "design", Title: "Observación arquitectónica",
		Status: store.FindingOpen, Scope: store.FindingScopePR, Advisory: true,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}
	if _, err := st.UpsertFinding(ctx, advisory); err != nil {
		t.Fatalf("persist advisory finding: %v", err)
	}

	gh := newFakeGitHub(t)
	gh.reviewCreateStatus = http.StatusUnprocessableEntity
	w, _ := newPublishWorker(t, st, gh)
	for i := 0; i < 2; i++ {
		if err := w.Publish(ctx, run.ID); err != nil {
			t.Fatalf("Publish() #%d error = %v", i+1, err)
		}
	}

	gh.mu.Lock()
	reviews, comments := len(gh.reviews), len(gh.comments)
	gh.mu.Unlock()
	if reviews != 0 {
		t.Fatalf("reviews = %d, want the rejected inline review not to exist", reviews)
	}
	if comments != 1 {
		t.Fatalf("summary comments = %d, want one edited summary", comments)
	}
	bodies := summaryBodies(t, gh)
	section := summaryFindingsSection(t, bodies[0])
	for _, want := range []string{
		"Diseño combinado", "src/a.go:2, src/b.go:10 (código anterior)",
		"Título normal", "Observación arquitectónica",
	} {
		if got := strings.Count(section, want); got != 1 {
			t.Errorf("%q appears %d times in the summary, want once: %q", want, got, section)
		}
	}
	savedAdvisory, err := st.Finding(ctx, advisory.ID)
	if err != nil {
		t.Fatalf("Finding(advisory) error = %v", err)
	}
	if savedAdvisory.GithubCommentID != 0 || len(savedAdvisory.Verification) != 0 {
		t.Errorf("advisory = comment %d / verification %s, want no inline comment and no verdict",
			savedAdvisory.GithubCommentID, savedAdvisory.Verification)
	}
	savedDesign, err := st.Finding(ctx, design.ID)
	if err != nil {
		t.Fatalf("Finding(design) error = %v", err)
	}
	if savedDesign.GithubCommentID != 0 {
		t.Errorf("design finding comment = %d, want summary-only", savedDesign.GithubCommentID)
	}
}
