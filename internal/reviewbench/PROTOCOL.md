# Reviewer benchmark protocol and pre-pilot thresholds (#229)

This document defines what must be agreed **before** any output is imported
into a versioned sample, and how the blind measurement is turned into a
decision. The values below are the proposed defaults. The team must copy the
agreed values into `manifest.thresholds` and sign the manifest lock before the
first observation is imported; changing them afterwards changes the lock and
invalidates the imported observations.

The thresholds in `testdata/v1/manifest.json` are deliberately small
demonstration values that exercise pass and fail paths. They are **not** the
pilot agreement.

## Agreed minimums (proposal)

| Field | Value | Meaning |
| --- | --- | --- |
| `min_evaluated_cases` | 100 | Completed blind reviews of the target reviewer. Class-level claims need at least 20 completed blind cases per language/change type. |
| `min_coverage` | 0.95 | Completed reviews / all blind observations. Incomplete coverage, abstention, timeout and delivery error count in the denominator. |
| `min_precision` | 0.85 | Unique observed findings that match ground truth / all unique observed findings. |
| `min_severe_detection` | 0.90 | `major`/`blocker` ground-truth findings detected / those findings. |
| `min_citation_accuracy` | 0.95 | Accurate head citations / citations checked. |
| `max_false_block_rate` | 0.05 | Blocking findings that are not a real `major`/`blocker` defect / blocking findings. |
| `max_duplicate_rate` | 0.05 | Duplicate comments / raw comments. |

These are absolute gates for the target reviewer. "Improvement over another
reviewer" is a separate paired claim and cannot substitute for them.

## Paired improvement rule

For each binary case outcome ("the case produced a false positive" and "all
`major`/`blocker` truth was detected"), the report computes the discordant
counts, the exact McNemar p-value and the paired difference with its exact
interval (see `stats.go` and the README).

A claim that reviewer A is better than reviewer B requires **all** of:

1. The paired difference points in the expected direction.
2. The 95% paired-difference interval excludes zero.
3. The exact McNemar p-value is below 0.05.
4. The claim is made on the pre-declared blind subset, not on a subset chosen
   after seeing the result.

Per-language and per-change-type comparisons are descriptive until each class
has at least 20 paired blind cases. No multiple-comparison adjustment is
applied; do not promote a class on a borderline per-class p-value alone.

## Sample and blinding rules

- The manifest fixes every base/head comparison, allowed context, push
  sequence, category, severity and ground truth **before** importing outputs.
- At least one case is blind. The blind subset is the evaluation set for the
  thresholds; the full sample can be inspected for diagnostics.
- New findings are adjudicated blind: labels and tokens only, names sealed.
  Every decision needs written evidence (`confirmed`, `rejected` or
  `duplicate`). Names are revealed only after all candidates are decided.
- An observation may only claim the fixed comparison. An earlier push is
  imported as `incomplete_coverage`/superseded, never as the final head.
- The manifest lock is recorded in every observation. A changed ground truth
  invalidates the observation; do not reuse it in a report against the new
  lock.
- Real observations require public repositories or data authorized for that
  evaluator. No secret, token or private review body belongs in the sample.

## Operational accounting

The sample reports, per reviewer and overall:

- `completed`, `incomplete_coverage`, `abstention`, `timeout`,
  `delivery_error`, `superseded_push`, and `coverage`.

These are outcomes of the sample, not missing data. A reviewer that abstains or
times out more often cannot win on precision alone; detection and coverage are
read together. Only completed observations enter precision, detection, citation
and blocking metrics, because partial output is not comparable with a fixed
head.

Latency p50/p95 is nearest-rank. It is reported for completed reviews and for
all observations (timeouts included). Cost per completed review is reported
only when every completed review has a cost; otherwise it is not comparable and
must not be compared across reviewers.

## Gate for starting the pilot

The benchmark is ready to support a pilot when:

1. The manifest is signed with agreed thresholds and the blind subset is
   reserved.
2. The target reviewer meets every minimum above on the blind subset.
3. No candidate has a false block that touches a `major`/`blocker` defect, and
   no duplicate remains unadjudicated.
4. Every novel finding is adjudicated with evidence; the reveal contains no
   pending token.
5. The paired improvement rule (if an improvement claim is made) is satisfied.
6. The sample size and uncertainty are recorded in #198 with the manifest
   version and lock.

Meeting these conditions authorizes an **observation-only or authorized
`COMMENT` pilot** with audit and rollback. It does not authorize changing the
check conclusion or the QA gate; that is a later decision under #198 and
requires the evidence collected by the pilot. With no data, there is no
promotion.

## Known limits

- Wilson and exact McNemar intervals are per metric; they are not adjusted for
  the number of classes examined.
- Zero observed failures do not prove a zero population rate. Report the
  interval, not just the count.
- Simulated fixtures validate the harness, not a reviewer. The 300-PR real
  sample, real provider latency/cost and the actual promotion remain pending.
