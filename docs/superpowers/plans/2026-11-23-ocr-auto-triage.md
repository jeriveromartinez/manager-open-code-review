# OCR Auto-Triage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let OCR verify its own findings inside each review run, auto-confirm the real ones, auto-withdraw false positives, and publish a curated PR comment so a human only sees the uncertain cases.

**Architecture:** A second, focused `ocr scan` runs over the files with findings while the clone is still alive. It returns one machine-readable verdict per finding with quoted evidence. The manager validates the evidence against the head file, applies a conservative policy, records the triage with an audit trail, and publishes the validated set in the existing summary comment. Everything is opt-in and defaults to `off`.

**Tech Stack:** Go 1.27, PostgreSQL 17, GORM, OCR 1.14.1 CLI, Vue 3 panel.

**Spec:** `docs/superpowers/specs/2026-11-23-ocr-auto-triage-design.md`

## Global Constraints

- Go module is `github.com/jeriveromartinez/qa-control-center`; no new dependencies.
- Tests run locally only. Never add CI workflow jobs.
- OCR stays pinned at 1.14.1 for this plan; do not bump it.
- Auto-triage is opt-in per team: `off` by default, `dry_run` first, `apply` only after review.
- Never auto-withdraw `major` or `blocker`; auto-withdraw only `minor`/`nit`.
- Auto-triage must be a no-op when disabled or when the verifier fails; findings stay `open` and untriaged.
- A human triage always wins over a verifier triage, including during a race.
- Commits: Conventional Commits, English, subject under 72 characters. One logical change per commit.
- User-facing panel copy is Spanish, matching the existing panel.
- The worker never receives `QA_MASTER_KEY`; the verifier uses the same worker key/profile path as the review.
- Treat finding bodies as untrusted model output: fence them and apply the existing sanitizer/redactor.
- Verification evidence must exist literally in the head file; otherwise the verdict is `uncertain`.

## Review Focus

These are the failure modes most likely to bite a user of this feature. Each has a test in the task that owns the code.

1. A `REFUTE` whose quoted evidence is not in the head file (hallucination or prompt injection) → must become `uncertain`, never withdraw.
2. A human confirms or dismisses a finding while the verifier runs → the human decision must win and the verifier must skip it.
3. The PR head moves or the run is superseded during verification → no verdicts applied, nothing published.
4. A `REFUTE` on a `major`/`blocker` finding → the finding stays open and keeps blocking; the evidence is recorded for the human.
5. The verifier times out, exhausts tokens, or `ocr scan` returns an unexpected schema → findings stay untriaged and the published comment falls back to the current "all open findings" behavior.

---

### Task 1: Spike `ocr scan` output and the verdict contract

**Files:**
- Create: `internal/review/testdata/ocr-scan-complete.json` (sanitized fixture)
- Create: `docs/superpowers/notes/2026-11-23-ocr-scan-spike.md` (throwaway notes)

**Interfaces:**
- Produces: the exact JSON shape consumed by `nativeReview` for `ocr scan`, and confirmation of whether the model honours the `QA-VERIFY` prefix.
- Produces: the fixture used by Task 5 tests.
- Consumes: the rule-file schema is already pinned by `internal/review/testdata/ocr-rule.json` (`{"schema_version":"1","groups":[{"group_id","source","pattern","files","rule"}]}`); the spike confirms that `ocr scan` accepts the same shape, not what the shape is.

- [ ] **Step 1: Run the spike inside the worker**

Use a small temp repo with one file containing a real guard (for example, a `nil` check). Write `verify-rule.json` and `verify-context.md` with the contract below, then run inside the worker with the same provider/profile the worker already uses:

```bash
ocr scan --repo <tmp-repo> --path <file> --rule verify-rule.json \
  --background-file verify-context.md --format json \
  --provider <provider> --model <model> --effort medium
```

Do not print the API key; reuse the worker's existing configuration.

- [ ] **Step 2: Record the shape**

In the spike note, list the top-level keys and whether each comment has `path`, `content`, `category`, `severity`, `start_line`, `end_line`, and whether `session_id` is present. Record whether the first line is `QA-VERIFY <id> CONFIRM|REFUTE|UNCERTAIN` and whether `EVIDENCE:` appears. Also record which budget flags `ocr scan --help` lists: if `--max-tokens-budget` or `--timeout` are not accepted, drop them from `buildScanArgs` and say so in the note.

- [ ] **Step 3: Save the sanitized fixture**

Copy the JSON with secrets/URLs/provider identifiers removed to `internal/review/testdata/ocr-scan-complete.json`. If the model did not honour the prefix, save the raw shape anyway and note that the fallback in Task 4 is required.

- [ ] **Step 4: Commit**

```bash
git add internal/review/testdata/ocr-scan-complete.json docs/superpowers/notes/2026-11-23-ocr-scan-spike.md
git commit -m "test(review): pin ocr scan output shape for verification"
```

---

### Task 2: Schema and models

**Files:**
- Create: `internal/store/migrations/00014_ocr_auto_triage.sql`
- Modify: `internal/store/models.go` (Finding, ReviewRun, QAPolicy)
- Test: `internal/store/review_test.go`

**Interfaces:**
- Produces: `Finding.ExistingCode`, `Finding.Verification`, `Finding.TriageSource`.
- Produces: `ReviewRun.VerifySessionID`, `ReviewRun.VerifyUsage`.
- Produces: `QAPolicy.AutoTriageMode`, `AutoWithdraw`, `VerifyMaxFindings`, `VerifyMaxFiles`, `VerifyTokenShare`, `VerifyTimeShare`, `VerifyTokenMax`.

