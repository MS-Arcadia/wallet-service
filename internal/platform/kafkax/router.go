package kafkax

import (
	"context"
	"log/slog"
	"sync"

	"github.com/MS-Arcadia/wallet-service/internal/platform/event"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
)

// Router dispatches an event to the handler registered for its type.
//
// A single topic carries many event types — `wallet-events` alone carries
// WalletDebited, WalletCredited, PaymentFailed and more — so one consumer needs a
// way to fan out by type. Unknown types are ignored rather than dead-lettered:
// another service adding a new event to a shared topic must not break us.
type Router struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	logger   *slog.Logger
	// onUnknown decides what happens to an unregistered event type.
	onUnknown UnknownPolicy
}

// UnknownPolicy controls the treatment of unrecognised event types.
type UnknownPolicy int

const (
	// IgnoreUnknown skips the event and commits the offset. This is the right
	// default on a shared topic.
	IgnoreUnknown UnknownPolicy = iota
	// DeadLetterUnknown routes the event to the DLQ. Use it on a dedicated command
	// topic, where an unrecognised command really is a contract violation.
	DeadLetterUnknown
)

// NewRouter returns an empty Router.
func NewRouter(logger *slog.Logger, policy UnknownPolicy) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	return &Router{
		handlers:  make(map[string]Handler),
		logger:    logger,
		onUnknown: policy,
	}
}

// On registers a handler for an event type. Registering the same type twice
// replaces the previous handler.
func (r *Router) On(eventType string, handler Handler) *Router {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[eventType] = handler
	return r
}

// OnFunc registers a function handler.
func (r *Router) OnFunc(eventType string, fn HandlerFunc) *Router {
	return r.On(eventType, fn)
}

// Handle implements Handler.
func (r *Router) Handle(ctx context.Context, envelope event.Envelope) error {
	r.mu.RLock()
	handler, found := r.handlers[envelope.EventType]
	r.mu.RUnlock()

	if !found {
		if r.onUnknown == DeadLetterUnknown {
			// A non-retryable error routes straight to the DLQ.
			return unknownEventError{eventType: envelope.EventType}
		}
		logx.FromContext(ctx).Debug("ignoring event with no registered handler",
			slog.String("event_type", envelope.EventType),
			slog.String("event_id", envelope.EventID),
		)
		return nil
	}
	return handler.Handle(ctx, envelope)
}

// EventTypes returns the registered types, for logging at boot.
func (r *Router) EventTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		types = append(types, t)
	}
	return types
}
