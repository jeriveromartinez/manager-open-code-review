# Multi-cuenta de GitHub (repositorios) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Que una sola GitHub App pública pueda instalarse en varias cuentas personales/organizaciones, y que sus repositorios aparezcan, se descubran y se gestionen desde la misma instancia.

**Architecture:** El modelo de `installations` ya existe. Se hace pública la App, se añade un cliente a nivel de App para descubrir instalaciones con `GET /app/installations` durante la reconciliación, se expone la cuenta de cada repo y se añade a la web la sección de cuentas conectadas con un botón “Añadir cuenta”.

**Tech Stack:** Go 1.27, chi v5, GORM/Postgres, google/go-github v93, ghinstallation/v2, River; Vue 3 + Vite + TS + Pinia; Vitest.

**Spec:** `docs/superpowers/specs/2026-11-19-multi-cuenta-github-design.md`

## Global Constraints

- Una sola GitHub App **pública** instalada en N cuentas/orgs; una sola credencial de App. No se implementan Apps privadas por cuenta.
- `BuildManifest` produce `public: true`. Los permisos/eventos/URLs no cambian.
- Los repositorios descubiertos se crean con `Enabled=false`; solo un admin los habilita.
- La poda de instalaciones solo ocurre tras una listación **completa y exitosa** de `GET /app/installations`; ante error de red o paginación no se borra nada.
- `SyncInstallation` es no-op si la instalación está suspendida; `installation.unsuspend` limpia `SuspendedAt` y encola una sync inmediata.
- “Cuentas conectadas” y “Añadir cuenta” son solo para admin. La columna “Cuenta” se muestra a quien ya ve ese repo, derivada de los repos permitidos.
- No se toca el motor de revisión, las políticas QA ni GitHub Teams.
- Go 1.27, Vue 3 + Vite + TS, PostgreSQL + GORM. Commits sin trailers de atribución.

## Review Focus

- Una cuenta desconocida instala la App pública: sus repos aparecen **deshabilitados** y sus PRs no se muestran hasta que un admin habilita repos.
- `GET /app/installations` falla o pagina mal: las instalaciones/repos existentes se conservan; no hay poda.
- Instalación suspendida: ninguna sync llama a GitHub; al reactivarla, la sync es inmediata.
- Visibilidad multi-cuenta: un no-admin no ve cuentas ajenas en la sección de cuentas; solo ve la cuenta de los repos de sus equipos.
- Botón “Añadir cuenta”: usa el `install_url` de la App; tras instalar, el callback encola sync y, si se pierde, el reconcile la descubre en ≤5 min.

---

### Task 1: Manifest público

**Files:**
- Modify: `internal/github/manifest.go`
- Test: `internal/github/manifest_test.go`

**Interfaces:**
- Consumes: nada nuevo.
- Produces: `BuildManifest` devuelve `Manifest` con `Public: true`; la firma no cambia.

- [ ] **Step 1: Write the failing test**

En `TestBuildManifest`, añade la aserción:

```go
if !m.Public {
    t.Fatalf("Public = false, want true (multi-cuenta requiere App pública)")
}
raw, _ := json.Marshal(m)
if !strings.Contains(string(raw), `"public":true`) {
    t.Fatalf("manifest JSON no declara public=true: %s", raw)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/github/ -run TestBuildManifest -count=1 -v`
Expected: FAIL porque `Public` es `false`.

- [ ] **Step 3: Implement**

