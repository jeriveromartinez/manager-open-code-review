package api_test

import (
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
