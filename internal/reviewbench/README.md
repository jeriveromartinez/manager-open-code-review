# reviewbench — blinded, paired reviewer benchmark (#229)

Local, deterministic harness for comparing reviewers on the same fixed
comparisons. It extends the corpus of #140 (clean and defective pull requests
with ground truth) with external observations, blind adjudication, paired
statistics and a pre-pilot protocol. It does not call a provider, GitHub, OCR or
the network, and it does not modify production review code.

The package is standard library only and runs on every platform with the rest
of the Go tests. The thin CLI in `cmd/reviewbench` is optional; the core is
usable without it.

## Files

```text
internal/reviewbench/
  manifest.go     fixed sample, ground truth and SHA-256 lock
  observation.go  import schema, validation, normalization and dataset
  match.go        path/title/citation normalization and finding matching
  severity.go     panel severity vocabulary (blocker/major/minor/nit)
  seal.go         blind reviewer labels, novel-finding grouping, sealed mapping
  adjudicate.go   evidence-backed adjudication of novel findings
  stats.go        Wilson, Clopper-Pearson, exact McNemar, nearest-rank p50/p95
  report.go       metrics, per-class reports, paired comparisons, thresholds
  PROTOCOL.md     pre-pilot thresholds and decision rules
  testdata/v1/    versioned, explicitly simulated sample and observations
  cmd/reviewbench/stdlib-flag CLI
```

## Manifest

`testdata/v1/manifest.json` is an example of the format. A manifest fixes,
before any output is imported:

- `version`: sample version (for example `2026-11-27.1`).
- `context_radius`: line distance that still counts as the same location
  (3, matching `internal/review`).
- `target_reviewer`: optional reviewer ID whose thresholds are evaluated.
- `thresholds`: the agreed protocol values; `configured` must be true for the
  report to evaluate them.
- `cases[]`: one fixed comparison per pull request:
  - `id`, `language` (`go`, `typescript`, `python`, `dockerfile`, `yaml`,
    `sql`, `graphql`) and `change_type` (`code`, `test`, `config`, `infra`,
    `cross_module`, `data`).
  - `comparison.base` and `comparison.head` (40-character lowercase hex).
  - `pushes[]`: ordered heads the pull request received. The last one must be
    `comparison.head`. An observation of an earlier push is imported as an
    incomplete sample result, never as the final comparison.
  - `allowed_context[]`: the only files the reviewer is allowed to review or
    comment on. Each must have content in `head_files`.
  - `head_files{}`: head content used to validate citations and expected
    findings.
  - `expected[]`: ground truth (`path`, `category`, canonical `severity`,
    `start_line`, `end_line`, optional `cited_code`). The cited code must
    appear in the cited head range.
  - `blind`: reserving the case for blind evaluation. At least one case must be
    blind.

`lock` is the SHA-256 of the canonical JSON with `lock` emptied. Changing any
case, comparison, allowed context, push, head content, expected finding, blind
flag or threshold changes the lock. Every observation records the lock it was
imported against, so a ground-truth change invalidates old observations instead
of silently reusing them.

`go run ./cmd/reviewbench -manifest internal/reviewbench/testdata/v1/manifest.json -print-lock`
prints the current lock. `Finalize()` does the same for a programmatically
built manifest.

## Observation schema

Observations are a strict JSON array. Unknown fields are rejected. One document
can represent the project's own observation or an external export (for example
CodeRabbit); no translation of the comparison is allowed.

```json
{
  "schema_version": "reviewbench.observation/v1",
  "case_id": "go-defect-auth",
  "manifest_lock": "<manifest lock>",
  "simulated": false,
  "reviewer": {
    "id": "coderabbit",
    "name": "CodeRabbit",
    "kind": "external",
    "version": "0.9.0",
    "config": {"mode": "chill"}
  },
  "date": "2026-11-26T11:01:00Z",
  "comparison": {"base": "<case base>", "head": "<reviewed push head>"},
  "reviewed_head": "<reviewed push head>",
  "result": "completed",
  "latency_ms": 2200,
  "cost_usd": 0.04,
  "coverage": {"reviewed": ["auth.go"]},
  "findings": [
    {
      "comment_id": "cr-auth-1",
      "path": "auth.go",
      "start_line": 7,
      "end_line": 7,
      "side": "RIGHT",
      "severity": "major",
      "category": "security",
      "title": "Case-insensitive password comparison",
      "existing_code": "return strings.ToLower(stored) == strings.ToLower(provided)",
      "blocking": true
    }
  ]
}
```

Rules enforced by the importer:

- `schema_version`, `case_id`, `manifest_lock`, `reviewer.id/name/kind/version`,
  `date` (RFC3339), `comparison`, `reviewed_head` and `result` are required.
- `comparison.base` must equal the fixed case base; `comparison.head` and
  `reviewed_head` must agree and be one of the declared pushes. Any other head
  is `ErrComparisonMismatch`.
- Results are `completed`, `incomplete_coverage`, `abstention`, `timeout` and
  `delivery_error`. A claimed `completed` run whose coverage does not include
  every allowed-context file is downgraded to `incomplete_coverage`; an earlier
  push is downgraded the same way and marked `superseded`.
- `coverage.reviewed` must be a subset of `allowed_context`.
- Severities accept the same aliases as `internal/review` (`critical`→blocker,
  `high`→major, `medium`→minor, `low`→nit). Anything else is
  `ErrUnknownSeverity`; there is no silent default to major.
- A finding needs a path, a valid range, a category and a title. Comment IDs
  are optional and only aid provenance.
- Reviewer identity (name, kind, version, config) cannot change between that
  reviewer's observations.
- There is at most one observation per reviewer and case.

`blocking` marks a finding that the reviewer treats as a block. A `blocker`
severity also counts as blocking even if the field is absent.

