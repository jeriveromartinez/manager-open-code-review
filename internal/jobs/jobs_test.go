package jobs_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	ghapp "github.com/jeriveromartinez/qa-control-center/internal/github"
	"github.com/jeriveromartinez/qa-control-center/internal/jobs"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
	"github.com/jeriveromartinez/qa-control-center/internal/syncer"
	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

type stubAppProvider struct {
	app *ghapp.App
}

func (p stubAppProvider) App(context.Context) (*ghapp.App, error) {
	return p.app, nil
}

func testPEM(t *testing.T) string {
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

// TestControlClientRunsSyncJob verifies the whole path from EnqueueSync through
// River to the real Syncer: the fake GitHub records the installation ID that
// reached its token endpoint.
func TestControlClientRunsSyncJob(t *testing.T) {
	s := testutil.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan int64, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				http.Error(w, "bad installation id", http.StatusBadRequest)
				return
			}
			received <- id
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.URL.Path == "/installation/repositories":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"total_count":0,"repositories":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	sy := &syncer.Syncer{
		Store: s,
		Apps: stubAppProvider{app: &ghapp.App{
			Creds:  ghapp.AppCredentials{AppID: 12345, PEM: testPEM(t)},
			APIURL: srv.URL,
		}},
	}
	c, err := jobs.NewControlClient(s.Pool, sy)
	if err != nil {
		t.Fatalf("NewControlClient() error = %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := c.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	}()

	if err := s.UpsertInstallation(ctx, store.Installation{ID: 7, AccountLogin: "acme", AccountType: "Organization"}); err != nil {
		t.Fatalf("UpsertInstallation(7): %v", err)
	}

	if err := jobs.NewEnqueuer(c).EnqueueSync(ctx, 7); err != nil {
		t.Fatalf("EnqueueSync(7) error = %v", err)
	}

	select {
	case id := <-received:
		if id != 7 {
			t.Fatalf("installation ID received by fake GitHub = %d, want 7", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync job did not reach fake GitHub within 5 seconds")
	}
}

// TestControlClientEnqueuesWorkerOwnedKinds verifies that the control process
// can insert jobs handled only by the worker process. River's unknown-kind
// check made health checks, reviews and therefore publication impossible.
func TestControlClientEnqueuesWorkerOwnedKinds(t *testing.T) {
	s := testutil.NewStore(t)
	client, err := jobs.NewControlClient(s.Pool, &syncer.Syncer{Store: s})
	if err != nil {
		t.Fatalf("NewControlClient() error = %v", err)
	}
	enqueuer := jobs.NewEnqueuer(client)
	ctx := context.Background()
	if err := enqueuer.EnqueueLLMHealth(ctx, 1); err != nil {
		t.Errorf("EnqueueLLMHealth() error = %v", err)
	}
	if err := enqueuer.EnqueueReview(ctx, 2); err != nil {
		t.Errorf("EnqueueReview() error = %v", err)
	}
}

func TestReviewJobArgs(t *testing.T) {
	args := jobs.ReviewRunArgs{RunID: 17}
	if args.Kind() != "review_run" {
		t.Errorf("Kind() = %q, want review_run", args.Kind())
	}
	opts := args.InsertOpts()
	if opts.Queue != "review" || opts.MaxAttempts != 3 {
		t.Errorf("InsertOpts() = queue %q attempts %d, want review/3", opts.Queue, opts.MaxAttempts)
	}
}

func TestPublishJobArgs(t *testing.T) {
	args := jobs.PublishReviewArgs{RunID: 23}
	if args.Kind() != "publish_review" {
		t.Errorf("Kind() = %q, want publish_review", args.Kind())
	}
	opts := args.InsertOpts()
	if opts.Queue != "publish" || opts.MaxAttempts != 5 {
		t.Errorf("InsertOpts() = queue %q attempts %d, want publish/5", opts.Queue, opts.MaxAttempts)
	}
}

func TestLLMHealthJobArgs(t *testing.T) {
	args := jobs.LLMHealthArgs{ProfileID: 31}
	if args.Kind() != "llm_health" {
		t.Errorf("Kind() = %q, want llm_health", args.Kind())
	}
	opts := args.InsertOpts()
	if opts.Queue != "llm_health" || opts.MaxAttempts != 1 {
		t.Errorf("InsertOpts() = queue %q attempts %d, want llm_health/1", opts.Queue, opts.MaxAttempts)
	}
}

func TestReviewTriggerJobArgs(t *testing.T) {
	args := jobs.ReviewTriggerArgs{PRID: 12, HeadSHA: "sha-abc"}
	if args.Kind() != "review_trigger" {
		t.Errorf("Kind() = %q, want review_trigger", args.Kind())
	}
	opts := args.InsertOpts()
	if opts.Queue != "control" || opts.MaxAttempts != 3 {
		t.Errorf("InsertOpts() = queue %q attempts %d, want control/3", opts.Queue, opts.MaxAttempts)
	}
}

// The debounce lives in the database, not in the process: the insert must carry
// the captured head and a scheduled_at far enough in the future, and a
// redelivery of the same push must collapse into the same job.
func TestEnqueueReviewTriggerSchedulesJob(t *testing.T) {
	s := testutil.NewStore(t)
	client, err := jobs.NewControlClient(s.Pool, &syncer.Syncer{Store: s})
	if err != nil {
		t.Fatalf("NewControlClient() error = %v", err)
	}
	enqueuer := jobs.NewEnqueuer(client)
	ctx := context.Background()

	before := time.Now()
	const delay = 2 * time.Minute
	if err := enqueuer.EnqueueReviewTrigger(ctx, 42, "sha-debounce", delay); err != nil {
		t.Fatalf("EnqueueReviewTrigger() error = %v", err)
	}
	if err := enqueuer.EnqueueReviewTrigger(ctx, 42, "sha-debounce", delay); err != nil {
		t.Fatalf("duplicate EnqueueReviewTrigger() error = %v", err)
	}

	var (
		kind        string
		rawArgs     []byte
		scheduledAt time.Time
		queue       string
		attempts    int
	)
	if err := s.Pool.QueryRow(ctx, `
		SELECT kind, args, scheduled_at, queue, max_attempts
		FROM river_job WHERE kind = 'review_trigger'`).
		Scan(&kind, &rawArgs, &scheduledAt, &queue, &attempts); err != nil {
		t.Fatalf("read scheduled job: %v", err)
	}
	if kind != "review_trigger" || queue != "control" || attempts != 3 {
		t.Errorf("job = kind %q queue %q attempts %d, want review_trigger/control/3", kind, queue, attempts)
	}

	var args struct {
		PRID    uint   `json:"pr_id"`
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		t.Fatalf("decode args %s: %v", rawArgs, err)
	}
	if args.PRID != 42 || args.HeadSHA != "sha-debounce" {
		t.Errorf("args = %+v, want pr_id 42 head_sha sha-debounce", args)
	}
	if scheduledAt.Before(before.Add(delay-time.Second)) || scheduledAt.After(time.Now().Add(delay+time.Second)) {
		t.Errorf("scheduled_at = %v, want about %v from now", scheduledAt, delay)
	}

	var count int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'review_trigger'`).Scan(&count); err != nil {
		t.Fatalf("count scheduled jobs: %v", err)
	}
	if count != 1 {
		t.Errorf("scheduled jobs = %d, want 1: identical pushes share the job", count)
	}
}

// A repair may enqueue a run the queue already holds. River must collapse the
// duplicate into the live job instead of running the same review twice, while a
// finalized job must not prevent a later repair from scheduling it again.
func TestEnqueueReviewCollapsesLiveDuplicate(t *testing.T) {
	s := testutil.NewStore(t)
	client, err := jobs.NewControlClient(s.Pool, &syncer.Syncer{Store: s})
	if err != nil {
		t.Fatalf("NewControlClient() error = %v", err)
	}
	enqueuer := jobs.NewEnqueuer(client)
	ctx := context.Background()

	count := func(where string) int {
		t.Helper()
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'review_run' `+where).Scan(&n); err != nil {
			t.Fatalf("count review jobs: %v", err)
		}
		return n
	}

	for range 2 {
		if err := enqueuer.EnqueueReview(ctx, 7); err != nil {
			t.Fatalf("EnqueueReview() error = %v", err)
		}
	}
	if live := count(`AND state IN ('available','running','scheduled','retryable','pending')`); live != 1 {
		t.Errorf("live review jobs = %d, want 1 for the same run", live)
	}

	// A finalized job releases the unique key: the run can be scheduled again
	// by the reconciliation sweep.
	if _, err := s.Pool.Exec(ctx,
		`UPDATE river_job SET state = 'completed', finalized_at = now() WHERE kind = 'review_run'`); err != nil {
		t.Fatalf("finalize review job: %v", err)
	}
	if err := enqueuer.EnqueueReview(ctx, 7); err != nil {
		t.Fatalf("EnqueueReview() after finalization error = %v", err)
	}
	if live := count(`AND state IN ('available','running','scheduled','retryable','pending')`); live != 1 {
		t.Errorf("live review jobs after finalization = %d, want the repaired one", live)
	}
	if total := count(`AND state = 'completed'`); total != 1 {
		t.Errorf("completed review jobs = %d, want the original one", total)
	}
}

type recordingTrigger struct {
	fired chan string
}

func (r *recordingTrigger) EnqueuePending(context.Context, int) (int, error) { return 0, nil }

func (r *recordingTrigger) ReconcileQueuedRuns(context.Context, int) (int, error) { return 0, nil }

func (r *recordingTrigger) FireDebounced(_ context.Context, prID uint, headSHA string) error {
	r.fired <- fmt.Sprintf("%d:%s", prID, headSHA)
	return nil
}

// The control client must run the scheduled job itself and call FireDebounced
// with the args captured at scheduling time.
func TestControlClientRunsReviewTriggerJob(t *testing.T) {
	s := testutil.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trigger := &recordingTrigger{fired: make(chan string, 1)}
	client, err := jobs.NewControlClient(s.Pool, &syncer.Syncer{Store: s}, jobs.ControlDeps{Trigger: trigger})
	if err != nil {
		t.Fatalf("NewControlClient() error = %v", err)
	}
	startClient(t, ctx, client)

	if err := jobs.NewEnqueuer(client).EnqueueReviewTrigger(ctx, 9, "sha-fire", 0); err != nil {
		t.Fatalf("EnqueueReviewTrigger() error = %v", err)
	}

	select {
	case got := <-trigger.fired:
		if got != "9:sha-fire" {
			t.Errorf("FireDebounced args = %q, want 9:sha-fire", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("review_trigger job was not worked within 10s")
	}
}
