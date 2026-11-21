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
// keep whatever cfg already had, which is how a partial save works. A rejected
// value leaves cfg partially written, so callers validate on a copy and only
// publish it once Apply returns nil.
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
		return setIntAtMost(&cfg.BackupRetention, key, raw, 999)
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

// setIntAtMost shares the integer rules and adds the upper bound a consumer
// really enforces; an out-of-range value would otherwise be stored and ignored.
func setIntAtMost(dst *int, key, raw string, max int) error {
	if err := setInt(dst, key, raw); err != nil {
		return err
	}
	if *dst > max {
		return &KeyError{key, fmt.Sprintf("must be between 1 and %d", max)}
	}
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
