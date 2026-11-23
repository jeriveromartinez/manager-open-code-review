# Review run token unknown (#129) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist `NULL` token columns when a run ends without an OCR result and show «sin datos» instead of zeros in the PR detail.

**Architecture:** The pipeline already knows whether it parsed an OCR result. Carry that fact as `runOutcome.tokensKnown` and `store.RunCompletion.TokensKnown`; `CompleteReviewRun` writes the seven token columns as `NULL` when it is false. The Vue detail already renders `NULL` as «sin datos»; only the copy must distinguish a cut run from a legacy row.

**Tech Stack:** Go 1.27, gorm + goose, PostgreSQL, Vue 3 + Vitest.

**Spec:** `docs/superpowers/specs/2026-11-22-review-run-recovery-design.md`

## Global Constraints

- One issue per branch and PR; this branch is `fix/review-run-token-unknown` and the PR body must contain `Closes #129`.
- Commits in English, Conventional Commits, subject under 72 characters. Conversation and user-facing copy in Spanish.
- No new dependencies.
- Tests run locally only; never add or edit CI workflows.
- Unknown token usage is `NULL`; a measured zero stays `0`.
- Go gate (Git Bash, Docker daemon `kiri`):

```sh
MSYS_NO_PATHCONV=1 docker --context kiri run --rm --network host \
  -v 'E:\Projects\manager-open-code-review:/src' \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v qa-go-mod:/go/pkg/mod -v qa-go-build:/root/.cache/go-build \
  -w /src -e DOCKER_HOST=unix:///var/run/docker.sock -e GOFLAGS=-buildvcs=false \
  golang:1.27 /usr/local/go/bin/go test ./internal/store ./internal/review -count=1
```

- Web gate: `npm --prefix web run test -- --run src/views/PrDetailView.test.ts` and `npm --prefix web run build` from the repo root.

## Review Focus

- A measured zero must stay `0`, not `NULL` (Task 1 test).
- A mixed aggregate of known and unknown runs must skip the unknown ones (Task 1 fixture extension).
- A run that never reached OCR must not be counted as zero in either aggregate (Task 1 fixture extension).
- The legacy message must remain for rows without `error_code`, and only cut runs get the new copy (Task 2 tests).
- A run with `error_code` set but token columns populated must keep showing its figures (existing success test; Task 1 keeps it passing).

---

### Task 0: Branch and docs

**Files:**
- Commit: `docs/superpowers/specs/2026-11-22-review-run-recovery-design.md`
- Commit: `docs/superpowers/plans/2026-11-22-review-run-token-unknown.md`

- [ ] **Step 1: Create the branch from an up-to-date main**

```sh
git switch main && git pull --rebase && git switch -c fix/review-run-token-unknown
```

- [ ] **Step 2: Commit the design and this plan**

```sh
git add docs/superpowers/specs/2026-11-22-review-run-recovery-design.md docs/superpowers/plans/2026-11-22-review-run-token-unknown.md
git commit -m "docs(review): plan unknown token usage and watchdog heartbeat"
```

---

### Task 1: Store NULLs when OCR reported no usage

**Files:**
- Modify: `internal/store/review.go` (`RunCompletion`, `CompleteReviewRun`)
- Modify: `internal/store/review_test.go` (existing completion test + new NULL test)
- Modify: `internal/store/qa_test.go` (cut run in both aggregate fixtures)
- Modify: `internal/review/run.go` (`runOutcome`, token persistence)
- Modify: `internal/review/run_test.go` (timeout and partial-result assertions)
- Test: same files

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type RunCompletion struct { ... TokensKnown bool }` in `internal/store`.
  - `runOutcome.tokensKnown bool` in `internal/review`.

- [ ] **Step 1: Write the failing store test**

Add to `internal/store/review_test.go`:

```go
type reviewRunTokenColumns struct {
	InputTokens      *int64 `gorm:"column:input_tokens"`
	OutputTokens     *int64 `gorm:"column:output_tokens"`
	ReasoningTokens  *int64 `gorm:"column:reasoning_tokens"`
	CacheReadTokens  *int64 `gorm:"column:cache_read_tokens"`
	CacheWriteTokens *int64 `gorm:"column:cache_write_tokens"`
	TotalTokens      *int64 `gorm:"column:total_tokens"`
	MaxContextTokens *int64 `gorm:"column:max_context_tokens"`
}

