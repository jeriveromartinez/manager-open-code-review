package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestActiveRunsAreScopedAndCarryThePhase(t *testing.T) {
	f := newSecureFixture(t)
	instA := f.installation(1201)
	instB := f.installation(1202)
	teamA := f.team("live-alpha")
	teamB := f.team("live-beta")
	repoA := f.repo(7201, instA.ID, "acme/live-a", &teamA.ID, true)
	repoB := f.repo(7202, instB.ID, "acme/live-b", &teamB.ID, true)
	devA := f.user("live-deva", false)
	f.member(teamA.ID, devA.ID, "developer")
	outsider := f.user("live-outsider", false)
	admin := f.user("live-admin", true)

	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "live-profile", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	prA := f.pr(repoA.ID, 31, store.StatusPending)
	prB := f.pr(repoB.ID, 32, store.StatusPending)
	queued, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: prA.ID, ProfileID: profile.ID, HeadSHA: "sha-a", Trigger: "webhook",
	})
	if err != nil {
		t.Fatalf("CreateReviewRun(queued): %v", err)
	}
	started := time.Now().UTC().Add(-time.Minute)
	running, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: prB.ID, ProfileID: profile.ID, HeadSHA: "sha-b",
		Trigger: "manual", Status: store.RunRunning, StartedAt: &started,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun(running): %v", err)
	}
	if err := f.st.SetReviewRunPhase(f.ctx, running.ID, store.RunPhaseReviewing); err != nil {
		t.Fatalf("SetReviewRunPhase: %v", err)
	}

	type runEntry struct {
		ID             uint    `json:"id"`
		PullRequestID  uint    `json:"pull_request_id"`
		PRNumber       int     `json:"pr_number"`
		PRTitle        string  `json:"pr_title"`
		Repo           string  `json:"repo"`
		Team           string  `json:"team"`
		Profile        string  `json:"profile"`
		Trigger        string  `json:"trigger"`
		Status         string  `json:"status"`
		Attempt        int     `json:"attempt"`
		Phase          string  `json:"phase"`
		PhaseStartedAt *string `json:"phase_started_at"`
		StartedAt      *string `json:"started_at"`
		CreatedAt      string  `json:"created_at"`
	}
	type runsBody struct {
		Runs []runEntry `json:"runs"`
	}

	rec := f.request(http.MethodGet, "/api/review-runs", nil, f.session(devA))
	assertStatus(t, rec, http.StatusOK)
	var body runsBody
	decodeJSON(t, rec, &body)
	if len(body.Runs) != 1 || body.Runs[0].ID != queued.ID {
		t.Fatalf("dev runs = %+v, want only the queued run of their team", body.Runs)
	}
	got := body.Runs[0]
	if got.Repo != "acme/live-a" || got.PRNumber != 31 || got.Team != teamA.Name {
		t.Errorf("dev run = %+v, want acme/live-a #31 of %q", got, teamA.Name)
	}
	if got.Phase != "" || got.PhaseStartedAt != nil {
		t.Errorf("queued run phase = %q/%v, want empty", got.Phase, got.PhaseStartedAt)
	}
	if strings.Contains(rec.Body.String(), "clone_token_enc") {
		t.Errorf("response leaks clone_token_enc: %s", rec.Body.String())
	}

	rec = f.request(http.MethodGet, "/api/review-runs", nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &body)
	if len(body.Runs) != 2 {
		t.Fatalf("admin runs = %+v, want both", body.Runs)
	}
	if body.Runs[0].ID != running.ID || body.Runs[0].Status != store.RunRunning {
		t.Fatalf("first admin run = %+v, want the running one first", body.Runs[0])
	}
	if body.Runs[0].Phase != store.RunPhaseReviewing || body.Runs[0].PhaseStartedAt == nil || body.Runs[0].StartedAt == nil {
		t.Errorf("running run = %+v, want phase and times", body.Runs[0])
	}
	if body.Runs[0].Profile != profile.Name || body.Runs[0].Team != teamB.Name {
		t.Errorf("running run = %+v, want profile %q and team %q", body.Runs[0], profile.Name, teamB.Name)
	}

	rec = f.request(http.MethodGet, "/api/review-runs", nil, f.session(outsider))
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &body)
	if body.Runs == nil || len(body.Runs) != 0 {
		t.Errorf("outsider runs = %+v, want an empty list", body.Runs)
	}
	if !strings.Contains(rec.Body.String(), `"runs":[]`) {
		t.Errorf("outsider body = %s, want an empty array", rec.Body.String())
	}
}

