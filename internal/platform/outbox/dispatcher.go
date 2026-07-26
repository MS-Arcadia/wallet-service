package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/clock"
	"github.com/MS-Arcadia/wallet-service/internal/platform/postgres"
	"github.com/jackc/pgx/v5"
)

// DispatcherConfig tunes the polling loop.
type DispatcherConfig struct {
	// PollInterval is how long to wait after an empty poll. Latency for a single
	// event is roughly half this value, so keep it short.
	PollInterval time.Duration
	// BatchSize is how many messages one iteration claims.
	BatchSize int
	// PublishTimeout bounds a single broker write.
	PublishTimeout time.Duration
	// PurgeInterval is how often delivered rows are swept. Zero disables sweeping.
	PurgeInterval time.Duration
	// PurgeRetention is how long a delivered row is kept for debugging.
	PurgeRetention time.Duration
}

func (c DispatcherConfig) withDefaults() DispatcherConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = 500 * time.Millisecond
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.PublishTimeout <= 0 {
		c.PublishTimeout = 10 * time.Second
	}
	if c.PurgeRetention <= 0 {
		c.PurgeRetention = 7 * 24 * time.Hour
	}
	return c
}

// Metrics is the hook the dispatcher reports through. pkg/metrics implements it.
type Metrics interface {
	// OutboxPublished counts successful deliveries per topic.
	OutboxPublished(topic string, count int)
	// OutboxFailed counts delivery failures per topic.
	OutboxFailed(topic string, count int)
	// OutboxBacklog reports the current pending/failed depth.
	OutboxBacklog(pending, failed int64, oldestAge time.Duration)
}

type nopMetrics struct{}

func (nopMetrics) OutboxPublished(string, int)               {}
func (nopMetrics) OutboxFailed(string, int)                  {}
func (nopMetrics) OutboxBacklog(int64, int64, time.Duration) {}

// Dispatcher drains the outbox table into the broker.
//
// It runs as a goroutine inside the service rather than as a separate deployment:
// one fewer moving part, and the dispatcher shares the service's connection pool
// and shutdown lifecycle. Several replicas may run concurrently — FOR UPDATE SKIP
// LOCKED keeps them from colliding.
type Dispatcher struct {
	store     *Store
	txm       postgres.TxManager
	publisher Publisher
	clock     clock.Clock
	logger    *slog.Logger
	metrics   Metrics
	cfg       DispatcherConfig

	// notify lets a use case wake the dispatcher the moment it commits, so the
	// happy path does not wait for the next poll tick.
	notify chan struct{}

	published atomic.Int64
	failed    atomic.Int64
}

// NewDispatcher assembles a Dispatcher.
func NewDispatcher(
	store *Store,
	txm postgres.TxManager,
	publisher Publisher,
	clk clock.Clock,
	logger *slog.Logger,
	metrics Metrics,
	cfg DispatcherConfig,
) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = nopMetrics{}
	}
	if clk == nil {
		clk = clock.System{}
	}
	return &Dispatcher{
		store:     store,
		txm:       txm,
		publisher: publisher,
		clock:     clk,
		logger:    logger.With(slog.String("component", "outbox-dispatcher")),
		metrics:   metrics,
		cfg:       cfg.withDefaults(),
		notify:    make(chan struct{}, 1),
	}
}

// Notify signals that new messages are available. It never blocks.
func (d *Dispatcher) Notify() {
	select {
	case d.notify <- struct{}{}:
	default:
		// A wake-up is already queued; one is enough.
	}
}

// Run polls until ctx is canceled. It is intended to be started in its own
// goroutine and returns nil on a clean shutdown.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.logger.Info("outbox dispatcher started",
		slog.Duration("poll_interval", d.cfg.PollInterval),
		slog.Int("batch_size", d.cfg.BatchSize),
	)

	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	var purgeTicker *time.Ticker
	var purgeC <-chan time.Time
	if d.cfg.PurgeInterval > 0 {
		purgeTicker = time.NewTicker(d.cfg.PurgeInterval)
		defer purgeTicker.Stop()
		purgeC = purgeTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("outbox dispatcher stopped",
				slog.Int64("published_total", d.published.Load()),
				slog.Int64("failed_total", d.failed.Load()),
			)
			return nil

		case <-purgeC:
			d.purge(ctx)

		case <-ticker.C:
			d.drain(ctx)

		case <-d.notify:
			d.drain(ctx)
		}
	}
}

