# QA Control Center — Fase 1 (Núcleo) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Binario Go único (`qa serve` / `qa worker` / `qa migrate`) que se instala con Docker Compose, crea su GitHub App por manifest, recibe webhooks y reconcilia PRs, con login GitHub, equipos/roles y un Dashboard Vue en vivo que muestra los PRs abiertos de los repos activos.

**Architecture:** `qa serve` expone API REST + SSE + SPA Vue embebida (`go:embed`), recibe webhooks y corre River (cola en Postgres) en la cola `control` para sincronización y reconciliación periódica. Estado en PostgreSQL vía GORM sobre un `pgxpool`; eventos en vivo vía `LISTEN/NOTIFY` → broker SSE. `qa worker` arranca y queda inactivo (la cola `review` llega en el plan de Fase 2).

**Tech Stack:** Go 1.27, chi v5, GORM + gorm.io/driver/postgres (pgx v5), pressly/goose v3 (migraciones SQL embebidas), riverqueue/river + riverpgxv5, google/go-github (última versión mayor), bradleyfitz/ghinstallation/v2, testcontainers-go (módulo postgres), Vue 3 + Vite + TypeScript + vue-router 4 + Pinia, Vitest + @vue/test-utils, Docker Compose (postgres:17).

**Spec:** `docs/superpowers/specs/2026-11-18-qa-control-center-design.md`

**Alcance:** este es el plan 1 de 4 (fases §16 del spec). Fase 2 (motor), 3 (gate y ciclo humano) y 4 (operación) se planifican después de ejecutar esta, sobre las interfaces reales. Fuera de este plan: revisiones LLM, comentarios en PR, checks, políticas QA, sincronización con GitHub Teams, analítica.

## Global Constraints

- Self-hosted con Docker Compose de 3 contenedores: `app` (`qa serve`), `worker` (`qa worker`), `postgres`. Una sola imagen.
- Stack fijo: Go, Vue 3 + Vite + TS, PostgreSQL + GORM, SSE, GitHub App. Sin Redis.
- Permisos de la GitHub App: Pull requests R/W, Checks R/W, Contents R, Metadata R, Commit statuses R, Members R (org), Issues R/W.
- Eventos de la App: `pull_request`, `pull_request_review_comment`, `issue_comment`, `check_suite`, `check_run`, `status` (+ `installation`, `installation_repositories`, que GitHub siempre envía).
- Webhooks verificados con HMAC (`X-Hub-Signature-256`). Secretos cifrados en reposo con AES-GCM y clave maestra de `QA_MASTER_KEY` (base64 estándar, 32 bytes).
- Reconciliación periódica por defecto cada 5 minutos (`QA_RECONCILE_INTERVAL`); `QA_WEBHOOKS_ENABLED=false` = modo solo polling.
- Login con OAuth de usuario de la misma GitHub App; el primer usuario que inicia sesión es admin de instancia.
- Roles: admin de instancia; por equipo `lead`, `qa`, `developer`. Un repo pertenece a exactamente un equipo. Los desarrolladores solo ven repos de sus equipos.
- Copy del Dashboard (español, del usuario): "QA Control Center", "Todos los repositorios", "Servicio activo"; KPIs "PRs abiertos", "Esperando tu revisión", "En evaluación", "Con problemas"; filtros "Todos", "En revisión", "Bloqueados", "Listos".
- Estados de PR (`qa_status`), todos definidos ya aunque Fase 1 solo use `draft`, `pending`, `closed`, `merged`: `draft`, `awaiting_approval`, `pending`, `reviewing`, `changes_requested`, `waiting_ci`, `ready_for_human`, `inconclusive`, `paused`, `merged`, `closed`.
- Commits sin trailers de atribución.

## Review Focus

- Webhooks duplicados o fuera de orden (`synchronize` antes que `opened`, redelivery de un evento viejo): el estado del PR nunca retrocede a un `head_sha`/`updated_at` anterior → Task 4.
- Repo quitado de la instalación o instalación borrada con PRs existentes: desaparecen del Dashboard sin errores → Task 4 y Task 8.
- Usuario no autenticado o de otro equipo consultando API/SSE: 401 o listas vacías; los eventos SSE solo llevan IDs y el cliente refetchea por la API filtrada → Task 10.
- Un extraño llega a `/setup` antes de configurar la instancia: requiere el token de setup impreso en el log; ya configurada, el setup responde 409 → Task 7.
- Organizaciones con >100 repos o PRs: la sincronización pagina → Task 8.

---

## Estructura de archivos

```
go.mod
cmd/qa/main.go                      subcomandos serve | worker | migrate
internal/config/config.go           Config + Load
internal/crypto/box.go              AES-GCM Box
internal/store/
  store.go                          Open, Store{DB,Pool}, Migrate
  models.go                         modelos GORM
  status.go                         constantes qa_status, grupos, OnSync
  prs.go                            instalaciones, repos, PRs, vistas, stats
  users.go                          usuarios, sesiones, equipos, roles, auditoría, app config
  migrations/00001_init.sql
internal/testutil/pg.go             NewStore(t) con testcontainers
internal/events/events.go           Publish (pg_notify) + Broker (LISTEN) + SSE handler
internal/github/
  manifest.go                       BuildManifest, ConvertManifest
  convert.go                        PRFromGitHub
  app.go                            App, AppProvider (desde Store+Box)
  webhook.go                        VerifySignature, WebhookHandler
internal/syncer/syncer.go           SyncInstallation
internal/jobs/jobs.go               River: args, workers, periodic, Enqueuer
internal/auth/auth.go               OAuth, sesiones, middleware, RBAC
internal/api/router.go              chi router, handlers REST, setup
internal/web/embed.go               go:embed dist + SPA fallback
web/                                Vue 3 + Vite + TS
deploy/Dockerfile, deploy/docker-compose.yml, deploy/.env.example
docs/install.md
```

---

### Task 1: Módulo Go, configuración y CLI