type historyRunEntry struct {
	ID              uint    `json:"id"`
	PullRequestID   uint    `json:"pull_request_id"`
	PRNumber        int     `json:"pr_number"`
	PRTitle         string  `json:"pr_title"`
	Repo            string  `json:"repo"`
	Team            string  `json:"team"`
	Profile         string  `json:"profile"`
	Trigger         string  `json:"trigger"`
	Status          string  `json:"status"`
	Attempt         int     `json:"attempt"`
	Phase           string  `json:"phase"`
	PhaseStartedAt  *string `json:"phase_started_at"`
	StartedAt       *string `json:"started_at"`
	FinishedAt      *string `json:"finished_at"`
	DurationSeconds *int64  `json:"duration_seconds"`
	ErrorCode       string  `json:"error_code"`
	ErrorMessage    string  `json:"error_message"`
	CreatedAt       string  `json:"created_at"`
}

type historyBody struct {
	Runs          []historyRunEntry `json:"runs"`
	NextBefore    int64             `json:"next_before"`
	NextCreatedAt *string           `json:"next_created_at"`
	HasMore       bool              `json:"has_more"`
}

// seedHistoryRun inserts a run with an explicit creation time so ordering does
// not depend on how fast the inserts happen.
func seedHistoryRun(t *testing.T, f *apiFixture, prID uint, status, trigger string, createdAt time.Time,
	startedAt, finishedAt *time.Time, code, message string) store.ReviewRun {
	t.Helper()

	run := store.ReviewRun{
		PullRequestID: prID, HeadSHA: "history-sha", Status: status, Trigger: trigger,
		Attempt: 2, CreatedAt: createdAt, StartedAt: startedAt, FinishedAt: finishedAt,
		ErrorCode: code, ErrorMessage: message,
	}
	if err := f.st.DB.WithContext(f.ctx).Create(&run).Error; err != nil {
		t.Fatalf("seed %s history run: %v", status, err)
	}
	return run
}

func historyIDs(runs []historyRunEntry) []uint {
	ids := make([]uint, len(runs))
	for i, run := range runs {
		ids[i] = run.ID
	}
	return ids
}

