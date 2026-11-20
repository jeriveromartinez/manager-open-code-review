package monitor_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/monitor"
)

func discardingLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(discard{}, nil))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func alert(now time.Time) monitor.Alert {
	return monitor.Alert{
		Code:    monitor.CodeQueueStalled,
		Message: "review queue stalled: 5 waiting jobs, none running, oldest waiting 3600s",
		Values:  map[string]any{"available": 5, "running": 0, "oldest_available_seconds": 3600},
		At:      now,
	}
}

func TestWebhookPayloadShape(t *testing.T) {
	var (
		mu   sync.Mutex
		body string
		ct   string
		srv  = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err == nil {
				mu.Lock()
				body = string(raw)
				ct = r.Header.Get("Content-Type")
				mu.Unlock()
			}
			w.WriteHeader(http.StatusAccepted)
		}))
	)
	defer srv.Close()

	n := monitor.NewWebhookNotifier(srv.URL, srv.Client(), discardingLogger())
	if err := n.Notify(context.Background(), alert(baseTime)); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}

	const want = `{"severity":"warning","code":"queue_stalled","message":"review queue stalled: 5 waiting jobs, none running, oldest waiting 3600s","values":{"available":5,"oldest_available_seconds":3600,"running":0},"timestamp":"2026-11-20T10:00:00Z"}`
	if body != want {
		t.Errorf("body =\n%s\nwant\n%s", body, want)
	}
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestWebhookRecoverySeverity(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
	}))
	defer srv.Close()

	recovered := alert(baseTime)
	recovered.Recovered = true
	if err := monitor.NewWebhookNotifier(srv.URL, srv.Client(), discardingLogger()).Notify(context.Background(), recovered); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}

	var payload struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Severity != monitor.SeverityRecovered {
		t.Errorf("severity = %q, want %q", payload.Severity, monitor.SeverityRecovered)
	}
	if payload.Code != monitor.CodeQueueStalled {
		t.Errorf("code = %q, want the recovered problem's code", payload.Code)
	}
}

func TestWebhookNon2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := monitor.NewWebhookNotifier(srv.URL, srv.Client(), discardingLogger()).Notify(context.Background(), alert(baseTime))
	if err == nil {
		t.Fatal("Notify() error = nil, want an error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention the status", err)
	}
}

func TestLogNotifierNeverFails(t *testing.T) {
	if err := monitor.NewLogNotifier(discardingLogger()).Notify(context.Background(), alert(baseTime)); err != nil {
		t.Fatalf("Notify() error = %v, want nil", err)
	}
}
