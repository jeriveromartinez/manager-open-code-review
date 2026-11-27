package github_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/crypto"
	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/review"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// gateFixture runs the real review.Trigger behind the webhook handler so a CI
// delivery is exercised through refresh, gate and enqueue together.
type gateFixture struct {
	*webhookFixture
}

type recordingReviewJobs struct {
	mu  sync.Mutex
	ids []uint
}

func (j *recordingReviewJobs) EnqueueReview(_ context.Context, runID uint) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.ids = append(j.ids, runID)
	return nil
}

func (j *recordingReviewJobs) EnqueueReviewRecovery(context.Context, uint, int) error { return nil }
func (j *recordingReviewJobs) EnqueuePublish(context.Context, uint) error             { return nil }
func (j *recordingReviewJobs) EnqueueLLMHealth(context.Context, uint) error           { return nil }
func (j *recordingReviewJobs) EnqueueReviewTrigger(context.Context, uint, string, time.Duration) error {
	return nil
}

type recordingChecks struct{}

func (c *recordingChecks) StartCheck(context.Context, uint) error  { return nil }
func (c *recordingChecks) CancelCheck(context.Context, uint) error { return nil }

func gateTestPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func newGateFixture(t *testing.T, api *fakeChecksGitHub) *gateFixture {
	t.Helper()

	f := newWebhookFixture(t)
	app := &ghapp.App{
		Creds: ghapp.AppCredentials{
			AppID:         12345,
			WebhookSecret: webhookTestSecret,
			PEM:           gateTestPEM(t),
		},
		APIURL: api.srv.URL,
	}
	provider := fakeAppProvider{app: app}
	f.handler.Apps = provider
	f.handler.Checks = &ghapp.Checks{Store: f.st, Apps: provider}

	box, err := crypto.NewBox(bytes.Repeat([]byte{0x44}, crypto.KeySize))
	if err != nil {
		t.Fatalf("NewBox(worker): %v", err)
	}
	jobs := &recordingReviewJobs{}
	checks := &recordingChecks{}
	f.handler.Reviews = &review.Trigger{
		Store: f.st, Apps: provider, Jobs: jobs, Checks: checks, Box: box,
	}
	return &gateFixture{webhookFixture: f}
}

// seedGatedPR creates the enabled/team-assigned repository, the user-visible
// pull request and a policy that gates reviews on green CI.
func (f *gateFixture) seedGatedPR() store.PullRequest {
	f.t.Helper()

	f.seedRepoWithTeam()
	pr := f.seedOpenPR()

	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "gated", Provider: "openai", Model: "gpt-4o-mini",
		APIKeyEnc: "sealed", Enabled: true, Concurrency: 1, HealthStatus: store.HealthUnknown,
	})
	if err != nil {
		f.t.Fatalf("CreateLLMProfile: %v", err)
	}
	if _, err := f.st.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: f.repoTeam(), LLMProfileID: profile.ID, RequireGreenCI: true,
	}); err != nil {
		f.t.Fatalf("SaveQAPolicy: %v", err)
	}
	return pr
}

func (f *gateFixture) runCount(prID uint) int {
	f.t.Helper()

	n, err := f.st.ListReviewRuns(f.ctx, prID)
	if err != nil {
		f.t.Fatalf("ListReviewRuns: %v", err)
	}
	return len(n)
}

// With gating off, a CI delivery refreshes the counters but must not become a
// second review trigger: the default behaviour of existing deployments stays
// exactly as it was.
func TestWebhookCheckEventDoesNotTriggerUngatedReview(t *testing.T) {
	api := newFakeChecksGitHub(t)
	f := newGateFixture(t, api)

	f.seedRepoWithTeam()
	pr := f.seedOpenPR()
	profile, err := f.st.CreateLLMProfile(f.ctx, store.LLMProfile{
		Name: "ungated", Provider: "openai", Model: "gpt-4o-mini",
		APIKeyEnc: "sealed", Enabled: true, Concurrency: 1, HealthStatus: store.HealthUnknown,
	})
	if err != nil {
		t.Fatalf("CreateLLMProfile: %v", err)
	}
	if _, err := f.st.SaveQAPolicy(f.ctx, store.QAPolicy{
		TeamID: f.repoTeam(), LLMProfileID: profile.ID,
	}); err != nil {
		t.Fatalf("SaveQAPolicy: %v", err)
	}

	api.setRuns("aaa111", "success", "success")
	body := []byte(`{"action":"completed","repository":{"id":501},"check_run":{"head_sha":"aaa111","status":"completed","conclusion":"success"}}`)
	assertStatus(t, f.deliverBody("check_run", body, signWebhook(body)), http.StatusAccepted)

	if got := f.runCount(pr.ID); got != 0 {
		t.Fatalf("runs = %d, want none from a check event with gating off", got)
	}
	if got := f.validationsOf(t, pr.ID, "aaa111"); got != (store.PRValidations{Passed: 2, Total: 2}) {
		t.Errorf("validations = %+v, want 2/2", got)
	}
}

func TestWebhookCIGateBlocksThenReleasesReview(t *testing.T) {
	api := newFakeChecksGitHub(t)
	f := newGateFixture(t, api)
	pr := f.seedGatedPR()

	api.setRunningRuns("aaa111", 2)
	body := []byte(`{"action":"created","repository":{"id":501},"check_run":{"head_sha":"aaa111","status":"in_progress"}}`)
	assertStatus(t, f.deliverBody("check_run", body, signWebhook(body)), http.StatusAccepted)
	if got := f.runCount(pr.ID); got != 0 {
		t.Fatalf("runs = %d, want none while checks are running", got)
	}

	api.setRuns("aaa111", "failure", "success")
	body = []byte(`{"action":"completed","repository":{"id":501},"check_run":{"head_sha":"aaa111","status":"completed","conclusion":"failure"}}`)
	assertStatus(t, f.deliverBody("check_run", body, signWebhook(body)), http.StatusAccepted)
	if got := f.runCount(pr.ID); got != 0 {
		t.Fatalf("runs = %d, want none while a check failed", got)
	}

	api.setRuns("aaa111", "success", "success")
	body = []byte(`{"action":"completed","repository":{"id":501},"check_suite":{"head_sha":"aaa111","status":"completed","conclusion":"success"}}`)
	assertStatus(t, f.deliverBody("check_suite", body, signWebhook(body)), http.StatusAccepted)
	if got := f.runCount(pr.ID); got != 1 {
		t.Fatalf("runs = %d, want exactly one once CI turned green", got)
	}

	// A redelivery of the green head must not queue a second run.
	assertStatus(t, f.deliverBody("check_suite", body, signWebhook(body)), http.StatusAccepted)
	if got := f.runCount(pr.ID); got != 1 {
		t.Errorf("runs = %d after redelivery, want 1", got)
	}

	stored, err := f.st.PRValidationsFor(f.ctx, pr.ID, "aaa111")
	if err != nil {
		t.Fatalf("PRValidationsFor: %v", err)
	}
	if stored != (store.PRValidations{Passed: 2, Total: 2}) {
		t.Errorf("stored validations = %+v, want 2/2", stored)
	}
}
