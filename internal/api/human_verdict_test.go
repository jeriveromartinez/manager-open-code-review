package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func newHumanVerdictFixture(t *testing.T, runStatus string) (*secureFixture, store.PullRequest, store.ReviewRun, store.Installation) {
	t.Helper()

	f := newSecureFixture(t)
	inst := f.installation(1501)
	team := f.team("human-verdict-team")
	repo := f.repo(7501, inst.ID, "acme/human", &team.ID, true)
	pr := f.pr(repo.ID, 51, store.StatusInconclusive)
	if err := f.st.DB.WithContext(f.ctx).Model(&store.PullRequest{}).Where("id = ?", pr.ID).
		Updates(map[string]any{
			"head_sha":       "human-head",
			"base_ref":       "main",
			"base_sha":       "tip-1",
			"merge_base_sha": "mb-1",
		}).Error; err != nil {
		t.Fatalf("set comparison: %v", err)
	}
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "human", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	run, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: "human-head",
		BaseRef: "main", Status: runStatus,
		Coverage: datatypes.JSON([]byte(`{}`)), Usage: datatypes.JSON([]byte(`{}`)),
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	pr, err = f.st.PullRequest(f.ctx, pr.ID)
	if err != nil {
		t.Fatalf("reload pull request: %v", err)
	}
	return f, pr, run, inst
}

func humanVerdictBody() map[string]any {
	return map[string]any{
		"decision":   "approved",
		"conclusion": "Revisé el rango que el motor no cubrió",
		"reason":     "el archivo quedó pendiente por timeout",
		"scope": map[string]any{
			"files": []map[string]any{
				{"path": "internal/auth.go", "start_line": 10, "end_line": 12},
			},
			"pending_findings": []string{},
			"complete":         true,
		},
	}
}

func TestRecordHumanVerdictIsShownSeparately(t *testing.T) {
	f, pr, run, _ := newHumanVerdictFixture(t, store.RunInconclusive)
	lead := f.user("human-lead", false)
	f.member(findingTeamID(t, f, pr), lead.ID, "lead")

	rec := f.request(http.MethodPost, fmt.Sprintf("/api/prs/%d/human-verdict", pr.ID), humanVerdictBody(), f.session(lead))
	assertStatus(t, rec, http.StatusOK)

	var body map[string]any
	decodeJSON(t, rec, &body)
	if body["decision"] != store.HumanVerdictApproved || body["actor_login"] != lead.Login {
		t.Errorf("response = %v, want an approved verdict by the lead", body)
	}
	if body["current"] != true {
		t.Errorf("current = %v, want true", body["current"])
	}

	// The automated result is untouched: recording a human verdict never rewrites
	// the run.
	saved, err := f.st.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if saved.Status != store.RunInconclusive {
		t.Errorf("run status = %q, want %q", saved.Status, store.RunInconclusive)
	}

	var audit store.AuditLog
	if err := f.st.DB.WithContext(f.ctx).
		Where("action = ? AND target_id = ?", "human_verdict.recorded", fmt.Sprint(pr.ID)).
		Take(&audit).Error; err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if audit.ActorUserID == nil || *audit.ActorUserID != lead.ID {
		t.Errorf("audit actor = %v, want %d", audit.ActorUserID, lead.ID)
	}

	// The detail returns the human decision next to, not instead of, the run.
	rec = f.request(http.MethodGet, fmt.Sprintf("/api/prs/%d", pr.ID), nil, f.session(lead))
	assertStatus(t, rec, http.StatusOK)
	var detail map[string]any
	decodeJSON(t, rec, &detail)
	human, ok := detail["human_verdict"].(map[string]any)
	if !ok {
		t.Fatalf("detail human_verdict = %v, want an object", detail["human_verdict"])
	}
	if human["decision"] != store.HumanVerdictApproved || human["current"] != true {
		t.Errorf("human_verdict = %v, want a current approval", human)
	}
	scope, ok := human["scope"].(map[string]any)
	if !ok || scope["complete"] != true {
		t.Errorf("scope = %v, want complete", human["scope"])
	}
	runs := detail["runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["status"] != store.RunInconclusive {
		t.Errorf("runs = %v, want the unchanged inconclusive run", runs)
	}
}

