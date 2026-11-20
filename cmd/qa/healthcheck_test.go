package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheckExitsZeroOnHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	if err := runHealthcheck([]string{srv.URL}); err != nil {
		t.Fatalf("runHealthcheck() error = %v, want nil", err)
	}
}

func TestHealthcheckFailsOn503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := runHealthcheck([]string{srv.URL}); err == nil {
		t.Fatal("runHealthcheck() error = nil, want an error for a 503")
	}
}

func TestHealthcheckFailsWhenUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	if err := runHealthcheck([]string{url}); err == nil {
		t.Fatal("runHealthcheck() error = nil, want an error for a closed server")
	}
}

func TestHealthcheckDefaultsToLoopback(t *testing.T) {
	got, err := healthcheckURL(nil)
	if err != nil {
		t.Fatalf("healthcheckURL() error = %v", err)
	}
	if got != "http://127.0.0.1:8080/healthz" {
		t.Errorf("healthcheckURL() = %q, want the loopback healthz", got)
	}
}
