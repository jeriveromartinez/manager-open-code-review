package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Notifier delivers an alert. Implementations must not panic or block the
// loop: a delivery failure is reported to the caller, never fatal.
type Notifier interface {
	Notify(ctx context.Context, alert Alert) error
}

// webhookPayload is the stable contract of the alert_webhook_url setting. It
// is a generic JSON document on purpose: no vendor-specific envelope, so any
// receiver can consume it.
type webhookPayload struct {
	Severity  string         `json:"severity"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Values    map[string]any `json:"values"`
	Timestamp string         `json:"timestamp"`
}

// WebhookNotifier POSTs the alert as JSON to a generic endpoint.
type WebhookNotifier struct {
	URL    string
	Client *http.Client
	Logger *slog.Logger
}

func NewWebhookNotifier(url string, client *http.Client, logger *slog.Logger) *WebhookNotifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &WebhookNotifier{URL: url, Client: client, Logger: logger}
}

func (w *WebhookNotifier) Notify(ctx context.Context, alert Alert) error {
	values := alert.Values
	if values == nil {
		values = map[string]any{}
	}
	body, err := json.Marshal(webhookPayload{
		Severity:  alert.Severity(),
		Code:      alert.Code,
		Message:   alert.Message,
		Values:    values,
		Timestamp: alert.At.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("encode alert: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build alert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.Client.Do(req)
	if err != nil {
		return fmt.Errorf("post alert: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook answered %d", resp.StatusCode)
	}
	return nil
}

// LogNotifier writes the alert to the process log. It is always installed, so
// a deployment with no webhook still records every incident.
type LogNotifier struct {
	Logger *slog.Logger
}

func NewLogNotifier(logger *slog.Logger) *LogNotifier {
	return &LogNotifier{Logger: logger}
}

func (n *LogNotifier) Notify(_ context.Context, alert Alert) error {
	attrs := []any{
		"code", alert.Code,
		"subject", alert.Subject,
		"severity", alert.Severity(),
	}
	attrs = append(attrs, "message", alert.Message)
	for key, value := range alert.Values {
		attrs = append(attrs, "observed_"+key, value)
	}

	if alert.Recovered {
		n.Logger.Info("worker recovered", attrs...)
		return nil
	}
	n.Logger.Warn("worker alert", attrs...)
	return nil
}

// MultiNotifier delivers to every notifier and reports the first failure,
// after trying all of them: a broken webhook must not silence the log.
type MultiNotifier []Notifier

func (m MultiNotifier) Notify(ctx context.Context, alert Alert) error {
	var firstErr error
	for _, n := range m {
		if err := n.Notify(ctx, alert); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