func TestRunHistoryReturnsTerminalRunsForTheUserTeams(t *testing.T) {
	f := newAPIFixture(t)
	inst := f.installation(2201)
	teamA := f.team("history-alpha")
	teamB := f.team("history-beta")
	repoA := f.repo(7701, inst.ID, "acme/history-a", &teamA.ID, true)
	repoB := f.repo(7702, inst.ID, "acme/history-b", &teamB.ID, true)
	devA := f.user("history-deva", false)
	f.member(teamA.ID, devA.ID, "developer")

	base := time.Now().UTC().Truncate(time.Millisecond)
	okStarted := base.Add(-10 * time.Minute)
	okFinished := okStarted.Add(90 * time.Second)
	okRun := seedHistoryRun(t, f, f.pr(repoA.ID, 41, store.StatusPending).ID,
		store.RunSucceeded, "webhook", base.Add(-9*time.Minute), &okStarted, &okFinished, "", "")

	failedStarted := base.Add(-8 * time.Minute)
	failedFinished := failedStarted.Add(3 * time.Minute)
	failedRun := seedHistoryRun(t, f, f.pr(repoA.ID, 42, store.StatusPending).ID,
		store.RunFailed, "manual", base.Add(-7*time.Minute), &failedStarted, &failedFinished,
		"clone_failed", "could not clone the repository")

	noTimeRun := seedHistoryRun(t, f, f.pr(repoA.ID, 43, store.StatusPending).ID,
		store.RunSuperseded, "webhook", base.Add(-6*time.Minute), nil, nil, "", "")

	// Active runs belong to the live view and another team's runs stay hidden.
	seedHistoryRun(t, f, f.pr(repoA.ID, 44, store.StatusPending).ID,
		store.RunRunning, "webhook", base.Add(-5*time.Minute), &okStarted, nil, "", "")
	seedHistoryRun(t, f, f.pr(repoB.ID, 45, store.StatusPending).ID,
		store.RunFailed, "webhook", base.Add(-4*time.Minute), &okStarted, &okFinished, "boom", "other team")

	rec := f.request(http.MethodGet, "/api/review-runs/history", nil, f.session(devA))
	assertStatus(t, rec, http.StatusOK)
	var body historyBody
	decodeJSON(t, rec, &body)

	wantOrder := []uint{noTimeRun.ID, failedRun.ID, okRun.ID}
	if got := historyIDs(body.Runs); fmt.Sprint(got) != fmt.Sprint(wantOrder) {
		t.Fatalf("run ids = %v, want newest first %v", got, wantOrder)
	}
	if body.HasMore {
		t.Error("has_more = true, want false for a single page")
	}
	if body.NextBefore != int64(okRun.ID) {
		t.Errorf("next_before = %d, want the last returned id %d", body.NextBefore, okRun.ID)
	}
	if body.NextCreatedAt != nil {
		t.Errorf("next_created_at = %q, want null when there is no next page", *body.NextCreatedAt)
	}

	failed := body.Runs[1]
	if failed.Repo != "acme/history-a" || failed.Team != teamA.Name || failed.PRNumber != 42 {
		t.Errorf("failed run = %+v, want acme/history-a #42 of %q", failed, teamA.Name)
	}
	if failed.Status != store.RunFailed || failed.Trigger != "manual" || failed.Attempt != 2 {
		t.Errorf("failed run = %+v, want failed/manual/attempt 2", failed)
	}
	if failed.ErrorCode != "clone_failed" || failed.ErrorMessage != "could not clone the repository" {
		t.Errorf("failed run error = %q/%q, want the stored code and message", failed.ErrorCode, failed.ErrorMessage)
	}
	if want := failedStarted.Format(time.RFC3339); failed.StartedAt == nil || *failed.StartedAt != want {
		t.Errorf("started_at = %v, want %q", failed.StartedAt, want)
	}
	if want := failedFinished.Format(time.RFC3339); failed.FinishedAt == nil || *failed.FinishedAt != want {
		t.Errorf("finished_at = %v, want %q", failed.FinishedAt, want)
	}
	if failed.DurationSeconds == nil || *failed.DurationSeconds != 180 {
		t.Errorf("duration_seconds = %v, want 180", failed.DurationSeconds)
	}
	if failed.Phase != "" || failed.PhaseStartedAt != nil {
		t.Errorf("failed run phase = %q/%v, want the empty active-run shape", failed.Phase, failed.PhaseStartedAt)
	}

	noTimes := body.Runs[0]
	if noTimes.StartedAt != nil || noTimes.FinishedAt != nil || noTimes.DurationSeconds != nil {
		t.Errorf("run without timestamps = %+v, want null start, finish and duration", noTimes)
	}
	if ok := body.Runs[2]; ok.DurationSeconds == nil || *ok.DurationSeconds != 90 {
		t.Errorf("succeeded duration_seconds = %v, want 90", ok.DurationSeconds)
	}

	// The row keeps the active-run shape so the panel can merge both lists.
	var raw map[string]any
	decodeJSON(t, rec, &raw)
	assertKeys(t, raw, "runs", "next_before", "next_created_at", "has_more")
	rawRuns, ok := raw["runs"].([]any)
	if !ok || len(rawRuns) != 3 {
		t.Fatalf("raw runs = %v, want the three rows", raw["runs"])
	}
	assertKeys(t, rawRuns[0].(map[string]any),
		"phase", "phase_started_at", "error_code", "error_message", "finished_at", "duration_seconds")

	rec = f.request(http.MethodGet,
		"/api/review-runs/history?status=failed&repo=acme/history-a&trigger=manual", nil, f.session(devA))
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &body)
	if ids := historyIDs(body.Runs); len(ids) != 1 || ids[0] != failedRun.ID {
		t.Errorf("filtered runs = %v, want only the failed run %d", ids, failedRun.ID)
	}
}