- [ ] **Step 1: Write the schema test**

Add to `internal/store/review_test.go`:

```go
func TestFindingVerificationFieldsRoundTrip(t *testing.T)
func TestReviewRunVerifyFieldsRoundTrip(t *testing.T)
func TestTriageSourceBackfillMarksExistingTriagesAsHuman(t *testing.T)
func TestQAPolicyAutoTriageFieldsRoundTrip(t *testing.T)
```

The backfill test seeds a finding with `triaged_by = 'qa-user'`, runs the same `UPDATE` statement the migration uses, and asserts `triage_source = 'human'`.

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/store/ -run 'Verification|VerifyFields|TriageSourceBackfill|AutoTriage' -count=1`
Expected: FAIL with unknown fields/columns.

- [ ] **Step 3: Write the migration**

Create `internal/store/migrations/00014_ocr_auto_triage.sql`:

```sql
-- +goose Up
ALTER TABLE findings ADD COLUMN existing_code text NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN verification jsonb;
ALTER TABLE findings ADD COLUMN triage_source text NOT NULL DEFAULT '';
UPDATE findings SET triage_source = 'human' WHERE triaged_by <> '' AND triage_source = '';
CREATE INDEX idx_findings_triage_source ON findings (triage_source);

ALTER TABLE review_runs ADD COLUMN verify_session_id text;
ALTER TABLE review_runs ADD COLUMN verify_usage jsonb;

ALTER TABLE qa_policies ADD COLUMN auto_triage_mode text NOT NULL DEFAULT 'off';
ALTER TABLE qa_policies ADD COLUMN auto_withdraw boolean NOT NULL DEFAULT false;
ALTER TABLE qa_policies ADD COLUMN verify_max_findings integer NOT NULL DEFAULT 50;
ALTER TABLE qa_policies ADD COLUMN verify_max_files integer NOT NULL DEFAULT 20;
ALTER TABLE qa_policies ADD COLUMN verify_token_share integer NOT NULL DEFAULT 20;
ALTER TABLE qa_policies ADD COLUMN verify_time_share integer NOT NULL DEFAULT 15;
-- Fixed cap used when the review has no token budget (review_token_budget = 0),
-- otherwise verification would never run for those teams.
ALTER TABLE qa_policies ADD COLUMN verify_token_max bigint NOT NULL DEFAULT 100000;

-- +goose Down
ALTER TABLE qa_policies DROP COLUMN verify_token_max,
  DROP COLUMN verify_time_share, DROP COLUMN verify_token_share,
  DROP COLUMN verify_max_files, DROP COLUMN verify_max_findings,
  DROP COLUMN auto_withdraw, DROP COLUMN auto_triage_mode;
ALTER TABLE review_runs DROP COLUMN verify_usage, DROP COLUMN verify_session_id;
DROP INDEX IF EXISTS idx_findings_triage_source;
ALTER TABLE findings DROP COLUMN triage_source, DROP COLUMN verification, DROP COLUMN existing_code;
```

- [ ] **Step 4: Add the Go fields**

In `internal/store/models.go`:

```go
// Finding
ExistingCode string         `gorm:"column:existing_code"`
Verification datatypes.JSON `gorm:"column:verification"`
TriageSource string         `gorm:"column:triage_source"`

// ReviewRun
VerifySessionID string         `gorm:"column:verify_session_id"`
VerifyUsage     datatypes.JSON `gorm:"column:verify_usage"`

// QAPolicy
AutoTriageMode   string `gorm:"column:auto_triage_mode;default:off"`
AutoWithdraw     bool   `gorm:"column:auto_withdraw"`
VerifyMaxFindings int   `gorm:"column:verify_max_findings"`
VerifyMaxFiles    int   `gorm:"column:verify_max_files"`
VerifyTokenShare  int   `gorm:"column:verify_token_share"`
VerifyTimeShare   int   `gorm:"column:verify_time_share"`
VerifyTokenMax    int64 `gorm:"column:verify_token_max"`
```

Add constants `AutoTriageOff = "off"`, `AutoTriageDryRun = "dry_run"`, `AutoTriageApply = "apply"`, and `ValidAutoTriageMode(string) bool`.

- [ ] **Step 5: Run the tests and the store suite**

Run: `go test ./internal/store/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrations/00014_ocr_auto_triage.sql internal/store/models.go internal/store/review_test.go
git commit -m "feat(store): add auto-triage schema and fields"
```

---

### Task 3: Persist the cited code

**Files:**
- Modify: `internal/review/output.go` (`AgentFinding`)
- Modify: `internal/review/ocr_review.go` (`nativeFindings`)
- Modify: `internal/review/findings.go` (`Reconcile`)
- Modify: `internal/store/review.go` (`upsertFinding` columns)
- Test: `internal/review/ocr_review_test.go`, `internal/review/findings_test.go`, `internal/store/review_test.go`

**Interfaces:**
- Consumes: `Finding.ExistingCode` from Task 2.
- Produces: `AgentFinding.ExistingCode string` and populated `store.Finding.ExistingCode`.

- [ ] **Step 1: Write the failing tests**

```go
func TestNativeFindingsKeepsExistingCode(t *testing.T)
func TestReconcileCarriesExistingCode(t *testing.T)
func TestReconcileCarriesTriageState(t *testing.T)
func TestUpsertFindingStoresExistingCode(t *testing.T)
```

Assert exact values from a `nativeComment{ExistingCode: "if err != nil {"}` and from a `store.Finding{ExistingCode: "guard()"}`. `TestReconcileCarriesTriageState` seeds an open finding with `TriageSource`, `TriageVerdict`, `TriagedBy`, `Verified`-style `Verification` JSON and asserts the upsert keeps them; the verifier policy reads them from the upsert.

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/review/ ./internal/store/ -run 'ExistingCode' -count=1`
Expected: FAIL with unknown field.

