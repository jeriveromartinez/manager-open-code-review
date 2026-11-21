// Package events carries live updates from the backend to browsers.
//
// Events only carry an ID, never data: clients refetch the affected resource
// through the regular, filtered API. The transport is PostgreSQL
// LISTEN/NOTIFY, fanned out in-process and exposed as Server-Sent Events.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChannelName is the PostgreSQL notification channel used by the publisher and
// the broker.
const ChannelName = "qa_events"

const (
	// subscriberBuffer is the per-subscriber queue size. When it is full the
	// event is dropped for that subscriber instead of blocking the broker.
	subscriberBuffer = 64
	// pingInterval is how often a comment line is sent to keep the SSE stream
	// alive through proxies.
	pingInterval = 25 * time.Second
	// reconnectDelay is the pause before reconnecting after a lost LISTEN
	// connection.
	reconnectDelay = 250 * time.Millisecond
)

// Event is a live-update notification. It deliberately carries only IDs; the
// client refetches the resource through the filtered API.
type Event struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Publisher publishes events to the notification channel.
type Publisher interface {
	Publish(ctx context.Context, ev Event) error
}

// pgPublisher publishes events with pg_notify through a pgx pool.
type pgPublisher struct {
	pool *pgxpool.Pool
}

// NewPGPublisher returns a Publisher backed by pool.
func NewPGPublisher(pool *pgxpool.Pool) Publisher {
	return &pgPublisher{pool: pool}
}

// Publish sends ev to ChannelName. The payload is the JSON-encoded event.
func (p *pgPublisher) Publish(ctx context.Context, ev Event) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if _, err := p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", ChannelName, string(payload)); err != nil {
		return fmt.Errorf("notify %s: %w", ChannelName, err)
	}
	return nil
}

// Broker listens for notifications and fans them out to subscribers.
type Broker struct {
	pool *pgxpool.Pool

	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBroker returns a Broker that listens on pool.
func NewBroker(pool *pgxpool.Pool) *Broker {
	return &Broker{
		pool: pool,
		subs: make(map[chan Event]struct{}),
	}
}

// Run listens for notifications until ctx is done, reconnecting with a short
// backoff whenever the LISTEN connection fails. It returns ctx.Err() on
// cancellation.
func (b *Broker) Run(ctx context.Context) error {
	for {
		err := b.listen(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reconnectDelay):
			}
			continue
		}
		// listen only returns without an error when the connection ends
		// cleanly; treat it as a reconnectable condition too.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

// listen holds a dedicated connection with LISTEN active and dispatches every
// notification it receives. The connection is released when listen returns.
func (b *Broker) listen(ctx context.Context) error {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listen connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ChannelName}.Sanitize()); err != nil {
		return fmt.Errorf("listen %s: %w", ChannelName, err)
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for notification: %w", err)
		}
		b.dispatch(notification.Payload)
	}
}

// dispatch decodes payload and delivers it to every subscriber without ever
// blocking: a subscriber whose buffer is full misses this event.
func (b *Broker) dispatch(payload string) {
	var ev Event
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		// A malformed payload is not retried and has no subscriber to report
		// to; ignore it so one bad publisher cannot stop the broker.
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Slow subscriber: drop the event for it.
		}
	}
}

// Subscribe returns a buffered channel of events and an idempotent unsubscribe
// function. unsubscribe removes the subscriber and closes the channel.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subscriberBuffer)

	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			close(ch)
			b.mu.Unlock()
		})
	}

	return ch, unsubscribe
}

// ServeSSE streams events to the client as Server-Sent Events until the request
// context is done. Each event is written as "event: <type>\ndata: <json>\n\n"
// and a ": ping" comment is sent every pingInterval.
func (b *Broker) ServeSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Write a body byte before waiting for the first event. Proxies buffer the
	// response until then, and the browser's EventSource does not fire onopen
	// until it has received the headers, so without this the stream appears
	// stuck connecting for a full ping interval.
	if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	eventsCh, unsubscribe := b.Subscribe()
	defer unsubscribe()

	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-eventsCh:
			if !open {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