En `BuildManifest`, cambia `Public: false` por `Public: true`. No cambies nada más.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/github/ -run TestBuildManifest -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/manifest.go internal/github/manifest_test.go
git commit -m "feat: GitHub App pública para varias cuentas"
```

---

### Task 2: Cliente de App y descubrimiento de instalaciones

**Files:**
- Modify: `internal/github/app.go`
- Test: `internal/github/app_test.go`
- Modify: `internal/syncer/syncer.go`
- Test: `internal/syncer/syncer_test.go`

**Interfaces:**
- Consumes: `App.Creds{AppID, PEM}`, `App.APIURL`; `Store.UpsertInstallation`, `Store.ListInstallations`, `Store.DeleteInstallation`; `events.Publisher`.
- Produces:
  ```go
  func (a *App) AppClient() (*gh.Client, error)
  func (s *Syncer) ReconcileAll(ctx context.Context) error // ahora descubre y poda
  ```
  `AppClient` usa `ghinstallation.NewAppsTransport(http.DefaultTransport, appID, pem)`, fija `BaseURL`/`UploadURL` al `APIURL` y devuelve un `gh.Client`.

- [ ] **Step 1: Write the failing tests**

En `internal/github/app_test.go`, `TestAppClientUsesAPIURL`: genera una clave RSA, crea `App{Creds: AppCredentials{AppID: 1, PEM: pem}, APIURL: fake.URL}`, llama `AppClient()` y `client.Apps.ListInstallations(ctx, nil)`; el fake debe recibir `GET /app/installations` con el JWT de la App y devolver `[]` o una instalación.

En `internal/syncer/syncer_test.go`:

```go
func TestReconcileDiscoversInstallations(t *testing.T)
// fake GET /app/installations -> 2 instalaciones (1 y 2, cuentas "acme" y "personal")
// fake /installation/repositories por instalación -> un repo cada una
// ReconcileAll: ambas filas en installations y ambos repos en repositories (Enabled=false)

func TestReconcilePrunesMissingInstallations(t *testing.T)
// BD con instalaciones 1 y 2; fake GET /app/installations -> solo 1
// ReconcileAll: 2 desaparece y sus repos/PRs también

func TestReconcileFallsBackOnDiscoveryError(t *testing.T)
// fake GET /app/installations -> 500; BD con instalación 1
// ReconcileAll: 1 sigue y se sincroniza; no se borra nada

func TestSyncInstallationSkipsSuspended(t *testing.T)
// instalación en BD con SuspendedAt != nil; contador de /installation/repositories = 0
// SyncInstallation devuelve nil
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock go test ./internal/github/ ./internal/syncer/ -run 'AppClient|Reconcile|Suspended' -count=1 -v`
Expected: FAIL (método/servicio no existe).

- [ ] **Step 3: Implement**

En `app.go`, `AppClient` con `ghinstallation.NewAppsTransport`. En `syncer.go`:

```go
func (s *Syncer) ReconcileAll(ctx context.Context) error {
    // 1. app, err := s.Apps.App(ctx); si falla, log y usa las de BD
    // 2. remote, err := s.listAppInstallations(ctx, app); si falla, log y usa las de BD
    // 3. si remote OK: UpsertInstallation de cada una, y DeleteInstallation de las de BD
    //    cuyo ID no está en remote
    // 4. SyncInstallation de cada instalación no suspendida
}
```

`SyncInstallation` empieza consultando la instalación por ID en `Store.DB`; si `SuspendedAt != nil`, devuelve nil sin llamar a GitHub. La listación pagina con `client.Apps.ListInstallations` y `NextPage`; solo se considera completa si no hubo error y se recorrieron todas las páginas.

- [ ] **Step 4: Run tests to verify they pass**

Run: `DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock go test ./internal/github/ ./internal/syncer/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/app.go internal/github/app_test.go internal/syncer/syncer.go internal/syncer/syncer_test.go
git commit -m "feat: descubrir y reconciliar instalaciones de varias cuentas"
```

---

### Task 3: Ciclo de suspensión y reactivación

**Files:**
- Modify: `internal/github/webhook.go`
- Test: `internal/github/webhook_test.go`

**Interfaces:**
- Consumes: `WebhookHandler.Jobs Enqueuer`; `Store.UpsertInstallation`.
- Produces: en `installation` `suspend`/`unsuspend` se actualiza `SuspendedAt`; `unsuspend` además llama `Jobs.EnqueueSync(installation.ID)`.

- [ ] **Step 1: Write the failing test**

```go
func TestWebhookUnsuspendEnqueuesSync(t *testing.T) {
    // fixture installation-unsuspended.json
    // firma válida; fake Jobs.Tras ServeHTTP: Jobs.ids == [installationID]
    // y la fila installation tiene SuspendedAt == nil
}
```

Añade también una aserción a `TestWebhookInstallationSuspended` (si existe): `suspend` no encola.

- [ ] **Step 2: Run test to verify it fails**

Run: `DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock go test ./internal/github/ -run 'WebhookUnsuspend' -count=1 -v`
Expected: FAIL (no encola).

- [ ] **Step 3: Implement**

En el `case "suspend"`/`"unsuspend"` del handler: actualiza `SuspendedAt`; solo en `unsuspend` llama `h.Jobs.EnqueueSync(ctx, installationID)` y responde 202. No encoles en `suspend`.

- [ ] **Step 4: Run test to verify it passes**

Run: `DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock go test ./internal/github/ -run 'Webhook.*Suspend' -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/github/webhook.go internal/github/webhook_test.go
git commit -m "feat: sincronizar al reactivar una instalación suspendida"
```

---

### Task 4: API de instalaciones y cuenta por repo

**Files:**
- Modify: `internal/api/router.go`, `internal/api/handlers.go`
- Test: `internal/api/api_test.go`

**Interfaces:**
- Consumes: `Store.ListInstallations`, `Store.ListRepositories`, `auth.RequireAdmin`.
- Produces:
  ```go
  // GET /api/installations (admin)
  type installationResponse struct {
      ID int64 `json:"id"`
      AccountLogin string `json:"account_login"`
      AccountType string `json:"account_type"`
      SuspendedAt *string `json:"suspended_at"`
  }
  ```
  `GET /api/repos` añade `"account_login"` a cada repo.

- [ ] **Step 1: Write the failing tests**

```go
func TestInstallationsAdminOnly(t *testing.T)
// admin GET /api/installations -> 200 con las cuentas
// developer -> 403