- [ ] **Step 3: Implement**

Add `ExistingCode string \`json:"existing_code"\`` to `AgentFinding`; set it in `nativeFindings` from `strings.TrimSpace(c.ExistingCode)`; copy it in `Reconcile`; add `"existing_code"` to `upsertFinding`'s `DoUpdates`.

In `Reconcile`, also copy from `prev`: `TriageVerdict`, `TriageSource`, `TriagedBy`, `TriagedAt` and `Verification`. They are not in `DoUpdates`, so this changes no column; it exists so the in-memory upserts the verifier's policy reads still know which findings a human already triaged and which have a stored verdict.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/review/ ./internal/store/ -run 'ExistingCode' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/review/output.go internal/review/ocr_review.go internal/review/findings.go internal/store/review.go internal/review/ocr_review_test.go internal/review/findings_test.go internal/store/review_test.go
git commit -m "feat(review): persist the code cited by a finding"
```

---

### Task 4: Verdict parser and policy

**Files:**
- Create: `internal/review/verify.go`
- Test: `internal/review/verify_test.go`

**Interfaces:**
- Produces:
```go
type Verdict string
const (
    VerdictConfirm   Verdict = "confirm"
    VerdictRefute    Verdict = "refute"
    VerdictUncertain Verdict = "uncertain"
)

type Verification struct {
    Verdict    Verdict
    Evidence   string
    HeadSHA    string
    Model      string
    SessionID  string
    VerifiedAt time.Time
}

type VerifyPolicy struct {
    Mode            string // store.AutoTriageOff/DryRun/Apply
    AutoWithdraw    bool
    MaxFindings     int
    MaxFiles        int
    TokenShare      int
    TimeShare       int
    TokenMax        int64
}

type Decision struct {
    FindingID    string
    Verdict      Verdict
    Verification Verification
    Apply        bool
    Status       string // store.FindingOpen or store.FindingWithdrawn
    Reason       string
}

func ParseVerdicts(comments []nativeComment) map[string]Verification
func EvidenceInFile(v Verification, content []byte) bool
func Decide(findings []store.Finding, verdicts map[string]Verification, files map[string][]byte, p VerifyPolicy) []Decision
```

- [ ] **Step 1: Write the failing tests**

```go
func TestParseVerdictsReadsConfirmRefuteUncertain(t *testing.T)
func TestParseVerdictsIgnoresMissingPrefix(t *testing.T)
func TestParseVerdictsKeepsFirstOfDuplicateIDs(t *testing.T)
func TestEvidenceInFileRejectsHallucinatedRefutation(t *testing.T)
func TestDecideMissingVerdictIsUncertain(t *testing.T)
func TestDecideRefuteWithoutEvidenceIsUncertain(t *testing.T)
func TestDecideNeverWithdrawsMajorOrBlocker(t *testing.T)
func TestDecideRefuteNeedsAutoWithdraw(t *testing.T)
func TestDecideConfirmAppliesForEverySeverity(t *testing.T)
func TestDecideSkipsHumanTriaged(t *testing.T)
func TestDecideDryRunNeverApplies(t *testing.T)
func TestDecideOffNeverApplies(t *testing.T)
```

The parser expects the first non-empty line to match:

```
QA-VERIFY <finding_id> <confirm|refute|uncertain>
EVIDENCE: <exact code line>
```

Comments without a valid id or verdict are ignored. `EvidenceInFile` checks the `EVIDENCE:` value as a literal substring of the head file (after trimming).

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/review/ -run 'ParseVerdicts|EvidenceInFile|Decide' -count=1`
Expected: FAIL because `ParseVerdicts` is undefined.

- [ ] **Step 3: Implement the parser and policy**

Parse with `strings.SplitN(line, " ", 3)`. `Decide` rules:
- non-`apply` mode → no decisions with `Apply`.
- missing/invalid verdict → `uncertain`, no apply.
- `refute` without evidence in the file → `uncertain`, no apply.
- `refute` with severity `major`/`blocker` → `uncertain`, no apply.
- `refute` with `AutoWithdraw == false` → `uncertain`, no apply.
- `confirm` → apply `status = open`, `triage_verdict = confirmed`.
- `refute` accepted → apply `status = withdrawn`, `triage_verdict = false_positive`.
- any finding with `TriageSource == "human"` → skip.