**Files:**
- Create: `go.mod` (module `github.com/jeriveromartinez/qa-control-center`), `cmd/qa/main.go`, `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  ```go
  type Config struct {
      DatabaseURL       string        // QA_DATABASE_URL (requerido)
      PublicURL         string        // QA_PUBLIC_URL (requerido, sin "/" final)
      ListenAddr        string        // QA_LISTEN_ADDR, default ":8080"
      MasterKey         []byte        // QA_MASTER_KEY base64 std, exactamente 32 bytes
      WebhooksEnabled   bool          // QA_WEBHOOKS_ENABLED, default true
      ReconcileInterval time.Duration // QA_RECONCILE_INTERVAL, default 5m
      GitHubURL         string        // QA_GITHUB_URL, default "https://github.com"
      GitHubAPIURL      string        // QA_GITHUB_API_URL, default "https://api.github.com"
  }
  func Load(getenv func(string) string) (Config, error)
  ```
  `main` dispatch: `qa serve`, `qa worker`, `qa migrate`; cualquier otro → uso + exit 2.

- [ ] **Step 1: Write the failing tests** — `TestLoadDefaults` (solo requeridos → `ListenAddr==":8080"`, `ReconcileInterval==5*time.Minute`, `WebhooksEnabled==true`, `GitHubURL=="https://github.com"`, `GitHubAPIURL=="https://api.github.com"`); `TestLoadMissingRequired` (sin `QA_DATABASE_URL` → error que menciona `QA_DATABASE_URL`); `TestLoadBadMasterKey` (base64 de 16 bytes → error que menciona `32 bytes`); `TestLoadTrimsPublicURL` (`"https://qa.example.com/"` → `"https://qa.example.com"`).
- [ ] **Step 2: Run** `go test ./internal/config/...` — Expected: FAIL (paquete no compila).
- [ ] **Step 3: Implement** `Load` y `cmd/qa/main.go` (logging con `log/slog` JSON; `serve`/`worker`/`migrate` de momento llaman a funciones `runServe`, `runWorker`, `runMigrate` que devuelven `errors.New("not implemented")`).
- [ ] **Step 4: Run** `go test ./internal/config/... && go build ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: config y CLI base"`

### Task 2: Cifrado de secretos

**Files:**
- Create: `internal/crypto/box.go`
- Test: `internal/crypto/box_test.go`

**Interfaces:**
- Produces: `func NewBox(key []byte) (*Box, error)`; `func (b *Box) Seal(plain []byte) (string, error)` (base64 std de `nonce||ciphertext`, nonce aleatorio de 12 bytes); `func (b *Box) Open(sealed string) ([]byte, error)`.

- [ ] **Step 1: Write the failing tests** — `TestRoundTrip` (Seal→Open devuelve el original); `TestNonceRandom` (dos Seal del mismo texto difieren); `TestWrongKey` (Open con otra clave → error); `TestTampered` (cambiar un byte → error); `TestKeyLength` (`NewBox` con 16 bytes → error).
- [ ] **Step 2: Run** `go test ./internal/crypto/...` — Expected: FAIL.
- [ ] **Step 3: Implement** con `crypto/aes` + `cipher.NewGCM`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: cifrado AES-GCM de secretos"`

### Task 3: Base de datos, migraciones y helper de tests

**Files:**
- Create: `internal/store/store.go`, `internal/store/models.go`, `internal/store/status.go`, `internal/store/migrations/00001_init.sql`, `internal/testutil/pg.go`
- Modify: `cmd/qa/main.go` (`runMigrate`)
- Test: `internal/store/store_test.go`, `internal/store/status_test.go`

**Interfaces:**
- Produces:
  ```go
  type Store struct { DB *gorm.DB; Pool *pgxpool.Pool }
  func Open(ctx context.Context, url string) (*Store, error) // pgxpool + gorm sobre stdlib.OpenDBFromPool
  func (s *Store) Migrate(ctx context.Context) error          // goose (embed migrations/*.sql) + rivermigrate Up
  func (s *Store) Close()
  // testutil
  func NewStore(t *testing.T) *store.Store // contenedor postgres:17 compartido por paquete (sync.Once), DB nueva por test, Migrate, Cleanup
  ```
  Modelos GORM (tablas en plural snake_case; `created_at`/`updated_at` en todas):
  - `AppConfig` (fila única `id=1`): `AppID int64`, `Slug`, `HTMLURL`, `ClientID`, `ClientSecretEnc`, `PrivateKeyEnc`, `WebhookSecretEnc string`.
  - `User`: `ID uint`, `GitHubID int64` único, `Login`, `AvatarURL`, `IsAdmin bool`.
  - `Session`: `ID uint`, `TokenHash string` único (sha256 hex), `UserID uint`, `ExpiresAt time.Time`.
  - `Installation`: `ID int64` (id de GitHub, PK), `AccountLogin`, `AccountType string`, `SuspendedAt *time.Time`.
  - `Team`: `ID uint`, `Name` único, `Slug` único.
  - `TeamMember`: PK (`TeamID uint`, `UserID uint`), `Role string` (`lead|qa|developer`).
  - `Repository`: `ID int64` (id GitHub, PK), `InstallationID int64` (FK cascade), `FullName` único, `Private bool`, `DefaultBranch`, `Enabled bool` default false, `TeamID *uint` (FK set null).
  - `PullRequest`: `ID uint`, `RepositoryID int64` (FK cascade), `Number int`, único (`repository_id`,`number`), `Title`, `AuthorLogin`, `AuthorAssociation`, `HeadSHA`, `BaseRef`, `HeadRef`, `HTMLURL string`, `Draft bool`, `State string` (`open|closed|merged`), `QAStatus string`, `GitHubUpdatedAt time.Time`.
  - `AuditLog`: `ID uint`, `ActorUserID *uint`, `Action`, `TargetType`, `TargetID string`, `Data datatypes.JSON`.
  - `status.go`: constantes `StatusDraft … StatusClosed` (valores del Global Constraints); `GroupOf(status string) string` → `"in_review"` para pending/reviewing/waiting_ci, `"blocked"` para changes_requested/inconclusive/awaiting_approval/paused, `"ready"` para ready_for_human, `""` resto; `OnSync(prev *PullRequest, in PullRequest) string` (regla Fase 1: `State` merged/closed → ese estado; `Draft` → `draft`; si `prev==nil`, `prev.HeadSHA != in.HeadSHA` o `prev.QAStatus` ∈ {draft, closed, merged} → `pending`; si no, conserva `prev.QAStatus`).