func TestRecordHumanVerdictRequiresLeadQAOrAdmin(t *testing.T) {
	f, pr, _, _ := newHumanVerdictFixture(t, store.RunInconclusive)
	dev := f.user("human-dev", false)
	f.member(findingTeamID(t, f, pr), dev.ID, "developer")
	outsider := f.user("human-outsider", false)

	path := fmt.Sprintf("/api/prs/%d/human-verdict", pr.ID)
	for name, user := range map[string]store.User{"developer": dev, "outsider": outsider} {
		t.Run(name, func(t *testing.T) {
			rec := f.request(http.MethodPost, path, humanVerdictBody(), f.session(user))
			assertStatus(t, rec, http.StatusForbidden)
		})
	}
	if _, ok, err := f.st.CurrentHumanVerdict(f.ctx, pr); err != nil || ok {
		t.Fatalf("CurrentHumanVerdict = %v, %v; want no verdict", ok, err)
	}
}

func TestRecordHumanVerdictRejectsAGenericApproval(t *testing.T) {
	f, pr, _, _ := newHumanVerdictFixture(t, store.RunInconclusive)
	admin := f.user("human-admin", true)
	body := humanVerdictBody()
	body["scope"] = map[string]any{"files": []any{}, "pending_findings": []string{}, "complete": false}

	rec := f.request(http.MethodPost, fmt.Sprintf("/api/prs/%d/human-verdict", pr.ID), body, f.session(admin))
	assertStatus(t, rec, http.StatusBadRequest)
	if _, ok, err := f.st.CurrentHumanVerdict(f.ctx, pr); err != nil || ok {
		t.Fatalf("CurrentHumanVerdict = %v, %v; want no verdict", ok, err)
	}
}

func TestRecordHumanVerdictConflictsWithAConclusiveRun(t *testing.T) {
	f, pr, _, _ := newHumanVerdictFixture(t, store.RunSucceeded)
	admin := f.user("human-admin-done", true)

	rec := f.request(http.MethodPost, fmt.Sprintf("/api/prs/%d/human-verdict", pr.ID), humanVerdictBody(), f.session(admin))
	assertStatus(t, rec, http.StatusConflict)
	if _, ok, err := f.st.CurrentHumanVerdict(f.ctx, pr); err != nil || ok {
		t.Fatalf("CurrentHumanVerdict = %v, %v; want no verdict", ok, err)
	}
}

func TestHumanVerdictIsObsoleteAfterAPush(t *testing.T) {
	f, pr, _, _ := newHumanVerdictFixture(t, store.RunInconclusive)
	lead := f.user("human-lead-push", false)
	f.member(findingTeamID(t, f, pr), lead.ID, "lead")
	if rec := f.request(http.MethodPost, fmt.Sprintf("/api/prs/%d/human-verdict", pr.ID), humanVerdictBody(), f.session(lead)); rec.Code != http.StatusOK {
		t.Fatalf("record verdict status = %d, body %s", rec.Code, rec.Body.String())
	}

	next := pr
	next.HeadSHA = "human-head-2"
	next.GitHubUpdatedAt = pr.GitHubUpdatedAt.Add(time.Minute)
	if _, changed, err := f.st.SyncPullRequest(f.ctx, next); err != nil || !changed {
		t.Fatalf("SyncPullRequest(push) changed=%v err=%v", changed, err)
	}

	rec := f.request(http.MethodGet, fmt.Sprintf("/api/prs/%d", pr.ID), nil, f.session(lead))
	assertStatus(t, rec, http.StatusOK)
	var detail map[string]any
	decodeJSON(t, rec, &detail)
	human := detail["human_verdict"].(map[string]any)
	if human["current"] != false || human["invalidated_reason"] != store.HumanVerdictInvalidatedHeadMoved {
		t.Errorf("human_verdict = %v, want the push to invalidate it", human)
	}
	if detail["qa_status"] != store.StatusPending {
		t.Errorf("qa_status = %v, want %q after a push", detail["qa_status"], store.StatusPending)
	}
}