`ParseVerdicts` sets `Verification.Verdict` and `Evidence`; the caller fills
`HeadSHA`, `Model`, `SessionID` and `VerifiedAt` once for the whole run.
`Decision.Verification` always carries the verdict and its evidence, including
`uncertain` and non-applied decisions, so `dry_run` and the audit trail keep
what the verifier answered. `VerifyPolicy.TokenMax` is only used by the caller
to size the call; `Decide` ignores it.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/review/ -run 'ParseVerdicts|EvidenceInFile|Decide' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/review/verify.go internal/review/verify_test.go
git commit -m "feat(review): parse verification verdicts and apply policy"
```

---

### Task 5: Native verifier invocation

**Files:**
- Create: `internal/review/ocr_verify.go`
- Modify: `internal/review/ocr_review.go` only to reuse package-private helpers (`writeNativeConfig`, `writeNativeBackground`, `reviewEnv`, `isolateProcessGroup`)
- Test: `internal/review/ocr_verify_test.go`
- Testdata: `internal/review/testdata/ocr-scan-complete.json` from Task 1

**Interfaces:**
- Produces:
```go
type NativeVerifier struct {
    Bin         string
    RepoDir     string
    Home        string
    Profile     store.LLMProfile
    APIKey      string
    Paths       []string
    Background  string
    MaxTokens   int
    TokenBudget int64
    Timeout     time.Duration
    OnLog       func(line string)
}

func (v *NativeVerifier) Review(ctx context.Context) (nativeReview, []byte, error)
func buildScanArgs(in verifyInvocation) []string
func verifyRuleJSON() []byte
func buildVerifyContext(findings []store.Finding) string
```

- [ ] **Step 1: Write the failing tests**

```go
func TestBuildScanArgsCarriesPathRuleAndBudget(t *testing.T)
func TestBuildScanArgsOmitsZeroBudget(t *testing.T)
func TestBuildVerifyContextContainsEachFindingID(t *testing.T)
func TestBuildVerifyContextFencesUntrustedBodies(t *testing.T)
func TestVerifyRuleRequiresMachineReadablePrefix(t *testing.T)
func TestNativeVerifierParsesScanFixture(t *testing.T)
```

Assert `--path` is comma-joined, `--rule` and `--background` (inline text, not a file flag) are present, `--max-tokens-budget`/`--timeout` appear only when nonzero, and the context contains `QA-VERIFY` plus the finding IDs. `ocr scan` has no `--effort` and no `--background-file`; do not add either. The fixture parse asserts at least one `CONFIRM` and one `REFUTE`.

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/review/ -run 'BuildScanArgs|BuildVerifyContext|VerifyRule|NativeVerifierParses' -count=1`
Expected: FAIL because the functions are undefined.

- [ ] **Step 3: Implement**

`verifyRuleJSON` returns `schema_version` `"1"` and one group `{group_id: 1, source: "system", pattern: "**/*", rule: <contract>}`, the schema pinned by `internal/review/testdata/ocr-rule.json`. The contract text is exact:

```
You are verifying findings already reported. For each finding in the context,
decide CONFIRM, REFUTE, or UNCERTAIN and emit exactly one comment placed at its
lines. The first non-empty line MUST be:
QA-VERIFY <finding_id> <CONFIRM|REFUTE|UNCERTAIN>
The second line MUST be:
EVIDENCE: <one line copied verbatim from the file at the reported revision>
If no such line proves the verdict, answer UNCERTAIN. Do not report new
findings. Do not report style, naming, or test suggestions. Never REFUTE
without evidence that contradicts the claim.
```

`buildVerifyContext` fences each finding body with the existing `fence` helper and includes `id`, `severity`, `category`, `path`, `start_line`, `end_line`, `title`, `body`, and `suggestion`.

`NativeVerifier.Review` mirrors `NativeReviewer.Review`: temp home, key helper, config, rule file (0600), `exec.CommandContext`, process group, `WaitDelay = 2s`, parse the JSON with the same `parseNativeReview` path. The finding context travels as the inline `--background` value; nothing is written for it.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/review/ -run 'BuildScanArgs|BuildVerifyContext|VerifyRule|NativeVerifierParses' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/review/ocr_verify.go internal/review/ocr_verify_test.go internal/review/testdata/ocr-scan-complete.json
git commit -m "feat(review): add focused ocr scan verifier"
```

---

### Task 6: Wire the verifier into the run

**Files:**
- Modify: `internal/review/run.go` (`runOutcome`, `execute`, `RunCompletion` construction)
- Modify: `internal/store/models.go` (`RunPhaseVerifying`)
- Modify: `internal/store/review.go` (`RunCompletion` verify fields and persistence)
- Modify: `internal/review/testdata/fakeopencode-review.sh` (handle `scan`)
- Modify: `web/src/runPhase.ts` (`verifying` label)
- Test: `internal/review/run_test.go`, `web/src/runPhase.test.ts` (new)

**Interfaces:**
- Consumes: `NativeVerifier`, `ParseVerdicts`, `Decide`, `VerifyPolicy`.
- Produces: `runOutcome.decisions []Decision`, `runOutcome.verifyUsage []byte`, `runOutcome.verifySessionID string`, `runOutcome.verificationSkipped string`.
- Produces: `RunPhaseVerifying = "verifying"`.
- Produces: `store.RunCompletion.VerifyUsage []byte`, `VerifySessionID string`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunVerifiesAndProducesDecisions(t *testing.T)
func TestRunSkipsVerificationWhenModeOff(t *testing.T)
func TestRunVerifierFailureLeavesFindingsUntriaged(t *testing.T)
func TestRunVerifierTokensAreAddedToRunUsage(t *testing.T)
func TestRunSetsVerifyingPhase(t *testing.T)
func TestRunVerifierRespectsMaxFindings(t *testing.T)
func TestRunReusesStoredVerdictsForSameHead(t *testing.T)
```

Web (`runPhase.test.ts`):

