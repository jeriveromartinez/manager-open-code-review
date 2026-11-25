package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// deliveryBackoffBase is the wait after the first failed delivery. It
	// doubles per attempt up to deliveryBackoffMax, so a black-holed receiver
	// is not hammered and cannot stall every tick.
	deliveryBackoffBaseSeconds = 30
	deliveryBackoffMaxSeconds  = 1800
	deliveryBackoffBase        = deliveryBackoffBaseSeconds * time.Second
	deliveryBackoffMax         = deliveryBackoffMaxSeconds * time.Second
)

// DeliveryStore is the outbox between detection and delivery. An alert is
// persisted before the first POST, so a failed or interrupted delivery is
// retried instead of lost, and a restart resumes it (#154).
type DeliveryStore interface {
	// Enqueue records the alert. It must be idempotent by Alert.ID.
	Enqueue(ctx context.Context, alert Alert) error
	// Pending returns the alerts due for a delivery attempt at now, oldest
	// first.
	Pending(ctx context.Context, now time.Time, limit int) ([]Alert, error)
	// Active returns the latest alert of every problem whose latest event is
	// still a warning, so a restarted tracker does not re-alert an ongoing
	// incident.
	Active(ctx context.Context) ([]Alert, error)
	// MarkDelivered records a successful delivery and removes the alert from
	// the pending set.
	MarkDelivered(ctx context.Context, id string, at time.Time) error
	// MarkFailed records a delivery failure; the alert stays pending.
	MarkFailed(ctx context.Context, id string, at time.Time, cause string) error
}

// AlertID is the stable identity of one alert event. The tracker gives every
// fire, reminder and recovery its own At, so the id changes exactly when the
// event does; retrying the same event keeps the same id.
func AlertID(alert Alert) string {
	raw := alert.Code + "\x00" + alert.Subject + "\x00" + alert.Severity() + "\x00" +
		alert.At.UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// deliveryBackoff is how long to wait before retrying an alert that already
// failed the given number of times.
func deliveryBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	wait := deliveryBackoffBase
	for i := 1; i < attempts && wait < deliveryBackoffMax; i++ {
		wait *= 2
	}
	if wait > deliveryBackoffMax {
		wait = deliveryBackoffMax
	}
	return wait
}

func dueAt(alert Alert) time.Time {
	if alert.LastAttemptAt == nil {
		return time.Time{}
	}
	return alert.LastAttemptAt.Add(deliveryBackoff(alert.Attempts))
}

func alertKey(alert Alert) string {
	return alert.Code + "\x00" + alert.Subject
}

// MemoryDeliveryStore is the in-process outbox. It is the fallback when the
// monitor has no database, and the delivery behaviour it gives to tests is the
// same one the database store gives in production, except durability.
type MemoryDeliveryStore struct {
	mu          sync.Mutex
	seq         int64
	order       map[string]int64
	pending     map[string]Alert
	latest      map[string]Alert
	delivered   map[string]bool
	attempts    map[string]int
	lastAttempt map[string]time.Time
	lastError   map[string]string
}

func NewMemoryDeliveryStore() *MemoryDeliveryStore {
	return &MemoryDeliveryStore{
		order:       map[string]int64{},
		pending:     map[string]Alert{},
		latest:      map[string]Alert{},
		delivered:   map[string]bool{},
		attempts:    map[string]int{},
		lastAttempt: map[string]time.Time{},
		lastError:   map[string]string{},
	}
}

func (m *MemoryDeliveryStore) Enqueue(_ context.Context, alert Alert) error {
	if alert.ID == "" {
		return errors.New("alert id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.delivered[alert.ID] {
		return nil
	}
	if _, exists := m.pending[alert.ID]; exists {
		return nil
	}
	m.seq++
	m.order[alert.ID] = m.seq
	m.pending[alert.ID] = alert
	m.latest[alertKey(alert)] = alert
	return nil
}

func (m *MemoryDeliveryStore) Pending(_ context.Context, now time.Time, limit int) ([]Alert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	alerts := make([]Alert, 0, len(m.pending))
	for id, alert := range m.pending {
		alert.Attempts = m.attempts[id]
		alert.LastError = m.lastError[id]
		if attempted, ok := m.lastAttempt[id]; ok {
			alert.LastAttemptAt = &attempted
		}
		if due := dueAt(alert); !due.IsZero() && now.Before(due) {
			continue
		}
		alerts = append(alerts, alert)
	}
	sort.Slice(alerts, func(i, j int) bool {
		if !alerts[i].At.Equal(alerts[j].At) {
			return alerts[i].At.Before(alerts[j].At)
		}
		return m.order[alerts[i].ID] < m.order[alerts[j].ID]
	})
	if limit > 0 && len(alerts) > limit {
		alerts = alerts[:limit]
	}
	return alerts, nil
}

func (m *MemoryDeliveryStore) Active(_ context.Context) ([]Alert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var alerts []Alert
	for _, alert := range m.latest {
		if !alert.Recovered {
			alerts = append(alerts, alert)
		}
	}
	sort.Slice(alerts, func(i, j int) bool {
		if !alerts[i].At.Equal(alerts[j].At) {
			return alerts[i].At.Before(alerts[j].At)
		}
		return alertKey(alerts[i]) < alertKey(alerts[j])
	})
	return alerts, nil
}

func (m *MemoryDeliveryStore) MarkDelivered(_ context.Context, id string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, id)
	delete(m.lastError, id)
	m.delivered[id] = true
	return nil
}

