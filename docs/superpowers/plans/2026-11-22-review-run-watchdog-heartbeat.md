# Watchdog heartbeat (#130) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the watchdog distinguish a worker that stopped responding from a review that overran its maximum time, and make the worker cancel a run the watchdog already closed.

**Architecture:** A running review refreshes `review_runs.heartbeat_at` once a minute. The watchdog keeps its single atomic UPDATE but classifies each stale row by heartbeat age: fresh heartbeat means overrun, silence means a lost worker. The worker's own heartbeat write fails with `ErrRunNotActive` once the run is terminal, so it cancels its pipeline instead of finishing blind.

**Tech Stack:** Go 1.27, gorm + goose, PostgreSQL, Vue settings panel (no web code expected beyond help text served from Go).

**Spec:** `docs/superpowers/specs/2026-11-22-review-run-recovery-design.md`

## Global Constraints

- One issue per branch and PR; this branch is `fix/review-watchdog-heartbeat` and the PR body must contain `Closes #130`.
- Commits in English, Conventional Commits, subject under 72 characters. Conversation and user-facing copy in Spanish.
- No new dependencies.
- Tests run locally only; never add or edit CI workflows.
- Do not change the default `review_timeout` (45m) or `review_stale_after` (60m).
- Go gate (Git Bash, Docker daemon `kiri`):

```sh
MSYS_NO_PATHCONV=1 docker --context kiri run --rm --network host \
  -v 'E:\Projects\manager-open-code-review:/src' \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v qa-go-mod:/go/pkg/mod -v qa-go-build:/root/.cache/go-build \
  -w /src -e DOCKER_HOST=unix:///var/run/docker.sock -e GOFLAGS=-buildvcs=false \
  golang:1.27 /usr/local/go/bin/go test ./internal/store ./internal/review ./internal/settings -count=1
```

## Review Focus

- A running row with a `NULL` heartbeat (pre-migration or claim without a beat) must be reported as a lost worker, not an overrun (Task 2).
- A fresh heartbeat with an old `started_at` must be reported as overrun (Task 2).
- Two concurrent sweeps must close a run exactly once and leave one final message (Task 2).
- Exactly the 5-minute margin must be accepted and anything below rejected (Task 4).
- A run closed externally must make the executing worker stop instead of waiting for its OCR budget (Task 3).

---

### Task 0: Branch

- [ ] **Step 1: Create the branch from an up-to-date main**

```sh
git switch main && git pull --rebase && git switch -c fix/review-watchdog-heartbeat
```

- [ ] **Step 2: Commit this plan**

```sh
git add docs/superpowers/plans/2026-11-22-review-run-watchdog-heartbeat.md
git commit -m "docs(review): plan the watchdog heartbeat"
```

---

### Task 1: Persist a heartbeat while a run executes