```ts
it('labels the verifying phase', () => expect(phaseLabel('verifying')).toBe('Verificando con OCR'))
```

Use the fake OCR script: it emits the review fixture for `review` and the scan fixture for `scan`. Assert `out.decisions` has one apply-confirm and one apply-withdraw, the run usage total includes both, and the log contains the `verifying` phase. For the reuse test, seed a finding whose `verification` JSON carries `head_sha` equal to the run head, run with a fake script that fails on `scan`, and assert the stored verdict became a decision without the second call.

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/review/ -run 'RunVerif|VerifyingPhase|ReusesStoredVerdicts' -count=1`
Expected: FAIL because the verifier is not invoked.

- [ ] **Step 3: Implement the phase and the call**

In `execute`, in the `case store.RunSucceeded` branch after `Reconcile` and before `return out, nil`:

1. If `cfg.Policy.AutoTriageMode == store.AutoTriageOff` or `len(out.findings) == 0`, return unchanged.
2. Reuse stored verdicts: index `out.open` by `ID` (`Reconcile` preserves the ID), unmarshal each `f.Verification`, and match it to the upsert with the same ID. A finding whose stored `head_sha` equals `headSHA` and whose `existing_code` is unchanged keeps that verdict. Put those into `verdicts` and leave them out of the call set.
3. Group the remaining `out.findings` by `Path`, cap at `VerifyMaxFiles`/`VerifyMaxFindings`, prioritizing `minor` over `nit` and earlier lines. If nothing remains, go to step 8 with the reused verdicts.
4. Compute `timeout` from the run context and `verifyTimeShare`; compute `budget` as `ReviewTokenBudget * verifyTokenShare / 100`, falling back to `VerifyTokenMax` when `ReviewTokenBudget == 0`. If either is below the minimum (one minute / 10k tokens), set `out.verificationSkipped = "insufficient_budget"` and return unchanged.
5. `r.setPhase(runCtx, run.ID, store.RunPhaseVerifying, log)`.
6. Build `NativeVerifier` with the same profile/key/home; call it through the profile-slot path (add `runNativeVerify` next to `runNativeReview`; both share a `nativeCaller` interface `Review(context.Context) (nativeReview, []byte, error)`).
7. On error: log it and set `out.verificationSkipped = "verifier_error"`; if `len(verdicts) == 0` (nothing was reused), return unchanged.
8. When the call succeeded, merge `ParseVerdicts(res.Comments)` into `verdicts`. In both paths (call ran or only reused verdicts), fill `HeadSHA`, `Model`, `SessionID` and `VerifiedAt` on every verdict, read the cited files from `workDir` into `files map[string][]byte`, and set `out.decisions = Decide(out.findings, verdicts, files, verifyPolicy)`. Only when the call ran, also set `verifyUsage := nativeTokenUsage(res)`; `out.verifyUsage = verifyUsage.json()`; `out.verifySessionID = res.SessionID`; `out.tokens.add(verifyUsage)`; and `out.tokensKnown = out.tokensKnown || res.hasSummary`.
9. Re-run `r.setPhase(runCtx, run.ID, store.RunPhaseFinishing, log)` so the panel's last phase is accurate, and return.

In `Run`, copy `VerifyUsage` and `VerifySessionID` into `store.RunCompletion`; persist them in `CompleteReviewRun` (`verify_usage`, `verify_session_id`). Do not persist decisions here; Task 8 applies them.

- [ ] **Step 4: Update the fake OCR script**

The script must inspect `$1`: `review` → existing fixture; `scan` → `ocr-scan-complete.json`. Keep stderr progress lines so the live log test still passes.

- [ ] **Step 5: Label the phase in the panel**

Add `verifying: 'Verificando con OCR'` to `phaseLabels` in `web/src/runPhase.ts`; the new `runPhase.test.ts` asserts that label.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/review/ -run 'RunVerif|VerifyingPhase|ReusesStoredVerdicts' -count=1 && npm --prefix web test -- runPhase`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/review/run.go internal/store/models.go internal/store/review.go internal/review/testdata/fakeopencode-review.sh internal/review/run_test.go web/src/runPhase.ts web/src/runPhase.test.ts
git commit -m "feat(review): run OCR verification inside the review run"
```

---

### Task 7: Store guard for verifier triage and upsert preservation

**Files:**
- Modify: `internal/store/review.go` (`TriageFinding`, `upsertFinding`, `FindingAudit`)
- Test: `internal/store/review_test.go`

**Interfaces:**
- Produces: `FindingAudit.Source string`.
- Produces: `ErrFindingHumanTriaged`.
- Produces: `TriageFinding` writes `triage_source`.
- Produces: `upsertFinding` does not reopen a triaged finding whose `existing_code`, `start_line` and `end_line` are unchanged.

- [ ] **Step 1: Write the failing tests**

```go
func TestTriageFindingVerifierCannotOverrideHuman(t *testing.T)
func TestTriageFindingVerifierWritesSourceAndIsIdempotent(t *testing.T)
func TestUpsertFindingKeepsDismissedWhenCodeUnchanged(t *testing.T)
func TestUpsertFindingReopensDismissedWhenCodeChanged(t *testing.T)
func TestUpsertFindingClearsStaleVerifierTriage(t *testing.T)
func TestUpsertFindingKeepsHumanTriageOnCodeChange(t *testing.T)
func TestUpsertFindingStillUpdatesUntriaged(t *testing.T)
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/store/ -run 'VerifierCannotOverride|VerifierWritesSource|UpsertFinding' -count=1`
Expected: FAIL because the guard and the conflict rule do not exist.

- [ ] **Step 3: Implement the guard**

In `TriageFinding`, inside the transaction after loading `current`:

```go
if current.TriageSource == "human" && entry.Source == "verifier" {
    return ErrFindingHumanTriaged
}
```

Add `triage_source` to the update map and to the idempotency check (`current.TriageSource == entry.Source`). Add `Source` to `FindingAudit` and write it in `writeFindingAudit`'s data.

Define `var ErrFindingHumanTriaged = errors.New("finding already triaged by a human")`.

- [ ] **Step 4: Implement the upsert preservation**

Add `existing_code` to `DoUpdates` and a conflict `Where`:

```go
clause.OnConflict{
    Columns: []clause.Column{{Name: "pull_request_id"}, {Name: "fingerprint"}},
    DoUpdates: clause.AssignmentColumns([]string{
        "path", "start_line", "end_line", "severity", "category", "title",
        "body", "suggestion", "confidence", "existing_code", "status",
        "last_seen_run_id", "updated_at",
    }),
    Where: clause.Where{Exprs: []clause.Expression{clause.Expr{
        SQL: "findings.triage_verdict = '' OR findings.existing_code <> EXCLUDED.existing_code" +
            " OR findings.start_line <> EXCLUDED.start_line OR findings.end_line <> EXCLUDED.end_line",
    }}},
}
```

A skipped update leaves the dismissed row exactly as it was. When the cited
code or range changed, the row reopens; the stale verifier dismissal must go
with it, but a human verdict never does. Before the `Create`, still inside
`upsertFinding`, clear only the verifier-sourced triage of a row whose cited
code changed:

```go
if err := db.Model(&Finding{}).
    Where("pull_request_id = ? AND fingerprint = ? AND triage_source = ?",
        f.PullRequestID, f.Fingerprint, "verifier").
    Where("existing_code <> ? OR start_line <> ? OR end_line <> ?",
        f.ExistingCode, f.StartLine, f.EndLine).
    Updates(map[string]any{
        "triage_verdict": "", "triaged_by": "", "triaged_at": nil, "triage_source": "",
    }).Error; err != nil {
    return Finding{}, fmt.Errorf("clear stale verifier triage for %q: %w", f.ID, err)
}
```

Add `TestUpsertFindingClearsStaleVerifierTriage` (verifier dismissal + changed
code → fields empty, row open) and `TestUpsertFindingKeepsHumanTriageOnCodeChange`
(human dismissal + changed code → verdict and `triage_source=human` survive).

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/store/ -run 'VerifierCannotOverride|VerifierWritesSource|UpsertFinding' -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/review.go internal/store/review_test.go
git commit -m "feat(store): guard verifier triage and preserve dismissals"
```

