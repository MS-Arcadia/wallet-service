// Package outbox implements the Transactional Outbox pattern.
//
// The problem it solves: a use case must change state *and* tell the rest of the
// platform about it. Writing to Postgres and publishing to Kafka are two
// separate systems, so a crash between them either loses the event or announces
// something that never happened.
//
// The fix is to make the announcement part of the same database transaction. The
// use case inserts a row into `outbox_messages` through the very pgx.Tx that
// carries its aggregate write, so both commit or neither does. A background
// dispatcher then drains the table into Kafka. Delivery is at-least-once, which
// the receiving side turns into effectively exactly-once via the inbox package.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/event"
	"github.com/jackc/pgx/v5"
)

// Status is the lifecycle of an outbox row.
type Status string

const (
	// StatusPending — waiting to be published.
	StatusPending Status = "PENDING"
	// StatusPublished — acknowledged by the broker.
	StatusPublished Status = "PUBLISHED"
	// StatusFailed — exhausted its retries and needs an operator. These rows are
	// what the "DLQ depth ≈ 0" alert watches.
	StatusFailed Status = "FAILED"
)

// Message is one row of the outbox table.
type Message struct {
	ID            string
	Topic         string
	PartitionKey  string
	EventID       string
	EventType     string
	Payload       []byte
	Status        Status
	AttemptCount  int
	LastError     string
	CreatedAt     time.Time
	AvailableAt   time.Time
	PublishedAt   *time.Time
	CorrelationID string
	TraceID       string
}

// Writer records events inside a caller-supplied transaction. Use cases depend
// on this narrow interface, never on the table.
type Writer interface {
	// Add appends one event to the outbox within tx.
	Add(ctx context.Context, tx pgx.Tx, topic string, envelope event.Envelope) error
	// AddAll appends several events within tx.
	AddAll(ctx context.Context, tx pgx.Tx, topic string, envelopes ...event.Envelope) error
}

// Publisher hands a message to the broker. Implemented by pkg/kafkax.
type Publisher interface {
	// Publish delivers payload to topic under key, returning an error if the
	// broker did not acknowledge the write.
	Publish(ctx context.Context, topic, key string, payload []byte, headers map[string]string) error
}

// Store is the Postgres-backed outbox.
type Store struct {
	// maxAttempts bounds retries before a row is parked as FAILED.
	maxAttempts int
}

// NewStore returns a Store. maxAttempts of 0 falls back to 10.
func NewStore(maxAttempts int) *Store {
	if maxAttempts <= 0 {
		maxAttempts = 10
	}
	return &Store{maxAttempts: maxAttempts}
}

const insertMessage = `
INSERT INTO outbox_messages (
    id, topic, partition_key, event_id, event_type, payload,
    status, attempt_count, created_at, available_at, correlation_id, trace_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8, $8, $9, $10)
ON CONFLICT (event_id) DO NOTHING`

// Add implements Writer.
//
// The ON CONFLICT clause makes this safe against a retried use case: the same
// event id is never queued twice.
func (s *Store) Add(ctx context.Context, tx pgx.Tx, topic string, envelope event.Envelope) error {
	if topic == "" {
		return errors.New("outbox: topic is required")
	}
	if err := envelope.Validate(); err != nil {
		return fmt.Errorf("outbox: %w", err)
	}

	payload, err := envelope.Marshal()
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, insertMessage,
		envelope.EventID, // the event id doubles as the row id: one row per event
		topic,
		envelope.PartitionKey(),
		envelope.EventID,
		envelope.EventType,
		payload,
		StatusPending,
		envelope.OccurredAt.UTC(),
		nullIfEmpty(envelope.CorrelationID),
		nullIfEmpty(envelope.TraceID),
	)
	if err != nil {
		return fmt.Errorf("outbox: insert %s: %w", envelope.EventType, err)
	}
	return nil
}

// AddAll implements Writer.
func (s *Store) AddAll(ctx context.Context, tx pgx.Tx, topic string, envelopes ...event.Envelope) error {
	for _, envelope := range envelopes {
		if err := s.Add(ctx, tx, topic, envelope); err != nil {
			return err
		}
	}
	return nil
}

// claimBatch locks a batch of due rows for this dispatcher instance.
//
// FOR UPDATE SKIP LOCKED is the whole trick behind running several dispatcher
// replicas safely: each transaction grabs rows nobody else holds and steps over
// the ones that are already claimed, so replicas share the work without ever
// publishing the same message twice concurrently.
const claimBatch = `
SELECT id, topic, partition_key, event_id, event_type, payload,
       attempt_count, created_at, available_at,
       coalesce(correlation_id, ''), coalesce(trace_id, '')
FROM outbox_messages
WHERE status = $1 AND available_at <= $2
ORDER BY available_at, created_at
LIMIT $3
FOR UPDATE SKIP LOCKED`

