package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func TestPRDetailIncludesRunsAndFindings(t *testing.T) {
	f := newSecureFixture(t)
	inst := f.installation(1001)
	team := f.team("detail-team")
	repo := f.repo(6001, inst.ID, "acme/app", &team.ID, true)
	pr := f.pr(repo.ID, 42, store.StatusChangesRequested)
	dev := f.user("dev", false)
	f.member(team.ID, dev.ID, "developer")
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "primary", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	run, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: "abc123",
		Status: store.RunSucceeded, Summary: "todo ok",
		Coverage:      datatypes.JSON([]byte(`{"lines":80}`)),
		Usage:         datatypes.JSON([]byte(`{"tokens":10}`)),
		CloneTokenEnc: "sealed-clone-token",
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if _, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-1", PullRequestID: pr.ID, Fingerprint: "fp-1",
		Path: "main.go", StartLine: 10, EndLine: 12, Severity: "major",
		Category: "bug", Title: "nil deref", Body: "explanation", Status: store.FindingOpen,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}); err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}

	path := fmt.Sprintf("/api/prs/%d", pr.ID)
	rec := f.request(http.MethodGet, path, nil, f.session(dev))
	assertStatus(t, rec, http.StatusOK)

	var raw map[string]any
	decodeJSON(t, rec, &raw)
	assertKeys(t, raw, "id", "repo", "number", "title", "author_login", "html_url",
		"qa_status", "group", "validations", "updated_at", "runs", "findings")

	runs, ok := raw["runs"].([]any)
	if !ok || len(runs) != 1 {
		t.Fatalf("runs = %v, want one", raw["runs"])
	}
	runObj := runs[0].(map[string]any)
	if runObj["status"] != store.RunSucceeded || runObj["summary"] != "todo ok" {
		t.Errorf("run = %v, want succeeded/todo ok", runObj)
	}
	if coverage, ok := runObj["coverage"].(map[string]any); !ok || coverage["lines"] != float64(80) {
		t.Errorf("coverage = %v, want {\"lines\":80}", runObj["coverage"])
	}
	if _, ok := runObj["clone_token_enc"]; ok {
		t.Errorf("run leaks clone_token_enc: %v", runObj)
	}

	findings, ok := raw["findings"].([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings = %v, want one", raw["findings"])
	}
	finding := findings[0].(map[string]any)
	if finding["title"] != "nil deref" || finding["severity"] != "major" || finding["path"] != "main.go" {
		t.Errorf("finding = %v, want nil deref/major/main.go", finding)
	}

	body := rec.Body.String()
	for _, forbidden := range []string{"sealed-clone-token", "clone_token_enc", "api_key_enc"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("detail response leaks %q: %s", forbidden, body)
		}
	}

	outsider := f.user("outsider", false)
	rec = f.request(http.MethodGet, path, nil, f.session(outsider))
	assertStatus(t, rec, http.StatusNotFound)
}

func TestRetryCreatesRunAndEnqueues(t *testing.T) {
	f := newSecureFixture(t)
	inst := f.installation(1002)
	team := f.team("retry-team")
	repo := f.repo(6002, inst.ID, "acme/api", &team.ID, true)
	pr := f.pr(repo.ID, 7, store.StatusPending)
	lead := f.user("lead", false)
	f.member(team.ID, lead.ID, "lead")
	outsider := f.user("outsider", false)
	admin := f.user("admin", true)
	path := fmt.Sprintf("/api/prs/%d/retry", pr.ID)

	rec := f.request(http.MethodPost, path, nil, f.session(lead))
	assertStatus(t, rec, http.StatusAccepted)
	if ids := f.reviews.snapshot(); len(ids) != 1 || ids[0] != pr.ID {
		t.Fatalf("Retry calls = %v, want [%d]", ids, pr.ID)
	}

	rec = f.request(http.MethodPost, path, nil, f.session(outsider))
	assertStatus(t, rec, http.StatusForbidden)
	if ids := f.reviews.snapshot(); len(ids) != 1 {
		t.Errorf("Retry calls = %v, want only the lead's call", ids)
	}

	rec = f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusAccepted)
	if ids := f.reviews.snapshot(); len(ids) != 2 || ids[1] != pr.ID {
		t.Errorf("Retry calls = %v, want two calls for PR %d", ids, pr.ID)
	}

	rec = f.request(http.MethodPost, "/api/prs/999999/retry", nil, f.session(admin))
	assertStatus(t, rec, http.StatusNotFound)

	// Wiring not configured must not panic.
	f.deps.Reviews = nil
	f.rebuildRouter()
	rec = f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusServiceUnavailable)
}
