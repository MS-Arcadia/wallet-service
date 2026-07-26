// Package publisher implements the outbound event port on top of the transactional
// outbox.
//
// The application layer publishes domain events by name and knows nothing about
// Kafka. This adapter is where an event type becomes a topic, which means a topic
// rename or a re-routing is an infrastructure change rather than a change to a use
// case.
package publisher

import (
	"context"
	"strings"

	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/event"
	"github.com/MS-Arcadia/wallet-service/internal/platform/outbox"
)

// Topics names the Kafka topics this service produces to.
type Topics struct {
	// WalletEvents carries the domain events other services subscribe to. The
	// architecture document calls this `wallet-events`.
	WalletEvents string
	// AuditEvents carries the immutable financial audit trail, which is archived to
	// the WORM sink rather than consumed by a service.
	AuditEvents string
}

// OutboxPublisher writes events into the outbox table.
type OutboxPublisher struct {
	store  *outbox.Store
	topics Topics
	// notify wakes the dispatcher after a commit so a single event does not wait for
	// the next poll tick.
	notify func()
}

// New builds an OutboxPublisher. The notify function is the dispatcher's Notify.
func New(store *outbox.Store, topics Topics, notify func()) *OutboxPublisher {
	if topics.WalletEvents == "" {
		topics.WalletEvents = "wallet-events"
	}
	if topics.AuditEvents == "" {
		topics.AuditEvents = "audit-events"
	}
	if notify == nil {
		notify = func() {}
	}
	return &OutboxPublisher{store: store, topics: topics, notify: notify}
}

// Publish appends envelopes to the outbox inside the caller's transaction.
//
// This is the whole point of the pattern: the rows land in the same transaction as
// the balance change that produced them, so the two either both commit or both
// vanish. There is no window in which a wallet has been debited but nothing knows.
func (p *OutboxPublisher) Publish(ctx context.Context, tx port.Tx, envelopes ...event.Envelope) error {
	for _, envelope := range envelopes {
		topic := p.topicFor(envelope.EventType)
		if err := p.store.Add(ctx, tx, topic, envelope); err != nil {
			return errs.Wrap(err, "failed to queue %s for topic %s", envelope.EventType, topic)
		}
	}
	return nil
}

// Notify wakes the outbox dispatcher. It must only be called after the transaction
// has committed; calling it earlier would send the dispatcher looking for rows that
// are not visible to it yet.
func (p *OutboxPublisher) Notify() { p.notify() }

// topicFor routes an event type to a topic.
func (p *OutboxPublisher) topicFor(eventType string) string {
	// Audit records are the only events that leave the domain stream: they go to a
	// long-retention topic that feeds the immutable audit sink, and no service
	// subscribes to them.
	if eventType == app.EventAuditRecorded || strings.HasSuffix(eventType, ".AuditRecorded") {
		return p.topics.AuditEvents
	}
	return p.topics.WalletEvents
}

var _ port.EventPublisher = (*OutboxPublisher)(nil)
