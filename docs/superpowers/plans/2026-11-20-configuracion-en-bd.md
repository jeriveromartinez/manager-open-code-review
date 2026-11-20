# Configuración en la base de datos Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Dejar `.env` en conexión y claves raíz, mover los 23 ajustes de comportamiento a PostgreSQL con una pantalla de configuración en el panel, y reducir el setup a pedir solo el nombre de la GitHub App.

**Architecture:** `config.Load` pasa a leer cuatro variables y rellenar el resto con defaults; tras abrir el store, `runServe`/`runWorker` cargan `app_settings`, las validan y sobrescriben el `config.Config` existente, de modo que ningún call site cambia. `internal/settings` es la única fuente de defaults, validación y metadatos (grupo y a quién hay que reiniciar). La API expone `GET/PUT /api/settings` solo para administradores y `SettingsView.vue` los edita por secciones. El setup deja de pedir token y organización: solo el nombre de la App, y la URL pública se detecta de la petición y se persiste.

**Tech Stack:** Go 1.27 (chi v5, GORM sobre pgxpool, goose), Vue 3 + Vite + TypeScript, Vitest + @vue/test-utils, PostgreSQL 17.

**Spec:** `docs/superpowers/specs/2026-11-20-configuracion-en-bd-design.md`

## Global Constraints

- `.env` queda exactamente en cuatro variables: `QA_DATABASE_URL`, `POSTGRES_PASSWORD`, `QA_MASTER_KEY`, `QA_WORKER_KEY`. Ninguna otra se lee para configurar el comportamiento.
- Los defaults de `internal/settings` son idénticos a los actuales: `:8080`, `true`, `5m`, `https://github.com`, `https://api.github.com`, `2`, `40`, `20m`, `45m`, `30m`, `500`, `https://github.com`, `/tmp/qa`, `ocr`, `opencode`, `:9090`, `10m`, `http://worker:9090`, `1m`, `30m`, `""`, `7`.
- Las variables legadas **no se importan**: gana la BD o el default. Cada una presente produce un `slog.Warn` al arrancar.
- Los ajustes se aplican al reiniciar; no hay recarga en caliente. El panel lo dice explícitamente.
- `public_url` puede quedar vacío: se detecta de la petición (`X-Forwarded-Proto`/`X-Forwarded-Host`, o TLS y `Host`) y se persiste.
- Validaciones: duraciones > 0, enteros > 0, URLs absolutas `http(s)` sin `/` final, `monitor_renotify_after >= monitor_interval`, `review_stale_after > review_timeout`.
- `QA_SETUP_TOKEN` es opcional: si está definido se exige; si no, el setup no pide token.
- Comentarios explican *por qué*, nunca *qué*. Sin dependencias nuevas. Sin reformatear código ajeno.
- Commits en inglés, Conventional Commits, asunto < 72 caracteres. Nunca commit a `main`.
- Suite completa solo local: `go vet ./...` y `go test -p 1 ./... -count=1` en Linux con Docker (`DOCKER_HOST=tcp://127.0.0.1:2375`), y `npm run test` + `npm run build` en `web/`.

---

## File Structure

| Fichero | Responsabilidad |
|---|---|
| `internal/store/migrations/00004_app_settings.sql` | tabla clave/valor |
| `internal/store/settings.go` | lectura y escritura de `app_settings` |
| `internal/settings/settings.go` | constantes, defaults, metadatos de grupo/reinicio |
| `internal/settings/apply.go` | validación, normalización y volcado sobre `config.Config` |
| `internal/config/config.go` | defaults + solo las cuatro esenciales; `Default()` |
| `cmd/qa/settings.go` | carga desde BD y aviso de variables legadas |
| `internal/api/settings.go` | `GET/PUT /api/settings` |
| `internal/api/publicurl.go` | detección de la URL pública desde la petición |
| `internal/api/setup.go` | setup por nombre, token opcional, persistencia de la URL |
| `web/src/views/SettingsView.vue` | formulario por secciones |
| `web/src/api.ts`, `web/src/router.ts`, `web/src/components/AppShell.vue` | tipos, ruta y entrada de navegación |
| `deploy/.env.example`, `deploy/docker-compose.yml`, `deploy/backup.sh`, `cmd/qa/monitor.go`, `README.md` | despliegue y perfiles opcionales |

---

## PR A — Configuración en la base de datos

### Task 1: Tabla `app_settings` y acceso desde el store

**Files:**
- Create: `internal/store/migrations/00004_app_settings.sql`
- Create: `internal/store/settings.go`
- Test: `internal/store/settings_test.go`

**Interfaces:**
- Produces: `func (s *Store) Settings(ctx context.Context) (map[string]string, error)`, `func (s *Store) SaveSettings(ctx context.Context, userID *uint, values map[string]string) error`

- [ ] **Step 1: Write the failing test**

`internal/store/settings_test.go`, siguiendo el patrón de los demás tests del paquete (`newTestStore(t)` abre Postgres con testcontainers y migra):

```go
package store

import (
	"context"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	values, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings on empty table: %v", err)
	}
	if len(values) != 0 {
		t.Errorf("Settings on empty table = %v, want empty", values)
	}

	if err := s.SaveSettings(ctx, nil, map[string]string{
		"review_timeout": "25m",
		"ocr_bin":        "ocr-dev",
	}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	values, err = s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if values["review_timeout"] != "25m" || values["ocr_bin"] != "ocr-dev" {
		t.Errorf("Settings = %v, want review_timeout=25m ocr_bin=ocr-dev", values)
	}
}

func TestSaveSettingsIsPartialAndRecordsUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	user := &User{GitHubID: 9001, Login: "admin", IsAdmin: true}
	if err := s.DB.WithContext(ctx).Create(user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.SaveSettings(ctx, nil, map[string]string{"ocr_bin": "ocr-a", "opencode_bin": "oc-a"}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if err := s.SaveSettings(ctx, &user.ID, map[string]string{"ocr_bin": "ocr-b"}); err != nil {
		t.Fatalf("SaveSettings partial: %v", err)
	}

	values, err := s.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if values["ocr_bin"] != "ocr-b" {
		t.Errorf("ocr_bin = %q, want ocr-b", values["ocr_bin"])
	}
	if values["opencode_bin"] != "oc-a" {
		t.Errorf("opencode_bin = %q, want oc-a: a partial write must keep the other keys", values["opencode_bin"])
	}

	var row settingRow
	if err := s.DB.WithContext(ctx).Where("key = ?", "ocr_bin").First(&row).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.UpdatedBy == nil || *row.UpdatedBy != user.ID {
		t.Errorf("updated_by = %v, want %d", row.UpdatedBy, user.ID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash -lc 'cd /mnt/e/Projects/manager-open-code-review/.worktrees/feat-db-settings; export PATH=/usr/local/go/bin:/usr/bin:/bin; export GOFLAGS=-buildvcs=false; export DOCKER_HOST=tcp://127.0.0.1:2375; go test ./internal/store/ -run "TestSettingsRoundTrip|TestSaveSettingsIsPartialAndRecordsUser" -count=1'
```

Expected: FAIL with `s.Settings undefined` / `s.SaveSettings undefined`.

- [ ] **Step 3: Write the migration**

`internal/store/migrations/00004_app_settings.sql`:

```sql
-- +goose Up
CREATE TABLE app_settings (
    key text PRIMARY KEY,
    value text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by bigint REFERENCES users (id) ON DELETE SET NULL
);

-- +goose Down
DROP TABLE app_settings;
```

- [ ] **Step 4: Write minimal implementation**

`internal/store/settings.go`:

```go
package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

type settingRow struct {
	Key       string    `gorm:"column:key;primaryKey"`
	Value     string    `gorm:"column:value;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
	UpdatedBy *uint     `gorm:"column:updated_by"`
}

func (settingRow) TableName() string { return "app_settings" }

// Settings returns the stored overrides. Keys absent from the map still use
// their defaults from internal/settings, so an empty table is a valid state.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	var rows []settingRow
	if err := s.DB.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list app settings: %w", err)
	}
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	return values, nil
}