// Claim locks and returns up to limit due messages. It must be called inside a
// transaction; the lock is held until that transaction ends.
func (s *Store) Claim(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Message, error) {
	rows, err := tx.Query(ctx, claimBatch, StatusPending, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: claim batch: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(
			&m.ID, &m.Topic, &m.PartitionKey, &m.EventID, &m.EventType, &m.Payload,
			&m.AttemptCount, &m.CreatedAt, &m.AvailableAt, &m.CorrelationID, &m.TraceID,
		); err != nil {
			return nil, fmt.Errorf("outbox: scan message: %w", err)
		}
		m.Status = StatusPending
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: iterate batch: %w", err)
	}
	return messages, nil
}

// MarkPublished flags a message as delivered.
func (s *Store) MarkPublished(ctx context.Context, tx pgx.Tx, id string, at time.Time) error {
	_, err := tx.Exec(ctx,
		`UPDATE outbox_messages
		 SET status = $1, published_at = $2, attempt_count = attempt_count + 1, last_error = NULL
		 WHERE id = $3`,
		StatusPublished, at.UTC(), id,
	)
	if err != nil {
		return fmt.Errorf("outbox: mark %s published: %w", id, err)
	}
	return nil
}

// MarkFailed records a delivery failure and schedules the next attempt with
// exponential backoff. Once maxAttempts is reached the row is parked as FAILED
// so that a permanently broken message cannot spin forever.
func (s *Store) MarkFailed(ctx context.Context, tx pgx.Tx, m Message, cause error, now time.Time) error {
	attempts := m.AttemptCount + 1
	status := StatusPending
	nextAttempt := now.UTC().Add(Backoff(attempts))
	if attempts >= s.maxAttempts {
		status = StatusFailed
	}

	_, err := tx.Exec(ctx,
		`UPDATE outbox_messages
		 SET status = $1, attempt_count = $2, last_error = $3, available_at = $4
		 WHERE id = $5`,
		status, attempts, truncateError(cause), nextAttempt, m.ID,
	)
	if err != nil {
		return fmt.Errorf("outbox: mark %s failed: %w", m.ID, err)
	}
	return nil
}

// Backoff returns the delay before attempt number n (1-based): 1s, 2s, 4s, ...
// capped at five minutes.
func Backoff(attempt int) time.Duration {
	const (
		base = time.Second
		max  = 5 * time.Minute
	)
	if attempt <= 1 {
		return base
	}
	if attempt > 20 {
		return max
	}
	delay := base << (attempt - 1)
	if delay > max || delay <= 0 {
		return max
	}
	return delay
}

// Stats summarises the table for metrics and health checks.
type Stats struct {
	Pending   int64
	Failed    int64
	OldestAge time.Duration
}

// Stats reads the current backlog.
func (s *Store) Stats(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, now time.Time) (Stats, error) {
	var stats Stats
	var oldest *time.Time
	err := q.QueryRow(ctx,
		`SELECT
		   count(*) FILTER (WHERE status = $1),
		   count(*) FILTER (WHERE status = $2),
		   min(created_at) FILTER (WHERE status = $1)
		 FROM outbox_messages`,
		StatusPending, StatusFailed,
	).Scan(&stats.Pending, &stats.Failed, &oldest)
	if err != nil {
		return Stats{}, fmt.Errorf("outbox: read stats: %w", err)
	}
	if oldest != nil {
		stats.OldestAge = now.UTC().Sub(oldest.UTC())
	}
	return stats, nil
}

// PurgePublished deletes delivered rows older than the retention window, keeping
// the hot table small enough that the dispatcher's index scan stays cheap.
func (s *Store) PurgePublished(ctx context.Context, tx pgx.Tx, before time.Time) (int64, error) {
	tag, err := tx.Exec(ctx,
		`DELETE FROM outbox_messages WHERE status = $1 AND published_at < $2`,
		StatusPublished, before.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("outbox: purge published: %w", err)
	}
	return tag.RowsAffected(), nil
}

const maxStoredErrorLength = 1000

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > maxStoredErrorLength {
		return msg[:maxStoredErrorLength]
	}
	return msg
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// DecodeEnvelope parses a stored payload back into an envelope, which the
// dispatcher needs in order to restore headers before publishing.
func DecodeEnvelope(payload []byte) (event.Envelope, error) {
	var envelope event.Envelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return event.Envelope{}, fmt.Errorf("outbox: decode stored envelope: %w", err)
	}
	return envelope, nil
}