func (m *MemoryDeliveryStore) MarkFailed(_ context.Context, id string, at time.Time, cause string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if alert, exists := m.pending[id]; exists {
		alert.Attempts++
		m.pending[id] = alert
		m.attempts[id]++
		m.lastAttempt[id] = at
		m.lastError[id] = cause
	}
	return nil
}

// PendingCount reports how many alerts are still waiting, for tests and logs.
func (m *MemoryDeliveryStore) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

// PGDeliveryStore persists the same outbox in PostgreSQL. The id primary key
// makes Enqueue idempotent, and delivered rows are never selected again.
type PGDeliveryStore struct {
	Pool *pgxpool.Pool
}

func NewPGDeliveryStore(pool *pgxpool.Pool) *PGDeliveryStore {
	return &PGDeliveryStore{Pool: pool}
}

func (s *PGDeliveryStore) Enqueue(ctx context.Context, alert Alert) error {
	values := alert.Values
	if values == nil {
		values = map[string]any{}
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode alert values: %w", err)
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO monitor_alert_deliveries
			(id, code, subject, message, values, alert_at, recovered)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		alert.ID, alert.Code, alert.Subject, alert.Message,
		raw, alert.At.UTC(), alert.Recovered)
	if err != nil {
		return fmt.Errorf("record alert %s: %w", alert.ID, err)
	}
	return nil
}

func (s *PGDeliveryStore) Pending(ctx context.Context, now time.Time, limit int) ([]Alert, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, code, subject, message, values, alert_at, recovered, attempts,
		       last_error, last_attempt_at
		FROM monitor_alert_deliveries
		WHERE delivered_at IS NULL
		  AND (
			last_attempt_at IS NULL
			OR last_attempt_at + (
				LEAST(`+fmt.Sprint(deliveryBackoffMaxSeconds)+`,
				      `+fmt.Sprint(deliveryBackoffBaseSeconds)+` * power(2, GREATEST(attempts - 1, 0)))::int
				* interval '1 second'
			) <= $1
		  )
		ORDER BY alert_at, created_at, id
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("read pending alerts: %w", err)
	}
	defer rows.Close()

	var alerts []Alert
	for rows.Next() {
		var (
			alert  Alert
			raw    []byte
			lastAt *time.Time
		)
		if err := rows.Scan(&alert.ID, &alert.Code, &alert.Subject, &alert.Message,
			&raw, &alert.At, &alert.Recovered, &alert.Attempts, &alert.LastError, &lastAt); err != nil {
			return nil, fmt.Errorf("scan pending alert: %w", err)
		}
		alert.LastAttemptAt = lastAt
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &alert.Values); err != nil {
				return nil, fmt.Errorf("decode alert %s values: %w", alert.ID, err)
			}
		}
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending alerts: %w", err)
	}
	return alerts, nil
}

func (s *PGDeliveryStore) Active(ctx context.Context) ([]Alert, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, code, subject, message, values, alert_at, recovered, attempts
		FROM (
			SELECT DISTINCT ON (code, subject)
			       id, code, subject, message, values, alert_at, recovered, attempts
			FROM monitor_alert_deliveries
			ORDER BY code, subject, alert_at DESC, created_at DESC, id DESC
		) latest
		WHERE NOT recovered`)
	if err != nil {
		return nil, fmt.Errorf("read active alerts: %w", err)
	}
	defer rows.Close()

	var alerts []Alert
	for rows.Next() {
		var (
			alert Alert
			raw   []byte
		)
		if err := rows.Scan(&alert.ID, &alert.Code, &alert.Subject, &alert.Message,
			&raw, &alert.At, &alert.Recovered, &alert.Attempts); err != nil {
			return nil, fmt.Errorf("scan active alert: %w", err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &alert.Values); err != nil {
				return nil, fmt.Errorf("decode alert %s values: %w", alert.ID, err)
			}
		}
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read active alerts: %w", err)
	}
	return alerts, nil
}

func (s *PGDeliveryStore) MarkDelivered(ctx context.Context, id string, at time.Time) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE monitor_alert_deliveries
		SET delivered_at = $2, last_error = '', last_attempt_at = $2
		WHERE id = $1 AND delivered_at IS NULL`, id, at.UTC())
	if err != nil {
		return fmt.Errorf("mark alert %s delivered: %w", id, err)
	}
	return nil
}

func (s *PGDeliveryStore) MarkFailed(ctx context.Context, id string, at time.Time, cause string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE monitor_alert_deliveries
		SET attempts = attempts + 1, last_error = $3, last_attempt_at = $2
		WHERE id = $1 AND delivered_at IS NULL`, id, at.UTC(), cause)
	if err != nil {
		return fmt.Errorf("mark alert %s failed: %w", id, err)
	}
	return nil
}