// SaveSettings upserts only the given keys, so two administrators editing
// different sections do not overwrite each other.
func (s *Store) SaveSettings(ctx context.Context, userID *uint, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	rows := make([]settingRow, 0, len(values))
	now := time.Now().UTC()
	for key, value := range values {
		rows = append(rows, settingRow{Key: key, Value: value, UpdatedAt: now, UpdatedBy: userID})
	}
	err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at", "updated_by"}),
	}).Create(&rows).Error
	if err != nil {
		return fmt.Errorf("save app settings: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run test to verify it passes**

Run the command from Step 2. Expected: `ok ... internal/store`.

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrations/00004_app_settings.sql internal/store/settings.go internal/store/settings_test.go
git commit -m "feat(store): persist settings in app_settings"
```

---

### Task 2: Paquete `internal/settings` con defaults, validación y volcado

**Files:**
- Create: `internal/settings/settings.go`
- Create: `internal/settings/apply.go`
- Test: `internal/settings/apply_test.go`
- Modify: `internal/config/config.go` (añadir `BackupRetention` y `Default()`)

**Interfaces:**
- Consumes: `config.Config` con todos los campos actuales más `BackupRetention int`.
- Produces: `settings.Fields() []Field`, `settings.Defaults() map[string]string`, `settings.ToMap(config.Config) map[string]string`, `settings.Apply(*config.Config, map[string]string) error`, `settings.KeyError{Key, Msg}`, y las constantes `settings.KeyPublicURL` … `settings.KeyBackupRetention`.

- [ ] **Step 1: Write the failing test**

`internal/settings/apply_test.go`:

```go
package settings

import (
	"errors"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

func TestApplyRejectsInvalidValuesByKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		raw  string
		want string
	}{
		{"zero duration", KeyReviewTimeout, "0s", KeyReviewTimeout},
		{"garbage duration", KeyReconcileInterval, "pronto", KeyReconcileInterval},
		{"negative int", KeyReviewConcurrency, "-1", KeyReviewConcurrency},
		{"garbage int", KeyFetchDepthMax, "muchos", KeyFetchDepthMax},
		{"bad bool", KeyWebhooksEnabled, "si", KeyWebhooksEnabled},
		{"relative url", KeyPublicURL, "/solo-ruta", KeyPublicURL},
		{"empty bin", KeyOCRBin, "  ", KeyOCRBin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			err := Apply(&cfg, map[string]string{tc.key: tc.raw})
			var keyErr *KeyError
			if !errors.As(err, &keyErr) {
				t.Fatalf("Apply(%s=%q) = %v, want *KeyError", tc.key, tc.raw, err)
			}
			if keyErr.Key != tc.want {
				t.Errorf("KeyError.Key = %q, want %q", keyErr.Key, tc.want)
			}
		})
	}
}

func TestApplyRejectsIncoherentPairs(t *testing.T) {
	cfg := config.Default()
	err := Apply(&cfg, map[string]string{KeyReviewTimeout: "60m"})
	var keyErr *KeyError
	if !errors.As(err, &keyErr) || keyErr.Key != KeyReviewStaleAfter {
		t.Fatalf("timeout 60m with stale 45m = %v, want KeyError on %s", err, KeyReviewStaleAfter)
	}

	cfg = config.Default()
	err = Apply(&cfg, map[string]string{KeyMonitorRenotifyAfter: "30s", KeyMonitorInterval: "1m"})
	if !errors.As(err, &keyErr) || keyErr.Key != KeyMonitorRenotifyAfter {
		t.Fatalf("renotify < interval = %v, want KeyError on %s", err, KeyMonitorRenotifyAfter)
	}
}

