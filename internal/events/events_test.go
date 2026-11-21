package events

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeriveromartinez/qa-control-center/internal/testutil"
)

// startBroker runs a Broker in the background for the duration of the test and
// fails the test if Run does not return cleanly after cancellation.
func startBroker(t *testing.T, broker *Broker) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- broker.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("Run returned %v, want context.Canceled or nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after context cancellation")
		}
	})
}

// publishUntil repeats the publish until want is received on ch or the timeout
// expires. LISTEN may become active slightly after Run starts, so a single
// publish is not reliable.
func publishUntil(t *testing.T, pub Publisher, ev Event, ch <-chan Event) Event {
	t.Helper()

	timeout := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	if err := pub.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}

	for {
		select {
		case got := <-ch:
			return got
		case <-ticker.C:
			if err := pub.Publish(context.Background(), ev); err != nil {
				t.Fatalf("publish: %v", err)
			}
		case <-timeout:
			t.Fatalf("timed out waiting for event %+v", ev)
		}
	}
}

func TestPublishReachesSubscriber(t *testing.T) {
	s := testutil.NewStore(t)
	broker := NewBroker(s.Pool)
	pub := NewPGPublisher(s.Pool)

	startBroker(t, broker)

	eventsCh, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	want := Event{Type: "pr.updated", ID: "42"}
	got := publishUntil(t, pub, want, eventsCh)

	if got != want {
		t.Fatalf("received %+v, want %+v", got, want)
	}
}

// TestServeSSEOpensImmediately guards the proxy-facing behaviour: a reverse
// proxy such as Cloudflare does not forward the response headers until the first
// body byte arrives. Without an opening comment the browser's EventSource stays
// CONNECTING until the first 25 s ping, so the panel shows "Reconectando…".
func TestServeSSEOpensImmediately(t *testing.T) {
	broker := NewBroker(nil)
	srv := httptest.NewServer(http.HandlerFunc(broker.ServeSSE))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open SSE stream: %v (no body byte arrived before the deadline)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read opening line: %v", err)
	}
	if !strings.HasPrefix(line, ":") {
		t.Fatalf("first line = %q, want an SSE comment", line)
	}
}

func TestServeSSEStreamsEvent(t *testing.T) {
	s := testutil.NewStore(t)
	broker := NewBroker(s.Pool)
	pub := NewPGPublisher(s.Pool)

	startBroker(t, broker)

	srv := httptest.NewServer(http.HandlerFunc(broker.ServeSSE))
	defer srv.Close()

	// Keep publishing until the client has read the event: the subscription is
	// only created once the request handler starts running.
	stop := make(chan struct{})
	publisherDone := make(chan struct{})
	defer func() {
		close(stop)
		<-publisherDone
	}()
	go func() {
		defer close(publisherDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := pub.Publish(context.Background(), Event{Type: "pr.updated", ID: "42"}); err != nil {
				return
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}

	var gotEvent, gotData string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			gotEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			gotData = strings.TrimPrefix(line, "data: ")
		}
		if gotEvent != "" && gotData != "" {
			break
		}
	}

	if gotEvent != "pr.updated" {
		t.Fatalf("event line = %q, want pr.updated", gotEvent)
	}
	if gotData != `{"type":"pr.updated","id":"42"}` {
		t.Fatalf("data line = %q, want %s", gotData, `{"type":"pr.updated","id":"42"}`)
	}
}

func TestUnsubscribeStops(t *testing.T) {
	s := testutil.NewStore(t)
	broker := NewBroker(s.Pool)

	startBroker(t, broker)

	eventsCh, unsubscribe := broker.Subscribe()
	unsubscribe()
	// Unsubscribe must be idempotent.
	unsubscribe()

	select {
	case _, ok := <-eventsCh:
		if ok {
			t.Fatal("received an event after unsubscribe")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel was not closed after unsubscribe")
	}
}