func TestCompleteReviewRunStoresNullTokensWithoutUsage(t *testing.T) {
	f := newFixture(t)
	installID := f.nextID()
	f.installation(installID)
	repo := f.repo(installID, installID, "acme/app", nil, true)
	pr := f.openPR(repo.ID, 1, store.StatusPending, false)

	tokenColumns := func(id uint) reviewRunTokenColumns {
		t.Helper()
		var cols reviewRunTokenColumns
		if err := f.s.DB.Raw(`SELECT input_tokens, output_tokens, reasoning_tokens,
			cache_read_tokens, cache_write_tokens, total_tokens, max_context_tokens
			FROM review_runs WHERE id = ?`, id).Scan(&cols).Error; err != nil {
			t.Fatalf("read token columns: %v", err)
		}
		return cols
	}
	runningRun := func() store.ReviewRun {
		t.Helper()
		run, err := f.s.CreateReviewRun(f.ctx, store.ReviewRun{
			PullRequestID: pr.ID, HeadSHA: pr.HeadSHA,
		})
		if err != nil {
			t.Fatalf("CreateReviewRun: %v", err)
		}
		if _, ok, err := f.s.ClaimReviewRun(f.ctx, run.ID); err != nil || !ok {
			t.Fatalf("ClaimReviewRun: ok = %v, err = %v", ok, err)
		}
		return run
	}

	unknown := runningRun()
	if err := f.s.CompleteReviewRun(f.ctx, unknown.ID, store.RunCompletion{
		Status: store.RunFailed, ErrorCode: "timeout",
	}); err != nil {
		t.Fatalf("CompleteReviewRun(unknown): %v", err)
	}
	cols := tokenColumns(unknown.ID)
	if cols.InputTokens != nil || cols.OutputTokens != nil || cols.ReasoningTokens != nil ||
		cols.CacheReadTokens != nil || cols.CacheWriteTokens != nil ||
		cols.TotalTokens != nil || cols.MaxContextTokens != nil {
		t.Errorf("token columns = %+v, want NULL when no engine reported usage", cols)
	}

	measured := runningRun()
	if err := f.s.CompleteReviewRun(f.ctx, measured.ID, store.RunCompletion{
		Status: store.RunSucceeded, TokensKnown: true,
	}); err != nil {
		t.Fatalf("CompleteReviewRun(measured zero): %v", err)
	}
	cols = tokenColumns(measured.ID)
	if cols.InputTokens == nil || *cols.InputTokens != 0 ||
		cols.TotalTokens == nil || *cols.TotalTokens != 0 {
		t.Errorf("token columns = %+v, want measured zeros stored as 0", cols)
	}
	if cols.MaxContextTokens != nil {
		t.Errorf("max_context_tokens = %v, want NULL when no sample was reported", *cols.MaxContextTokens)
	}
}
```

- [ ] **Step 2: Run it and confirm the failure**

Run the Go gate with `-run '^TestCompleteReviewRunStoresNullTokensWithoutUsage$'` (same container command). Expected: compile error `unknown field TokensKnown` / zeros stored instead of NULL.

- [ ] **Step 3: Add the flag and NULL writes**

In `internal/store/review.go`, add to `RunCompletion`:

```go
	// TokensKnown reports whether an engine measured consumption. When false the
	// token columns are stored as NULL, not as a fake zero (#129).
	TokensKnown bool