### Duplicate comments

Findings are sorted deterministically and deduplicated within one observation:
same normalized path, category and title, nearby ranges (within
`context_radius`), and compatible quoted code. The survivor is the earliest
range; removed comments are counted as `duplicate_comments`. Precision uses
unique findings; the duplicate rate uses raw comments (`unique + duplicates`).

### Citation validation

A citation is accurate when its path is in `allowed_context`, the cited side is
the head (`RIGHT`), the range exists in `head_files`, and any `existing_code`
appears (whitespace-normalized) inside the cited range. A base-side citation, a
missing file, an out-of-context path or a wrong quote is inaccurate. Expected
ground-truth citations are checked the same way when the manifest is loaded.

### Matching

A ground-truth finding matches an observed finding with the same normalized
path and category and overlapping or nearby ranges. Severity does not decide
the match; severity mismatches are counted separately so understating a
`major` as `minor` stays visible while the defect still counts as detected.

## Blind adjudication

`Seal` replaces reviewer identity with labels assigned by the SHA-256 of the
reviewer ID and manifest lock, so the report contains labels and tokens only.
This is procedural blinding: someone who knows every reviewer ID and the lock
could recompute a label, so keep `SealedMapping` away from the adjudicator and
reveal only after all candidates are decided.

New findings (those matching no ground truth) are grouped across reviewers. A
token is derived from the lock, case and finding identity and contains no
reviewer name. `SealedMapping` is the separate, deliberately non-serializable
mapping from labels and tokens to reviewer identity (`json.Marshal` returns
`{}`).

Adjudication decisions are `confirmed`, `rejected` or `duplicate`, and every
decision requires non-empty evidence. Unknown tokens, duplicate decisions,
unknown verdicts and missing evidence are errors. `Reveal` refuses while any
candidate is pending; after complete adjudication it returns the names from the
sealed mapping.

## Metrics

Per reviewer, and per reviewer within the blind subset:

| Metric | Numerator / denominator |
| --- | --- |
| `precision` | matched observed findings / unique observed findings (evaluated observations) |
| `detection` | ground-truth findings matched / ground-truth findings |
| `severe_detection` | ground-truth `major`/`blocker` findings matched / those findings |
| `citation_accuracy` | accurate citations / findings checked |
| `false_block_rate` | blocking findings that do not match a `major`/`blocker` ground truth / blocking findings |
| `duplicate_rate` | duplicate comments / raw comments |
| `coverage` | completed observations / observations |
| `severity_mismatches` | matched findings whose observed canonical severity differs from ground truth |

Observations with `incomplete_coverage`, `abstention`, `timeout` or
`delivery_error` are always counted in `sample` and `operational`. Only
`completed` observations enter precision, detection, citation and blocking
metrics; partial output is not compared against a fixed head. `superseded_push`
counts observations of earlier pushes separately from ordinary incomplete
coverage.

Per-class reports (`language` and `change_type`) repeat every denominator and
Wilson interval so a class claim is never made from an aggregate alone.

Latency reports nearest-rank p50/p95 over completed observations and over all
observations with a positive latency (timeouts included, so they stay visible).
Cost per completed review is reported only when every completed observation has
a cost; otherwise it is `null` and `comparable` is false.

### Statistical methods

- **Proportions:** Wilson score 95% interval, which stays inside [0,1] for
  small denominators. Empty denominators have no estimate and no interval.
- **Paired binary outcomes:** exact McNemar, two-sided: `2*P(X<=min(b,c))` with
  `X~Binomial(b+c,1/2)`, capped at 1. The paired difference is `(b-c)/n` where
  `b` is Reviewer A only and `c` Reviewer B only. Its interval is derived from
  the exact Clopper-Pearson interval of the discordant proportion
  `b/(b+c)`, scaled by `(b+c)/n` through `2p-1`. This avoids a Wald interval
  with very few discordant pairs. Pairs are computed overall and per
  language/change type; the outcomes are "the case produced a false positive"
  and "all `major`/`blocker` ground truth was detected".
- **Latency:** nearest-rank percentiles, no interpolation.

## CLI

```sh
go run ./cmd/reviewbench \
  -manifest internal/reviewbench/testdata/v1/manifest.json \
  -observations internal/reviewbench/testdata/v1/observations.json \
  -out /tmp/report.json

# optional, only after adjudication is complete
go run ./cmd/reviewbench \
  -manifest internal/reviewbench/testdata/v1/manifest.json \
  -observations internal/reviewbench/testdata/v1/observations.json \
  -adjudications /tmp/decisions.json \
  -out /tmp/report.json \
  -reveal-out /tmp/revealed.json
```

`-adjudications` reads a JSON array of
`{"token","verdict","evidence","notes"}`. Without it the report lists pending
tokens. `-reveal-out` fails while anything is pending.

## Tests

```sh
go test ./internal/reviewbench/ -count=1
```

The versioned fixture is simulated and is labelled as such; see
`testdata/v1/README.md`. It covers two reviewers, clean, defective and
multi-push cases, a false block, a duplicate comment, a bad citation, an
incomplete run and a timeout, and the tests assert exact metric counts,
p50/p95, paired differences, lock invalidation, comparison rejection, blinding
and threshold evaluation.

## What this does not measure yet

- Real external observations: the fixture is simulated and no CodeRabbit export
  is included. The schema is the adapter contract, not a live client.
- The real 300-PR sample and real provider latency, cost, retries and delivery.
- Architectural findings or production publication; this package does not hook
  into `internal/review`, `internal/publish` or `internal/store`.
- Multiple-comparison adjustment for per-class paired tests; class results are
  descriptive until the agreed sample is reached.
- Promotion under #198: the protocol only defines the evidence required to
  start an authorized pilot.