func TestRunHistoryRejectsInvalidParams(t *testing.T) {
	f := newAPIFixture(t)
	cookie := f.session(f.user("history-admin", true))

	for _, query := range []string{
		"before=-1",
		"before=abc",
		"limit=0",
		"limit=201",
		"limit=abc",
		"status=bogus",
		"status=failed,bogus",
		"repo=0",
		"before=5&before_created_at=not-a-time",
		"before_created_at=2026-01-02T03:04:05Z",
	} {
		t.Run(query, func(t *testing.T) {
			rec := f.request(http.MethodGet, "/api/review-runs/history?"+query, nil, cookie)
			assertStatus(t, rec, http.StatusBadRequest)
			var body map[string]string
			decodeJSON(t, rec, &body)
			if body["error"] == "" {
				t.Errorf("error body = %v, want a message", body)
			}
		})
	}

	for _, query := range []string{"before=0&limit=1", "limit=200", "status=failed&status=succeeded"} {
		t.Run("accepts "+query, func(t *testing.T) {
			rec := f.request(http.MethodGet, "/api/review-runs/history?"+query, nil, cookie)
			assertStatus(t, rec, http.StatusOK)
		})
	}
}

func TestRunHistoryPaginatesByBefore(t *testing.T) {
	f := newAPIFixture(t)
	inst := f.installation(2301)
	team := f.team("history-pages")
	repo := f.repo(7801, inst.ID, "acme/history-pages", &team.ID, true)
	cookie := f.session(f.user("history-admin", true))

	base := time.Now().UTC().Truncate(time.Millisecond)
	// The last inserted run gets the newest id but the oldest creation time, so
	// an id-only cursor would skip it. Creation order: runs[2], runs[1],
	// runs[0], runs[3].
	createdAt := []time.Time{
		base.Add(-5 * time.Minute),
		base.Add(-4 * time.Minute),
		base.Add(-3 * time.Minute),
		base.Add(-6 * time.Minute),
	}
	runs := make([]store.ReviewRun, len(createdAt))
	for i, at := range createdAt {
		runs[i] = seedHistoryRun(t, f, f.pr(repo.ID, 50+i, store.StatusPending).ID,
			store.RunSucceeded, "webhook", at, nil, nil, "", "")
	}

	var seen []uint
	rec := f.request(http.MethodGet, "/api/review-runs/history?limit=2", nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var page historyBody
	decodeJSON(t, rec, &page)
	if got := historyIDs(page.Runs); fmt.Sprint(got) != fmt.Sprint([]uint{runs[2].ID, runs[1].ID}) {
		t.Fatalf("page 1 = %v, want the two newest by creation time", got)
	}
	if !page.HasMore || page.NextBefore != int64(runs[1].ID) {
		t.Fatalf("page 1 cursor = %d/%v, want %d/true", page.NextBefore, page.HasMore, runs[1].ID)
	}
	if want := createdAt[1].Format(time.RFC3339Nano); page.NextCreatedAt == nil || *page.NextCreatedAt != want {
		t.Fatalf("page 1 next_created_at = %v, want %q", page.NextCreatedAt, want)
	}
	seen = append(seen, historyIDs(page.Runs)...)

	rec = f.request(http.MethodGet, fmt.Sprintf(
		"/api/review-runs/history?before=%d&before_created_at=%s&limit=2",
		page.NextBefore, *page.NextCreatedAt), nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &page)
	if got := historyIDs(page.Runs); fmt.Sprint(got) != fmt.Sprint([]uint{runs[0].ID, runs[3].ID}) {
		t.Fatalf("page 2 = %v, want the remaining runs including the outlier %d", got, runs[3].ID)
	}
	if page.HasMore || page.NextBefore != int64(runs[3].ID) {
		t.Fatalf("page 2 cursor = %d/%v, want %d/false", page.NextBefore, page.HasMore, runs[3].ID)
	}
	if page.NextCreatedAt != nil {
		t.Errorf("page 2 next_created_at = %q, want null without a next page", *page.NextCreatedAt)
	}
	seen = append(seen, historyIDs(page.Runs)...)

	unique := make(map[uint]bool, len(seen))
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("run %d repeated across pages", id)
		}
		unique[id] = true
	}
	if len(unique) != len(runs) {
		t.Errorf("pages covered %d runs, want all %d", len(unique), len(runs))
	}
}

