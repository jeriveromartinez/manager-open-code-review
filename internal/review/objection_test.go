package review

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type fakeObjectionVerifier struct {
	verdict Verification
	err     error
	called  bool
}

func (v *fakeObjectionVerifier) VerifyObjection(_ context.Context, f store.Finding, _ string, workDir, headSHA string) (Verification, error) {
	v.called = true
	if v.err != nil {
		return Verification{}, v.err
	}
	if _, err := os.Stat(filepath.Join(workDir, f.Path)); err != nil {
		return Verification{}, err
	}
	out := v.verdict
	out.HeadSHA = headSHA
	return out, nil
}

type objectionTestJobs struct {
	mu        sync.Mutex
	evaluated []uint
	published []uint
}

func (j *objectionTestJobs) EnqueueReview(context.Context, uint) error    { return nil }
func (j *objectionTestJobs) EnqueuePublish(context.Context, uint) error   { return nil }
func (j *objectionTestJobs) EnqueueLLMHealth(context.Context, uint) error { return nil }

func (j *objectionTestJobs) EnqueueObjection(_ context.Context, id uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.evaluated = append(j.evaluated, id)
	return nil
}

func (j *objectionTestJobs) EnqueueObjectionPublish(_ context.Context, id uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.published = append(j.published, id)
	return nil
}

func (j *objectionTestJobs) snapshot() []uint {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]uint(nil), j.published...)
}

// objectionRunnerFixture seeds the review configuration the worker needs and
// returns an open finding plus its queued objection intent.
func objectionRunnerFixture(t *testing.T, authorized bool) (*Runner, store.FindingObjection, *fakeObjectionVerifier, *objectionTestJobs) {
	t.Helper()

	st := testutil.NewStore(t)
	ctx := context.Background()
	inst := store.Installation{ID: 991, AccountLogin: "acme", AccountType: "Organization"}
	if err := st.UpsertInstallation(ctx, inst); err != nil {
		t.Fatalf("seed installation: %v", err)
	}
	team := store.Team{Name: "QA", Slug: "qa"}
	if err := st.DB.Create(&team).Error; err != nil {
		t.Fatalf("seed team: %v", err)
	}
	repo := store.Repository{ID: 501, InstallationID: 991, FullName: "acme/app", DefaultBranch: "main", Enabled: true, TeamID: &team.ID}
	if err := st.UpsertRepository(ctx, repo); err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	profile := store.LLMProfile{Name: "test", Provider: "openai", Model: "model", Enabled: true}
	if err := st.DB.Create(&profile).Error; err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	policy := store.QAPolicy{
		TeamID: team.ID, LLMProfileID: profile.ID,
		AutoTriageMode: store.AutoTriageApply, AutoWithdraw: true,
		BlockingThreshold: "major",
	}
	if err := st.DB.Create(&policy).Error; err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	pr := store.PullRequest{
		RepositoryID: 501, Number: 7, Title: "PR", AuthorLogin: "octocat",
		HeadSHA: "head-1", BaseRef: "main", State: "open", QAStatus: store.StatusChangesRequested,
	}
	if err := st.DB.Create(&pr).Error; err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	finding := store.Finding{
		ID: "f1", PullRequestID: pr.ID, Fingerprint: "fp-1", IdentityVersion: 2,
		Path: "a.go", StartLine: 3, EndLine: 3, Severity: "minor", Category: "bug",
		Status: store.FindingOpen, ExistingCode: "func guard() {",
	}
	if err := st.DB.Create(&finding).Error; err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	intent, _, err := st.EnqueueFindingObjection(ctx, store.FindingObjection{
		FindingID: "f1", PullRequestID: pr.ID, HeadSHA: "head-1",
		CommentID: 7001, Actor: "octocat", Authorized: authorized,
		Body: "la función ya valida nil", FilePath: "a.go",
		FileContent: []byte("package a\n\nfunc guard() {\n}\n"),
	})
	if err != nil {
		t.Fatalf("enqueue objection: %v", err)
	}

	verifier := &fakeObjectionVerifier{verdict: Verification{Verdict: VerdictRefute, Evidence: "func guard() {"}}
	jobs := &objectionTestJobs{}
	runner := &Runner{Store: st, ObjectionVerifier: verifier, Jobs: jobs}
	return runner, intent, verifier, jobs
}