- [ ] **Step 1: Write the failing tests** — `TestMigrateCreatesTables` (tras `NewStore`, `DB.Migrator().HasTable` true para las 9 tablas y `river_job`); `TestMigrateIdempotent` (segundo `Migrate` sin error); tabla de `TestOnSync` cubriendo: nuevo abierto → pending; nuevo draft → draft; draft→ready → pending; mismo SHA en `changes_requested` → conserva; SHA nuevo en `ready_for_human` → pending; cerrado → closed; merged → merged; reabierto desde closed → pending. `TestGroupOf` para los 11 estados.
- [ ] **Step 2: Run** `go test ./internal/store/...` — Expected: FAIL.
- [ ] **Step 3: Implement** migración SQL que coincida con los modelos (FKs, índices únicos), `Open`, `Migrate`, `OnSync`, `GroupOf`, `NewStore`, y `runMigrate` (Load config → Open → Migrate).
- [ ] **Step 4: Run** `go test ./internal/store/...` — Expected: PASS (requiere Docker).
- [ ] **Step 5: Commit** `git commit -m "feat: esquema, migraciones y estados de PR"`

### Task 4: Store — instalaciones, repos, PRs, vistas y KPIs

**Files:**
- Create: `internal/store/prs.go`
- Test: `internal/store/prs_test.go`

**Interfaces:**
- Consumes: modelos y `OnSync`/`GroupOf` (Task 3).
- Produces:
  ```go
  var ErrNotFound = errors.New("not found")
  func (s *Store) UpsertInstallation(ctx, in Installation) error
  func (s *Store) DeleteInstallation(ctx, id int64) error            // cascade repos y PRs
  func (s *Store) ListInstallations(ctx) ([]Installation, error)
  func (s *Store) UpsertRepository(ctx, r Repository) error           // en conflicto NO toca Enabled ni TeamID
  func (s *Store) RemoveRepository(ctx, id int64) error
  func (s *Store) EnabledRepositories(ctx, installationID int64) ([]Repository, error)
  func (s *Store) SyncPullRequest(ctx, in PullRequest) (pr PullRequest, changed bool, err error)
  func (s *Store) CloseMissingPullRequests(ctx, repoID int64, openNumbers []int) error // PRs abiertos en BD que GitHub ya no lista → closed
  type PRFilter struct{ Group string } // "", "in_review", "blocked", "ready"
  type PRView struct {
      ID uint; Repo string; Number int; Title, AuthorLogin, HTMLURL, QAStatus, Group string
      Validations struct{ Passed, Total int } // Fase 1 siempre 0/0
      UpdatedAt time.Time
  }
  func (s *Store) ListVisiblePullRequests(ctx, u User, f PRFilter) ([]PRView, error)
  type Stats struct{ Open, AwaitingHuman, InEvaluation, WithProblems int }
  func (s *Store) Stats(ctx, u User) (Stats, error)
  ```
  Visibilidad: solo PRs `State=open` de repos `Enabled`; admin ve todo; otro usuario solo repos cuyo `TeamID` está entre sus equipos. Orden: `updated_at` desc. `Stats`: `Open` = visibles abiertos; `AwaitingHuman` = ready; `InEvaluation` = in_review; `WithProblems` = blocked.

- [ ] **Step 1: Write the failing tests**
  - `TestUpsertRepositoryKeepsSettings`: habilitar y asignar equipo, volver a upsert con otro `FullName` → `Enabled` y `TeamID` intactos, nombre actualizado.
  - `TestSyncPullRequestIgnoresStaleEvent`: sync con `GitHubUpdatedAt=t2, HeadSHA="b"`, luego con `t1<t2, HeadSHA="a"` → `changed==false`, sigue `"b"`.
  - `TestSyncPullRequestDuplicateIsNoop`: mismo payload dos veces → segunda `changed==false`.
  - `TestDeleteInstallationRemovesPRs`: tras borrar, `ListVisiblePullRequests(admin)` vacío.
  - `TestCloseMissingPullRequests`: abiertos #1,#2; llamar con `[1]` → #2 `State="closed"`, `QAStatus="closed"`.
  - `TestVisibility`: repo A equipo T1, repo B equipo T2, repo C deshabilitado; developer de T1 ve solo PRs de A; admin ve A y B; nadie ve C; usuario sin equipo ve vacío.
  - `TestFilterAndStats`: PRs en pending, changes_requested, ready_for_human, draft → `Stats{Open:4, AwaitingHuman:1, InEvaluation:1, WithProblems:1}`; `Group:"ready"` devuelve solo el ready.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'Upsert|Sync|Delete|Close|Visib|Filter'` — Expected: FAIL.
- [ ] **Step 3: Implement** con `clause.OnConflict`; `SyncPullRequest` en transacción con `SELECT … FOR UPDATE`, descarta si `in.GitHubUpdatedAt` < guardado, calcula `QAStatus` con `OnSync`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: store de instalaciones, repos y PRs"`

### Task 5: Store — usuarios, sesiones, equipos, auditoría y config de la App

**Files:**
- Create: `internal/store/users.go`
- Test: `internal/store/users_test.go`

