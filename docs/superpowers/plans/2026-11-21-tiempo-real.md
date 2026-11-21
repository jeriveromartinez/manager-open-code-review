# Live Updates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every authenticated view refreshes by itself when any user or process changes the data it shows.

**Architecture:** The Go writers that do not notify yet call `events.Publisher.Publish` with `{type, id}`. The existing broker serves those events over SSE. The Vue app opens one `EventSource` for the whole session. Each view subscribes to the event types it shows and refetches through the normal API, debounced, without overwriting a form someone is editing.

**Tech Stack:** Go 1.27, GORM, testcontainers (Docker), Vue 3 + Pinia + Vitest.

**Spec:** `docs/superpowers/specs/2026-11-21-tiempo-real-design.md`

## Global Constraints

- Events carry only `{type, id}`. Data always arrives through the existing filtered endpoints.
- New types: `team.updated`, `llm_profile.updated`, `settings.updated` (with `id` `""`), `user.updated`. Existing ones stay as they are: `pr.updated`, `repo.updated`, `run.updated`.
- A failed publish never reverts or fails a write that is already stored: `slog.Warn`. The only exception is `patchRepo`, which keeps its current behaviour.
- One SSE connection per tab. Debounce of 300 ms. After a reconnect, the views refetch.
- No new dependencies. Tests run locally. `internal/review` only runs on Linux: use the `golang:1.27` container with `TESTCONTAINERS_HOST_OVERRIDE=host.docker.internal` and `TESTCONTAINERS_RYUK_DISABLED=true`.

## Review Focus

- **A refetch arrives while the withdraw dialog is open.** The dialog and the typed reason stay. Test in Task 4.
- **The session expires while the SSE is connected.** The `EventSource` closes on logout and does not reopen without a session. Test in Task 3 (`stop()`).
- **A burst of 20 events.** One refetch per view. Test in Task 3.
- **Settings has unsaved changes in one group and `settings.updated` arrives.** That group keeps its values and shows the notice; the others refresh. Test in Task 5.
- **An event for a different PR while a PR detail is open.** No refetch. Test in Task 4.

---

### Task 1: Backend: publish from the API, auth and review (branch `feat/live-events`, its own PR)

**Files:**
- Modify:
  - `internal/api/findings.go` (`findingTransition`)
  - `internal/api/handlers.go` (`createTeam`, `setTeamMember`, `removeTeamMember`)
  - `internal/api/agents.go` (`createLLMProfile`, `patchLLMProfile`, `deleteLLMProfile`, `enqueueLLMHealth`, `putPolicy`)
  - `internal/api/settings.go` (`putSettings`)
- Create: `internal/api/notify.go` with `func (s *server) notify(ctx context.Context, ev events.Event)`. It does nothing when `s.Events == nil` and logs `slog.Warn` on error.
- Modify:
  - `internal/auth/auth.go`: field `Events events.Publisher`; `user.updated` after `UpsertUserFromGitHub`.
  - `internal/review/trigger.go`: `pr.updated` when `enqueue` creates a run, which covers webhooks and retries.
  - `internal/review/run.go`: `pr.updated` right after a successful `ClaimReviewRun`. `CheckProfile` passes `r.Events`.
  - `internal/review/health.go`: field `Events events.Publisher` on `ProfileChecker`; `llm_profile.updated` after every `SetLLMHealth`.
  - `cmd/qa/main.go`: wire `Events` into `auth.Auth` and into the worker's `review.ProfileChecker`.
- Tests:
  - `internal/api/*_test.go`, using `recordingPublisher`;
  - `internal/review/{trigger,run,health}_test.go`;
  - `internal/auth/auth_test.go`.

- [ ] **Step 1: write the failing tests.** Each test asserts that the recorder contains exactly the expected `events.Event{Type, ID}`:
  - `TestTriageNotifiesThePR`, both confirm and withdraw: `pr.updated` with the PR id, also when the status did not change.
  - `TestTeamWritesNotifyTheTeam`: create, add a member, remove a member, save the policy, each giving `team.updated` with the team id.
  - `TestLLMProfileWritesNotifyTheProfile`: create, patch, delete and health give `llm_profile.updated` with the profile id.
  - `TestSettingsSaveNotifies`: `settings.updated` with id `""`.
  - `TestLoginNotifiesTheUser`: `user.updated` with the user id.
  - `TestEnqueueNotifiesThePR`: a new run gives `pr.updated`; a redelivery that only repairs gives nothing.
  - `TestClaimNotifiesThePR`: `pr.updated` before the pipeline; an unclaimed run gives nothing.
  - `TestProfileCheckNotifies`: ok and error both give `llm_profile.updated`.
- [ ] **Step 2: run them and confirm they fail.**
- [ ] **Step 3: implement as listed in Files.**
- [ ] **Step 4: verify.** Run `go vet ./...` and `go test ./... -count=1` in the Linux container. Every package must be `ok`. Check gofmt with `tr -d '\r' < f | gofmt -l`.
- [ ] **Step 5: commit, push and open the PR.**

