package github_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func writeObjectionJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func (f *webhookFixture) seedObjectionFinding(prID uint, id, severity, status string) store.Finding {
	f.t.Helper()

	finding := f.seedThreadFinding(prID, id, status)
	if err := f.st.DB.WithContext(f.ctx).Model(&store.Finding{}).Where("id = ?", id).
		Update("severity", severity).Error; err != nil {
		f.t.Fatalf("set finding severity: %v", err)
	}
	finding.Severity = severity
	return finding
}

func (f *webhookFixture) objection(id uint) store.FindingObjection {
	f.t.Helper()

	out, err := f.st.FindingObjection(f.ctx, id)
	if err != nil {
		f.t.Fatalf("load objection %d: %v", id, err)
	}
	return out
}

// The app has no OCR: a reply must be queued as a durable intent, never
// evaluated inline and never answered from the webhook.
func TestWebhookReviewCommentQueuesObjection(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)

	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusAccepted)

	queued := f.objections.evaluatedSnapshot()
	if len(queued) != 1 {
		t.Fatalf("queued evaluations = %v, want one", queued)
	}
	if got := f.objections.publishedSnapshot(); len(got) != 0 {
		t.Errorf("queued publications = %v, want none before the evaluation", got)
	}
	intent := f.objection(queued[0])
	if intent.FindingID != "f1" || intent.HeadSHA != "aaa111" || intent.CommentID != 5002 ||
		intent.Actor != "qa-lead" || !intent.Authorized || intent.State != store.ObjectionQueued {
		t.Errorf("intent = %+v, want the authorized reply queued for f1 at aaa111", intent)
	}
	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want the open finding untouched by the app", got)
	}
	if rows := f.auditRows(); len(rows) != 0 {
		t.Errorf("audit rows = %+v, want no inline decision", rows)
	}
	if got := f.findings.republishSnapshot(); len(got) != 0 {
		t.Errorf("gate republishes = %+v, want none from the webhook", got)
	}
}

func TestWebhookReviewCommentStaleSHAIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)

	body := bytes.ReplaceAll(f.fixture("review-comment-created.json"),
		[]byte(`"commit_id": "aaa111"`), []byte(`"commit_id": "old999"`))
	assertStatus(t, f.deliverBody("pull_request_review_comment", body, signWebhook(body)), http.StatusNoContent)

	if got := f.objections.evaluatedSnapshot(); len(got) != 0 {
		t.Errorf("queued evaluations = %+v, want none for a stale reply", got)
	}
	if got := f.countRows("finding_objections"); got != 0 {
		t.Errorf("objection rows = %d, want 0", got)
	}
}

func TestWebhookReviewCommentRedeliveryKeepsOneIntent(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)

	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusAccepted)
	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusAccepted)

	if got := f.countRows("finding_objections"); got != 1 {
		t.Fatalf("objection rows = %d, want one per review comment", got)
	}
	queued := f.objections.evaluatedSnapshot()
	if len(queued) != 2 || queued[0] != queued[1] {
		t.Errorf("queued evaluations = %v, want the same intent re-asserted", queued)
	}
}

func TestWebhookReviewCommentUnauthorizedIsQueuedForStateOnly(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedDeveloper()
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)

	body := bytes.ReplaceAll(f.fixture("review-comment-created.json"), []byte("8001"), []byte("8002"))
	body = bytes.ReplaceAll(body, []byte("qa-lead"), []byte("dev"))
	assertStatus(t, f.deliverBody("pull_request_review_comment", body, signWebhook(body)), http.StatusAccepted)

	queued := f.objections.evaluatedSnapshot()
	if len(queued) != 1 {
		t.Fatalf("queued evaluations = %v, want one", queued)
	}
	intent := f.objection(queued[0])
	if intent.Authorized {
		t.Error("intent = authorized, want the unauthorized sender recorded state-only")
	}
	if got := f.finding("f1").Status; got != store.FindingOpen {
		t.Errorf("finding status = %q, want the open finding untouched", got)
	}
}

func TestWebhookReviewCommentLeavesHumanTriageUntouched(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	lead := f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)
	if _, err := f.st.TriageFinding(f.ctx, "f1", store.FindingOpen, store.FindingAudit{
		Action: "finding.confirmed", Actor: lead.Login, ActorID: &lead.ID,
		Source: store.TriageSourceHuman, Reason: "revisado",
	}); err != nil {
		t.Fatalf("TriageFinding: %v", err)
	}

	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusNoContent)

	if got := f.objections.evaluatedSnapshot(); len(got) != 0 {
		t.Errorf("queued evaluations = %+v, want none for a human-triaged finding", got)
	}
	if got := f.countRows("finding_objections"); got != 0 {
		t.Errorf("objection rows = %d, want 0", got)
	}
	got := f.finding("f1")
	if got.Status != store.FindingOpen || got.TriageSource != store.TriageSourceHuman || got.TriagedBy != lead.Login {
		t.Errorf("finding = %+v, want the human decision preserved", got)
	}
}

func TestWebhookReviewCommentAppEchoIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)

	body := bytes.ReplaceAll(f.fixture("review-comment-created.json"), []byte(`"type": "User"`), []byte(`"type": "Bot"`))
	assertStatus(t, f.deliverBody("pull_request_review_comment", body, signWebhook(body)), http.StatusNoContent)

	if got := f.countRows("finding_objections"); got != 0 {
		t.Errorf("objection rows = %d, want none for an App echo", got)
	}
	if got := f.objections.evaluatedSnapshot(); len(got) != 0 {
		t.Errorf("queued evaluations = %+v, want none", got)
	}
}

func TestWebhookReviewCommentUnknownCommentIgnored(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)

	body := bytes.ReplaceAll(f.fixture("review-comment-created.json"),
		[]byte(`"in_reply_to_id": 5001`), []byte(`"in_reply_to_id": 9999`))
	assertStatus(t, f.deliverBody("pull_request_review_comment", body, signWebhook(body)), http.StatusNoContent)

	if got := f.countRows("finding_objections"); got != 0 {
		t.Errorf("objection rows = %d, want none for a foreign comment", got)
	}
	if got := f.objections.evaluatedSnapshot(); len(got) != 0 {
		t.Errorf("queued evaluations = %+v, want none", got)
	}
}

func testObjectionPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func seedObjectionReviewConfig(t *testing.T, f *webhookFixture) {
	t.Helper()

	profile := store.LLMProfile{Name: "fetch-test", Provider: "openai", Model: "model", Enabled: true}
	if err := f.st.DB.WithContext(f.ctx).Create(&profile).Error; err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	policy := store.QAPolicy{
		TeamID: f.repoTeam(), LLMProfileID: profile.ID,
		MaxFilesPerPass: 40, MaxDiffLinesPerPass: 3000, MaxPasses: 4,
		ExcludeGlobs: datatypes.JSON([]byte(`[]`)),
	}
	if err := f.st.DB.WithContext(f.ctx).Create(&policy).Error; err != nil {
		t.Fatalf("seed policy: %v", err)
	}
}

func TestWebhookReviewCommentStoresCitedFile(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)
	seedObjectionReviewConfig(t, f)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/991/access_tokens":
			writeObjectionJSON(w, map[string]any{"token": "read-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/backend-api/contents/src/a.go":
			if r.Header.Get("Authorization") != "Bearer read-token" {
				http.Error(w, "missing token", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte("package a\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	f.handler.Box = box
	f.handler.Apps = fakeAppProvider{app: &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 123, PEM: testObjectionPEM(t), WebhookSecret: webhookTestSecret},
		APIURL: server.URL,
	}}

	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusAccepted)

	queued := f.objections.evaluatedSnapshot()
	if len(queued) != 1 {
		t.Fatalf("queued evaluations = %v, want one", queued)
	}
	intent := f.objection(queued[0])
	if string(intent.FileContent) != "package a\n" {
		t.Errorf("file_content = %q, want the cited file fetched at webhook time", intent.FileContent)
	}
	if intent.ReadTokenEnc != "" {
		t.Error("read_token_enc = set, want no fallback token when the content was fetched")
	}
}

func TestWebhookReviewCommentSealsTokenWhenFetchFails(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)
	seedObjectionReviewConfig(t, f)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/991/access_tokens":
			writeObjectionJSON(w, map[string]any{"token": "read-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/backend-api/contents/src/a.go":
			http.Error(w, "temporarily unavailable", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, crypto.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	f.handler.Box = box
	f.handler.Apps = fakeAppProvider{app: &ghapp.App{
		Creds:  ghapp.AppCredentials{AppID: 123, PEM: testObjectionPEM(t), WebhookSecret: webhookTestSecret},
		APIURL: server.URL,
	}}

	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusAccepted)

	queued := f.objections.evaluatedSnapshot()
	if len(queued) != 1 {
		t.Fatalf("queued evaluations = %v, want one", queued)
	}
	intent := f.objection(queued[0])
	if len(intent.FileContent) != 0 {
		t.Errorf("file_content = %q, want empty after the fetch failure", intent.FileContent)
	}
	if intent.ReadTokenEnc == "" {
		t.Error("read_token_enc = empty, want the sealed fallback token")
	}
}

// A terminal failed intent must not spend another OCR pass on redelivery.
func TestWebhookReviewCommentFailedIntentIsNoOp(t *testing.T) {
	f := newWebhookFixture(t)
	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	f.seedQALead(f.repoTeam())
	f.seedObjectionFinding(pr.ID, "f1", "minor", store.FindingOpen)

	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusAccepted)
	queued := f.objections.evaluatedSnapshot()
	if len(queued) != 1 {
		t.Fatalf("queued evaluations = %v, want one", queued)
	}
	if err := f.st.DB.WithContext(f.ctx).Model(&store.FindingObjection{}).
		Where("id = ?", queued[0]).Update("state", store.ObjectionFailed).Error; err != nil {
		t.Fatalf("mark intent failed: %v", err)
	}

	assertStatus(t, f.deliver("pull_request_review_comment", "review-comment-created.json"), http.StatusNoContent)
	if got := f.objections.evaluatedSnapshot(); len(got) != 1 {
		t.Errorf("queued evaluations = %v, want no new evaluation for a failed intent", got)
	}
	if got := f.objections.publishedSnapshot(); len(got) != 0 {
		t.Errorf("queued publications = %v, want none", got)
	}
}