// drain keeps dispatching while full batches come back, so a burst is cleared in
// one pass instead of one batch per tick.
func (d *Dispatcher) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		dispatched, err := d.DispatchBatch(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				d.logger.Error("outbox batch failed", slog.String("error", err.Error()))
			}
			return
		}
		if dispatched < d.cfg.BatchSize {
			return
		}
	}
}

// DispatchBatch claims and publishes one batch, returning how many rows it
// handled. It is exported so that tests and the admin API can step the loop
// deterministically.
func (d *Dispatcher) DispatchBatch(ctx context.Context) (int, error) {
	var handled int

	err := d.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		now := d.clock.Now()
		messages, err := d.store.Claim(ctx, tx, now, d.cfg.BatchSize)
		if err != nil {
			return err
		}
		handled = len(messages)
		if handled == 0 {
			return nil
		}

		for _, message := range messages {
			// The row stays locked for the whole publish. That keeps a second
			// dispatcher from racing us, at the cost of holding the transaction open
			// for the duration of the broker write — which is why PublishTimeout is
			// deliberately short.
			publishCtx, cancel := context.WithTimeout(ctx, d.cfg.PublishTimeout)
			publishErr := d.publisher.Publish(publishCtx, message.Topic, message.PartitionKey, message.Payload, headersFor(message))
			cancel()

			if publishErr != nil {
				d.failed.Add(1)
				d.metrics.OutboxFailed(message.Topic, 1)
				d.logger.Warn("failed to publish outbox message",
					slog.String("event_id", message.EventID),
					slog.String("event_type", message.EventType),
					slog.String("topic", message.Topic),
					slog.Int("attempt", message.AttemptCount+1),
					slog.String("error", publishErr.Error()),
				)
				if err := d.store.MarkFailed(ctx, tx, message, publishErr, d.clock.Now()); err != nil {
					return err
				}
				continue
			}

			if err := d.store.MarkPublished(ctx, tx, message.ID, d.clock.Now()); err != nil {
				return err
			}
			d.published.Add(1)
			d.metrics.OutboxPublished(message.Topic, 1)
			d.logger.Debug("published outbox message",
				slog.String("event_id", message.EventID),
				slog.String("event_type", message.EventType),
				slog.String("topic", message.Topic),
			)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return handled, nil
}

// ReportBacklog publishes current depth to the metrics sink. The scheduler calls
// this periodically; the "DLQ depth ≈ 0" alert is built on it.
func (d *Dispatcher) ReportBacklog(ctx context.Context) error {
	var stats Stats
	err := d.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stats, err = d.store.Stats(ctx, tx, d.clock.Now())
		return err
	})
	if err != nil {
		return err
	}

	d.metrics.OutboxBacklog(stats.Pending, stats.Failed, stats.OldestAge)
	if stats.Failed > 0 {
		d.logger.Warn("outbox has permanently failed messages requiring an operator",
			slog.Int64("failed", stats.Failed))
	}
	return nil
}

func (d *Dispatcher) purge(ctx context.Context) {
	before := d.clock.Now().Add(-d.cfg.PurgeRetention)
	err := d.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		deleted, err := d.store.PurgePublished(ctx, tx, before)
		if err != nil {
			return err
		}
		if deleted > 0 {
			d.logger.Info("purged published outbox rows", slog.Int64("deleted", deleted))
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		d.logger.Error("failed to purge outbox", slog.String("error", err.Error()))
	}
}

// headersFor builds the Kafka headers that carry correlation across the broker.
func headersFor(m Message) map[string]string {
	headers := map[string]string{
		"event_id":   m.EventID,
		"event_type": m.EventType,
	}
	if m.CorrelationID != "" {
		headers["correlation_id"] = m.CorrelationID
	}
	if m.TraceID != "" {
		headers["trace_id"] = m.TraceID
	}
	return headers
}