**Interfaces:**
- Produces:
  ```go
  var ErrNotConfigured = errors.New("github app not configured")
  func (s *Store) UpsertUserFromGitHub(ctx, ghID int64, login, avatar string) (User, error) // primer usuario de la tabla → IsAdmin=true (transacción con lock de tabla)
  func (s *Store) CreateSession(ctx, userID uint, ttl time.Duration) (token string, err error) // token 32 bytes aleatorios base64url; guarda sha256
  func (s *Store) UserBySessionToken(ctx, token string) (User, error) // ErrNotFound si no existe o expiró
  func (s *Store) DeleteSession(ctx, token string) error
  func (s *Store) ListUsers(ctx) ([]User, error)
  func (s *Store) CreateTeam(ctx, name string) (Team, error) // slug derivado del nombre
  func (s *Store) ListTeams(ctx) ([]TeamWithMembers, error)
  func (s *Store) SetTeamMember(ctx, teamID, userID uint, role string) error // role ∈ lead|qa|developer, si no error
  func (s *Store) RemoveTeamMember(ctx, teamID, userID uint) error
  func (s *Store) TeamRole(ctx, userID, teamID uint) (string, error)       // "" si no es miembro
  func (s *Store) RepoRole(ctx, userID uint, repoID int64) (string, error) // rol en el equipo dueño del repo, "" si ninguno
  func (s *Store) SetRepositoryEnabled(ctx, repoID int64, enabled bool) error
  func (s *Store) AssignRepositoryTeam(ctx, repoID int64, teamID *uint) error
  func (s *Store) ListRepositories(ctx, u User) ([]Repository, error) // admin todos; otros los de sus equipos
  func (s *Store) Audit(ctx, actorID *uint, action, targetType, targetID string, data any) error
  func (s *Store) SaveAppConfig(ctx, c AppConfig) error
  func (s *Store) LoadAppConfig(ctx) (AppConfig, error) // ErrNotConfigured si no hay fila
  ```
  `TeamWithMembers` = `Team` + `Members []struct{ User User; Role string }`.

- [ ] **Step 1: Write the failing tests** — `TestFirstUserIsAdmin` (1º admin, 2º no; re-login del 1º sigue admin y actualiza login/avatar); `TestSessionLifecycle` (crear → resolver; expirada con ttl negativo → ErrNotFound; borrar → ErrNotFound; en BD no aparece el token en claro); `TestTeamRoles` (`SetTeamMember` con rol `"owner"` → error; `RepoRole` devuelve el rol del equipo dueño y `""` para repo sin equipo); `TestAppConfigNotConfigured` (`LoadAppConfig` en BD vacía → `ErrNotConfigured`; tras `Save` → devuelve lo guardado); `TestAuditWritesRow`.
- [ ] **Step 2: Run** `go test ./internal/store/ -run 'FirstUser|Session|TeamRoles|AppConfig|Audit'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/store/...` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: usuarios, sesiones, equipos y auditoría"`

### Task 6: Eventos en vivo (NOTIFY → SSE)

**Files:**
- Create: `internal/events/events.go`
- Test: `internal/events/events_test.go`

**Interfaces:**
- Produces:
  ```go
  type Event struct { Type string `json:"type"`; ID string `json:"id"` } // solo IDs, nunca datos: el cliente refetchea vía API filtrada
  const ChannelName = "qa_events"
  type Publisher interface{ Publish(ctx context.Context, ev Event) error }
  func NewPGPublisher(pool *pgxpool.Pool) Publisher            // SELECT pg_notify('qa_events', json)
  type Broker struct{ /* … */ }
  func NewBroker(pool *pgxpool.Pool) *Broker
  func (b *Broker) Run(ctx context.Context) error              // LISTEN con reconexión
  func (b *Broker) Subscribe() (<-chan Event, func())          // buffer 64; si se llena, descarta para ese suscriptor
  func (b *Broker) ServeSSE(w http.ResponseWriter, r *http.Request) // text/event-stream, "event: <type>\ndata: <json>\n\n", ping ": ping" cada 25s
  ```
  Tipos de evento Fase 1: `pr.updated` (ID = PR id), `repo.updated` (ID = repo id).

- [ ] **Step 1: Write the failing tests** — `TestPublishReachesSubscriber` (con `testutil.NewStore`: `Run` en goroutine, `Subscribe`, `Publish(pr.updated,"42")` → llega en <2s); `TestServeSSEStreamsEvent` (`httptest` server con `ServeSSE`, cliente lee la línea `event: pr.updated` y `data: {"type":"pr.updated","id":"42"}`); `TestUnsubscribeStops` (tras cancelar, el canal se cierra).
- [ ] **Step 2: Run** `go test ./internal/events/...` — Expected: FAIL.
- [ ] **Step 3: Implement** con `pool.Acquire` + `conn.Conn().WaitForNotification`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: broker de eventos SSE sobre LISTEN/NOTIFY"`

### Task 7: GitHub App — manifest, credenciales y clientes

**Files:**
- Create: `internal/github/manifest.go`, `internal/github/app.go`, `internal/github/convert.go`
- Test: `internal/github/manifest_test.go`, `internal/github/app_test.go`, `internal/github/convert_test.go`

