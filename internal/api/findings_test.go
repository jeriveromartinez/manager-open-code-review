package api_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// recordingFindings applies the finding transitions the panel asks for and
// records the republish calls, so the endpoints can be checked without a
// GitHub client.
type recordingFindings struct {
	mu        sync.Mutex
	republish []uint
	threads   []string
	failErr   error
}

func (r *recordingFindings) Republish(_ context.Context, prID uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failErr != nil {
		return r.failErr
	}
	r.republish = append(r.republish, prID)
	return nil
}

func (r *recordingFindings) SetFindingThread(_ context.Context, _ uint, finding store.Finding, resolved bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failErr != nil {
		return false, r.failErr
	}
	if finding.GithubThreadID == "" {
		return false, nil
	}
	action := "open"
	if resolved {
		action = "resolved"
	}
	r.threads = append(r.threads, finding.ID+"="+action)
	return true, nil
}

func (r *recordingFindings) republished() []uint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint(nil), r.republish...)
}

func (r *recordingFindings) threadCalls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.threads...)
}

func newFindingFixture(t *testing.T) (*secureFixture, store.PullRequest, store.Finding) {
	t.Helper()

	f := newSecureFixture(t)
	inst := f.installation(1401)
	team := f.team("findings-team")
	repo := f.repo(7401, inst.ID, "acme/findings", &team.ID, true)
	pr := f.pr(repo.ID, 21, store.StatusChangesRequested)
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "findings", Provider: "openai", Model: "gpt-4o-mini", APIKeyEnc: "sealed", Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	run, err := f.st.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, ProfileID: profile.ID, HeadSHA: "sha-findings",
		Status: store.RunSucceeded, Coverage: datatypes.JSON([]byte(`{}`)),
		Usage: datatypes.JSON([]byte(`{}`)), CloneTokenEnc: "sealed",
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}
	finding, err := f.st.UpsertFinding(f.ctx, store.Finding{
		ID: "finding-panel", PullRequestID: pr.ID, Fingerprint: "fp-panel",
		Path: "internal/firewall.go", StartLine: 196, EndLine: 198,
		Severity: "blocker", Category: "security", Title: "regla permisiva",
		Body: "acepta cualquier origen", Status: store.FindingOpen,
		FirstSeenRunID: run.ID, LastSeenRunID: run.ID,
		GithubCommentID: 555, GithubThreadID: "thread-abc",
	})
	if err != nil {
		t.Fatalf("UpsertFinding: %v", err)
	}
	return f, pr, finding
}

func TestWithdrawFindingFromPanel(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	lead := f.user("panel-lead", false)
	f.member(findingTeamID(t, f, pr), lead.ID, "lead")

	path := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", pr.ID, finding.ID)
	body := map[string]any{"reason": "falso positivo"}
	rec := f.request(http.MethodPost, path, body, f.session(lead))
	assertStatus(t, rec, http.StatusOK)

	var got store.Finding
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", finding.ID).Take(&got).Error; err != nil {
		t.Fatalf("reload finding: %v", err)
	}
	if got.Status != store.FindingWithdrawn {
		t.Errorf("status = %q, want %q", got.Status, store.FindingWithdrawn)
	}

	var audit store.AuditLog
	if err := f.st.DB.WithContext(f.ctx).
		Where("action = ? AND target_id = ?", "finding.withdrawn", finding.ID).Take(&audit).Error; err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if !bytes.Contains([]byte(audit.Data), []byte("falso positivo")) {
		t.Errorf("audit data = %s, want the reason", audit.Data)
	}
	if !bytes.Contains([]byte(audit.Data), []byte(lead.Login)) {
		t.Errorf("audit data = %s, want the actor login", audit.Data)
	}
}

func TestWithdrawFindingRequiresLead(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	dev := f.user("panel-dev", false)
	f.member(findingTeamID(t, f, pr), dev.ID, "developer")
	outsider := f.user("panel-outsider", false)

	path := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", pr.ID, finding.ID)
	rec := f.request(http.MethodPost, path, nil, f.session(dev))
	assertStatus(t, rec, http.StatusForbidden)
	rec = f.request(http.MethodPost, path, nil, f.session(outsider))
	assertStatus(t, rec, http.StatusForbidden)

	var got store.Finding
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", finding.ID).Take(&got).Error; err != nil {
		t.Fatalf("reload finding: %v", err)
	}
	if got.Status != store.FindingOpen {
		t.Errorf("status = %q, want it unchanged", got.Status)
	}
}

func TestWithdrawFindingRejectsAnotherPullRequest(t *testing.T) {
	f, pr, _ := newFindingFixture(t)
	admin := f.user("panel-admin", true)
	other := f.pr(pr.RepositoryID, 99, store.StatusPending)

	path := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", other.ID, "finding-panel")
	rec := f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusNotFound)
}

func TestConfirmFindingRestoresIt(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	admin := f.user("panel-admin-confirm", true)
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Finding{}).
		Where("id = ?", finding.ID).Update("status", store.FindingWithdrawn).Error; err != nil {
		t.Fatalf("seed withdrawn finding: %v", err)
	}

	path := fmt.Sprintf("/api/prs/%d/findings/%s/confirm", pr.ID, finding.ID)
	rec := f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	var got store.Finding
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", finding.ID).Take(&got).Error; err != nil {
		t.Fatalf("reload finding: %v", err)
	}
	if got.Status != store.FindingOpen {
		t.Errorf("status = %q, want %q", got.Status, store.FindingOpen)
	}
}

// TestWithdrawFindingRepublishesAndMirrorsThread proves the panel action reaches
// GitHub like a /qa command does, so the check and the thread stop disagreeing.
func TestWithdrawFindingRepublishesAndMirrorsThread(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	findings := &recordingFindings{}
	f.deps.Findings = findings
	f.rebuildRouter()
	admin := f.user("panel-admin-republish", true)

	path := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", pr.ID, finding.ID)
	rec := f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	if got := findings.republished(); len(got) != 1 || got[0] != pr.ID {
		t.Errorf("Republish calls = %v, want [%d]", got, pr.ID)
	}
	if got := findings.threadCalls(); len(got) != 1 || got[0] != finding.ID+"=resolved" {
		t.Errorf("SetFindingThread calls = %v, want the thread resolved", got)
	}
}

func TestWithdrawFindingWithoutPublishingWiring(t *testing.T) {
	f, pr, finding := newFindingFixture(t)
	admin := f.user("panel-admin-nowiring", true)

	path := fmt.Sprintf("/api/prs/%d/findings/%s/withdraw", pr.ID, finding.ID)
	rec := f.request(http.MethodPost, path, nil, f.session(admin))
	assertStatus(t, rec, http.StatusOK)

	var got store.Finding
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", finding.ID).Take(&got).Error; err != nil {
		t.Fatalf("reload finding: %v", err)
	}
	if got.Status != store.FindingWithdrawn {
		t.Errorf("status = %q, want %q", got.Status, store.FindingWithdrawn)
	}
}

// findingTeamID returns the team that owns the pull request's repository.
func findingTeamID(t *testing.T, f *secureFixture, pr store.PullRequest) uint {
	t.Helper()
	var repo store.Repository
	if err := f.st.DB.WithContext(f.ctx).Where("id = ?", pr.RepositoryID).Take(&repo).Error; err != nil {
		t.Fatalf("load repository: %v", err)
	}
	if repo.TeamID == nil {
		t.Fatal("repository has no team")
	}
	return *repo.TeamID
}
