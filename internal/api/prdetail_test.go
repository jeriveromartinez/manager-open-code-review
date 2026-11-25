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
	tokens := func(v int64) *int64 { return &v }
	run, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: "abc123",
		Status: store.RunSucceeded, Summary: "todo ok",
		Coverage:         datatypes.JSON([]byte(`{"lines":80}`)),
		Usage:            datatypes.JSON([]byte(`{"tokens":10}`)),
		CloneTokenEnc:    "sealed-clone-token",
		InputTokens:      tokens(1200),
		OutputTokens:     tokens(300),
		ReasoningTokens:  tokens(250),
		CacheReadTokens:  tokens(5000),
		CacheWriteTokens: tokens(400),
		TotalTokens:      tokens(6900),
		MaxContextTokens: tokens(6600),
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
	// A run without a phase (queued, terminal or pre-#123) exposes an empty
	// string and a null start so the panel can render "—".
	if runObj["phase"] != "" {
		t.Errorf("run phase = %v, want empty", runObj["phase"])
	}
	if started, ok := runObj["phase_started_at"]; !ok || started != nil {
		t.Errorf("phase_started_at = %v (present %v), want null", started, ok)
	}
	if runObj["input_tokens"] != float64(1200) || runObj["output_tokens"] != float64(300) ||
		runObj["reasoning_tokens"] != float64(250) ||
		runObj["cache_read_tokens"] != float64(5000) || runObj["cache_write_tokens"] != float64(400) ||
		runObj["total_tokens"] != float64(6900) || runObj["max_context_tokens"] != float64(6600) {
		t.Errorf("token columns = %v, want 1200/300/250/5000/400/6900/6600", runObj)
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

func TestPRDetailShowsVerificationEvidence(t *testing.T) {
	f := newSecureFixture(t)
	inst := f.installation(1003)
	team := f.team("detail-triage")
	repo := f.repo(6003, inst.ID, "acme/verify", &team.ID, true)
	pr := f.pr(repo.ID, 43, store.StatusReadyForHuman)
	dev := f.user("dev", false)
	f.member(team.ID, dev.ID, "developer")

	verification := datatypes.JSON([]byte(`{"verdict":"refute","evidence":"if err != nil {","head_sha":"abc123","model":"nemotron","session_id":"verify-session","verified_at":"2026-11-23T10:00:00Z"}`))
	if _, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-verified", PullRequestID: pr.ID, Fingerprint: "fp-verified",
		Path: "main.go", StartLine: 4, EndLine: 4, Severity: "minor",
		Category: "bug", Title: "error ignorado", Body: "se ignora el error",
		Status: store.FindingWithdrawn, FirstSeenRunID: 1, LastSeenRunID: 1,
		Verification: verification, TriageSource: store.TriageSourceVerifier,
		TriageVerdict: store.VerdictFalsePositive, TriagedBy: "ocr-verify:nemotron",
	}); err != nil {
		t.Fatalf("UpsertFinding(verified): %v", err)
	}
	if _, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-plain", PullRequestID: pr.ID, Fingerprint: "fp-plain",
		Path: "main.go", StartLine: 9, EndLine: 9, Severity: "major",
		Category: "bug", Title: "nil deref", Status: store.FindingOpen,
		FirstSeenRunID: 1, LastSeenRunID: 1,
	}); err != nil {
		t.Fatalf("UpsertFinding(plain): %v", err)
	}

	rec := f.request(http.MethodGet, fmt.Sprintf("/api/prs/%d", pr.ID), nil, f.session(dev))
	assertStatus(t, rec, http.StatusOK)

	var raw map[string]any
	decodeJSON(t, rec, &raw)
	byID := map[string]map[string]any{}
	for _, item := range raw["findings"].([]any) {
		finding := item.(map[string]any)
		byID[finding["id"].(string)] = finding
	}
	verified, ok := byID["finding-verified"]
	if !ok {
		t.Fatalf("findings = %v, want finding-verified", raw["findings"])
	}
	if verified["triage_source"] != store.TriageSourceVerifier {
		t.Errorf("triage_source = %v, want %q", verified["triage_source"], store.TriageSourceVerifier)
	}
	evidence, ok := verified["verification"].(map[string]any)
	if !ok {
		t.Fatalf("verification = %v, want the stored JSON object", verified["verification"])
	}
	if evidence["verdict"] != "refute" || evidence["evidence"] != "if err != nil {" ||
		evidence["head_sha"] != "abc123" || evidence["model"] != "nemotron" {
		t.Errorf("verification = %v, want the refutation with its cited evidence", evidence)
	}

	plain, ok := byID["finding-plain"]
	if !ok {
		t.Fatalf("findings = %v, want finding-plain", raw["findings"])
	}
	if plain["triage_source"] != "" {
		t.Errorf("plain triage_source = %v, want empty", plain["triage_source"])
	}
	if empty, ok := plain["verification"].(map[string]any); !ok || len(empty) != 0 {
		t.Errorf("plain verification = %v, want an empty object", plain["verification"])
	}
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

func TestPRDetailExposesFindingCitationsForThePanel(t *testing.T) {
	f := newSecureFixture(t)
	inst := f.installation(2001)
	team := f.team("citations-team")
	repo := f.repo(7001, inst.ID, "acme/citations", &team.ID, true)
	pr := f.pr(repo.ID, 43, store.StatusChangesRequested)
	dev := f.user("citations-dev", false)
	f.member(team.ID, dev.ID, "developer")
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "citations", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	run, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: "def456", Status: store.RunSucceeded,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	if _, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-citations", PullRequestID: pr.ID, Fingerprint: "fp-citations",
		Path: "src/a.go", StartLine: 2, EndLine: 2, Severity: "minor",
		Category: "design", Title: "Límite entre módulos", Status: store.FindingOpen,
		Scope: store.FindingScopeFiles,
		Citations: store.EncodeFindingCitations([]store.FindingCitation{
			{Path: "src/a.go", StartLine: 2, EndLine: 2, Side: store.FindingCitationHead},
			{Path: "src/b.go", StartLine: 10, EndLine: 10, Side: store.FindingCitationBase},
		}),
		Advisory:       true,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
	}); err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}

	rec := f.request(http.MethodGet, fmt.Sprintf("/api/prs/%d", pr.ID), nil, f.session(dev))
	assertStatus(t, rec, http.StatusOK)

	var raw struct {
		Findings []struct {
			ID        string                  `json:"id"`
			Scope     string                  `json:"scope"`
			Citations []store.FindingCitation `json:"citations"`
			Advisory  bool                    `json:"advisory"`
		} `json:"findings"`
	}
	decodeJSON(t, rec, &raw)
	if len(raw.Findings) != 1 {
		t.Fatalf("findings = %d, want one", len(raw.Findings))
	}
	finding := raw.Findings[0]
	if finding.ID != "finding-citations" || finding.Scope != store.FindingScopeFiles || !finding.Advisory {
		t.Errorf("finding = %+v, want citations/files/advisory", finding)
	}
	if len(finding.Citations) != 2 || finding.Citations[0].Path != "src/a.go" ||
		finding.Citations[1].Path != "src/b.go" || finding.Citations[1].Side != store.FindingCitationBase {
		t.Errorf("citations = %+v, want both sides of the finding", finding.Citations)
	}
}