### Task 2: API client: event stream with every type

**Files:** Modify `web/src/api.ts`. Test `web/src/api.test.ts`.

**Interfaces:**
- Produces:
  - `export const LIVE_EVENT_TYPES = ['pr.updated','repo.updated','run.updated','team.updated','llm_profile.updated','settings.updated','user.updated'] as const`;
  - `export function openEventStream(onEvent: (e: ReviewEvent) => void, onState: (connected: boolean) => void): () => void`.
- `openEventStream` replaces `subscribeEvents`. It registers a listener for each type in `LIVE_EVENT_TYPES`.

- [ ] Test: with an `EventSource` stub, a `team.updated` message reaches `onEvent`, and `close()` removes the listeners. Then implement it and run `npx vitest run src/api.test.ts`.

### Task 3: Live store and `useLiveRefresh`

**Files:**
- Create:
  - `web/src/stores/live.ts`
  - `web/src/composables/useLiveRefresh.ts`
  - `web/src/stores/live.test.ts`
  - `web/src/composables/useLiveRefresh.test.ts`
- Modify:
  - `web/src/components/AppShell.vue`: calls `start()` once there is a session and `stop()` on unmount and logout. Shows the live dot (`data-service-badge`, classes `live`/`live--on`) before the brand.
  - `web/src/stores/prs.ts`: remove its own SSE handling (`start`, `stop`, `connected`, `scheduleRefresh`).

**Interfaces:**
- Produces:
  - `useLiveStore()` with `connected: Ref<boolean>`, `start(): void` (idempotent), `stop(): void` and `subscribe(types: string[], handler: (e: ReviewEvent) => void): () => void`;
  - the synthetic event `{ type: 'resync', id: '' }`, which goes to every subscriber when the connection comes back after an error;
  - `useLiveRefresh(types: string[], reload: () => unknown, filter?: (e: ReviewEvent) => boolean): void`, which debounces 300 ms, also fires on `resync`, and unsubscribes in `onBeforeUnmount`.

- [ ] Tests:
  - two `start()` calls open a single stream;
  - only matching types reach the subscriber;
  - error followed by open sends `resync`;
  - `stop()` closes the stream and sets `connected` to false;
  - 20 events within 300 ms give 1 `reload`;
  - an event rejected by the `filter` gives no reload;
  - unmounting cancels a pending reload.
- [ ] Move the live dot and SSE tests from `DashboardView.test.ts` to `AppShell.test.ts` and `live.test.ts`. Same assertions, now in the component that owns them.

### Task 4: Read views (queue, PR detail, lead, Repos, Teams)

**Files:** Modify `DashboardView.vue`, `PrDetailView.vue`, `LeadView.vue`, `ReposView.vue` and `TeamsView.vue`, plus their `*.test.ts`.

| View | Types | Reload |
|---|---|---|
| Queue | `pr.updated`, `repo.updated` | `prsStore.refresh()` |
| PR detail | `pr.updated` with `e.id === String(id)` | `load({ silent: true })` |
| Lead | `pr.updated`, `repo.updated`, `team.updated` | `loadTeam({ silent: true })` |
| Repos | `repo.updated`, `team.updated` | `load({ silent: true })` |
| Teams | `team.updated`, `user.updated` | `load({ silent: true })` |

`silent` means the reload does not set `loading` and does not hide what is already on screen.

- [ ] Per view, a test that a relevant event triggers a refetch and an unrelated one does not.
- [ ] PR detail: a test that the open dialog and its `reason` survive a refetch.
- [ ] Teams: a test that an add-member draft survives a refetch.

### Task 5: Form views (Agents, Settings)

**Files:** Modify `AgentsView.vue` and `SettingsView.vue`, plus their `*.test.ts`.

- **Agents:**
  - `llm_profile.updated` refreshes the profile list (`refreshProfiles`).
  - If the modal is editing the profile with that id, the modal shows "Otra persona cambió este perfil · Recargar" (`data-stale-profile`) and its fields are left alone.
  - `team.updated` for the selected team calls `loadPolicy()` if the policy has no unsaved changes. If it does, it shows "Otra persona cambió esta política · Recargar" (`data-stale-policy`).
- **Settings:**
  - `settings.updated` fetches the settings again.
  - Groups without changes take the new values and a new `savedSnapshot`.
  - Groups with changes keep theirs and show "Otra persona cambió estos ajustes · Recargar" (`data-stale-group`). "Recargar" discards that group's changes.

- [ ] Tests:
  - an event with the form clean updates the values;
  - an event with the form dirty keeps the values and shows the notice;
  - "Recargar" applies the server values.

### Task 6: Verification and PR (frontend)

- [ ] Run `cd web && npx vitest run` (all green) and `npm run build`.
- [ ] Check it in a browser against the local fixture server: the queue refreshes when an event is emitted.
- [ ] Commit the spec, the plan and the code, then open the PR. Mention that it depends on the backend PR for the new types.
