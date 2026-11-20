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