func TestApplySetsEveryField(t *testing.T) {
	cfg := config.Default()
	err := Apply(&cfg, map[string]string{
		KeyPublicURL:            "https://qa.example.com/",
		KeyWebhooksEnabled:      "false",
		KeyReconcileInterval:    "2m",
		KeyGitHubURL:            "https://ghe.example.com/",
		KeyGitHubAPIURL:         "https://ghe.example.com/api/v3/",
		KeyListenAddr:           ":9091",
		KeyReviewConcurrency:    "4",
		KeyReviewAgentSteps:     "60",
		KeyReviewTimeout:        "30m",
		KeyReviewStaleAfter:     "90m",
		KeyLLMHealthInterval:    "10m",
		KeyFetchDepthMax:        "900",
		KeyGitBaseURL:           "https://git.example.com/",
		KeyWorkspaceDir:         "/var/qa",
		KeyOCRBin:               "/usr/local/bin/ocr",
		KeyOpencodeBin:          "/usr/local/bin/opencode",
		KeyWorkerHealthAddr:     "127.0.0.1:9092",
		KeyWorkerQueueStale:     "20m",
		KeyWorkerHealthURL:      "http://worker:9090/",
		KeyMonitorInterval:      "30s",
		KeyMonitorRenotifyAfter: "5m",
		KeyAlertWebhookURL:      "https://hooks.example.com/qa",
		KeyBackupRetention:      "14",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if cfg.PublicURL != "https://qa.example.com" {
		t.Errorf("PublicURL = %q", cfg.PublicURL)
	}
	if cfg.WebhooksEnabled {
		t.Error("WebhooksEnabled = true, want false")
	}
	if cfg.ReconcileInterval != 2*time.Minute || cfg.ReviewTimeout != 30*time.Minute ||
		cfg.ReviewStaleAfter != 90*time.Minute || cfg.LLMHealthInterval != 10*time.Minute ||
		cfg.WorkerQueueStale != 20*time.Minute || cfg.MonitorRenotifyAfter != 5*time.Minute {
		t.Error("one of the durations was not applied")
	}
	if cfg.ReviewConcurrency != 4 || cfg.ReviewAgentSteps != 60 || cfg.FetchDepthMax != 900 || cfg.BackupRetention != 14 {
		t.Error("one of the integers was not applied")
	}
	if cfg.WorkerHealthURL != "http://worker:9090" || cfg.GitHubURL != "https://ghe.example.com" ||
		cfg.GitHubAPIURL != "https://ghe.example.com/api/v3" || cfg.GitBaseURL != "https://git.example.com" {
		t.Error("one of the URLs was not trimmed")
	}
	if cfg.WorkspaceDir != "/var/qa" || cfg.OCRBin != "/usr/local/bin/ocr" ||
		cfg.OpencodeBin != "/usr/local/bin/opencode" || cfg.ListenAddr != ":9091" ||
		cfg.WorkerHealthAddr != "127.0.0.1:9092" || cfg.AlertWebhookURL != "https://hooks.example.com/qa" {
		t.Error("one of the strings was not applied")
	}
}

func TestDefaultsAndFieldsAgree(t *testing.T) {
	defaults := Defaults()
	fields := Fields()
	if len(defaults) != len(fields) {
		t.Fatalf("Defaults() has %d keys, Fields() has %d", len(defaults), len(fields))
	}
	for _, field := range fields {
		if _, ok := defaults[field.Key]; !ok {
			t.Errorf("field %q missing from Defaults()", field.Key)
		}
		if field.Restart != "" && field.Restart != "app" && field.Restart != "worker" {
			t.Errorf("field %q has invalid restart %q", field.Key, field.Restart)
		}
	}
}

func TestToMapRoundTripsThroughApply(t *testing.T) {
	cfg := config.Default()
	values := ToMap(cfg)
	other := config.Config{}
	if err := Apply(&other, values); err != nil {
		t.Fatalf("Apply(ToMap(default)): %v", err)
	}
	if ToMap(other)["review_timeout"] != "20m0s" && ToMap(other)["review_timeout"] != "20m" {
		t.Errorf("ToMap round trip lost review_timeout: %q", ToMap(other)["review_timeout"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd .worktrees/feat-db-settings && go test ./internal/settings/
```

Expected: FAIL, `no non-test Go files in .../internal/settings`.

- [ ] **Step 3: Add `BackupRetention` and `Default()` to config**

En `internal/config/config.go`, añadir el campo al struct:

```go
	BackupRetention      int           // backup_retention, default 7 (lo lee deploy/backup.sh)
```

Extraer los defaults a `Default()` y usarlo desde `Load`:

```go
// Default returns the configuration used when nothing is configured yet. Load
// starts from it and the database overrides only what an administrator saved.
func Default() Config {
	return Config{
		ListenAddr:           ":8080",
		WebhooksEnabled:      true,
		ReconcileInterval:    5 * time.Minute,
		GitHubURL:            "https://github.com",
		GitHubAPIURL:         "https://api.github.com",
		ReviewConcurrency:    2,
		ReviewAgentSteps:     40,
		ReviewTimeout:        20 * time.Minute,
		ReviewStaleAfter:     45 * time.Minute,
		LLMHealthInterval:    30 * time.Minute,
		WorkspaceDir:         "/tmp/qa",
		WorkerHealthAddr:     ":9090",
		WorkerQueueStale:     10 * time.Minute,
		OCRBin:               "ocr",
		OpencodeBin:          "opencode",
		FetchDepthMax:        500,
		GitBaseURL:           "https://github.com",
		WorkerHealthURL:      "http://worker:9090",
		MonitorInterval:      time.Minute,
		MonitorRenotifyAfter: 30 * time.Minute,
		BackupRetention:      7,
	}
}
```

- [ ] **Step 4: Write `internal/settings/settings.go`**

Constantes de clave, `Group`, `Field`, `Fields()`, `Defaults()` y `ToMap()`:

```go
package settings

import (
	"strconv"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

const (
	KeyPublicURL            = "public_url"
	KeyWebhooksEnabled      = "webhooks_enabled"
	KeyReconcileInterval    = "reconcile_interval"
	KeyGitHubURL            = "github_url"
	KeyGitHubAPIURL         = "github_api_url"
	KeyListenAddr           = "listen_addr"
	KeyReviewConcurrency    = "review_concurrency"
	KeyReviewAgentSteps     = "review_agent_steps"
	KeyReviewTimeout        = "review_timeout"
	KeyReviewStaleAfter     = "review_stale_after"
	KeyLLMHealthInterval    = "llm_health_interval"
	KeyFetchDepthMax        = "fetch_depth_max"
	KeyGitBaseURL           = "git_base_url"
	KeyWorkspaceDir         = "workspace_dir"
	KeyOCRBin               = "ocr_bin"
	KeyOpencodeBin          = "opencode_bin"
	KeyWorkerHealthAddr     = "worker_health_addr"
	KeyWorkerQueueStale     = "worker_queue_stale_after"
	KeyWorkerHealthURL      = "worker_health_url"
	KeyMonitorInterval      = "monitor_interval"
	KeyMonitorRenotifyAfter = "monitor_renotify_after"
	KeyAlertWebhookURL      = "alert_webhook_url"
	KeyBackupRetention      = "backup_retention"
)

type Group string

const (
	GroupGeneral Group = "general"
	GroupReview  Group = "review"
	GroupWorker  Group = "worker"
	GroupAlerts  Group = "alerts"
	GroupBackups Group = "backups"
)

// Field tells the panel how to group a key and what has to restart for a new
// value to take effect. The UI renders explicit inputs; this is metadata only.
type Field struct {
	Key     string `json:"key"`
	Group   Group  `json:"group"`
	Default string `json:"default"`
	Restart string `json:"restart"` // "", "app" o "worker"
	Secret  bool   `json:"secret"`
}

func Fields() []Field {
	return []Field{
		{KeyPublicURL, GroupGeneral, "", "app", false},
		{KeyWebhooksEnabled, GroupGeneral, "true", "app", false},
		{KeyReconcileInterval, GroupGeneral, "5m", "app", false},
		{KeyGitHubURL, GroupGeneral, "https://github.com", "app", false},
		{KeyGitHubAPIURL, GroupGeneral, "https://api.github.com", "app", false},
		{KeyListenAddr, GroupGeneral, ":8080", "app", false},
		{KeyReviewConcurrency, GroupReview, "2", "worker", false},
		{KeyReviewAgentSteps, GroupReview, "40", "worker", false},
		{KeyReviewTimeout, GroupReview, "20m", "worker", false},
		{KeyReviewStaleAfter, GroupReview, "45m", "worker", false},
		{KeyLLMHealthInterval, GroupReview, "30m", "worker", false},
		{KeyFetchDepthMax, GroupReview, "500", "worker", false},
		{KeyGitBaseURL, GroupReview, "https://github.com", "worker", false},
		{KeyWorkspaceDir, GroupWorker, "/tmp/qa", "worker", false},
		{KeyOCRBin, GroupWorker, "ocr", "worker", false},
		{KeyOpencodeBin, GroupWorker, "opencode", "worker", false},
		{KeyWorkerHealthAddr, GroupWorker, ":9090", "worker", false},
		{KeyWorkerQueueStale, GroupWorker, "10m", "worker", false},
		{KeyWorkerHealthURL, GroupAlerts, "http://worker:9090", "worker", false},
		{KeyMonitorInterval, GroupAlerts, "1m", "worker", false},
		{KeyMonitorRenotifyAfter, GroupAlerts, "30m", "worker", false},
		{KeyAlertWebhookURL, GroupAlerts, "", "worker", true},
		{KeyBackupRetention, GroupBackups, "7", "worker", false},
	}
}

func Defaults() map[string]string {
	out := make(map[string]string, 23)
	for _, field := range Fields() {
		out[field.Key] = field.Default
	}
	return out
}

// ToMap serialises the effective configuration, so the panel shows what the
// process actually uses rather than what is stored.
func ToMap(cfg config.Config) map[string]string {
	return map[string]string{
		KeyPublicURL:            cfg.PublicURL,
		KeyWebhooksEnabled:      strconv.FormatBool(cfg.WebhooksEnabled),
		KeyReconcileInterval:    cfg.ReconcileInterval.String(),
		KeyGitHubURL:            cfg.GitHubURL,
		KeyGitHubAPIURL:         cfg.GitHubAPIURL,
		KeyListenAddr:           cfg.ListenAddr,
		KeyReviewConcurrency:    strconv.Itoa(cfg.ReviewConcurrency),
		KeyReviewAgentSteps:     strconv.Itoa(cfg.ReviewAgentSteps),
		KeyReviewTimeout:        cfg.ReviewTimeout.String(),
		KeyReviewStaleAfter:     cfg.ReviewStaleAfter.String(),
		KeyLLMHealthInterval:    cfg.LLMHealthInterval.String(),
		KeyFetchDepthMax:        strconv.Itoa(cfg.FetchDepthMax),
		KeyGitBaseURL:           cfg.GitBaseURL,
		KeyWorkspaceDir:         cfg.WorkspaceDir,
		KeyOCRBin:               cfg.OCRBin,
		KeyOpencodeBin:          cfg.OpencodeBin,
		KeyWorkerHealthAddr:     cfg.WorkerHealthAddr,
		KeyWorkerQueueStale:     cfg.WorkerQueueStale.String(),
		KeyWorkerHealthURL:      cfg.WorkerHealthURL,
		KeyMonitorInterval:      cfg.MonitorInterval.String(),
		KeyMonitorRenotifyAfter: cfg.MonitorRenotifyAfter.String(),
		KeyAlertWebhookURL:      cfg.AlertWebhookURL,
		KeyBackupRetention:      strconv.Itoa(cfg.BackupRetention),
	}
}
```

- [ ] **Step 5: Write `internal/settings/apply.go`**

```go
package settings

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
)

// KeyError names the setting that made a save fail, so the panel can point at
// the field instead of showing a generic message.
type KeyError struct {
	Key string
	Msg string
}

func (e *KeyError) Error() string { return e.Key + ": " + e.Msg }

// Apply validates and writes the given keys onto cfg. Keys absent from values
// keep whatever cfg already had, which is how a partial save works.
func Apply(cfg *config.Config, values map[string]string) error {
	for key, raw := range values {
		if err := applyOne(cfg, key, raw); err != nil {
			return err
		}
	}
	return validatePairs(cfg)
}

func applyOne(cfg *config.Config, key, raw string) error {
	switch key {
	case KeyPublicURL:
		return setURL(&cfg.PublicURL, key, raw, true)
	case KeyGitHubURL:
		return setURL(&cfg.GitHubURL, key, raw, false)
	case KeyGitHubAPIURL:
		return setURL(&cfg.GitHubAPIURL, key, raw, false)
	case KeyGitBaseURL:
		return setURL(&cfg.GitBaseURL, key, raw, false)
	case KeyWorkerHealthURL:
		return setURL(&cfg.WorkerHealthURL, key, raw, false)
	case KeyAlertWebhookURL:
		return setURL(&cfg.AlertWebhookURL, key, raw, true)
	case KeyListenAddr:
		return setString(&cfg.ListenAddr, key, raw)
	case KeyWorkspaceDir:
		return setString(&cfg.WorkspaceDir, key, raw)
	case KeyOCRBin:
		return setString(&cfg.OCRBin, key, raw)
	case KeyOpencodeBin:
		return setString(&cfg.OpencodeBin, key, raw)
	case KeyWorkerHealthAddr:
		return setString(&cfg.WorkerHealthAddr, key, raw)
	case KeyWebhooksEnabled:
		v, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return &KeyError{key, "must be true or false"}
		}
		cfg.WebhooksEnabled = v
		return nil
	case KeyReconcileInterval:
		return setDuration(&cfg.ReconcileInterval, key, raw)
	case KeyReviewTimeout:
		return setDuration(&cfg.ReviewTimeout, key, raw)
	case KeyReviewStaleAfter:
		return setDuration(&cfg.ReviewStaleAfter, key, raw)
	case KeyLLMHealthInterval:
		return setDuration(&cfg.LLMHealthInterval, key, raw)
	case KeyWorkerQueueStale:
		return setDuration(&cfg.WorkerQueueStale, key, raw)
	case KeyMonitorInterval:
		return setDuration(&cfg.MonitorInterval, key, raw)
	case KeyMonitorRenotifyAfter:
		return setDuration(&cfg.MonitorRenotifyAfter, key, raw)
	case KeyReviewConcurrency:
		return setInt(&cfg.ReviewConcurrency, key, raw)
	case KeyReviewAgentSteps:
		return setInt(&cfg.ReviewAgentSteps, key, raw)
	case KeyFetchDepthMax:
		return setInt(&cfg.FetchDepthMax, key, raw)
	case KeyBackupRetention:
		return setInt(&cfg.BackupRetention, key, raw)
	default:
		return &KeyError{key, "unknown setting"}
	}
}

func setString(dst *string, key, raw string) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		return &KeyError{key, "must not be empty"}
	}
	*dst = v
	return nil
}

// setURL trims the trailing slash because every consumer concatenates paths
// onto it; an empty value is only accepted for keys allowed to be unset.
func setURL(dst *string, key, raw string, optional bool) error {
	v := strings.TrimRight(strings.TrimSpace(raw), "/")
	if v == "" {
		if optional {
			*dst = ""
			return nil
		}
		return &KeyError{key, "must not be empty"}
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &KeyError{key, "must be an absolute http(s) URL"}
	}
	*dst = v
	return nil
}

func setDuration(dst *time.Duration, key, raw string) error {
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return &KeyError{key, "must be a duration such as 30s, 5m or 2h"}
	}
	if d <= 0 {
		return &KeyError{key, "must be greater than zero"}
	}
	*dst = d
	return nil
}

func setInt(dst *int, key, raw string) error {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return &KeyError{key, "must be an integer"}
	}
	if n <= 0 {
		return &KeyError{key, "must be greater than zero"}
	}
	*dst = n
	return nil
}

// validatePairs runs after every write because a partial save can still make
// the resulting configuration incoherent.
func validatePairs(cfg *config.Config) error {
	if cfg.ReviewStaleAfter <= cfg.ReviewTimeout {
		return &KeyError{KeyReviewStaleAfter, fmt.Sprintf(
			"must exceed review_timeout (%s) or the watchdog kills healthy runs", cfg.ReviewTimeout)}
	}
	if cfg.MonitorRenotifyAfter < cfg.MonitorInterval {
		return &KeyError{KeyMonitorRenotifyAfter, "must be greater than or equal to monitor_interval"}
	}
	return nil
}
```

- [ ] **Step 6: Run test to verify it passes**

```bash
cd .worktrees/feat-db-settings && go test ./internal/settings/ ./internal/config/
```

Expected: `ok` en `internal/settings`. Si `internal/config` falla por el nuevo campo, arreglarlo en la Task 3 (los tests de env siguen pasando porque aún no se han tocado).

- [ ] **Step 7: Commit**

```bash
git add internal/settings internal/config/config.go
git commit -m "feat(settings): add defaults, validation and config apply"
```

---

### Task 3: `config.Load` se queda en las cuatro esenciales

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `config.Default()` de la Task 2.
- Produces: `config.Load(getenv)` que solo lee `QA_DATABASE_URL`, `QA_PUBLIC_URL`, `QA_MASTER_KEY` y `QA_WORKER_KEY`; `config.LegacyEnvKeys() []string` con las 22 variables de comportamiento que ya no se leen.

`QA_PUBLIC_URL` se queda una tarea más: el setup que la deduce y la persiste llega en la Task 8, y quitarla antes dejaría a una instalación existente sin URL de callback. `LoadMonitor` sigue vivo hasta la Task 7, que es donde deja de usarse.

- [ ] **Step 1: Rewrite the failing test**

Sustituir en `internal/config/config_test.go` los tests de override por uno que afirme lo contrario, y añadir el listado de legadas:

```go
func TestLoadOnlyReadsTheEssentials(t *testing.T) {
	cfg, err := Load(func(key string) string {
		switch key {
		case "QA_DATABASE_URL":
			return "postgres://x/y"
		case "QA_WORKER_KEY":
			return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("w"), 32))
		case "QA_MASTER_KEY":
			return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("m"), 32))
		case "QA_REVIEW_TIMEOUT":
			return "99m"
		case "QA_PUBLIC_URL":
			return "https://ignored.example.com"
		case "QA_OCR_BIN":
			return "/nope"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewTimeout != 20*time.Minute {
		t.Errorf("ReviewTimeout = %s, want the 20m default: the environment must be ignored", cfg.ReviewTimeout)
	}
	if cfg.PublicURL != "https://ignored.example.com" {
		t.Errorf("PublicURL = %q, want the provided value: it is still read until the setup detects it", cfg.PublicURL)
	}
	if cfg.OCRBin != "ocr" {
		t.Errorf("OCRBin = %q, want the default", cfg.OCRBin)
	}
}