**Files:**
- Create: `internal/store/migrations/00013_review_run_heartbeat.sql`
- Modify: `internal/store/models.go` (`ReviewRun`)
- Modify: `internal/store/review.go` (`ClaimReviewRun`, new `HeartbeatReviewRun`)
- Test: `internal/store/review_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `ReviewRun.HeartbeatAt *time.Time` (`heartbeat_at`).
  - `func (s *Store) HeartbeatReviewRun(ctx context.Context, id uint) error`, returning `ErrRunNotActive` when the run is not `running`.

- [ ] **Step 1: Write the failing store test**

Add to `internal/store/review_test.go`:

```go
func TestReviewRunHeartbeat(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)
	run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
	})
	if err != nil {
		t.Fatalf("CreateReviewRun: %v", err)
	}

	claimed, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("ClaimReviewRun: ok = %v, err = %v", ok, err)
	}
	if claimed.HeartbeatAt == nil {
		t.Fatal("claimed run has no heartbeat_at")
	}

	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", run.ID).Update("heartbeat_at", old).Error; err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	if err := f.s.HeartbeatReviewRun(f.ctx, run.ID); err != nil {
		t.Fatalf("HeartbeatReviewRun: %v", err)
	}
	stored, err := f.s.ReviewRun(f.ctx, run.ID)
	if err != nil {
		t.Fatalf("ReviewRun: %v", err)
	}
	if stored.HeartbeatAt == nil || !stored.HeartbeatAt.After(old) {
		t.Errorf("heartbeat_at = %v, want a time after %v", stored.HeartbeatAt, old)
	}

	if err := f.s.CompleteReviewRun(f.ctx, run.ID, store.RunCompletion{
		Status: store.RunFailed, ErrorCode: "timeout",
	}); err != nil {
		t.Fatalf("CompleteReviewRun: %v", err)
	}
	if err := f.s.HeartbeatReviewRun(f.ctx, run.ID); !errors.Is(err, store.ErrRunNotActive) {
		t.Fatalf("HeartbeatReviewRun on a finished run = %v, want ErrRunNotActive", err)
	}
}
```

- [ ] **Step 2: Run it and confirm the failure**

Run the Go gate with `-run '^TestReviewRunHeartbeat$'`. Expected: compile error (`HeartbeatAt` and `HeartbeatReviewRun` do not exist).

- [ ] **Step 3: Implement the column, model field and method**

Create `internal/store/migrations/00013_review_run_heartbeat.sql`:

```sql
-- +goose Up
-- Last sign of life of the worker executing a run (#130). The watchdog uses it
-- to tell a vanished worker from a review that overran its maximum time.
ALTER TABLE review_runs ADD COLUMN heartbeat_at timestamptz;

-- +goose Down
ALTER TABLE review_runs DROP COLUMN IF EXISTS heartbeat_at;
```

In `internal/store/models.go`, add inside `ReviewRun` next to `Phase`:

```go
	// HeartbeatAt is refreshed by the executing worker every minute. Nil on
	// runs claimed before the heartbeat existed (#130).
	HeartbeatAt *time.Time `gorm:"column:heartbeat_at"`
```

In `ClaimReviewRun`, add `"heartbeat_at": now,` to the `Updates` map.

Add in `internal/store/review.go`:

```go
// HeartbeatReviewRun records that the worker executing the run is alive. Only a
// running run accepts the write: ErrRunNotActive tells the worker that the
// watchdog or a new head already closed it, so it must stop the pipeline.
func (s *Store) HeartbeatReviewRun(ctx context.Context, id uint) error {
	res := s.DB.WithContext(ctx).Model(&ReviewRun{}).
		Where("id = ? AND status = ?", id, RunRunning).
		Update("heartbeat_at", time.Now().UTC())
	if res.Error != nil {
		return fmt.Errorf("heartbeat review run %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("heartbeat review run %d: %w", id, ErrRunNotActive)
	}
	return nil
}
```

- [ ] **Step 4: Run the store tests**

Run: Go gate with `-run 'TestReviewRunHeartbeat|TestReviewRunClaim|TestCompleteReviewRun'`. Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/store/migrations/00013_review_run_heartbeat.sql internal/store/models.go internal/store/review.go internal/store/review_test.go
git commit -m "feat(review): record a heartbeat while a run executes"
```

---

### Task 2: Classify stale runs by heartbeat

**Files:**
- Modify: `internal/store/review.go` (`InterruptStaleRuns` + new `StaleRunPolicy`)
- Modify: `internal/review/watchdog.go` (constants and messages)
- Test: `internal/store/review_test.go`, `internal/review/watchdog_test.go`

**Interfaces:**
- Consumes: `ReviewRun.HeartbeatAt` from Task 1.
- Produces:
  - `type StaleRunPolicy struct { StartedBefore, HeartbeatBefore time.Time; OverranMessage, LostWorkerMessage string }`.
  - `func (s *Store) InterruptStaleRuns(ctx context.Context, p StaleRunPolicy) ([]ReviewRun, error)`.
  - Package constants `heartbeatInterval = time.Minute` and `heartbeatTimeout = 3 * time.Minute` in `internal/review`.

- [ ] **Step 1: Write the failing store test**

In `internal/store/review_test.go`, update `TestInterruptStaleRuns`: add an `overrun` run alongside `stale`, keep `recent` and `queued`, and call:

```go
	now := time.Now().UTC()
	policy := store.StaleRunPolicy{
		StartedBefore:     now.Add(-time.Hour),
		HeartbeatBefore:   now.Add(-3 * time.Minute),
		OverranMessage:    "la revisión superó el tiempo máximo de 60 min sin terminar",
		LostWorkerMessage: "el worker dejó de responder hace más de 3 min (posible reinicio o caída)",
	}
	runs, err := f.s.InterruptStaleRuns(f.ctx, policy)
```

The `overrun` run starts 3h ago and gets a fresh heartbeat:

```go
	overrunStarted := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Microsecond)
	overrunPR := f.openPR(repo.ID, 4, store.StatusReviewing, false)
	overrun, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
		PullRequestID: overrunPR.ID, HeadSHA: overrunPR.HeadSHA, Status: store.RunRunning,
		StartedAt: &overrunStarted, CloneTokenEnc: "sealed-token",
	})
	if err != nil {
		t.Fatalf("create overrun run: %v", err)
	}
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", overrun.ID).
		Update("heartbeat_at", time.Now().UTC()).Error; err != nil {
		t.Fatalf("set overrun heartbeat: %v", err)
	}
```

Assert both were returned and their messages differ:

```go
	byID := map[uint]store.ReviewRun{}
	for _, run := range runs {
		byID[run.ID] = run
	}
	if len(byID) != 2 {
		t.Fatalf("interrupted runs = %v, want %d and %d", prIDsOfRuns(runs), stale.ID, overrun.ID)
	}
	for _, run := range []store.ReviewRun{stale, overrun} {
		got, err := f.s.ReviewRun(f.ctx, run.ID)
		if err != nil {
			t.Fatalf("ReviewRun(%d): %v", run.ID, err)
		}
		if got.Status != store.RunFailed || got.ErrorCode != store.RunErrorInterrupted {
			t.Errorf("run %d = %q/%q, want failed/%s",
				run.ID, got.Status, got.ErrorCode, store.RunErrorInterrupted)
		}
	}
	if msg := byID[stale.ID].ErrorMessage; !strings.Contains(msg, "dejó de responder") {
		t.Errorf("stale run message = %q, want the lost-worker wording", msg)
	}
	if msg := byID[overrun.ID].ErrorMessage; !strings.Contains(msg, "tiempo máximo") {
		t.Errorf("overrun run message = %q, want the overrun wording", msg)
	}
```

Add the `strings` import to `internal/store/review_test.go`.

Update the second-sweep call to the same `policy`, and update `TestInterruptStaleRunsConcurrent` to build an equivalent policy. In the concurrent test, assert the final message contains `"dejó de responder"` (that run has a `NULL` heartbeat).

- [ ] **Step 2: Write the failing watchdog test**

In `internal/review/watchdog_test.go`, add a helper after `runningRun`:

```go
func (f *watchdogFixture) beat(t *testing.T, run store.ReviewRun, at time.Time) {
	t.Helper()
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", run.ID).Update("heartbeat_at", at).Error; err != nil {
		t.Fatalf("set heartbeat of run %d: %v", run.ID, err)
	}
}
```

In `TestWatchdogSweepPublishesInterruptedRuns`, after creating `first` and `second`:

```go
	f.beat(t, first, time.Now().UTC().Add(-time.Minute)) // alive: overran
	// second keeps a NULL heartbeat: its worker is gone.
```

Inside the per-run loop, assert the message:

```go
		wantMessage := map[uint]string{
			first.ID:  "tiempo máximo",
			second.ID: "dejó de responder",
		}[run.ID]
		if !strings.Contains(stored.ErrorMessage, wantMessage) {
			t.Errorf("run %d message = %q, want %q", run.ID, stored.ErrorMessage, wantMessage)
		}
```

Add the `strings` import.

- [ ] **Step 3: Run the tests and confirm the failure**

Run: Go gate with `-run 'TestInterruptStaleRuns|TestWatchdogSweep'`. Expected: compile error (`StaleRunPolicy` does not exist).

- [ ] **Step 4: Implement the policy and the messages**

In `internal/store/review.go`, replace the `InterruptStaleRuns` signature and query:

```go
// StaleRunPolicy decides which running rows the watchdog closes and the message
// each case leaves behind. HeartbeatBefore is the oldest heartbeat considered
// alive: older silence means the worker is gone, not that the review overran.
type StaleRunPolicy struct {
	StartedBefore     time.Time
	HeartbeatBefore   time.Time
	OverranMessage    string
	LostWorkerMessage string
}

// InterruptStaleRuns fails every running run whose start is older than the
// policy cutoff and returns the runs it moved. A fresh heartbeat classifies the
// run as overrun; silence classifies it as a lost worker. Queued and terminal
// runs are never touched, and the single UPDATE keeps concurrent sweeps exact.
func (s *Store) InterruptStaleRuns(ctx context.Context, p StaleRunPolicy) ([]ReviewRun, error) {
	now := time.Now().UTC()
	const query = `
UPDATE review_runs
SET status = ?, error_code = ?, error_message = CASE
        WHEN heartbeat_at IS NULL OR heartbeat_at < ? THEN ?
        ELSE ? END,
    clone_token_enc = '', phase = '', phase_started_at = NULL, finished_at = ?, updated_at = ?
WHERE status = ? AND COALESCE(started_at, created_at) < ?
RETURNING *`

	var runs []ReviewRun
	err := s.DB.WithContext(ctx).Raw(query,
		RunFailed, RunErrorInterrupted,
		p.HeartbeatBefore, p.LostWorkerMessage, p.OverranMessage,
		now, now, RunRunning, p.StartedBefore,
	).Scan(&runs).Error
	if err != nil {
		return nil, fmt.Errorf("interrupt review runs started before %s: %w",
			p.StartedBefore.UTC().Format(time.RFC3339), err)
	}
	return runs, nil
}
```

In `internal/review/watchdog.go`, add the constants and rewrite the sweep header:

```go
const (
	// heartbeatInterval is how often a running review refreshes heartbeat_at.
	heartbeatInterval = time.Minute
	// heartbeatTimeout tolerates three missed beats, so one slow tick does not
	// look like a dead worker.
	heartbeatTimeout = 3 * time.Minute
)

// reviewMinutes renders a duration in whole minutes for operator-facing text.
func reviewMinutes(d time.Duration) string {
	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}
```

In `Sweep`:

```go
	now := time.Now().UTC()
	runs, err := w.Store.InterruptStaleRuns(ctx, store.StaleRunPolicy{
		StartedBefore:   now.Add(-w.StaleAfter),
		HeartbeatBefore: now.Add(-heartbeatTimeout),
		OverranMessage: fmt.Sprintf(
			"la revisión superó el tiempo máximo de %s sin terminar", reviewMinutes(w.StaleAfter)),
		LostWorkerMessage: fmt.Sprintf(
			"el worker dejó de responder hace más de %s (posible reinicio o caída)",
			reviewMinutes(heartbeatTimeout)),
	})
```

Keep the rest of `Sweep` unchanged (PR status, events, publish jobs, log line).

- [ ] **Step 5: Run the tests and confirm they pass**

Run: Go gate with `-run 'TestInterruptStaleRuns|TestWatchdogSweep'`. Expected: PASS.

- [ ] **Step 6: Commit**

```sh
git add internal/store/review.go internal/store/review_test.go internal/review/watchdog.go internal/review/watchdog_test.go
git commit -m "fix(review): distinguish an overrun review from a lost worker"
```

---

### Task 3: Cancel the pipeline when the watchdog closes the run

**Files:**
- Modify: `internal/review/run.go` (`Runner`, `heartbeat`, `execute`)
- Test: `internal/review/run_test.go`

**Interfaces:**
- Consumes: `Store.HeartbeatReviewRun`, `ErrRunNotActive` (Task 1), `heartbeatInterval` (Task 2).
- Produces: `Runner.heartbeatEvery time.Duration` (test-only override; zero uses `heartbeatInterval`).

- [ ] **Step 1: Write the failing test**

Add to `internal/review/run_test.go`:

```go
// TestRunStopsWhenWatchdogClosesTheRun proves the worker abandons a review the
// watchdog already failed instead of waiting for OCR to finish its budget.
func TestRunStopsWhenWatchdogClosesTheRun(t *testing.T) {
	f := newRunTestFixture(t)
	f.setOCR(t, `{
	  "status": "complete",
	  "summary": {"files_reviewed": 1, "comments": 0, "total_tokens": 10},
	  "manifest": {"coverage": {"completed": [{"path": "auth.go"}],
	    "failed": [], "waived": [], "excluded": []}}
	}`)
	t.Setenv("RUN_FAKE_OCR_SLEEP", "10")

	runner := f.runner()
	runner.heartbeatEvery = 20 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- runner.Run(f.ctx, f.runID) }()

	time.Sleep(100 * time.Millisecond)
	if err := f.s.DB.WithContext(f.ctx).Model(&store.ReviewRun{}).
		Where("id = ?", f.runID).
		Updates(map[string]any{
			"status":     store.RunFailed,
			"error_code": store.RunErrorInterrupted,
		}).Error; err != nil {
		t.Fatalf("close the run like the watchdog: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after the run was closed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run() kept executing after the watchdog closed the run")
	}
}
```

- [ ] **Step 2: Run it and confirm the failure**

Run: Go gate with `-run '^TestRunStopsWhenWatchdogClosesTheRun$'`. Expected: compile error (`heartbeatEvery` does not exist); after adding only the field it fails with `Run() kept executing after the watchdog closed the run`.

- [ ] **Step 3: Implement the heartbeat**

In `internal/review/run.go`, add to `Runner`:

```go
	// heartbeatEvery overrides heartbeatInterval in tests. Zero uses the
	// production interval.
	heartbeatEvery time.Duration
```

Add next to `execute`:

```go
const heartbeatWriteTimeout = 5 * time.Second

// heartbeat keeps the run marked alive while the worker executes it. When the
// store reports the run is no longer running, the watchdog or a new head closed
// it, so abort cancels the pipeline instead of letting it finish blind.
func (r *Runner) heartbeat(ctx context.Context, runID uint, log *runLogSink, abort context.CancelFunc) func() {
	interval := r.heartbeatEvery
	if interval <= 0 {
		interval = heartbeatInterval
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), heartbeatWriteTimeout)
				err := r.Store.HeartbeatReviewRun(writeCtx, runID)
				cancel()
				switch {
				case err == nil:
				case errors.Is(err, store.ErrRunNotActive):
					log.Log(context.WithoutCancel(ctx), "worker", "", "warn", logPhaseFinish,
						"el run ya no está activo; se cancela la revisión en curso")
					abort()
					return
				default:
					slog.Warn("review run heartbeat failed", "run_id", runID, "error", err)
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
```

In `execute`, replace the current context block (`runCtx := ctx` … `defer cancel()`) with:

```go
	pipelineCtx, cancelPipeline := context.WithCancel(ctx)
	defer cancelPipeline()
	runCtx := pipelineCtx
	if r.Cfg.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(pipelineCtx, r.Cfg.Timeout)
		defer cancelTimeout()
	}
	stopHeartbeat := r.heartbeat(runCtx, run.ID, log, cancelPipeline)
	defer stopHeartbeat()
```

- [ ] **Step 4: Run the test and the review suite**

Run: Go gate with `-run '^TestRunStopsWhenWatchdogClosesTheRun$'` → PASS.
Run: Go gate with `-run 'TestRun'` → PASS (confirm no existing run test regressed).

- [ ] **Step 5: Commit**

```sh
git add internal/review/run.go internal/review/run_test.go
git commit -m "fix(review): cancel a run the watchdog already closed"
```

---

### Task 4: Enforce the margin and document both settings

**Files:**
- Modify: `internal/settings/apply.go` (constant + `validatePairs`)
- Modify: `internal/settings/settings.go` (help text for both keys)
- Test: `internal/settings/apply_test.go`

**Interfaces:**
- Produces: `settings.MinReviewStaleMargin = 5 * time.Minute`.

- [ ] **Step 1: Write the failing test**

Add to `internal/settings/apply_test.go`:

```go
func TestApplyEnforcesMinimumStaleMargin(t *testing.T) {
	cfg := config.Default()
	err := Apply(&cfg, map[string]string{
		KeyReviewTimeout:    "57m",
		KeyReviewStaleAfter: "60m",
	})
	var keyErr *KeyError
	if !errors.As(err, &keyErr) || keyErr.Key != KeyReviewStaleAfter {
		t.Fatalf("timeout 57m with stale 60m = %v, want KeyError on %s", err, KeyReviewStaleAfter)
	}

	cfg = config.Default()
	if err := Apply(&cfg, map[string]string{
		KeyReviewTimeout:    "55m",
		KeyReviewStaleAfter: "60m",
	}); err != nil {
		t.Fatalf("timeout 55m with stale 60m = %v, want the 5m margin accepted", err)
	}
}
```

- [ ] **Step 2: Run it and confirm the failure**

Run the Go gate with `-run '^TestApplyEnforcesMinimumStaleMargin$'`. Expected: FAIL because 57m is accepted today.

- [ ] **Step 3: Implement the margin**

In `internal/settings/apply.go`, above `validatePairs`:

```go
// MinReviewStaleMargin is the room the watchdog needs between the OCR deadline
// and the orphan threshold to clone, parse and publish (#130).
const MinReviewStaleMargin = 5 * time.Minute
```

Replace the `ReviewStaleAfter` check with:

```go
	if margin := cfg.ReviewStaleAfter - cfg.ReviewTimeout; margin < MinReviewStaleMargin {
		return &KeyError{KeyReviewStaleAfter, fmt.Sprintf(
			"must exceed review_timeout (%s) by at least %s so the watchdog leaves room to clone and publish",
			cfg.ReviewTimeout, MinReviewStaleMargin)}
	}
```

- [ ] **Step 4: Update the settings help**

In `internal/settings/settings.go`:

- `KeyReviewTimeout` Help: `"Presupuesto de la revisión. OCR lo reparte entre sus tareas y entrega lo revisado antes de agotarlo; depende del modelo y del tamaño del PR."`
- `KeyReviewStaleAfter` Help: `"Red de seguridad del watchdog: un run en curso más antiguo que esto se cierra. Si el worker sigue enviando latido, el aviso habla de tiempo máximo; si dejó de latir, de worker sin respuesta. Debe superar el tiempo máximo por al menos 5 min."`

- [ ] **Step 5: Run the settings and API tests**

Run: Go gate with `-run 'TestApply|TestSettings'`. Expected: PASS.

- [ ] **Step 6: Commit**

```sh
git add internal/settings/apply.go internal/settings/settings.go internal/settings/apply_test.go
git commit -m "fix(settings): keep a margin above the review timeout"
```

---

### Task 5: Publish the branch

- [ ] **Step 1: Run the full local gate**

```sh
MSYS_NO_PATHCONV=1 docker --context kiri run --rm --network host \
  -v 'E:\Projects\manager-open-code-review:/src' \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v qa-go-mod:/go/pkg/mod -v qa-go-build:/root/.cache/go-build \
  -w /src -e DOCKER_HOST=unix:///var/run/docker.sock -e GOFLAGS=-buildvcs=false \
  golang:1.27 sh -lc '/usr/local/go/bin/go vet ./... && /usr/local/go/bin/go test ./... -count=1'
npm --prefix web run test -- --run
npm --prefix web run build
```

Expected: all pass. Report the real summary lines.

- [ ] **Step 2: Push and open the PR**

```sh
git push -u origin fix/review-watchdog-heartbeat
gh pr create --title "fix(review): tell an overrun review from a lost worker" --body-file - <<'EOF'
## What changed
...

## Why
Closes #130

## How it was tested
...

## Follow-up left out
...
EOF
```

The body must explain the heartbeat, the two messages, the worker-side cancellation, the 5-minute margin, the real test output, and that exposing the heartbeat in the panel is out of scope. End with the Claude Code attribution line required by the session.

- [ ] **Step 3: Report the PR URL to the user.**
