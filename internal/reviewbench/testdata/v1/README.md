# reviewbench testdata v1 — simulated only

Everything in this directory is **simulated**. It was written by hand to
validate the harness and its metrics; it is not a real CodeRabbit export, not a
real review of a public repository and not production data. The reviewer name
`CodeRabbit` and its version are illustrative. Every observation carries
`"simulated": true` to keep the provenance explicit.

The sample exists to make the computation repeatable and to exercise failure
paths. It is deliberately small; do not use its numbers as a measurement of a
reviewer.

## What the sample covers

| Case | Language / change | Scenario |
| --- | --- | --- |
| `go-clean` | go / code | clean pull request, both reviewers quiet |
| `go-defect-auth` | go / code | two ground-truth defects (major + minor), a duplicate comment, an external false positive |
| `ts-multi-push` | typescript / cross_module | two pushes; both observations reviewed the earlier head |
| `py-bad-citation` | python / code | the target reviewer cites code that is not in the head and understates severity |
| `docker-clean-block` | dockerfile / infra | clean pull request with a false blocking comment |
| `yaml-incomplete` | yaml / config | claimed completed run missing one allowed-context file |
| `sql-timeout` | sql / data | target reviewer timed out |
| `go-abstention` | go / code | target reviewer abstained |
| `go-delivery-error` | go / test | target reviewer delivery failed |

Two reviewers (`own`, simulated project reviewer; `coderabbit`, simulated
external reviewer) cover completed, incomplete, superseded, abstention,
timeout and delivery-error results, a false block, a duplicate, a bad citation,
p50/p95 latency and a comparable cost for one reviewer only.

## Reproducing the report

```sh
go test ./internal/reviewbench/ -count=1
go run ./cmd/reviewbench \
  -manifest internal/reviewbench/testdata/v1/manifest.json \
  -observations internal/reviewbench/testdata/v1/observations.json
```

The manifest lock covers all ground truth. If you edit any case, expected
finding, comparison, allowed context, push or threshold, recompute the lock:

```sh
go run ./cmd/reviewbench \
  -manifest internal/reviewbench/testdata/v1/manifest.json -print-lock
```

Then update `lock` in `manifest.json` **and** `manifest_lock` in every
observation. This is intentional: observations imported against an older
ground truth must not be evaluated against the new one. For real work, bump
`version` when the sample changes.

The current lock is:

```text
87ce45dec5301fba4d5d209c4c151609cf85313c75eebdd3da54a5b9aaee80eb
```