func TestLegacyEnvKeysCoverEveryMovedSetting(t *testing.T) {
	keys := LegacyEnvKeys()
	if len(keys) != 22 {
		t.Fatalf("LegacyEnvKeys() has %d entries, want 22", len(keys))
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			t.Errorf("duplicate legacy key %q", key)
		}
		seen[key] = true
	}
	for _, want := range []string{"QA_REVIEW_TIMEOUT", "QA_MONITOR_INTERVAL", "QA_WORKER_HEALTH_ADDR", "QA_BACKUP_RETENTION"} {
		if !seen[want] {
			t.Errorf("LegacyEnvKeys() missing %q", want)
		}
	}
	if seen["QA_PUBLIC_URL"] {
		t.Error("QA_PUBLIC_URL is still read until the setup detects it")
	}
	if seen["QA_DATABASE_URL"] || seen["QA_WORKER_KEY"] {
		t.Error("the essentials are not legacy")
	}
}
```

Conservar sin cambios los tests de las claves (`TestLoadBadMasterKey`, `TestLoadMissingWorkerKey`, `TestLoadBadWorkerKey`, `TestRequireMasterKey`) y `TestLoadDefaults`, ajustando solo las expectativas que dependían de `PublicURL` requerido.

- [ ] **Step 2: Run test to verify it fails**

```bash
cd .worktrees/feat-db-settings && go test ./internal/config/
```

Expected: FAIL, `undefined: LegacyEnvKeys` y `ReviewTimeout = 99m0s, want the 20m default`.

- [ ] **Step 3: Rewrite `Load`**

Eliminar todas las ramas `getenv("QA_...")` salvo las tres esenciales, y dejar:

```go
// LegacyEnvKeys lists the variables that used to configure behaviour and are
// now ignored: they live in the database and are edited from the panel.
func LegacyEnvKeys() []string {
	return []string{
		"QA_LISTEN_ADDR", "QA_WEBHOOKS_ENABLED", "QA_RECONCILE_INTERVAL",
		"QA_GITHUB_URL", "QA_GITHUB_API_URL",
		"QA_REVIEW_CONCURRENCY", "QA_REVIEW_AGENT_STEPS", "QA_REVIEW_TIMEOUT",
		"QA_REVIEW_STALE_AFTER", "QA_LLM_HEALTH_INTERVAL", "QA_FETCH_DEPTH_MAX", "QA_GIT_BASE_URL",
		"QA_WORKSPACE_DIR", "QA_OCR_BIN", "QA_OPENCODE_BIN",
		"QA_WORKER_HEALTH_ADDR", "QA_WORKER_QUEUE_STALE_AFTER",
		"QA_WORKER_HEALTH_URL", "QA_MONITOR_INTERVAL", "QA_MONITOR_RENOTIFY_AFTER", "QA_ALERT_WEBHOOK_URL",
		"QA_BACKUP_RETENTION",
	}
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Default()

	cfg.DatabaseURL = strings.TrimSpace(getenv("QA_DATABASE_URL"))
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("QA_DATABASE_URL is required")
	}

	// QA_PUBLIC_URL disappears once the setup can detect it (Task 8); until
	// then an existing installation must keep its callback URL.
	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(getenv("QA_PUBLIC_URL")), "/")
	if cfg.PublicURL == "" {
		return Config{}, errors.New("QA_PUBLIC_URL is required")
	}

	if raw := strings.TrimSpace(getenv("QA_MASTER_KEY")); raw != "" {
		key, err := decodeKey(raw, "QA_MASTER_KEY")
		if err != nil {
			return Config{}, err
		}
		cfg.MasterKey = key
	}

	rawWorkerKey := strings.TrimSpace(getenv("QA_WORKER_KEY"))
	if rawWorkerKey == "" {
		return Config{}, errors.New("QA_WORKER_KEY is required")
	}
	workerKey, err := decodeKey(rawWorkerKey, "QA_WORKER_KEY")
	if err != nil {
		return Config{}, err
	}
	cfg.WorkerKey = workerKey

	return cfg, nil
}
```

(`decodeKey` es el helper que hoy valida base64 y que decodifique exactamente 32 bytes; extraerlo del bloque actual sin cambiar su comportamiento. `LegacyEnvKeys` tiene 23 entradas: las 23 claves de `app_settings`.)

- [ ] **Step 4: Leave `LoadMonitor` alone for now**

`config.LoadMonitor` y `loadMonitorSettings` siguen compilando y con sus tests intactos: `cmd/qa/monitor.go` todavía los usa. La Task 7 los retira junto con `internal/config/monitor_test.go`, cuando el monitor ya lee de la base.

```bash
grep -rn "LoadMonitor" --include=*.go . | grep -v node_modules
```

Expected: solo `internal/config` y `cmd/qa/monitor.go`.

- [ ] **Step 5: Run test to verify it passes**

```bash
cd .worktrees/feat-db-settings && go build ./... && go vet ./... && go test ./internal/config/ ./internal/settings/
```

Expected: build limpio y ambos paquetes `ok`. Cada tarea deja la rama compilando.

- [ ] **Step 6: Commit**

```bash
git add internal/config && git commit -m "refactor(config): read only the essentials from the environment"
```

---

### Task 4: Cargar los ajustes al arrancar app y worker

**Files:**
- Create: `cmd/qa/settings.go`
- Test: `cmd/qa/settings_test.go`
- Modify: `cmd/qa/main.go` (`runServe`, `runWorker`)

**Interfaces:**
- Consumes: `store.Store.Settings`, `settings.Apply`, `config.Default`, `config.LegacyEnvKeys`.
- Produces: `func loadSettings(ctx context.Context, st *store.Store, cfg *config.Config) error`, `func legacyEnvSet(getenv func(string) string) []string`.

- [ ] **Step 1: Write the failing test**

`cmd/qa/settings_test.go`:

```go
package main