**Interfaces:**
- Consumes: `Store.SaveAppConfig/LoadAppConfig` (Task 5), `crypto.Box` (Task 2).
- Produces:
  ```go
  type Manifest struct { /* JSON de GitHub: name, url, hook_attributes{url,active}, redirect_url, callback_urls, setup_url, public, default_permissions, default_events */ }
  func BuildManifest(publicURL string, webhooksEnabled bool, name string) Manifest
  type AppCredentials struct { AppID int64; Slug, HTMLURL, ClientID, ClientSecret, WebhookSecret, PEM string }
  func ConvertManifest(ctx context.Context, httpc *http.Client, apiURL, code string) (AppCredentials, error) // POST {api}/app-manifests/{code}/conversions
  func SaveCredentials(ctx, st *store.Store, box *crypto.Box, c AppCredentials) error
  type App struct { Creds AppCredentials; APIURL string }
  func (a *App) InstallationClient(installationID int64) (*gh.Client, error) // ghinstallation; respeta APIURL (enterprise/fake)
  func (a *App) InstallURL() string                                          // HTMLURL + "/installations/new"
  type AppProvider interface{ App(ctx context.Context) (*App, error) }        // ErrNotConfigured si falta
  func NewStoreAppProvider(st *store.Store, box *crypto.Box, apiURL string) AppProvider // cachea tras primera carga
  func PRFromGitHub(repoID int64, p *gh.PullRequest) store.PullRequest // convert.go; State: merged si MergedAt!=nil, si no closed/open según p.State
  // Interfaz mínima para encolar sincronizaciones sin importar internal/jobs (evita ciclo github→jobs→syncer→github)
  type SyncEnqueuer interface{ EnqueueSync(ctx context.Context, installationID int64) error }
  ```
  Regla de dependencias: `internal/github` solo importa `store`, `crypto` y `events`; `syncer` importa `github`; `jobs` importa `syncer`; `api` importa todos.
  Manifest: `hook_attributes.url = publicURL+"/webhooks/github"`, `active = webhooksEnabled`; `redirect_url = publicURL+"/setup/callback"`; `callback_urls = [publicURL+"/auth/callback"]`; `setup_url = publicURL+"/setup/installed"`; `public=false`; permisos y eventos exactamente los del Global Constraints (`pull_requests:write, checks:write, contents:read, metadata:read, statuses:read, members:read, issues:write`).

- [ ] **Step 1: Write the failing tests** — `TestBuildManifest` (URLs exactas para `https://qa.example.com`, mapa de permisos y lista de eventos exactos, `active=false` cuando webhooks deshabilitados); `TestConvertManifest` (fake `httptest` que responde el JSON de conversión de GitHub con `id, slug, html_url, client_id, client_secret, webhook_secret, pem` → mapeo correcto; status 404 → error); `TestProviderRoundTrip` (`SaveCredentials` cifra: la fila guardada no contiene el PEM en claro; `NewStoreAppProvider(...).App` lo devuelve descifrado; BD vacía → `ErrNotConfigured`); `TestInstallationClientUsesAPIURL` (PEM RSA generado en el test; el cliente pide el token a `{fake}/app/installations/7/access_tokens`); `TestPRFromGitHub` (merged/closed/open/draft).
- [ ] **Step 2: Run** `go test ./internal/github/...` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: GitHub App por manifest y clientes de instalación"`

### Task 8: Sincronización de instalaciones y jobs River

**Files:**
- Create: `internal/syncer/syncer.go`, `internal/jobs/jobs.go`
- Test: `internal/syncer/syncer_test.go`, `internal/jobs/jobs_test.go`

**Interfaces:**
- Consumes: `AppProvider`, `App.InstallationClient` (Task 7); store (Tasks 4–5); `events.Publisher` (Task 6).
- Produces:
  ```go
  // syncer
  type Syncer struct { Store *store.Store; Apps github.AppProvider; Events events.Publisher }
  func (s *Syncer) SyncInstallation(ctx context.Context, installationID int64) error
  //  1. GET /installation/repositories (paginado, per_page=100) → UpsertRepository; RemoveRepository para los que ya no están
  //  2. por cada repo Enabled: GET /repos/{o}/{r}/pulls?state=open (paginado) → SyncPullRequest; CloseMissingPullRequests
  //  3. Publish pr.updated por cada changed y repo.updated por cada repo nuevo/eliminado
  func (s *Syncer) ReconcileAll(ctx context.Context) error // ListInstallations → SyncInstallation; un error por instalación se registra y no detiene las demás
  // jobs
  type SyncInstallationArgs struct{ InstallationID int64 } // Kind "sync_installation", queue "control", unique por args 1 min
  type ReconcileArgs struct{}                               // Kind "reconcile", queue "control", periódico cada cfg.ReconcileInterval, RunOnStart
  type Enqueuer interface{ EnqueueSync(ctx context.Context, installationID int64) error } // mismo método que github.SyncEnqueuer
  func NewControlClient(pool *pgxpool.Pool, s *syncer.Syncer, interval time.Duration) (*river.Client[pgx.Tx], error)
  func NewEnqueuer(c *river.Client[pgx.Tx]) Enqueuer
  ```

- [ ] **Step 1: Write the failing tests** (fake GitHub con `httptest` que implementa token de instalación, `/installation/repositories` y `/repos/*/pulls`, con cabecera `Link` para paginar):
  - `TestSyncPaginates`: 150 repos en 2 páginas → 150 en BD; 120 PRs abiertos en un repo habilitado → 120 PRs.
  - `TestSyncRemovesRepo`: segunda sync sin el repo B → B borrado y sus PRs fuera de `ListVisiblePullRequests`.
  - `TestSyncSkipsDisabledRepos`: repo deshabilitado → no se llama a `/pulls` (contador del fake = 0).
  - `TestSyncClosesMissing`: PR #5 ya no listado → closed.
  - `TestSyncPublishesEvents`: Publisher falso registra `pr.updated` por PR nuevo y nada en una segunda sync idéntica.
  - `TestReconcileContinuesOnError`: instalación 1 responde 500, la 2 se sincroniza.
  - `TestControlClientRunsSyncJob` (jobs): `EnqueueSync(ctx, 7)` con River en marcha → el syncer falso recibe 7 en <5s.
- [ ] **Step 2: Run** `go test ./internal/syncer/... ./internal/jobs/...` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: sincronización de instalaciones y reconciliación periódica"`

### Task 9: Webhooks

**Files:**
- Create: `internal/github/webhook.go`
- Test: `internal/github/webhook_test.go`