```

In `CompleteReviewRun`, build nullable values before `updates`:

```go
		var input, output, reasoning, cacheRead, cacheWrite, total any
		if c.TokensKnown {
			input, output, reasoning = c.Tokens.Input, c.Tokens.Output, c.Tokens.Reasoning
			cacheRead, cacheWrite, total = c.Tokens.CacheRead, c.Tokens.CacheWrite, c.Tokens.Total
		}
		var maxContext any
		if c.TokensKnown && c.Tokens.MaxContext != nil {
			maxContext = *c.Tokens.MaxContext
		}
```

Replace the existing `var maxContext any` block with the one above, and assign the new variables to the seven `"*_tokens"` keys instead of `c.Tokens.*`.

Update the existing `TestCompleteReviewRun` completion (the one carrying `Tokens: store.RunUsage{Input: 123, ...}`) to include `TokensKnown: true`.

- [ ] **Step 4: Run the store tests and confirm they pass**

Run: Go gate with `-run 'TestCompleteReviewRun|TestCompleteReviewRunStoresNullTokensWithoutUsage'`. Expected: PASS.

- [ ] **Step 5: Write the failing pipeline assertions**

In `internal/review/run_test.go`, inside `TestRunInconclusiveWhenOCRMissing` (after `assertRunTerminal`), add:

```go
	if run.InputTokens != nil || run.OutputTokens != nil || run.ReasoningTokens != nil ||
		run.CacheReadTokens != nil || run.CacheWriteTokens != nil || run.TotalTokens != nil {
		t.Errorf("token columns = %v/%v/%v/%v/%v/%v, want NULL after an OCR failure",
			run.InputTokens, run.OutputTokens, run.ReasoningTokens,
			run.CacheReadTokens, run.CacheWriteTokens, run.TotalTokens)
	}
```

In `TestRunBudgetPartialPublishesFindings` (after the existing budget assertions), add:

```go
	if run.InputTokens == nil || *run.InputTokens != 700 ||
		run.OutputTokens == nil || *run.OutputTokens != 200 ||
		run.TotalTokens == nil || *run.TotalTokens != 900 {
		t.Errorf("token columns = %v/%v/%v, want the OCR summary 700/200/900",
			run.InputTokens, run.OutputTokens, run.TotalTokens)
	}
```

- [ ] **Step 6: Run them and confirm the failure**

Run: Go gate with `-run 'TestRunInconclusiveWhenOCRMissing|TestRunBudgetPartialPublishesFindings'`. Expected: FAIL — the timeout case stores zeros.

- [ ] **Step 7: Carry the flag through the runner**

In `internal/review/run.go`, add to `runOutcome`:

```go
	// tokensKnown is set only when an engine returned a parseable result, so a
	// cut run persists NULL instead of a fake zero (#129).
	tokensKnown bool
```

After `out.tokens = nativeTokenUsage(res)` in `execute`, set `out.tokensKnown = true`.

In the `store.RunCompletion` built by `Run`, add `TokensKnown: outcome.tokensKnown,`.

Update the `tokenTotals` doc comment: it no longer claims a run without calls writes accurate zeros; say the returned usage is only persisted when `tokensKnown` is true, and that unknown usage is stored as NULL.

- [ ] **Step 8: Run the review tests and confirm they pass**

Run: Go gate with `-run 'TestRunInconclusiveWhenOCRMissing|TestRunBudgetPartialPublishesFindings|TestRunPipelineSucceeds|TestRunNativeStoresNullMaxContext'`. Expected: PASS.

- [ ] **Step 9: Pin both aggregates with a cut run**

In `internal/store/qa_test.go`:

- `TestQASummaryAggregatesRunTokens`: after `zeroNative`, add

```go
	cut := f.openPR(repo.ID, 8, store.StatusReadyForHuman, false)
	cutRun := f.run(cut, store.RunFailed)
	f.exec(`UPDATE review_runs SET finished_at = $1, error_code = 'timeout'
		WHERE id = $2`, now.Add(-time.Hour), cutRun.ID)