func TestReevaluateObjectionRecordsTheDecision(t *testing.T) {
	runner, intent, verifier, jobs := objectionRunnerFixture(t, true)

	if err := runner.ReevaluateObjection(context.Background(), intent.ID); err != nil {
		t.Fatalf("ReevaluateObjection: %v", err)
	}
	if !verifier.called {
		t.Fatal("the bounded verifier was not called")
	}

	finding, err := runner.Store.Finding(context.Background(), "f1")
	if err != nil {
		t.Fatalf("Finding: %v", err)
	}
	if finding.Status != store.FindingWithdrawn || finding.TriageSource != store.TriageSourceVerifier {
		t.Errorf("finding = %+v, want the verifier withdrawal", finding)
	}
	if !strings.Contains(string(finding.Verification), "func guard() {") {
		t.Errorf("verification = %s, want the evidence", finding.Verification)
	}
	evaluated, err := runner.Store.FindingObjection(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if evaluated.State != store.ObjectionEvaluated || evaluated.Action != store.ObjectionWithdraw || !evaluated.Apply {
		t.Errorf("intent = %+v, want an applied withdrawal", evaluated)
	}
	if got := jobs.snapshot(); len(got) != 1 || got[0] != intent.ID {
		t.Fatalf("publish enqueues = %v, want one for the intent", got)
	}

	// A retry re-asserts the publication without duplicating the audit.
	if err := runner.ReevaluateObjection(context.Background(), intent.ID); err != nil {
		t.Fatalf("retried ReevaluateObjection: %v", err)
	}
	if got := jobs.snapshot(); len(got) != 2 || got[1] != intent.ID {
		t.Errorf("publish enqueues after retry = %v, want the intent re-asserted", got)
	}
	var audits int64
	if err := runner.Store.DB.Model(&store.AuditLog{}).
		Where("target_type = 'finding' AND target_id = 'f1'").Count(&audits).Error; err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("audit rows = %d, want 1", audits)
	}
}