**Interfaces:**
- Consumes: `AppProvider` (secreto), store, `PRFromGitHub`, `SyncEnqueuer` (Task 7), `events.Publisher`.
- Produces:
  ```go
  func VerifySignature(secret, body []byte, header string) bool // "sha256=<hex>", hmac.Equal
  type WebhookHandler struct { Store *store.Store; Apps AppProvider; Events events.Publisher; Jobs SyncEnqueuer }
  func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)
  ```
  Comportamiento: cuerpo máx. 25 MiB; firma inválida → 401; App no configurada → 503; evento desconocido → 204.
  - `installation` `created` → `UpsertInstallation` + `EnqueueSync(id)`; `deleted` → `DeleteInstallation`; `suspend`/`unsuspend` → actualiza `SuspendedAt`.
  - `installation_repositories` `added`/`removed` → upsert/remove repos + `repo.updated`.
  - `pull_request` (`opened`, `synchronize`, `reopened`, `ready_for_review`, `converted_to_draft`, `edited`, `closed`) → si el repo existe y está `Enabled`: `SyncPullRequest(PRFromGitHub)` y `pr.updated` si changed; si no, 204 sin cambios.
  - Resto de eventos del manifest → 204 (se procesan en fases 2–3).
  - Respuesta 202 en eventos procesados.

- [ ] **Step 1: Write the failing tests** — `TestVerifySignature` (válida, inválida, sin prefijo `sha256=`); `TestWebhookBadSignature401`; `TestWebhookInstallationCreated` (fila creada y Enqueuer falso recibe `EnqueueSync(ID)`); `TestWebhookPROpenedEnabledRepo` (PR `pending` y evento publicado); `TestWebhookPRDisabledRepoIgnored`; `TestWebhookOutOfOrder` (`synchronize` con `updated_at` posterior y luego `opened` con anterior → queda el SHA del `synchronize`); `TestWebhookRedelivery` (mismo `synchronize` dos veces → un solo evento publicado); `TestWebhookDraftThenReady` (draft → `draft`; `ready_for_review` → `pending`); `TestWebhookInstallationDeleted`. Payloads como fixtures JSON mínimos en `internal/github/testdata/`.
- [ ] **Step 2: Run** `go test ./internal/github/ -run Webhook\|Verify` — Expected: FAIL.
- [ ] **Step 3: Implement** con `gh.ParseWebHook`.
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: receptor de webhooks de GitHub"`

### Task 10: Autenticación, RBAC, API REST y setup

**Files:**
- Create: `internal/auth/auth.go`, `internal/api/router.go`, `internal/api/handlers.go`, `internal/api/setup.go`
- Modify: `cmd/qa/main.go` (`runServe`, `runWorker`)
- Test: `internal/auth/auth_test.go`, `internal/api/api_test.go`

**Interfaces:**
- Consumes: todo lo anterior.
- Produces:
  ```go
  // auth
  const SessionCookie = "qa_session" // HttpOnly, SameSite=Lax, Secure si PublicURL es https, 7 días
  type Auth struct { Store *store.Store; Apps github.AppProvider; GitHubURL, APIURL, PublicURL string; HTTP *http.Client }
  func (a *Auth) Login(w, r)    // GET /auth/login: cookie "qa_oauth_state" aleatoria → 302 a {GitHubURL}/login/oauth/authorize?client_id&state&redirect_uri={PublicURL}/auth/callback
  func (a *Auth) Callback(w, r) // valida state; POST {GitHubURL}/login/oauth/access_token; GET {APIURL}/user; UpsertUserFromGitHub; CreateSession; 302 "/"
  func (a *Auth) Logout(w, r)   // POST /auth/logout
  func (a *Auth) Middleware(next http.Handler) http.Handler // carga usuario si hay cookie válida
  func UserFrom(ctx context.Context) (store.User, bool)
  func RequireUser(next http.Handler) http.Handler  // 401 JSON {"error":"unauthorized"}
  func RequireAdmin(next http.Handler) http.Handler // 403 si no admin
  // api
  type Deps struct { Cfg config.Config; Store *store.Store; Box *crypto.Box; Apps github.AppProvider; Auth *auth.Auth; Webhooks http.Handler; Broker *events.Broker; Events events.Publisher; Jobs jobs.Enqueuer; SetupToken string; Static http.Handler }
  func NewRouter(d Deps) http.Handler
  ```
  Rutas:
  | Ruta | Acceso | Respuesta |
  |---|---|---|
  | `GET /healthz` | público | `{"status":"ok"}` |
  | `GET /api/setup/status` | público | `{"configured":bool,"install_url":string}` |
  | `POST /api/setup/manifest` `{"setup_token","org"}` | token de setup | `{"action_url","manifest"}`; `action_url` = `{GitHubURL}/settings/apps/new` u `/organizations/{org}/settings/apps/new`; token incorrecto 403; ya configurado 409 |
  | `GET /setup/callback?code` | público (solo sin configurar, si no 409) | `ConvertManifest` + `SaveCredentials` + audit `app.configured` → 302 a `InstallURL()` |
  | `GET /setup/installed?installation_id` | público | `EnqueueSync` → 302 `/` |
  | `/auth/login`, `/auth/callback`, `POST /auth/logout` | — | ver Auth |
  | `POST /webhooks/github` | firma | WebhookHandler |
  | `GET /api/me` | usuario | `{id,login,avatar_url,is_admin,teams:[{id,name,role}]}` |
  | `GET /api/stats` | usuario | `Stats` (snake_case) |
  | `GET /api/prs?group=` | usuario | `[]PRView` (snake_case) |
  | `GET /api/repos` | usuario | `ListRepositories` |
  | `PATCH /api/repos/{id}` `{"enabled"?,"team_id"?}` | admin | 204; audit `repo.updated`; publica `repo.updated`; al habilitar, `EnqueueSync` de su instalación |
  | `GET /api/teams` | usuario | `ListTeams` |
  | `POST /api/teams` `{"name"}` | admin | 201 |
  | `PUT /api/teams/{id}/members/{userId}` `{"role"}` | admin o lead del equipo | 204; audit |
  | `DELETE /api/teams/{id}/members/{userId}` | admin o lead del equipo | 204; audit |
  | `GET /api/users` | admin | `ListUsers` |
  | `GET /api/events` | usuario | `Broker.ServeSSE` |
  | resto | público | `Static` (SPA) |

  `runServe`: Load → Open → Migrate → Box → AppProvider → Broker.Run → River control client Start → si `LoadAppConfig` da `ErrNotConfigured`, genera `SetupToken` (16 bytes hex) y lo registra con `slog.Warn("setup required", "setup_token", …, "url", PublicURL+"/setup")` → `http.Server` con shutdown ordenado ante SIGTERM. `runWorker`: Load → Open → log "worker idle: review queue arrives in phase 2" → espera SIGTERM.