func TestRunHistoryHidesOtherTeamsRuns(t *testing.T) {
	f := newAPIFixture(t)
	inst := f.installation(2401)
	teamA := f.team("history-a")
	teamB := f.team("history-b")
	repoA := f.repo(7901, inst.ID, "acme/history-mine", &teamA.ID, true)
	repoB := f.repo(7902, inst.ID, "acme/history-other", &teamB.ID, true)
	devA := f.user("history-member", false)
	f.member(teamA.ID, devA.ID, "developer")

	base := time.Now().UTC().Truncate(time.Millisecond)
	mine := seedHistoryRun(t, f, f.pr(repoA.ID, 61, store.StatusPending).ID,
		store.RunSucceeded, "webhook", base, nil, nil, "", "")
	theirs := seedHistoryRun(t, f, f.pr(repoB.ID, 62, store.StatusPending).ID,
		store.RunFailed, "webhook", base.Add(time.Minute), nil, nil, "boom", "other team")
	cookie := f.session(devA)

	rec := f.request(http.MethodGet, "/api/review-runs/history", nil, cookie)
	assertStatus(t, rec, http.StatusOK)
	var body historyBody
	decodeJSON(t, rec, &body)
	if ids := historyIDs(body.Runs); len(ids) != 1 || ids[0] != mine.ID {
		t.Fatalf("dev runs = %v, want only their own run %d", ids, mine.ID)
	}

	for _, repoFilter := range []string{
		fmt.Sprintf("%d", repoB.ID), // numeric id
		"acme/history-other",        // full name, as the panel sends it
	} {
		t.Run(repoFilter, func(t *testing.T) {
			rec := f.request(http.MethodGet, "/api/review-runs/history?repo="+repoFilter, nil, cookie)
			assertStatus(t, rec, http.StatusOK)
			if !strings.Contains(rec.Body.String(), `"runs":[]`) {
				t.Errorf("body = %s, want an empty runs array", rec.Body.String())
			}
			var page historyBody
			decodeJSON(t, rec, &page)
			if len(page.Runs) != 0 || page.HasMore || page.NextBefore != 0 {
				t.Errorf("page = %+v, want an empty page with no cursor", page)
			}
			if strings.Contains(rec.Body.String(), "boom") {
				t.Errorf("response leaks another team's run: %s", rec.Body.String())
			}
		})
	}

	adminCookie := f.session(f.user("history-root", true))
	rec = f.request(http.MethodGet, fmt.Sprintf("/api/review-runs/history?repo=%d", repoB.ID), nil, adminCookie)
	assertStatus(t, rec, http.StatusOK)
	decodeJSON(t, rec, &body)
	if ids := historyIDs(body.Runs); len(ids) != 1 || ids[0] != theirs.ID {
		t.Errorf("admin runs = %v, want the other team's run %d", ids, theirs.ID)
	}
}
