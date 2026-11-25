# Architecture baseline

`.architecture/baseline.json` declares the architecture decisions and forbidden
dependency directions the review engine checks. The file is resolved from the
trusted base commit of the pull request, never from its head.

The JSON schema:

```json
{
  "version": "1",
  "decisions": [
    { "id": "ADR-001", "path": "docs/adr/0001-layers.md", "version": "1" }
  ],
  "rules": [
    {
      "id": "ARCH-001",
      "version": "1",
      "from": "internal/store",
      "to": "internal/review",
      "reason": "why the direction is forbidden"
    }
  ],
  "exceptions": [
    {
      "id": "EX-001",
      "rule": "ARCH-001",
      "from": "internal/store/legacy",
      "to": "internal/review",
      "reason": "why the exception exists",
      "approved_by": "architecture-board"
    }
  ]
}
```

- `version` is required. A null, empty or ruleset-less document is reported as
  unknown, never as a pass.
- `decisions[].path` is read at the base commit and recorded with its `version`
  and blob SHA. A decision deleted, modified or replaced by the pull request
  makes the baseline unknown.
- `rules[].from` and `rules[].to` are repository-relative packages or full
  import paths. Only imports added by the change are checked, and a rename is
  compared with the file's old path and old package.
- `exceptions[]` apply only when the team policy is `baseline`, and only with a
  non-empty `approved_by` and `reason`.
- A pull request that changes this file is always evaluated against the base
  version, and its own change is recorded as evidence. If the change introduces
  a violation under the base rules, the violation wins.
- If `.architecture/baseline.json` is missing or unreadable at the base, the
  baseline is unknown. Missing evidence is not a pass.