- [ ] **Step 1: Write the failing tests** (`httptest` + `testutil.NewStore` + fake GitHub para OAuth):
  - `TestOAuthFlow`: `/auth/login` pone cookie de state y redirige con `client_id`; `/auth/callback` con state erróneo → 400; con state correcto → cookie `qa_session` y usuario creado admin.
  - `TestUnauthenticated401`: `/api/me`, `/api/prs`, `/api/events` sin cookie → 401.
  - `TestDeveloperCannotPatchRepo`: → 403.
  - `TestLeadManagesOwnTeamOnly`: lead de T1 puede `PUT` miembros en T1 (204) y no en T2 (403).
  - `TestSetupRequiresToken`: token incorrecto → 403; correcto → 200 con `action_url` de org cuando `org` viene; tras configurar → 409; `/setup/callback` tras configurar → 409.
  - `TestPRsEndpointFiltersByTeam`: mismo escenario que `TestVisibility` pero vía HTTP.
  - `TestPatchRepoEnableEnqueuesSync`: Enqueuer falso recibe `EnqueueSync` con la instalación del repo; fila de auditoría creada.
  - `TestHealthz`.
- [ ] **Step 2: Run** `go test ./internal/auth/... ./internal/api/...` — Expected: FAIL.
- [ ] **Step 3: Implement** router chi, handlers, `runServe`, `runWorker`.
- [ ] **Step 4: Run** `go test ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat: autenticación, RBAC, API REST y setup de la App"`

### Task 11: Web — base, sesión, setup y Dashboard

**Files:**
- Create: `web/` (Vite vue-ts), `web/src/api.ts`, `web/src/stores/session.ts`, `web/src/stores/prs.ts`, `web/src/router.ts`, `web/src/views/{SetupView,LoginView,DashboardView}.vue`, `web/src/components/{AppShell,KpiCard,FilterTabs,PrCard}.vue`, `web/src/status.ts`, `web/src/styles.css`
- Test: `web/src/status.test.ts`, `web/src/components/PrCard.test.ts`, `web/src/views/DashboardView.test.ts`

**Interfaces:**
- Consumes: rutas de Task 10 (JSON snake_case).
- Produces:
  ```ts
  // api.ts — fetch con credentials:'same-origin'; 401 → router a /login
  export type PRView = { id:number; repo:string; number:number; title:string; author_login:string; html_url:string; qa_status:QAStatus; group:''|'in_review'|'blocked'|'ready'; validations:{passed:number; total:number}; updated_at:string }
  export const api = { me, stats, prs(group?), repos, patchRepo, teams, createTeam, setMember, removeMember, users, setupStatus, setupManifest }
  export function subscribeEvents(onEvent:(e:{type:string; id:string})=>void): () => void // EventSource('/api/events'), reconexión nativa
  // status.ts
  export function statusLabel(s: QAStatus): { tag: string; action: string; tone: 'ok'|'busy'|'bad'|'muted' }
  ```
  Copy de `statusLabel` (del mockup del usuario): `ready_for_human` → "Listo para ti" / "Pendiente de aprobación" / ok; `pending` y `reviewing` → "Revisando IA" / "Análisis en curso" / busy; `waiting_ci` → "Esperando CI" / "Análisis en curso" / busy; `changes_requested` → "Con defectos" / "Requiere corrección" / bad; `inconclusive` → "Inconcluso" / "Requiere atención" / bad; `awaiting_approval` → "Esperando aprobación" / "Requiere aprobación" / bad; `paused` → "Pausado" / "Pausado" / muted; `draft` → "Borrador" / "—" / muted.
  - Rutas: `/setup` (si `setup_status.configured=false`, toda navegación va aquí; formulario token + org opcional; al recibir `action_url`/`manifest` envía un `<form method=post>` con campo `manifest` a GitHub), `/login` (botón "Entrar con GitHub" → `/auth/login`), `/` Dashboard, `/repos`, `/teams`.
  - Dashboard: cabecera "QA Control Center", subtítulo "Todos los repositorios", insignia "Servicio activo" (verde si SSE conectado, gris "Reconectando…" si no); 4 `KpiCard` con los textos del Global Constraints; `FilterTabs` Todos/En revisión/Bloqueados/Listos → `group` `''`/`in_review`/`blocked`/`ready`; encabezado "Pull Requests" + "N repositorios"; rejilla de `PrCard` (`repo #number`, etiqueta, título, línea "x/y validaciones correctas" solo si `total>0`, acción; enlace a `html_url`); estado vacío "No hay PRs abiertos en tus repositorios".
  - En cada evento SSE `pr.updated`/`repo.updated`, el store refetchea `stats` y `prs` (debounce 300 ms).
  - Estilos: tokens CSS en `:root` con modo oscuro por `prefers-color-scheme`; layout usable a 375 px de ancho.

- [ ] **Step 1: Write the failing tests** — `status.test.ts` (los 8 mapeos exactos); `PrCard.test.ts` (renderiza `backend-api #142`, "Listo para ti", "9/9 validaciones correctas" con `total=9`; oculta la línea con `total=0`); `DashboardView.test.ts` (con `api` mockeado: muestra los 4 KPIs con valores; clic en "Listos" llama `api.prs('ready')`; un evento simulado `pr.updated` provoca refetch).
- [ ] **Step 2: Run** `npm --prefix web run test -- --run` — Expected: FAIL.
- [ ] **Step 3: Implement** (proxy de Vite `/api`, `/auth`, `/setup`, `/webhooks` → `http://localhost:8080`).
- [ ] **Step 4: Run** `npm --prefix web run test -- --run && npm --prefix web run build` — Expected: PASS y `web/dist` generado.
- [ ] **Step 5: Commit** `git commit -m "feat(web): setup, login y dashboard en vivo"`