---

### Task 8: Apply decisions after the findings exist

**Files:**
- Modify: `internal/review/run.go` (after `CompleteReviewRun`, before `SetPRStatus`)
- Modify: `internal/review/run.go` (announce/log)
- Modify: `internal/store/review.go` (`RecordFindingVerification`)
- Test: `internal/review/run_test.go`, `internal/store/review_test.go`

**Interfaces:**
- Consumes: `runOutcome.decisions` from Task 6, `TriageFinding` and `ErrFindingHumanTriaged` from Task 7.
- Produces: `Store.RecordFindingVerification(ctx context.Context, findingID string, verification []byte) error`, which writes `findings.verification`.
- Produces: applied `triage_verdict`, `status`, `triage_source = verifier`, and an audit event `finding.auto_triaged`.

- [ ] **Step 1: Write the failing tests**

```go
func TestRunAppliesConfirmAndWithdrawDecisions(t *testing.T)
func TestRunSkipsDecisionWhenHumanTriagedMeanwhile(t *testing.T)
func TestRunDoesNotApplyDecisionsWhenHeadMoved(t *testing.T)
func TestRunLogsAutoTriageCounts(t *testing.T)
func TestRunDryRunStoresVerificationWithoutChangingStatus(t *testing.T)
func TestRecordFindingVerificationStoresJSON(t *testing.T)
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/review/ ./internal/store/ -run 'AppliesConfirm|SkipsDecisionWhenHuman|DoesNotApplyDecisionsWhenHead|DryRunStoresVerification|RecordFindingVerification' -count=1`
Expected: FAIL because decisions are never applied and the store method does not exist.

- [ ] **Step 3: Implement the persistence**

Add to `internal/store/review.go`:

```go
func (s *Store) RecordFindingVerification(ctx context.Context, findingID string, verification []byte) error
```

It updates `findings.verification` only; a missing row returns `ErrNotFound`.

- [ ] **Step 4: Implement the application**

After `CompleteReviewRun` succeeds and before `SetPRStatus`:

1. Reload the PR with `r.Store.ReviewConfigForPR(persistCtx, run.PullRequestID)` and confirm `cfg.PR.HeadSHA` still equals `run.HeadSHA`; otherwise skip all decisions and log it.
2. Marshal `decision.Verification` and call `RecordFindingVerification` for **every** decision, applied or not, so `dry_run` and `uncertain` keep their evidence. A marshal or write error is logged and does not stop the loop.
3. For each `decision` with `Apply`, call:

