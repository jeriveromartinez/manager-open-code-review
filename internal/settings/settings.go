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

// Field tells the panel how to group a key, how to present it to a person and
// what has to restart for a new value to take effect. The UI renders explicit
// inputs; this is metadata only. Restart names the process that reads the key
// at startup: the panel is useless if it tells an administrator to restart
// something that never read it.
type Field struct {
	Key     string `json:"key"`
	Group   Group  `json:"group"`
	Label   string `json:"label"`
	Help    string `json:"help"`
	Kind    string `json:"kind"` // duration, int, bool, url, text, path or secret
	Unit    string `json:"unit"` // duration unit: s, m or h
	Default string `json:"default"`
	Restart string `json:"restart"` // "", "app", "worker" or "monitor"
	Secret  bool   `json:"secret"`
}

func Fields() []Field {
	return []Field{
		{
			Key: KeyPublicURL, Group: GroupGeneral,
			Label: "URL pública",
			Help:  "Dirección con la que GitHub entrega los webhooks. Vacía, se detecta al configurar la App.",
			Kind:  "url", Restart: "app",
		},
		{
			Key: KeyWebhooksEnabled, Group: GroupGeneral,
			Label: "Recibir webhooks",
			Help:  "Desactivado, el panel solo descubre cambios en cada reconciliación.",
			Kind:  "bool", Default: "true", Restart: "app",
		},
		{
			Key: KeyReconcileInterval, Group: GroupGeneral,
			Label: "Intervalo de reconciliación",
			Help:  "Cada cuánto se revisan los repositorios y PRs en GitHub.",
			Kind:  "duration", Unit: "m", Default: "5m", Restart: "app",
		},
		{
			Key: KeyGitHubURL, Group: GroupGeneral,
			Label: "URL de GitHub",
			Help:  "Cámbiala solo para GitHub Enterprise.",
			Kind:  "url", Default: "https://github.com", Restart: "app",
		},
		{
			Key: KeyGitHubAPIURL, Group: GroupGeneral,
			Label: "URL de la API de GitHub",
			Help:  "Endpoint REST. En GitHub.com es https://api.github.com.",
			Kind:  "url", Default: "https://api.github.com", Restart: "app",
		},
		{
			Key: KeyListenAddr, Group: GroupGeneral,
			Label: "Dirección de escucha",
			Help:  "Host y puerto donde el proceso app atiende peticiones.",
			Kind:  "text", Default: ":8080", Restart: "app",
		},
		{
			Key: KeyReviewConcurrency, Group: GroupReview,
			Label: "Revisiones simultáneas",
			Help:  "Cuántas revisiones ejecuta el worker a la vez.",
			Kind:  "int", Default: "2", Restart: "worker",
		},
		{
			Key: KeyReviewAgentSteps, Group: GroupReview,
			Label: "Pasos por revisión",
			Help:  "Máximo de pasos del agente opencode en una revisión.",
			Kind:  "int", Default: "40", Restart: "worker",
		},
		{
			Key: KeyReviewTimeout, Group: GroupReview,
			Label: "Tiempo máximo por revisión",
			Help:  "Al superarlo, el run falla y el PR queda inconcluso.",
			Kind:  "duration", Unit: "m", Default: "20m", Restart: "worker",
		},
		{
			Key: KeyReviewStaleAfter, Group: GroupReview,
			Label: "Umbral de runs huérfanos",
			Help:  "Los runs en curso más antiguos que esto se marcan como interrumpidos. Debe superar el tiempo máximo.",
			Kind:  "duration", Unit: "m", Default: "45m", Restart: "worker",
		},
		{
			Key: KeyLLMHealthInterval, Group: GroupReview,
			Label: "Intervalo de salud LLM",
			Help:  "Cada cuánto el worker comprueba los perfiles de lenguaje habilitados.",
			Kind:  "duration", Unit: "m", Default: "30m", Restart: "worker",
		},
		{
			Key: KeyFetchDepthMax, Group: GroupReview,
			Label: "Profundidad de fetch",
			Help:  "Commits que se descargan del repositorio para calcular el diff.",
			Kind:  "int", Default: "500", Restart: "worker",
		},
		{
			Key: KeyGitBaseURL, Group: GroupReview,
			Label: "URL base de Git",
			Help:  "Se usa para clonar. Cámbiala solo para GitHub Enterprise.",
			Kind:  "url", Default: "https://github.com", Restart: "worker",
		},
		{
			Key: KeyWorkspaceDir, Group: GroupWorker,
			Label: "Directorio de trabajo",
			Help:  "Ruta temporal donde el worker clona los repositorios.",
			Kind:  "path", Default: "/tmp/qa", Restart: "worker",
		},
		{
			Key: KeyOCRBin, Group: GroupWorker,
			Label: "Binario ocr",
			Help:  "Nombre o ruta del ejecutable ocr dentro del worker.",
			Kind:  "text", Default: "ocr", Restart: "worker",
		},
		{
			Key: KeyOpencodeBin, Group: GroupWorker,
			Label: "Binario opencode",
			Help:  "Nombre o ruta del ejecutable opencode dentro del worker.",
			Kind:  "text", Default: "opencode", Restart: "worker",
		},
		{
			Key: KeyWorkerHealthAddr, Group: GroupWorker,
			Label: "Dirección de salud del worker",
			Help:  "Host y puerto de /healthz y /metrics del worker.",
			Kind:  "text", Default: ":9090", Restart: "worker",
		},
		{
			Key: KeyWorkerQueueStale, Group: GroupWorker,
			Label: "Cola estancada",
			Help:  "Tiempo sin avanzar que marca la cola como estancada.",
			Kind:  "duration", Unit: "m", Default: "10m", Restart: "worker",
		},
		{
			Key: KeyWorkerHealthURL, Group: GroupAlerts,
			Label: "URL de salud del worker",
			Help:  "Base que consulta el monitor. Normalmente http://worker:9090.",
			Kind:  "url", Default: "http://worker:9090", Restart: "monitor",
		},
		{
			Key: KeyMonitorInterval, Group: GroupAlerts,
			Label: "Intervalo del monitor",
			Help:  "Cada cuánto sondea al worker.",
			Kind:  "duration", Unit: "m", Default: "1m", Restart: "monitor",
		},
		{
			Key: KeyMonitorRenotifyAfter, Group: GroupAlerts,
			Label: "Recordatorio de alerta",
			Help:  "Cada cuánto repite un problema que sigue abierto.",
			Kind:  "duration", Unit: "m", Default: "30m", Restart: "monitor",
		},
		{
			Key: KeyAlertWebhookURL, Group: GroupAlerts,
			Label: "Webhook de alertas",
			Help:  "POST opcional para las alertas. Vacío, solo se escribe en el log.",
			Kind:  "url", Restart: "monitor", Secret: true,
		},
		{
			Key: KeyBackupRetention, Group: GroupBackups,
			Label: "Copias conservadas",
			Help:  "Cuántos dumps diarios conserva el perfil backup.",
			Kind:  "int", Default: "7",
		},
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