### Task 12: Web — Repositorios y Equipos

**Files:**
- Create: `web/src/views/{ReposView,TeamsView}.vue`
- Test: `web/src/views/ReposView.test.ts`, `web/src/views/TeamsView.test.ts`

**Interfaces:**
- Consumes: `api.repos/patchRepo/teams/createTeam/setMember/removeMember/users`, `useSessionStore().me`.
- Produces: vistas `/repos` y `/teams`.
  - Repos: tabla `full_name`, privado/público, interruptor "Revisión automática" (`enabled`), selector de equipo. Solo admin ve controles editables; otros, solo lectura.
  - Teams: lista de equipos con miembros y rol; admin crea equipos; admin o lead del equipo añade (selector de `users`) / cambia rol (`lead`/`qa`/`developer`, etiquetas "Lead", "QA", "Desarrollador") / quita miembros.

- [ ] **Step 1: Write the failing tests** — `ReposView`: como admin, activar el interruptor llama `patchRepo(id,{enabled:true})`; como developer, el interruptor está deshabilitado. `TeamsView`: como lead de T1 ve controles en T1 y no en T2; crear equipo solo visible para admin.
- [ ] **Step 2: Run** `npm --prefix web run test -- --run` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "feat(web): gestión de repositorios y equipos"`

### Task 13: Embebido, imagen Docker, Compose y guía de instalación

**Files:**
- Create: `internal/web/embed.go`, `internal/web/dist/.gitkeep`, `deploy/Dockerfile`, `deploy/docker-compose.yml`, `deploy/.env.example`, `docs/install.md`, `README.md`, `Makefile`
- Modify: `cmd/qa/main.go` (Static = `web.Handler()`), `.gitignore` (`web/node_modules`, `web/dist`, `internal/web/dist/*` salvo `.gitkeep`)
- Test: `internal/web/embed_test.go`

**Interfaces:**
- Produces: `func Handler() http.Handler` — sirve archivos de `dist`; rutas sin extensión que no existan → `index.html` (SPA); `/assets/*` con `Cache-Control: public, max-age=31536000, immutable`.
  - `Makefile`: `web` (build Vue y copia `web/dist` → `internal/web/dist`), `build` (web + `go build -o bin/qa ./cmd/qa`), `test` (`go test ./...` + tests web).
  - Dockerfile multi-stage: `node:24` construye web → `golang:1.27` compila con dist embebido (`CGO_ENABLED=0`) → runtime `debian:bookworm-slim` con `ca-certificates` y `git`, usuario no root `qa`, `ENTRYPOINT ["/usr/local/bin/qa"]`. (Node, `ocr` y `opencode` se añaden en Fase 2.)
  - Compose: `postgres` (postgres:17, volumen, healthcheck `pg_isready`), `app` (`command: ["serve"]`, puerto `8080:8080`, `depends_on` healthy, `env_file: .env`), `worker` (`command: ["worker"]`, volumen `opencode-data` reservado para Fase 2).
  - `.env.example`: `QA_DATABASE_URL`, `QA_PUBLIC_URL`, `QA_MASTER_KEY` (con comentario `openssl rand -base64 32`), `QA_WEBHOOKS_ENABLED`, `QA_RECONCILE_INTERVAL`, `POSTGRES_PASSWORD`.
  - `docs/install.md`: requisitos, `.env`, `docker compose up -d`, leer el setup token con `docker compose logs app | grep setup_token`, crear la App desde `/setup` (personal u org), instalarla en repos, entrar, habilitar repos y crear equipos, modo solo polling cuando no hay URL pública.

- [ ] **Step 1: Write the failing test** — `TestSPAFallback` (con un `fstest.MapFS` inyectable: `/teams` → contenido de `index.html`; `/assets/a.js` → el archivo con cabecera immutable; `/missing.js` → 404).
- [ ] **Step 2: Run** `go test ./internal/web/...` — Expected: FAIL.
- [ ] **Step 3: Implement** `embed.go` (`handlerFS(fs.FS)` interno para el test) y archivos de despliegue.
- [ ] **Step 4: Verify end-to-end**
  Run: `make test && cp deploy/.env.example deploy/.env` (rellenar `QA_MASTER_KEY` con `openssl rand -base64 32`) `&& docker compose -f deploy/docker-compose.yml up -d --build && sleep 10 && curl -s localhost:8080/healthz && curl -s localhost:8080/api/setup/status && docker compose -f deploy/docker-compose.yml logs app | grep setup_token`
  Expected: `{"status":"ok"}`, `{"configured":false,...}`, una línea con `setup_token`; `http://localhost:8080/` redirige a la pantalla de setup. Luego `docker compose -f deploy/docker-compose.yml down -v`.
- [ ] **Step 5: Commit** `git commit -m "feat: imagen única, docker compose y guía de instalación"`

---

## Siguientes planes

- **Fase 2 — Motor:** cola `review` en el worker, Node + `ocr` + `opencode` fijados en la imagen, perfiles LLM, pipeline `ocr delegate` + `opencode run` con agente de solo lectura, multi-pasada, esquema de hallazgos, fingerprints, aislamiento por usuario de sistema y redacción de secretos, comentario de resumen, comentarios en línea, check runs, resolución de hilos, aprobación de PRs externos y límites.
- **Fase 3 — Gate y ciclo humano:** Quality Gate con CI, `waiting_ci`, solicitud de revisión, disputas y comandos `/qa`, inconclusos y reintentos, políticas QA (equipo + override por repo), detalle de PR.
- **Fase 4 — Operación:** Workers, Analítica, Auditoría en la web, sincronización con GitHub Teams, endurecimiento.