```go
changed, err := r.Store.TriageFinding(persistCtx, decision.FindingID, decision.Status, store.FindingAudit{
    Action:  "finding.auto_triaged",
    Actor:   "ocr-verify:" + cfg.Profile.Model,
    Source:  "verifier",
    Reason:  decision.Reason,
})
```

4. On `ErrFindingHumanTriaged`, count as skipped and continue. On other errors, log and continue.
5. Log `auto-triage: X confirmados, Y descartados, Z omitidos` plus `verification_skipped=<reason>` when `out.verificationSkipped` is set, and publish `pr.updated`.

The existing `SetPRStatus` runs after this block. Auto-withdraw is only `minor`/`nit`, so the gate is unchanged; the ordering is still correct if a future policy changes it.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/review/ ./internal/store/ -run 'AppliesConfirm|SkipsDecisionWhenHuman|DoesNotApplyDecisionsWhenHead|LogsAutoTriage|DryRunStoresVerification|RecordFindingVerification' -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/review/run.go internal/store/review.go internal/review/run_test.go internal/store/review_test.go
git commit -m "feat(review): apply verifier triage with audit trail"
```

---

### Task 9: Curated comment and inline filtering

**Files:**
- Modify: `internal/publish/summary.go` (`buildSummary`, `findingsWithoutInlineComment`)
- Modify: `internal/publish/comments.go` (`publishInlineComments`)
- Test: `internal/publish/comment_test.go`, `internal/publish/publish_test.go`

**Interfaces:**
- Consumes: `store.Finding.Verification`, `TriageVerdict`, `TriageSource`; `store.ReviewRun.VerifySessionID`; `publishContext.policy.AutoTriageMode`.
- Produces: `func (p *publishContext) verificationApplied() bool` — true when `p.run.VerifySessionID != ""` and `p.policy.AutoTriageMode == store.AutoTriageApply`.
- Produces: summary sections `Validado (N)`, `Necesita tu criterio (K)`, `Descartado automáticamente (M)`; inline comments only for confirmed findings when verification was applied.

- [ ] **Step 1: Write the failing tests**

```go
func TestSummaryListsValidatedAndUncertain(t *testing.T)
func TestSummaryCountsAutoDismissed(t *testing.T)
func TestSummaryNotesSkippedVerification(t *testing.T)
func TestInlineOnlyForConfirmedWhenVerificationRan(t *testing.T)
func TestInlineFallsBackWhenVerificationDisabled(t *testing.T)
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/publish/ -run 'SummaryLists|SummaryCounts|SummaryNotes|InlineOnlyForConfirmed|InlineFallsBack' -count=1`
Expected: FAIL because the sections do not exist.

- [ ] **Step 3: Implement the summary**

In `buildSummary`, after the severity counts, add a section when `p.run.VerifySessionID != ""` or any finding has `TriageSource == "verifier"`:

```markdown
**Verificación automática:** N validados · K para tu criterio · M descartados
```

When `p.policy.AutoTriageMode == store.AutoTriageDryRun`, append
`_(simulación: no se cambió el estado de ningún hallazgo)_` so nobody reads a
dry-run verdict as an applied decision.

Then list validated findings (`TriageVerdict == confirmed` with `TriageSource == verifier`) as `- **severity** · path:line — title`, and uncertain findings (open, no verdict) under a collapsible `<details><summary>Necesita tu criterio (K)</summary>` block. Do not list auto-dismissed findings; add `M descartados automáticamente · [ver en el panel](…)`. When `p.policy.AutoTriageMode != store.AutoTriageOff`, `p.run.VerifySessionID == ""` and no finding has a verifier source, add `> La verificación automática no se ejecutó; estos hallazgos no están confirmados.` With auto-triage off, the summary stays exactly as today.

Use `p.sanitize` on every title/body and keep the existing summary marker.

- [ ] **Step 4: Implement the inline filter**

In both `findingsWithoutInlineComment` (`summary.go`) and `publishInlineComments` (`comments.go`), when `p.verificationApplied()` is true also require `f.TriageVerdict == store.VerdictConfirmed`. When false, keep today's behavior. Factor the per-finding predicate into one helper used by both so the summary and the inline review cannot drift.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/publish/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/publish/summary.go internal/publish/comments.go internal/publish/comment_test.go internal/publish/publish_test.go
git commit -m "feat(publish): publish the curated verified findings"
```

---

### Task 10: Policy settings, API and panel

**Files:**
- Modify: `internal/store/review.go` (`ReviewConfig` and `ReviewConfigForPR`)
- Modify: `internal/api/agents.go` (`qaPolicyResponse`, `qaPolicyDTO`, validation)
- Modify: `web/src/api.ts`
- Modify: `web/src/views/AgentsView.vue`
- Test: `internal/api/agents_test.go`, `web/src/views/AgentsView.test.ts`

**Interfaces:**
- Produces: policy fields in `qaPolicyResponse` and in `ReviewConfig` (`AutoTriageMode`, `AutoWithdraw`, `VerifyMaxFindings`, `VerifyMaxFiles`, `VerifyTokenShare`, `VerifyTimeShare`, `VerifyTokenMax`).
- Produces: defaults `off`, `false`, `50`, `20`, `20`, `15`, `100000`.

- [ ] **Step 1: Write the failing tests**

Go:

```go
func TestPolicyAcceptsAutoTriageFields(t *testing.T)
func TestPolicyRejectsInvalidAutoTriageMode(t *testing.T)
func TestPolicyRejectsVerifySharesOutsideRange(t *testing.T)
func TestPolicyRejectsVerifyTokenMaxOutOfRange(t *testing.T)
func TestPolicyDefaultsAutoTriageOff(t *testing.T)
```

