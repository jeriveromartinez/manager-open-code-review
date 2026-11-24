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
	replies            []string
	checkPatches       []checkPatch
	gqlQueries         []string
	nextCommentID      int64
	omitReviewComments bool
	// reviewCommentsWithoutLine mimics GitHub returning a just-created review's
	// comments with no line fields, only their position.
	reviewCommentsWithoutLine bool
	failResolveOnce           bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	return &fakeGitHub{t: t, files: defaultPRFiles, nextCommentID: 1000}
}

func (g *fakeGitHub) failNextResolve() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failResolveOnce = true
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

	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/repo/pulls/7/reviews":
		var review fakeReview
		_ = json.Unmarshal(body, &review)
		g.reviews = append(g.reviews, review)
		writeJSON(struct {
			ID int64 `json:"id"`
		}{ID: 900}, http.StatusOK)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/repo/pulls/7/reviews/"):
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
	raw, err := json.Marshal(map[string]string{
		"verdict": verdict, "evidence": "guard()", "head_sha": "abcdef1234567890",
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
