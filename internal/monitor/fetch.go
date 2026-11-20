package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBodyBytes bounds a read so a wedged endpoint cannot exhaust the
// monitor's memory. The real bodies are a few hundred bytes.
const maxBodyBytes = 1 << 20

// Fetcher polls the worker's endpoints. It knows only HTTP: the base URL
// points at the worker's host, and the paths are the contract worker.Handler
// serves.
type Fetcher struct {
	BaseURL string
	Client  *http.Client
}

// NewFetcher builds a fetcher. A nil client gets one with a timeout, because
// a monitor that blocks forever stops monitoring.
func NewFetcher(baseURL string, client *http.Client) *Fetcher {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Fetcher{BaseURL: strings.TrimRight(baseURL, "/"), Client: client}
}

// Observe reads both endpoints once. A failure to reach the worker at all is
// a ReachError; anything the worker answers is an observation, degraded or
// not, because only the evaluator decides what it means.
func (f *Fetcher) Observe(ctx context.Context) Observation {
	var obs Observation

	var health Health
	code, err := f.get(ctx, "/healthz", &health)
	obs.HealthCode = code
	switch {
	case err != nil:
		obs.ReachError = fmt.Errorf("/healthz: %w", err)
		return obs
	case !looksLikeHealth(health):
		obs.ReachError = fmt.Errorf("/healthz returned %d without a health report", code)
		return obs
	}
	obs.Health = health

	var metrics Metrics
	if _, err := f.get(ctx, "/metrics", &metrics); err != nil {
		obs.MetricsError = err
		return obs
	}
	obs.Metrics = metrics
	return obs
}

func (f *Fetcher) get(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.BaseURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("%s answered %d with invalid JSON: %w", path, resp.StatusCode, err)
	}
	return resp.StatusCode, nil
}

// looksLikeHealth rejects a 200 that is not the worker's contract, such as a
// proxy error page or the SPA catch-all. Without the status field there is
// nothing to evaluate, and silence would look healthy.
func looksLikeHealth(h Health) bool {
	return h.Status != ""
}
