# Finding identity (#144)

## Problem

`findings.ID` was `"f_" + fingerprint[:16]` and the fingerprint hashed only
path, category, title and the ±3 lines around the range. Two pull requests that
reported the same text produced the same ID, so the second insert hit the
primary key; and two occurrences of the same title in one file collapsed into a
single finding even when they cited different code. The fingerprint also
included line numbers indirectly through the context window, so a push that
moved a finding changed its identity.

## Identity rule

The version-2 fingerprint is SHA-256 over, each field NUL-separated:

1. pull request id;
2. normalized path (`./` stripped, trimmed);
3. category;
4. normalized title (lowercase, whitespace collapsed);
5. content anchor;
6. occurrence ordinal.

The anchor is the normalized `ExistingCode` when OCR cited code, otherwise the
normalized ±3 source lines around the range. Normalization collapses whitespace,
so moving a finding without changing its content keeps the anchor. Paths are
normalized with `filepath.ToSlash` (plus an explicit separator replacement so a
backslash path is one identity on Linux too) and `path.Clean`.

Line numbers never enter the hash. They order occurrences that share every other
field, and when rows from a previous run exist each occurrence keeps the ordinal
recovered from its stored fingerprint; a survivor that moved beyond the context
radius falls back to the nearest previous row (preferring the same body).
Removing one of two identical occurrences therefore does not move the survivor to
the removed ordinal: the survivor keeps its ID, and the removed occurrence is
resolved. New occurrences take an ordinal above every previous row, so they
cannot reuse a removed occurrence's identity.

The stored ID is `"f_" + hex(sha256("<prID>|<fingerprint>"))[:16]`, and
`(pull_request_id, fingerprint)` stays the upsert/dedupe key.

`ocr_review` merges nearby duplicates by path, category, title and range
proximity; when both findings cite code, the cited code must also match.
Repeats of the same occurrence (same range) keep the last one, while distinct
occurrences survive to receive different ordinals.

## Migration and dual-read

Migration `00015_finding_identity.sql` adds `findings.identity_version
smallint NOT NULL`, leaves existing rows at `1` and sets the default to `2`.
Existing IDs are not rewritten.

`Reconcile` matches a stored row by the new fingerprint first. If none matches,
it looks for a version-1 row by its old path/category/title fingerprint; when it
finds one it keeps the row's ID and references, emits the new fingerprint and
version `2`, and the store updates that row in place instead of inserting. A
matched legacy row is marked as seen, so its old fingerprint does not make the
absence resolution mark it resolved. Resolution still only runs on a complete
run and only for open findings.

## Left out of scope

`trigger.go`, `convert.go`, `webhook.go`, `publish/`, `syncer/` and the
review-run comparison fields are untouched; #141 owns the comparison changes.
No comment, thread, verification or audit rows are re-keyed, because the finding
ID is preserved through the in-place migration.

## Risks

- `ExistingCode` wins over the source lines whenever OCR cites code. The same
  snippet repeated in several places therefore shares a base identity and is
  separated only by the ordinal and line order; a reordering can swap their IDs.
- The occurrence ordinal depends on `(StartLine, EndLine, Body)`. If a push
  reorders two occurrences that share every other identity field, their IDs can
  swap. They still describe the same two occurrences.
- A legacy row whose new content no longer matches its stored path, category or
  title keeps its ID only for the run that migrates it; later runs match it by
  the new fingerprint, so the first post-migration review must see it.
- If another row of the same PR already owns the target fingerprint, the legacy
  row keeps its ID but stays on the version-1 scheme instead of failing the run.
- Sixteen hex characters of a second hash are the stored ID; a collision across
  PRs would surface as a primary key error rather than silent data loss.