import "testing"

func TestLegacyEnvSetReportsOnlyPresentVariables(t *testing.T) {
	env := map[string]string{
		"QA_REVIEW_TIMEOUT": "40m",
		"QA_OCR_BIN":        "/usr/bin/ocr",
		"QA_DATABASE_URL":   "postgres://x/y",
	}
	got := legacyEnvSet(func(key string) string { return env[key] })
	if len(got) != 2 {
		t.Fatalf("legacyEnvSet = %v, want the two legacy variables", got)
	}
	for _, key := range got {
		if key == "QA_DATABASE_URL" {
			t.Error("QA_DATABASE_URL is essential, not legacy")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd .worktrees/feat-db-settings && go test ./cmd/qa/ -run TestLegacyEnvSet -count=1
```

Expected: FAIL, `undefined: legacyEnvSet`.

- [ ] **Step 3: Write `cmd/qa/settings.go`**

```go
package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jeriveromartinez/qa-control-center/internal/config"
	"github.com/jeriveromartinez/qa-control-center/internal/settings"
	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

// legacyEnvSet returns the variables that used to configure behaviour and are
// now ignored. Logging them at startup is the only signal an operator gets
// that their .env no longer does anything.
func legacyEnvSet(getenv func(string) string) []string {
	var found []string
	for _, key := range config.LegacyEnvKeys() {
		if getenv(key) != "" {
			found = append(found, key)
		}
	}
	return found
}

// loadSettings applies the stored overrides on top of the defaults.
func loadSettings(ctx context.Context, st *store.Store, cfg *config.Config) error {
	values, err := st.Settings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if err := settings.Apply(cfg, values); err != nil {
		return fmt.Errorf("apply stored settings: %w", err)
	}
	if legacy := legacyEnvSet(getenvOrEmpty); len(legacy) > 0 {
		slog.Warn("these variables are ignored; configure them in the panel",
			"variables", legacy)
	}
	return nil
}
```

Con `getenvOrEmpty` como envoltorio de `os.Getenv` en el mismo fichero:

```go
func getenvOrEmpty(key string) string { return os.Getenv(key) }
```

- [ ] **Step 4: Wire it into `runServe` and `runWorker`**

En `runServe`, después de `st.Migrate(ctx)` y antes de construir `apps`, `authn`, `webhooks` y el router:

```go
	if err := loadSettings(ctx, st, &cfg); err != nil {
		return fmt.Errorf("load config: %w", err)
	}
```

Lo mismo en `runWorker`, después de `store.Open` y antes de `checkBinary`/`review.Runner`.

- [ ] **Step 5: Run the full Go suite**

```bash
cd .worktrees/feat-db-settings && go build ./... && go vet ./...
MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash -lc 'cd /mnt/e/Projects/manager-open-code-review/.worktrees/feat-db-settings; export PATH=/usr/local/go/bin:/usr/bin:/bin; export GOFLAGS=-buildvcs=false; export DOCKER_HOST=tcp://127.0.0.1:2375; go vet ./... && go test -p 1 ./... -count=1'
```

Expected: todo `ok` salvo `cmd/qa`, que aún referencia `LoadMonitor` en `monitor.go`; se arregla en la Task 6. Si el resto no está verde, no continuar.

- [ ] **Step 6: Commit**

```bash
git add cmd/qa && git commit -m "feat(worker): load runtime settings from the database"
```

---

### Task 5: `GET/PUT /api/settings` y detección de la URL pública

**Files:**
- Create: `internal/api/settings.go`
- Create: `internal/api/publicurl.go`
- Test: `internal/api/settings_test.go`
- Modify: `internal/api/router.go`

**Interfaces:**
- Consumes: `store.Settings`, `store.SaveSettings`, `settings.Fields`, `settings.ToMap`, `settings.Apply`, `auth.RequireAdmin`, `s.Cfg`, `s.Store`.
- Produces: `func (s *server) requestPublicURL(r *http.Request) string`, `GET /api/settings`, `PUT /api/settings`.

- [ ] **Step 1: Write the failing tests**

`internal/api/settings_test.go`, con los helpers existentes (`newSecureFixture`, `f.request`, `assertStatus`, `decodeJSON`). El paquete es `api_test`, así que el fichero abre con:

```go
package api_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)
```

```go
func TestSettingsRequireAdmin(t *testing.T) {
	f := newSecureFixture(t)
	lead := f.user("lead", false)
	cookie := f.session(lead)
	assertStatus(t, f.request(http.MethodGet, "/api/settings", nil, cookie), http.StatusForbidden)
	assertStatus(t, f.request(http.MethodPut, "/api/settings", map[string]any{"values": map[string]string{"ocr_bin": "x"}}, cookie), http.StatusForbidden)
	assertStatus(t, f.request(http.MethodGet, "/api/settings", nil, nil), http.StatusUnauthorized)
}

func TestSettingsReturnsDefaultsAndMetadata(t *testing.T) {
	f := newSecureFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodGet, "/api/settings", nil, cookie)
	assertStatus(t, rec, http.StatusOK)

	var body struct {
		Values map[string]string `json:"values"`
		Fields []struct {
			Key     string `json:"key"`
			Group   string `json:"group"`
			Restart string `json:"restart"`
		} `json:"fields"`
	}
	decodeJSON(t, rec, &body)
	if body.Values["review_timeout"] != "20m0s" && body.Values["review_timeout"] != "20m" {
		t.Errorf("review_timeout = %q, want the 20m default", body.Values["review_timeout"])
	}
	if len(body.Fields) != 23 {
		t.Fatalf("fields = %d, want 23", len(body.Fields))
	}
}

func TestSettingsPutPersistsAndRejectsBadValues(t *testing.T) {
	f := newSecureFixture(t)
	cookie := f.session(f.user("admin", true))

	rec := f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"ocr_bin": "/usr/local/bin/ocr", "review_concurrency": "3"},
	}, cookie)
	assertStatus(t, rec, http.StatusOK)

	stored, err := f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if stored["ocr_bin"] != "/usr/local/bin/ocr" || stored["review_concurrency"] != "3" {
		t.Errorf("stored = %v", stored)
	}
	if _, ok := stored["opencode_bin"]; ok {
		t.Error("a partial save must not write untouched keys")
	}

	rec = f.request(http.MethodPut, "/api/settings", map[string]any{
		"values": map[string]string{"review_timeout": "0s"},
	}, cookie)
	assertStatus(t, rec, http.StatusBadRequest)
	var apiErr map[string]string
	decodeJSON(t, rec, &apiErr)
	if !strings.Contains(apiErr["error"], "review_timeout") {
		t.Errorf("error = %q, want the offending key", apiErr["error"])
	}
}