func TestReposIncludeAccountLogin(t *testing.T)
// admin GET /api/repos -> cada repo tiene account_login "acme"/"personal"

func TestReposAccountLoginTeamScoped(t *testing.T)
// developer del equipo T1 ve el repo de su equipo con account_login;
// no ve el repo de otra cuenta/equipo
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock go test ./internal/api/ -run 'Installations|AccountLogin' -count=1 -v`
Expected: FAIL (404/campo ausente).

- [ ] **Step 3: Implement**

Añade `GET /api/installations` al grupo `RequireAdmin`. En `repoResponse` añade `AccountLogin`; en el handler `repos`, carga `Store.ListInstallations` una vez y mapea `installation_id → account_login` para los repos visibles. `suspended_at` se serializa como RFC3339 o `null`. No añadas datos de instalaciones a no-admin.

- [ ] **Step 4: Run tests to verify they pass**

Run: `DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock go test ./internal/api/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/router.go internal/api/handlers.go internal/api/api_test.go
git commit -m "feat: API de cuentas conectadas y cuenta por repo"
```

---

### Task 5: Web de cuentas conectadas

**Files:**
- Modify: `web/src/api.ts`
- Modify: `web/src/views/ReposView.vue`
- Test: `web/src/views/ReposView.test.ts`

**Interfaces:**
- Consumes: `api.repos()`, `api.installations()`, `api.setupStatus()`, `useSessionStore().me`.
- Produces:
  ```ts
  export type Installation = { id:number; account_login:string; account_type:string; suspended_at:string|null }
  export type Repo = { ...; account_login:string }
  api.installations(): Promise<Installation[]>
  ```
  En `ReposView`, administrador: sección “Cuentas conectadas” + botón “Añadir cuenta” (`href=install_url`, `target=_blank`); todos: columna “Cuenta”.

- [ ] **Step 1: Write the failing tests**

En `ReposView.test.ts`:

```ts
it('admin ve cuentas conectadas y el botón Añadir cuenta', ...)
// api.installations -> [{account_login:'acme'},{account_login:'personal'}]
// api.setupStatus -> {configured:true, install_url:'https://github.com/apps/x/installations/new'}
// espera textos 'Cuentas conectadas', 'acme', 'personal', y un enlace con href correcto

