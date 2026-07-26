// Package event defines the wire envelope shared by every Arcadia domain event
// and integration command.
//
// The payload is service-specific, but the envelope is not: consumers rely on
// event_id for idempotency, on schema_version to decide whether they can decode
// the payload at all, and on correlation_id/trace_id to stitch a distributed
// saga back together in Tempo.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Envelope wraps every message published to Kafka.
type Envelope struct {
	// EventID uniquely identifies this event. Consumers deduplicate on it, which
	// is what turns the outbox's at-least-once delivery into effectively
	// exactly-once processing.
	EventID string `json:"event_id"`
	// EventType is the fully qualified name, e.g. "arcadia.wallet.v1.WalletDebited".
	EventType string `json:"event_type"`
	// SchemaVersion is the major version of the payload shape.
	SchemaVersion int `json:"schema_version"`
	// OccurredAt is when the domain fact happened, not when it was published.
	OccurredAt time.Time `json:"occurred_at"`
	// Producer names the emitting service.
	Producer string `json:"producer"`
	// AggregateType and AggregateID identify what changed. AggregateID doubles as
	// the Kafka partition key, which is what preserves per-wallet ordering.
	AggregateType string `json:"aggregate_type"`
	AggregateID   string `json:"aggregate_id"`
	// CorrelationID ties every message of one business transaction together.
	CorrelationID string `json:"correlation_id,omitempty"`
	// CausationID is the EventID of the message that triggered this one.
	CausationID string `json:"causation_id,omitempty"`
	// TraceID carries the correlation identifier across the broker, so a consumer's
	// log lines join up with the request that caused them.
	TraceID string `json:"trace_id,omitempty"`
	// Payload is the event-specific body.
	Payload json.RawMessage `json:"payload"`
}

// Validation errors.
var (
	ErrMissingEventID     = errors.New("event: event_id is required")
	ErrMissingEventType   = errors.New("event: event_type is required")
	ErrMissingAggregateID = errors.New("event: aggregate_id is required")
	ErrMissingOccurredAt  = errors.New("event: occurred_at is required")
	ErrUnsupportedVersion = errors.New("event: unsupported schema version")
	ErrMalformedEnvelope  = errors.New("event: malformed envelope")
)

// Validate checks the envelope's structural invariants.
func (e Envelope) Validate() error {
	switch {
	case e.EventID == "":
		return ErrMissingEventID
	case e.EventType == "":
		return ErrMissingEventType
	case e.AggregateID == "":
		return ErrMissingAggregateID
	case e.OccurredAt.IsZero():
		return ErrMissingOccurredAt
	case e.SchemaVersion <= 0:
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, e.SchemaVersion)
	}
	return nil
}

// PartitionKey returns the key Kafka should partition on. Keying by aggregate
// guarantees that two events about the same wallet are never processed out of
// order by two consumers in the same group.
func (e Envelope) PartitionKey() string { return e.AggregateID }

// Marshal serialises the envelope.
func (e Envelope) Marshal() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("event: marshal envelope %s: %w", e.EventType, err)
	}
	return data, nil
}

// Unmarshal parses and validates an envelope off the wire.
func Unmarshal(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrMalformedEnvelope, err)
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// DecodePayload unmarshals the payload into target.
func (e Envelope) DecodePayload(target any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("event: %s has an empty payload", e.EventType)
	}
	if err := json.Unmarshal(e.Payload, target); err != nil {
		return fmt.Errorf("event: decode %s payload: %w", e.EventType, err)
	}
	return nil
}

// Builder assembles envelopes with the producer name and schema version already
// filled in, so that a use case only supplies what is domain-specific.
type Builder struct {
	producer      string
	schemaVersion int
}

// NewBuilder returns a Builder for the given producer.
func NewBuilder(producer string, schemaVersion int) *Builder {
	if schemaVersion <= 0 {
		schemaVersion = 1
	}
	return &Builder{producer: producer, schemaVersion: schemaVersion}
}

// Build creates an envelope around payload, which is marshalled to JSON.
func (b *Builder) Build(
	eventID, eventType, aggregateType, aggregateID string,
	occurredAt time.Time,
	payload any,
) (Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("event: marshal %s payload: %w", eventType, err)
	}
	envelope := Envelope{
		EventID:       eventID,
		EventType:     eventType,
		SchemaVersion: b.schemaVersion,
		OccurredAt:    occurredAt.UTC(),
		Producer:      b.producer,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Payload:       body,
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}