func TestRequestPublicURL(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		tls     bool
		want    string
	}{
		{"plain", "qa.local:8080", nil, false, "http://qa.local:8080"},
		{"tls", "qa.example.com", nil, true, "https://qa.example.com"},
		{"forwarded", "app:8080", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "qa.example.com"}, false, "https://qa.example.com"},
		{"first value wins", "app:8080", map[string]string{"X-Forwarded-Proto": "https, http"}, false, "https://app:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := (&server{}).requestPublicURL(r); got != tc.want {
				t.Errorf("requestPublicURL = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd .worktrees/feat-db-settings && go test ./internal/api/ -run "TestSettings|TestRequestPublicURL" -count=1
```

Expected: FAIL, `404` en las rutas y `s.requestPublicURL undefined`.

- [ ] **Step 3: Write `internal/api/publicurl.go`**

```go
package api

import (
	"net/http"
	"strings"
)

// requestPublicURL reconstructs the address the browser used. public_url is
// empty until the instance is configured, and an existing installation that
// drops it from .env must still get a working callback URL.
func (s *server) requestPublicURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := firstHeaderValue(r, "X-Forwarded-Proto"); proto == "http" || proto == "https" {
		scheme = proto
	}
	host := strings.TrimSpace(r.Host)
	if forwarded := firstHeaderValue(r, "X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + strings.TrimRight(host, "/")
}

func firstHeaderValue(r *http.Request, name string) string {
	raw := r.Header.Get(name)
	if raw == "" {
		return ""
	}
	if comma := strings.IndexByte(raw, ','); comma >= 0 {
		raw = raw[:comma]
	}
	return strings.TrimSpace(raw)
}
```

- [ ] **Step 4: Write `internal/api/settings.go`**

```go
package api

import (
	"encoding/json"
	"net/http"

	"github.com/jeriveromartinez/qa-control-center/internal/settings"
)

type settingsResponse struct {
	Values map[string]string `json:"values"`
	Fields []settings.Field  `json:"fields"`
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	values, err := s.Store.Settings(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "read settings")
		return
	}
	effective := settings.ToMap(s.Cfg)
	for key, value := range values {
		effective[key] = value
	}
	writeJSON(w, http.StatusOK, settingsResponse{Values: effective, Fields: settings.Fields()})
}

func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(body.Values) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no values to save")
		return
	}

	next := s.Cfg
	if err := settings.Apply(&next, body.Values); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	user := auth.UserFrom(r.Context())
	var userID *uint
	if user != nil {
		userID = &user.ID
	}
	if err := s.Store.SaveSettings(r.Context(), userID, body.Values); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save settings")
		return
	}
	// No se muta s.Cfg: los handlers concurrentes lo leen y la respuesta ya se
	// construye mezclando lo recién guardado sobre la configuración del proceso.
	s.getSettings(w, r)
}
```

Ajustar a los nombres reales: comprobar cómo obtiene el usuario el resto de handlers (`auth.UserFrom` o `s.currentUser(r)`), y usar ese. `s.Cfg` se reasigna para que el `GET` siguiente refleje lo guardado sin reiniciar.

- [ ] **Step 5: Register the routes**

En `internal/api/router.go`, dentro del grupo `auth.RequireAdmin`, añadir:

```go
				r.Get("/settings", s.getSettings)
				r.Put("/settings", s.putSettings)
```

- [ ] **Step 6: Run test to verify it passes**

```bash
cd .worktrees/feat-db-settings && go test ./internal/api/ -run "TestSettings|TestRequestPublicURL" -count=1
```

Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add internal/api && git commit -m "feat(api): expose settings and detect the public URL"
```

---

### Task 6: Pantalla de configuración en el panel

**Files:**
- Create: `web/src/views/SettingsView.vue`
- Create: `web/src/views/SettingsView.test.ts`
- Modify: `web/src/api.ts`
- Modify: `web/src/router.ts`
- Modify: `web/src/components/AppShell.vue`

**Interfaces:**
- Consumes: `GET/PUT /api/settings` de la Task 5.
- Produces: `api.settings()`, `api.saveSettings(values)`, tipos `SettingsField` y `SettingsResponse`.

- [ ] **Step 1: Write the failing test**

`web/src/views/SettingsView.test.ts`, con el estilo de `AgentsView.test.ts` (`vi.mock('../api')`, `mount`, `flushPromises`):

```ts
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api'
import SettingsView from './SettingsView.vue'

vi.mock('../api', () => ({ api: { settings: vi.fn(), saveSettings: vi.fn() } }))

const fields = [
  { key: 'review_timeout', group: 'review', default: '20m', restart: 'worker', secret: false },
  { key: 'ocr_bin', group: 'worker', default: 'ocr', restart: 'worker', secret: false },
  { key: 'alert_webhook_url', group: 'alerts', default: '', restart: 'worker', secret: true },
]

beforeEach(() => {
  vi.mocked(api.settings).mockResolvedValue({
    values: { review_timeout: '20m0s', ocr_bin: 'ocr', alert_webhook_url: '' },
    fields,
  })
  vi.mocked(api.saveSettings).mockResolvedValue({
    values: { review_timeout: '30m0s', ocr_bin: 'ocr', alert_webhook_url: '' },
    fields,
  })
})

describe('SettingsView', () => {
  it('renders a section per group with the stored value', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    expect(wrapper.text()).toContain('Revisión')
    expect((wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value).toBe('20m0s')
  })

  it('saves only the edited section', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="review_timeout"]').setValue('30m')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(api.saveSettings).toHaveBeenCalledWith({ review_timeout: '30m' })
  })

  it('shows which process has to restart', async () => {
    const wrapper = mount(SettingsView)
    await flushPromises()
    expect(wrapper.text()).toContain('requiere reiniciar worker')
  })

  it('keeps the typed value and shows the server error when a save is rejected', async () => {
    vi.mocked(api.saveSettings).mockRejectedValue(new Error('review_stale_after: must exceed review_timeout (30m0s)'))
    const wrapper = mount(SettingsView)
    await flushPromises()

    await wrapper.find('input[name="review_timeout"]').setValue('30m')
    await wrapper.find('form[data-group="review"]').trigger('submit')
    await flushPromises()

    expect(wrapper.text()).toContain('review_stale_after')
    expect((wrapper.find('input[name="review_timeout"]').element as HTMLInputElement).value).toBe('30m')
  })
})
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd .worktrees/feat-db-settings/web && npm run test -- SettingsView
```

Expected: FAIL, no se resuelve `./SettingsView.vue`.

- [ ] **Step 3: Add the API client**

En `web/src/api.ts`:

```ts
export interface SettingsField {
  key: string
  group: 'general' | 'review' | 'worker' | 'alerts' | 'backups'
  default: string
  restart: '' | 'app' | 'worker'
  secret: boolean
}

export interface SettingsResponse {
  values: Record<string, string>
  fields: SettingsField[]
}
```

y en el objeto `api`:

```ts
  settings: () => request<SettingsResponse>('/api/settings'),
  saveSettings: (values: Record<string, string>) =>
    request<SettingsResponse>('/api/settings', { method: 'PUT', body: { values } }),
```

- [ ] **Step 4: Write `SettingsView.vue`**

Cinco secciones con inputs explícitos; cada `<form>` lleva `data-group` y envía solo sus campos:

```vue
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api, type SettingsField, type SettingsResponse } from '../api'

const values = ref<Record<string, string>>({})
const fields = ref<SettingsField[]>([])
const saving = ref('')
const error = ref('')
const saved = ref('')

const groups: Array<{ id: SettingsField['group']; title: string; hint: string }> = [
  { id: 'general', title: 'General', hint: 'Dirección pública, webhooks y reconciliación.' },
  { id: 'review', title: 'Revisión', hint: 'Cuánto explora y cuánto tarda una revisión.' },
  { id: 'worker', title: 'Worker', hint: 'Rutas y puertos del contenedor que ejecuta las revisiones.' },
  { id: 'alerts', title: 'Alertas', hint: 'Perfil monitor: a quién vigila y a dónde avisa.' },
  { id: 'backups', title: 'Backups', hint: 'Cuántos dumps conserva el perfil backup.' },
]

function fieldsOf(group: SettingsField['group']): SettingsField[] {
  return fields.value.filter((field) => field.group === group)
}

function restartLabel(group: SettingsField['group']): string {
  const targets = new Set(fieldsOf(group).map((field) => field.restart))
  if (targets.has('app')) return 'requiere reiniciar app'
  if (targets.has('worker')) return 'requiere reiniciar worker'
  return ''
}

function restart(group: SettingsField['group']): string {
  return restartLabel(group)
}

async function load(): Promise<void> {
  error.value = ''
  try {
    const response = await api.settings()
    values.value = { ...response.values }
    fields.value = response.fields
  } catch {
    error.value = 'No se pudieron cargar los ajustes.'
  }
}

async function save(group: SettingsField['group']): Promise<void> {
  saving.value = group
  error.value = ''
  saved.value = ''
  const payload: Record<string, string> = {}
  for (const field of fieldsOf(group)) {
    payload[field.key] = values.value[field.key] ?? ''
  }
  try {
    const response = await api.saveSettings(payload)
    values.value = { ...response.values }
    saved.value = `Guardado ${group}.`
  } catch (err) {
    error.value = err instanceof Error ? err.message : 'No se pudieron guardar los ajustes.'
  } finally {
    saving.value = ''
  }
}

onMounted(load)
</script>

<template>
  <section class="stack">
    <header class="page-header">
      <h1 class="page-title">Ajustes</h1>
      <p class="page-subtitle">
        Se guardan en la base de datos. Los cambios se aplican al reiniciar el proceso indicado.
      </p>
    </header>

    <p v-if="error" class="form-error" role="alert">{{ error }}</p>
    <p v-if="saved" class="form-hint" role="status">{{ saved }}</p>

    <form
      v-for="group in groups"
      :key="group.id"
      class="card"
      :data-group="group.id"
      @submit.prevent="save(group.id)"
    >
      <header class="card__header">
        <h2 class="card__title">{{ group.title }}</h2>
        <span v-if="restart(group.id)" class="badge badge--warn">{{ restart(group.id) }}</span>
      </header>
      <p class="card__hint">{{ group.hint }}</p>

      <label v-for="field in fieldsOf(group.id)" :key="field.key" class="field">
        <span class="field__label">{{ field.key }}</span>
        <input
          v-model="values[field.key]"
          class="field__input"
          :type="field.secret ? 'password' : 'text'"
          :name="field.key"
          :placeholder="field.default || 'sin valor'"
          autocomplete="off"
        />
      </label>

      <button type="submit" class="button button--primary" :disabled="saving === group.id">
        {{ saving === group.id ? 'Guardando…' : 'Guardar' }}
      </button>
    </form>
  </section>
</template>
```