Web (`AgentsView.test.ts`):

```ts
it('saves auto-triage mode and verify limits', ...)
it('renders auto-triage off by default', ...)
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/api/ -run 'AutoTriage|VerifyShares' -count=1 && npm --prefix web test -- AgentsView`
Expected: FAIL with unknown fields.

- [ ] **Step 3: Implement the API and config**

Add the fields to `qaPolicyResponse` with JSON names matching the DB columns. In `qaPolicyDTO`, default `AutoTriageMode` to `off` and the numeric fields to their defaults when zero. In the policy patch validation, accept only `off|dry_run|apply`, shares `0..100`, `VerifyTokenMax` between `10000` and `ReviewTokenBudgetMax`, and limits `> 0` (or `0` meaning default). In `ReviewConfigForPR`, copy the fields into `ReviewConfig`, applying the same defaults, including `VerifyTokenMax = 100000` when zero.

- [ ] **Step 4: Implement the panel**

In `AgentsView.vue`, add a "Verificación automática" group next to the review controls: a select for `auto_triage_mode` (`Apagada`, `Solo simular`, `Aplicar`), a checkbox for `auto_withdraw`, and number inputs for the limits, shares and `verify_token_max`. `ocr scan` has no effort flag, so there is no `verify_effort` control. Include them in the dirty check and in the save payload. Mirror the existing layout and Spanish copy.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/api/ -run 'AutoTriage|VerifyShares|VerifyTokenMax' -count=1 && npm --prefix web test -- AgentsView`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/review.go internal/api/agents.go web/src/api.ts web/src/views/AgentsView.vue internal/api/agents_test.go web/src/views/AgentsView.test.ts
git commit -m "feat(panel): configure OCR auto-triage per team"
```

---

### Task 11: Audit metrics, PR detail and end-to-end

**Files:**
- Modify: `internal/store/qa.go` (`QASummary`)
- Modify: `internal/api/prdetail.go` (finding DTO)
- Modify: `web/src/views/PrDetailView.vue` (or the existing finding component)
- Test: `internal/store/qa_test.go`, `internal/api/prdetail_test.go`, `internal/review/e2e_test.go`
- Modify: `docs/roadmap.md` (mark the follow-up)

**Interfaces:**
- Produces: `QASummary.AutoConfirmed`, `AutoDismissed`, `Uncertain` counts.
- Produces: finding DTO fields `verification`, `triage_source`.
- Produces: the end-to-end assertion that one false positive is withdrawn and one real finding is confirmed and published.

- [ ] **Step 1: Write the failing tests**

```go
func TestQASummaryCountsAutoTriage(t *testing.T)
func TestPRDetailShowsVerificationEvidence(t *testing.T)
func TestE2EAutoTriagePublishesCuratedSummary(t *testing.T)
```

The E2E test uses the fake OCR review + scan fixtures: one `REFUTE` with evidence present in the file and one `CONFIRM`. Assert the dismissed finding is `withdrawn` with `triage_source=verifier`, the confirmed one is `open` with `triage_verdict=confirmed`, the summary contains `Validado (1)` and `Descartado automáticamente (1)`, and only the confirmed finding received an inline comment.

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/store/ ./internal/api/ ./internal/review/ -run 'QASummaryCountsAutoTriage|PRDetailShowsVerification|E2EAutoTriage' -count=1`
Expected: FAIL because the counters/DTO/E2E are missing.

- [ ] **Step 3: Implement the counters and DTO**

In `QASummary`, count findings with `triage_source = 'verifier'` and `triage_verdict = 'confirmed'` / `'false_positive'`, and open findings with no verdict in the ready queue. In `prdetail.go`, add `verification` (unmarshalled JSON) and `triage_source` to the finding DTO. Render the verdict/evidence under each finding in the PR detail, with the existing Spanish labels.

- [ ] **Step 4: Implement the E2E fixture and assertions**

Extend `internal/review/e2e_test.go` with a table-driven test that uses the fake binaries. Keep the existing E2E fixtures untouched; add a new one for the scan.

- [ ] **Step 5: Run the full gate**

Run: `go test ./... -count=1`
Run: `go vet ./...`
Run: `npm --prefix web test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/qa.go internal/store/qa_test.go internal/api/prdetail.go internal/api/prdetail_test.go internal/review/e2e_test.go web/src/views/PrDetailView.vue docs/roadmap.md
git commit -m "feat(panel): show auto-triage outcomes and cover E2E"
```

---

## Rollout

1. Merge with `auto_triage_mode = off`; nothing changes for any team.
2. Enable `dry_run` for `mitra`. Findings get verdicts and evidence, but no status changes and no dismissals. Compare a week of verdicts with the manual triage.
3. Enable `apply` without `auto_withdraw`. Confirmations are recorded; nothing is dismissed.
4. Enable `auto_withdraw` for `minor`/`nit` only. Watch the human re-confirmations of auto-dismissed findings; if they appear, disable the flag and revisit the rule.
5. Revisit the budgets (`verify_token_share`, `verify_time_share`) with real consumption from `verify_usage`.

## Open Items After This Plan

- Persist OCR sessions to a stable volume to enable `ocr session compare` for a future consensus mode.
- Evaluate `ocr review --analysis` prose as a third signal for `major` findings.
- Bump OCR to 1.14.2 after the verification contract is stable.