it('developer no ve la sección de cuentas ni Añadir cuenta', ...)
// me.is_admin=false; espera la columna 'Cuenta' con el account_login del repo visible,
// pero no 'Cuentas conectadas' ni 'Añadir cuenta'
```

- [ ] **Step 2: Run test to verify it fails**

Run: `npm --prefix web run test -- --run ReposView`
Expected: FAIL.

- [ ] **Step 3: Implement**

Carga `api.repos()` y `api.teams()` como hoy. Si `isAdmin`, carga `api.installations()` y `api.setupStatus()` en paralelo; errores no fatales (la tabla sigue). Añade la sección y la columna. No llames a `api.installations` si no eres admin.

- [ ] **Step 4: Run test to verify it passes**

Run: `npm --prefix web run test -- --run`
Expected: PASS (todos los tests).

- [ ] **Step 5: Commit**

```bash
git add web/src/api.ts web/src/views/ReposView.vue web/src/views/ReposView.test.ts
git commit -m "feat(web): cuentas conectadas y columna de cuenta"
```

---

### Task 6: Documentación y verificación end-to-end

**Files:**
- Modify: `docs/install.md`, `docs/superpowers/specs/2026-11-18-qa-control-center-design.md`, `README.md`

**Interfaces:**
- Consumes: todo lo anterior.

- [ ] **Step 1: Update docs**

En `docs/install.md`, añade “Añadir otra cuenta u organización”:
1. Entra como admin → Repositorios → “Añadir cuenta”.
2. Instala la App en la otra cuenta/org.
3. Vuelve al panel; los repos aparecen deshabilitados; habilita los que quieras y asígnalos a un equipo.
4. Con webhooks la instalación entra al instante; en polling se descubre en la próxima reconciliación (≤5 min).

Aclara que la App es pública (cualquier cuenta puede instalarla) y que los repos quedan deshabilitados hasta que un admin los active. En la spec original, actualiza §2 para remitir al diseño multi-cuenta.

- [ ] **Step 2: Run the full verification**

Run:
```bash
DOCKER_HOST=unix:///Users/julio/.kiri/run/docker.sock make test
make build
```
Expected: Go + Vitest verdes y `bin/qa` generado.

- [ ] **Step 3: Manual e2e (si hay una segunda cuenta disponible)**

Con la instancia levantada y la App pública:
1. Crear/instalar la App en la cuenta A; verificar que sus repos aparecen.
2. “Añadir cuenta” → instalar en la cuenta B; verificar que aparecen sus repos, deshabilitados.
3. Habilitar un repo de B y asignarlo a un equipo; verificar visibilidad para un developer de ese equipo.
4. Si no hay webhooks, esperar/forzar la reconciliación y comprobar que la instalación se descubre.

Expected: ambas cuentas listadas, repos de ambas visibles para admin, aislamiento por equipo intacto.

- [ ] **Step 4: Commit**

```bash
git add docs/install.md docs/superpowers/specs/2026-11-18-qa-control-center-design.md README.md
git commit -m "docs: añadir cuentas de GitHub a la instalación"
```

---

## Self-Review

- Cobertura: manifest público (T1), descubrimiento/poda (T2), suspensión (T3), API (T4), web (T5), docs/e2e (T6). El diseño §4.1–4.5 queda cubierto.
- Dependencias: T2 usa `AppClient`; T3 usa el handler existente; T4 usa las instalaciones; T5 usa la API. T6 al final.
- Sin migración de esquema. Sin cambios en el motor de revisión.
- Review Focus cubierto: repos deshabilitados (T2/T6), fallo de descubrimiento (T2), suspensión (T2/T3), visibilidad (T4/T5), botón/instalación (T5/T6).