func TestReevaluateObjectionUnauthorizedDoesNotRunTheVerifier(t *testing.T) {
	runner, intent, verifier, jobs := objectionRunnerFixture(t, false)

	if err := runner.ReevaluateObjection(context.Background(), intent.ID); err != nil {
		t.Fatalf("ReevaluateObjection: %v", err)
	}
	if verifier.called {
		t.Error("the verifier ran for an unauthorized objection")
	}
	evaluated, err := runner.Store.FindingObjection(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if evaluated.Action != store.ObjectionIgnored || evaluated.Apply {
		t.Errorf("intent = %+v, want an ignored decision", evaluated)
	}
	if got := jobs.snapshot(); len(got) != 1 {
		t.Errorf("publish enqueues = %v, want the explanation queued", got)
	}
	finding, err := runner.Store.Finding(context.Background(), "f1")
	if err != nil {
		t.Fatalf("Finding: %v", err)
	}
	if finding.Status != store.FindingOpen {
		t.Errorf("finding status = %q, want the gate unchanged", finding.Status)
	}
}

func TestBuildObjectionContextIncludesTheObjection(t *testing.T) {
	background := buildObjectionContext(testFinding("f_1", "minor", ""), "la función ya valida nil")
	if !strings.Contains(background, "f_1") || !strings.Contains(background, "la función ya valida nil") {
		t.Errorf("background = %q, want the finding and the objection", background)
	}
}

func TestWriteObjectionCheckoutRejectsEscapingPaths(t *testing.T) {
	for _, path := range []string{"", filepath.Join("..", "a.go"), filepath.Join(t.TempDir(), "a.go")} {
		if _, err := writeObjectionCheckout(path, []byte("package a\n")); err == nil {
			t.Errorf("path %q was accepted, want rejected", path)
		}
	}
	dir, err := writeObjectionCheckout("src/a.go", []byte("package a\n"))
	if err != nil {
		t.Fatalf("writeObjectionCheckout: %v", err)
	}
	defer os.RemoveAll(dir)
	content, err := os.ReadFile(filepath.Join(dir, "src", "a.go"))
	if err != nil || string(content) != "package a\n" {
		t.Errorf("checkout content = %q (err=%v), want the cited file", content, err)
	}
}

func TestParseVerdictsReadsProposedSeverity(t *testing.T) {
	got := ParseVerdicts([]nativeComment{{
		Content: "QA-VERIFY f_1 REFUTE\nEVIDENCE: func guard() {\nSEVERITY: minor",
	}})
	if got["f_1"].DowngradeTo != "minor" {
		t.Fatalf("downgrade = %q, want minor", got["f_1"].DowngradeTo)
	}
	if got["f_1"].Evidence != "func guard() {" {
		t.Errorf("evidence = %q, want the quoted line", got["f_1"].Evidence)
	}
}

func TestDecideObjectionWithdrawsMinorWithEvidence(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	decision := DecideObjection(f, verified(VerdictRefute, "func guard() {"),
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if decision.Action != store.ObjectionWithdraw || !decision.Apply || decision.Status != store.FindingWithdrawn {
		t.Fatalf("decision = %+v, want an applied withdrawal", decision)
	}
	if decision.Verification.Evidence != "func guard() {" || decision.Verification.HeadSHA != verifyHead {
		t.Errorf("verification = %+v, want the evidence at the reviewed head", decision.Verification)
	}
}

func TestDecideObjectionBlockerEscalatesInsteadOfDowngrading(t *testing.T) {
	for _, severity := range []string{"major", "blocker"} {
		f := testFinding("f_1", severity, "")
		decision := DecideObjection(f, verified(VerdictRefute, "func guard() {"),
			testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
		if decision.Action != store.ObjectionEscalate || decision.Apply || decision.Status != f.Status {
			t.Errorf("severity %s: decision = %+v, want an escalation that keeps the gate", severity, decision)
		}
		if decision.Verification.Reason == "" {
			t.Errorf("severity %s: escalation must record why a human must decide", severity)
		}
	}
}

func TestDecideObjectionRejectsEvidenceFromAnotherHead(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	decision := DecideObjection(f, Verification{Verdict: VerdictRefute, Evidence: "func guard() {", HeadSHA: "head-2"},
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if decision.Action != store.ObjectionEscalate || decision.Apply || decision.Status != f.Status {
		t.Fatalf("decision = %+v, want an escalation without a status change", decision)
	}
	if decision.Verification.Reason != "la verificación no corresponde al head revisado" {
		t.Errorf("reason = %q, want the stale-head reason", decision.Verification.Reason)
	}
}

func TestDecideObjectionConfirmUpholdsTheFinding(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	decision := DecideObjection(f, verified(VerdictConfirm, "func guard() {"),
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if decision.Action != store.ObjectionUphold || decision.Apply || decision.Status != f.Status {
		t.Fatalf("decision = %+v, want an upheld finding with no status change", decision)
	}
}

func TestDecideObjectionUncertainEscalates(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	decision := DecideObjection(f, verified(VerdictUncertain, ""), nil, verifyHead, applyPolicy())
	if decision.Action != store.ObjectionEscalate || decision.Apply || decision.Reason == "" {
		t.Fatalf("decision = %+v, want an escalation with its reason", decision)
	}
}

func TestDecideObjectionNeverReevaluatesHumanTriage(t *testing.T) {
	f := testFinding("f_1", "minor", store.TriageSourceHuman)
	decision := DecideObjection(f, verified(VerdictRefute, "func guard() {"),
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if decision.Action != store.ObjectionEscalate || decision.Apply || decision.Status != f.Status {
		t.Fatalf("decision = %+v, want a human decision left untouched", decision)
	}
}

func TestDecideObjectionDowngradeIsProposedOnly(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	verification := verified(VerdictRefute, "func guard() {")
	verification.DowngradeTo = "nit"
	decision := DecideObjection(f, verification, testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if decision.Action != store.ObjectionDowngrade || decision.Apply || decision.Status != f.Status {
		t.Fatalf("decision = %+v, want a downgrade proposal that waits for a human", decision)
	}

	verification.DowngradeTo = "major"
	decision = DecideObjection(f, verification, testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if decision.Action != store.ObjectionEscalate {
		t.Fatalf("decision = %+v, want an escalation for a downgrade that is not lower", decision)
	}
}

func TestDecideObjectionDryRunDoesNotApply(t *testing.T) {
	f := testFinding("f_1", "minor", "")
	policy := VerifyPolicy{Mode: store.AutoTriageDryRun, AutoWithdraw: true}
	decision := DecideObjection(f, verified(VerdictRefute, "func guard() {"),
		testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, policy)
	if decision.Action != store.ObjectionWithdraw || decision.Apply || decision.Status != store.FindingWithdrawn {
		t.Fatalf("decision = %+v, want the withdrawal recorded but not applied", decision)
	}
}

func TestObjectionSweeperHealsLostJobs(t *testing.T) {
	runner, intent, _, _ := objectionRunnerFixture(t, true)
	ctx := context.Background()

	// A queued intent whose worker job was lost.
	if err := runner.Store.DB.Model(&store.FindingObjection{}).Where("id = ?", intent.ID).
		Update("created_at", time.Now().Add(-2*time.Minute)).Error; err != nil {
		t.Fatalf("age queued intent: %v", err)
	}
	// An evaluated intent whose reply job was lost.
	second, _, err := runner.Store.EnqueueFindingObjection(ctx, store.FindingObjection{
		FindingID: "f1", PullRequestID: intent.PullRequestID, HeadSHA: intent.HeadSHA,
		CommentID: 7002, Actor: "octocat", Authorized: true,
	})
	if err != nil {
		t.Fatalf("enqueue second objection: %v", err)
	}
	if _, err := runner.Store.RecordFindingObjectionEvaluation(ctx, second.ID, store.ObjectionEvaluation{
		Action: store.ObjectionEscalate, Reason: "requiere humano",
	}); err != nil {
		t.Fatalf("evaluate second objection: %v", err)
	}

	jobs := &objectionTestJobs{}
	sweeper := &ObjectionSweeper{Store: runner.Store, Jobs: jobs}
	if _, err := sweeper.EnqueuePendingObjections(ctx, 10); err != nil {
		t.Fatalf("EnqueuePendingObjections: %v", err)
	}
	if got := jobs.evaluated; len(got) != 1 || got[0] != intent.ID {
		t.Errorf("re-enqueued evaluations = %v, want the stale intent", got)
	}
	if got := jobs.published; len(got) != 1 || got[0] != second.ID {
		t.Errorf("re-enqueued publications = %v, want the evaluated intent", got)
	}
}

func TestReevaluateObjectionWithoutFileEscalates(t *testing.T) {
	runner, intent, verifier, jobs := objectionRunnerFixture(t, true)
	if err := runner.Store.DB.Model(&store.FindingObjection{}).Where("id = ?", intent.ID).
		Updates(map[string]any{"file_content": nil, "read_token_enc": ""}).Error; err != nil {
		t.Fatalf("clear objection file: %v", err)
	}

	if err := runner.ReevaluateObjection(context.Background(), intent.ID); err == nil {
		t.Fatal("ReevaluateObjection returned nil, want a retryable fetch error")
	}
	if verifier.called {
		t.Error("the verifier ran without the cited file")
	}
	stored, err := runner.Store.FindingObjection(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if stored.State != store.ObjectionQueued {
		t.Errorf("state = %q, want the intent left queued for the retry", stored.State)
	}
	if got := jobs.snapshot(); len(got) != 0 {
		t.Errorf("publish enqueues = %v, want none before the retry budget is spent", got)
	}

	if err := runner.AbandonObjection(context.Background(), intent.ID, "no cited file"); err != nil {
		t.Fatalf("AbandonObjection: %v", err)
	}
	evaluated, err := runner.Store.FindingObjection(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection after abandon: %v", err)
	}
	if evaluated.Action != store.ObjectionEscalate {
		t.Errorf("action = %q, want the exhausted fetch escalated", evaluated.Action)
	}
	if got := jobs.snapshot(); len(got) != 1 {
		t.Errorf("publish enqueues = %v, want the escalation reply", got)
	}
}

func TestFetchObjectionFileUsesTheReadToken(t *testing.T) {
	var gotAuth, gotPath, gotRef string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotRef = r.URL.Query().Get("ref")
		_, _ = w.Write([]byte("package a\n"))
	}))
	defer server.Close()

	content, err := fetchObjectionFile(context.Background(), server.URL, "acme/app", "src/a.go", "head-1", "read-token")
	if err != nil {
		t.Fatalf("fetchObjectionFile: %v", err)
	}
	if string(content) != "package a\n" {
		t.Errorf("content = %q", content)
	}
	if gotAuth != "Bearer read-token" || gotPath != "/repos/acme/app/contents/src/a.go" || gotRef != "head-1" {
		t.Errorf("request = auth %q path %q ref %q", gotAuth, gotPath, gotRef)
	}
}

func TestDecideObjectionRejectsEvidenceAsUncertain(t *testing.T) {
	for _, verdict := range []Verdict{VerdictConfirm, VerdictRefute} {
		f := testFinding("f_1", "minor", "")
		decision := DecideObjection(f, verified(verdict, "func other() {"),
			testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
		if decision.Action != store.ObjectionEscalate || decision.Verification.Verdict != VerdictUncertain {
			t.Errorf("verdict %s: decision = %+v, want an uncertain escalation", verdict, decision)
		}
		if decision.Verification.Reason == "" || decision.Apply {
			t.Errorf("verdict %s: reason/apply = %q/%v, want a recorded reason and no apply", verdict, decision.Verification.Reason, decision.Apply)
		}
	}
}

func TestDecideObjectionBlockerWithDowngradeEscalates(t *testing.T) {
	f := testFinding("f_1", "blocker", "")
	verification := verified(VerdictRefute, "func guard() {")
	verification.DowngradeTo = "minor"
	decision := DecideObjection(f, verification, testFiles("package a\n\nfunc guard() {\n}\n"), verifyHead, applyPolicy())
	if decision.Action != store.ObjectionEscalate || decision.Apply {
		t.Fatalf("decision = %+v, want a blocker escalation even with SEVERITY", decision)
	}
}

func TestReevaluateObjectionRetriesVerifierError(t *testing.T) {
	runner, intent, verifier, jobs := objectionRunnerFixture(t, true)
	verifier.err = errors.New("ocr temporarily unavailable")

	if err := runner.ReevaluateObjection(context.Background(), intent.ID); err == nil {
		t.Fatal("ReevaluateObjection returned nil, want the transient error for a retry")
	}
	stored, err := runner.Store.FindingObjection(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if stored.State != store.ObjectionQueued {
		t.Errorf("state = %q, want the intent left queued for the retry", stored.State)
	}
	if got := jobs.snapshot(); len(got) != 0 {
		t.Errorf("publish enqueues = %v, want none for a retryable failure", got)
	}

	if err := runner.AbandonObjection(context.Background(), intent.ID, "ocr temporarily unavailable"); err != nil {
		t.Fatalf("AbandonObjection: %v", err)
	}
	stored, err = runner.Store.FindingObjection(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection after abandon: %v", err)
	}
	if stored.State != store.ObjectionEvaluated || stored.Action != store.ObjectionEscalate {
		t.Errorf("intent = %+v, want the exhausted attempt escalated", stored)
	}
	if got := jobs.snapshot(); len(got) != 1 {
		t.Errorf("publish enqueues = %v, want the escalation reply", got)
	}
}

func TestReevaluateObjectionStaleHeadEscalates(t *testing.T) {
	runner, intent, verifier, _ := objectionRunnerFixture(t, true)
	if err := runner.Store.DB.Model(&store.PullRequest{}).Where("id = ?", intent.PullRequestID).
		Update("head_sha", "head-2").Error; err != nil {
		t.Fatalf("move head: %v", err)
	}

	if err := runner.ReevaluateObjection(context.Background(), intent.ID); err != nil {
		t.Fatalf("ReevaluateObjection: %v", err)
	}
	if verifier.called {
		t.Error("the verifier ran for a stale head")
	}
	evaluated, err := runner.Store.FindingObjection(context.Background(), intent.ID)
	if err != nil {
		t.Fatalf("FindingObjection: %v", err)
	}
	if evaluated.Action != store.ObjectionEscalate {
		t.Errorf("action = %q, want an escalation for the stale head", evaluated.Action)
	}
}