Usar las clases que ya existen en el panel; si `badge--warn` no existe, reutilizar la clase de tono que use `AgentsView.vue` para estados degradados en vez de inventar una.

- [ ] **Step 5: Add the route and the nav entry**

`web/src/router.ts`: importar `SettingsView` y añadir

```ts
    { path: '/settings', name: 'settings', component: SettingsView },
```

`web/src/components/AppShell.vue`, junto a «Agentes LLM»:

```vue
          <RouterLink v-if="session.me?.is_admin" to="/settings" class="nav-link">
            Ajustes
          </RouterLink>
```

- [ ] **Step 6: Run the tests and the build**

```bash
cd .worktrees/feat-db-settings/web && npm run test && npm run build
```

Expected: todos los tests verdes (48 anteriores + 4 nuevos) y `vue-tsc` sin errores.

- [ ] **Step 7: Commit**

```bash
git add web && git commit -m "feat(web): add the settings screen"
```

---

### Task 7: Despliegue, monitor y backups leyendo de la base

**Files:**
- Modify: `deploy/.env.example`
- Modify: `deploy/docker-compose.yml`
- Modify: `deploy/backup.sh`
- Modify: `cmd/qa/monitor.go`
- Modify: `README.md`, `docs/backup.md`
- Test: `cmd/qa/monitor_test.go` (nuevo)

**Interfaces:**
- Consumes: `store.Store.Settings`, `settings.Apply`, `config.Default`.
- Produces: `func monitorConfig(ctx context.Context, dsn string) (config.Config, error)` que devuelve defaults si la BD no responde.

- [ ] **Step 1: Write the failing test**

`cmd/qa/monitor_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"
)

func TestMonitorConfigFallsBackToDefaults(t *testing.T) {
	cfg, err := monitorConfig(context.Background(), "")
	if err != nil {
		t.Fatalf("monitorConfig without a database = %v, want defaults and no error", err)
	}
	if cfg.WorkerHealthURL != "http://worker:9090" || cfg.MonitorInterval != time.Minute {
		t.Errorf("fallback = %+v, want the defaults", cfg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd .worktrees/feat-db-settings && go test ./cmd/qa/ -run TestMonitorConfig -count=1
```

Expected: FAIL, `undefined: monitorConfig`.

- [ ] **Step 3: Rewrite the monitor config**

En `cmd/qa/monitor.go`, sustituir `config.LoadMonitor(os.Getenv)`:

```go
// monitorConfig reads the stored settings, because the monitor is configured
// from the panel like everything else. A missing or unreachable database must
// not stop it: a monitor that refuses to start when PostgreSQL is down cannot
// report that PostgreSQL is down, so it falls back to the defaults.
func monitorConfig(ctx context.Context, dsn string) (config.Config, error) {
	cfg := config.Default()
	if dsn == "" {
		slog.Warn("no database URL; the monitor uses the default settings")
		return cfg, nil
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		slog.Warn("database unavailable; the monitor uses the default settings", "error", err)
		return cfg, nil
	}
	defer st.Close()

	values, err := st.Settings(ctx)
	if err != nil {
		slog.Warn("cannot read settings; the monitor uses the defaults", "error", err)
		return cfg, nil
	}
	if err := settings.Apply(&cfg, values); err != nil {
		slog.Warn("stored settings are invalid; the monitor uses the defaults", "error", err)
		return config.Default(), nil
	}
	return cfg, nil
}
```

y en `runMonitor`:

```go
	cfg, err := monitorConfig(ctx, strings.TrimSpace(os.Getenv("QA_DATABASE_URL")))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
```

Usar `cfg.MonitorInterval`, `cfg.MonitorRenotifyAfter`, `cfg.WorkerHealthURL` y `cfg.AlertWebhookURL`. Con esto `config.LoadMonitor`, `loadMonitorSettings` y `MonitorConfig` quedan sin uso: borrarlos junto con `internal/config/monitor_test.go`, y comprobar que sus reglas siguen cubiertas por `internal/settings/apply_test.go` (defaults, duraciones > 0, `renotify >= interval`, URL absoluta).

- [ ] **Step 4: `backup.sh` reads the retention from the database**

Sustituir `retencion="${QA_BACKUP_RETENTION:-7}"` por:

```sh
retencion=$(psql "$QA_DATABASE_URL" -tAc \
    "SELECT value FROM app_settings WHERE key = 'backup_retention'" 2>/dev/null || true)
retencion=$(printf '%s' "$retencion" | tr -d '[:space:]')
case "$retencion" in
    ''|*[!0-9]*) retencion=7 ;;
esac
```

Con un comentario que explique por qué hay fallback: el script ya necesita la base para `pg_dump`, pero una lectura fallida no debe abortar el backup.

- [ ] **Step 5: Shrink `.env.example`, compose and docs**

`deploy/.env.example` queda en:

```sh
# Copiar a deploy/.env. Todo lo demás se configura desde el panel (Ajustes).
QA_DATABASE_URL=postgres://postgres:change-me@postgres:5432/qa_control_center?sslmode=disable
POSTGRES_PASSWORD=change-me
# Sigue aquí hasta que el setup pueda detectarla (Task 8).
QA_PUBLIC_URL=http://localhost:8080
# Generar con: openssl rand -base64 32 (32 bytes). Solo las usa la app/worker;
# cifran los secretos que viven en la base, por eso no pueden estar en ella.
QA_MASTER_KEY=
QA_WORKER_KEY=
```

`deploy/docker-compose.yml`: dejar en `app` y `worker` solo `QA_DATABASE_URL`, `QA_WORKER_KEY` (y `QA_MASTER_KEY` solo en `app`); añadir `QA_DATABASE_URL` al servicio `monitor`. Eliminar de ambos las variables movidas.

`README.md`: reemplazar la tabla/lista de variables opcionales por una referencia a *Ajustes* en el panel, dejar la tabla de variables de entorno con solo las cuatro, y documentar en el setup que `QA_SETUP_TOKEN` es opcional.

`docs/backup.md`: `QA_BACKUP_RETENTION` pasa a explicarse como ajuste del panel.

- [ ] **Step 6: Run everything**

```bash
cd .worktrees/feat-db-settings && go build ./... && go vet ./...
MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash -lc 'cd /mnt/e/Projects/manager-open-code-review/.worktrees/feat-db-settings; export PATH=/usr/local/go/bin:/usr/bin:/bin; export GOFLAGS=-buildvcs=false; export DOCKER_HOST=tcp://127.0.0.1:2375; go vet ./... && go test -p 1 ./... -count=1'
cd web && npm run test && npm run build
```

Expected: todo verde, 23 campos y sin referencias a las variables retiradas:

```bash
grep -rn "QA_REVIEW_TIMEOUT\|QA_OCR_BIN\|QA_PUBLIC_URL" --include=*.yml --include=*.sh --include=*.example deploy/ || echo "sin restos"
```

- [ ] **Step 7: Commit**

```bash
git add deploy cmd/qa README.md docs/backup.md
git commit -m "feat(deploy): read monitor, backup and runtime settings from the database"
```

---

## PR B — Setup sin token

### Task 8: El setup pide solo el nombre de la App

**Files:**
- Modify: `internal/api/setup.go`
- Modify: `internal/api/api_test.go`
- Modify: `web/src/views/SetupView.vue`
- Test: `web/src/views/SetupView.test.ts` (nuevo)

**Interfaces:**
- Consumes: `github.BuildManifest(publicURL string, webhooksEnabled bool, name string)`, `s.requestPublicURL(r)`, `s.Store.SaveSettings`.
- Produces: `POST /api/setup/manifest` con cuerpo `{"name": "..."}`; `public_url` persistida.

- [ ] **Step 1: Write the failing Go tests**

En `internal/api/api_test.go`, sustituir `TestSetupRequiresToken` por:

```go
func TestSetupAsksOnlyForTheAppName(t *testing.T) {
	f := newSecureFixture(t)

	rec := f.request(http.MethodPost, "/api/setup/manifest", map[string]any{}, nil)
	assertStatus(t, rec, http.StatusBadRequest)

	rec = f.request(http.MethodPost, "/api/setup/manifest", map[string]any{"name": "Acme QA"}, nil)
	assertStatus(t, rec, http.StatusOK)

	var body setupManifestResponse
	decodeJSON(t, rec, &body)
	if body.Manifest.Name != "Acme QA" {
		t.Errorf("manifest name = %q, want Acme QA", body.Manifest.Name)
	}
	if body.ActionURL != "https://github.com/settings/apps/new" {
		t.Errorf("action_url = %q, want the personal account URL", body.ActionURL)
	}
}

func TestSetupPersistsTheDetectedPublicURL(t *testing.T) {
	f := newSecureFixture(t)
	// El fixture fija una URL pública; se vacía para comprobar que el setup la
	// deduce de la petición, que es el caso de una instalación existente.
	f.deps.Cfg.PublicURL = ""
	f.rebuildRouter()

	req := httptest.NewRequest(http.MethodPost, "/api/setup/manifest",
		strings.NewReader(`{"name":"Acme QA"}`))
	req.Host = "qa.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	assertStatus(t, rec, http.StatusOK)

	stored, err := f.st.Settings(f.ctx)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if stored["public_url"] != "https://qa.example.com" {
		t.Errorf("public_url = %q, want https://qa.example.com", stored["public_url"])
	}
}

func TestSetupTokenIsEnforcedOnlyWhenConfigured(t *testing.T) {
	f := newSecureFixture(t)
	f.deps.SetupToken = "s3cret"
	f.rebuildRouter()

	rec := f.request(http.MethodPost, "/api/setup/manifest", map[string]any{"name": "Acme QA"}, nil)
	assertStatus(t, rec, http.StatusForbidden)
	rec = f.request(http.MethodPost, "/api/setup/manifest", map[string]any{"name": "Acme QA", "setup_token": "s3cret"}, nil)
	assertStatus(t, rec, http.StatusOK)
}
```

(`newSecureFixture` vive en `agents_test.go` y expone `deps`, `router`, `st`, `ctx`, `request`; `rebuildRouter()` reconstruye el handler tras cambiar `deps`. Usar esos nombres, no inventar otros.)

- [ ] **Step 2: Run test to verify it fails**

```bash
cd .worktrees/feat-db-settings && go test ./internal/api/ -run TestSetup -count=1
```

Expected: FAIL con `403 invalid setup token` en el caso sin token y `manifest name = ""`.

- [ ] **Step 3: Rewrite the handler**

En `internal/api/setup.go`:

```go
func (s *server) setupManifest(w http.ResponseWriter, r *http.Request) {
	if s.isConfigured(r) {
		writeAPIError(w, http.StatusConflict, "already configured")
		return
	}

	var body struct {
		Name       string `json:"name"`
		SetupToken string `json:"setup_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeAPIError(w, http.StatusBadRequest, "name is required")
		return
	}
	// The token is optional: it only exists for operators who want to lock the
	// setup window down. Leaving it unset keeps the first-run wizard honest
	// instead of asking for a value copied from the logs.
	if s.SetupToken != "" && body.SetupToken != s.SetupToken {
		writeAPIError(w, http.StatusForbidden, "invalid setup token")
		return
	}

	publicURL := strings.TrimSpace(s.Cfg.PublicURL)
	if publicURL == "" {
		publicURL = s.requestPublicURL(r)
	}
	if publicURL == "" {
		writeAPIError(w, http.StatusBadRequest, "cannot determine the public URL")
		return
	}
	if s.Cfg.PublicURL == "" {
		if err := s.Store.SaveSettings(r.Context(), nil, map[string]string{"public_url": publicURL}); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "save public url")
			return
		}
	}

	manifest := github.BuildManifest(publicURL, s.Cfg.WebhooksEnabled, name)
	writeJSON(w, http.StatusOK, setupManifestResponse{
		ActionURL: strings.TrimRight(s.Cfg.GitHubURL, "/") + "/settings/apps/new",
		Manifest:  manifest,
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd .worktrees/feat-db-settings && go test ./internal/api/ -count=1
```

Expected: `ok`. Los tests de `TestSetupStatus` y `TestSetupInstalled*` siguen pasando.

- [ ] **Step 5: Retire `QA_PUBLIC_URL` from the environment**

Ahora que el setup la detecta y la persiste, deja de leerse:

- `internal/config/config.go`: borrar el bloque que lee `QA_PUBLIC_URL` y su error; añadir `"QA_PUBLIC_URL"` a `LegacyEnvKeys()`.
- `internal/config/config_test.go`: `TestLegacyEnvKeysCoverEveryMovedSetting` pasa a esperar 23 y a exigir que `QA_PUBLIC_URL` **sí** esté en la lista; en `TestLoadOnlyReadsTheEssentials`, borrar la aserción sobre `PublicURL` (o afirmar que queda vacío). Ajustar `TestLoadDefaults` y cualquier test que dependa de que sea obligatoria.
- `deploy/.env.example`: eliminar `QA_PUBLIC_URL`.
- `deploy/docker-compose.yml`: eliminar `QA_PUBLIC_URL` de `app` y `worker`.

Verificar que no queda ninguna lectura:

```bash
grep -rn "QA_PUBLIC_URL" --include=*.go --include=*.yml --include=*.example . | grep -v node_modules
```

Expected: solo `internal/config/config.go` (la lista de legadas) y `.env.example`/compose sin ella, más la documentación.

- [ ] **Step 5: Rewrite the setup form**

`web/src/views/SetupView.vue`: un único `name`:

```vue
<script setup lang="ts">
import { ref } from 'vue'
import { ApiError, api, type SetupManifest } from '../api'

const name = ref('')
const busy = ref(false)
const error = ref('')

function postManifest({ action_url, manifest }: SetupManifest): void {
  const form = document.createElement('form')
  form.method = 'post'
  form.action = action_url
  const input = document.createElement('input')
  input.type = 'hidden'
  input.name = 'manifest'
  input.value = JSON.stringify(manifest)
  form.appendChild(input)
  document.body.appendChild(form)
  form.submit()
}

async function submit(): Promise<void> {
  error.value = ''
  busy.value = true
  try {
    postManifest(await api.setupManifest({ name: name.value.trim() }))
  } catch (err) {
    if (err instanceof ApiError && err.status === 403) {
      error.value = 'El token de setup del servidor no coincide.'
    } else if (err instanceof ApiError && err.status === 409) {
      error.value = 'La instancia ya está configurada.'
    } else {
      error.value = 'No se pudo crear la GitHub App. Inténtalo de nuevo.'
    }
    busy.value = false
  }
}
</script>
```

Plantilla: un `<label>` «Nombre de la App» con `name="name"`, `required`, y el subtítulo «Elige el nombre con el que se creará la GitHub App de esta instancia.»

`web/src/api.ts`: `SetupManifestRequest` pasa a `{ name: string }`.

- [ ] **Step 6: Write the component test**

`web/src/views/SetupView.test.ts` verificando que hay **un** campo (`input`), que es `name`, y que enviar llama a `api.setupManifest` con `{ name: 'Acme QA' }`.

- [ ] **Step 7: Run everything**

```bash
cd .worktrees/feat-db-settings && go build ./... && go vet ./...
cd web && npm run test && npm run build
MSYS_NO_PATHCONV=1 wsl -d Ubuntu -- bash -lc 'cd /mnt/e/Projects/manager-open-code-review/.worktrees/feat-db-settings; export PATH=/usr/local/go/bin:/usr/bin:/bin; export GOFLAGS=-buildvcs=false; export DOCKER_HOST=tcp://127.0.0.1:2375; go vet ./... && go test -p 1 ./... -count=1'
```

Expected: todo verde.

- [ ] **Step 8: Document and commit**

README: el setup pide el nombre; `QA_SETUP_TOKEN` es opcional y sirve para cerrar la ventana de configuración; recomendar terminar el setup antes de exponer la instancia.

```bash
git add internal/api web README.md
git commit -m "feat(setup): ask only for the app name"
```

---

## Self-Review

- **Cobertura del spec**: `.env` (Task 7), `app_settings` y defaults (Tasks 1–2), carga sin cambiar call sites (Tasks 3–4), ignorar el env con aviso (Tasks 3–4), validación cruzada (Task 2), API admin y PUT parcial (Task 5), pantalla por secciones con marca de reinicio (Task 6), monitor con fallback (Task 7), retención de backups desde la BD (Task 7), setup por nombre con token opcional y URL detectada (Task 8). Nada del spec queda sin tarea.
- **Placeholders**: los pasos de código llevan el código real; los helpers del fixture (`newSecureFixture`, `f.request`, `f.st`, `f.ctx`, `f.router`, `f.rebuildRouter`) están verificados contra `internal/api/agents_test.go` y `internal/api/api_test.go`.
- **Consistencia de tipos**: `settings.Key*`, `settings.Field` (`key/group/default/restart/secret`), `settings.ToMap`, `settings.Apply`, `*settings.KeyError`, `store.Settings/SaveSettings`, `config.Default()`, `config.LegacyEnvKeys()`, `server.requestPublicURL`, `monitorConfig`, `api.settings/saveSettings` se usan con la misma firma en todas las tareas.