```

Keep `want.Reviews` at 3; add the comment that a cut run has NULL columns and must be skipped.

- `TestProfileTokenUsageGroupsRunsAndChecks`: add a cut run for `alpha` with the same pattern (`f.run(pr, store.RunFailed)`, `error_code = 'timeout'`, no token columns) and keep `wantAlpha.Runs` at 2.

- [ ] **Step 10: Run the store tests**

Run: Go gate with `-run 'TestQASummary|TestProfileTokenUsage'`. Expected: PASS.

- [ ] **Step 11: Commit**

```sh
git add internal/store/review.go internal/store/review_test.go internal/store/qa_test.go internal/review/run.go internal/review/run_test.go
git commit -m "fix(review): store unknown token usage as NULL"
```

---

### Task 2: Explain a cut run in the PR detail

**Files:**
- Modify: `web/src/views/PrDetailView.vue` (no-tokens paragraph)
- Modify: `web/src/api.ts` (`total_tokens` doc comment)
- Modify: `web/src/views/PrDetailView.test.ts`

**Interfaces:**
- Consumes: `ReviewRun.total_tokens: number | null` and `ReviewRun.error_code: string` from Task 1's API shape (already present).

- [ ] **Step 1: Write the failing tests**

In `web/src/views/PrDetailView.test.ts`, in the existing `it('labels a run without token data instead of showing zeros')`, add `error_code: ''` to the overridden run (so it covers the legacy branch) and assert:

```ts
    expect(wrapper.get('[data-run-no-tokens]').text()).toContain('ejecución anterior al registro de consumo')
```

Then add:

```ts
  it('explains that a cut run never reported its token usage', async () => {
    mocks.api.prDetail.mockResolvedValue({
      ...detail,
      runs: [
        {
          ...detail.runs[0],
          status: 'failed',
          error_code: 'timeout',
          input_tokens: null,
          output_tokens: null,
          reasoning_tokens: null,
          cache_read_tokens: null,
          cache_write_tokens: null,
          total_tokens: null,
          max_context_tokens: null,
        },
      ],
    })

    const wrapper = mountDetail()
    await flushPromises()

    expect(wrapper.find('[data-run-tokens]').exists()).toBe(false)
    expect(wrapper.get('[data-run-no-tokens]').text()).toContain('se cortó antes de que OCR informara el consumo')

    wrapper.unmount()
  })
```

- [ ] **Step 2: Run the web test and confirm the failure**

Run: `npm --prefix web run test -- --run src/views/PrDetailView.test.ts`. Expected: FAIL on the new cut-run assertion.

- [ ] **Step 3: Implement the copy**

In `web/src/views/PrDetailView.vue`, replace the `v-else` paragraph with:

```vue
              <p v-else class="run__no-tokens" data-run-no-tokens>
                {{ run.error_code
                  ? 'Sin datos de tokens: la revisión se cortó antes de que OCR informara el consumo'
                  : 'Sin datos de tokens: ejecución anterior al registro de consumo' }}
              </p>
```

In `web/src/api.ts`, update the `total_tokens` comment to say it is also `null` when a run was cut before OCR reported usage (#129).

- [ ] **Step 4: Run the web tests and the typecheck**

Run: `npm --prefix web run test -- --run src/views/PrDetailView.test.ts` → PASS.
Run: `npm --prefix web run build` → PASS (vue-tsc + vite build).

- [ ] **Step 5: Commit**

```sh
git add web/src/views/PrDetailView.vue web/src/api.ts web/src/views/PrDetailView.test.ts
git commit -m "fix(panel): explain a cut run has no token data"
```

---

### Task 3: Publish the branch

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
git push -u origin fix/review-run-token-unknown
gh pr create --title "fix(review): store unknown token usage as NULL" --body-file - <<'EOF'
## What changed
...

## Why
Closes #129

## How it was tested
...

## Follow-up left out
...
EOF
```

The body must state that unknown OCR usage is persisted as NULL, that both aggregates skip it, the real test output, and that resuming OCR sessions is out of scope. End with the Claude Code attribution line required by the session.

- [ ] **Step 3: Report the PR URL to the user.**
