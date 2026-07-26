package kafkax

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/event"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
	"github.com/segmentio/kafka-go"
)

// Handler processes one decoded event.
//
// The contract is deliberate about failure. Returning a non-retryable error (see
// errs.IsRetryable) sends the message straight to the dead-letter topic, because
// replaying a message that violates a business rule will fail identically
// forever. Returning a retryable error causes bounded in-process retries and then
// dead-lettering.
type Handler interface {
	Handle(ctx context.Context, envelope event.Envelope) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, envelope event.Envelope) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, envelope event.Envelope) error {
	return f(ctx, envelope)
}

// ConsumerConfig configures a Consumer.
type ConsumerConfig struct {
	Brokers []string
	// GroupID is the consumer group. All replicas of one service share it so that
	// partitions are divided between them.
	GroupID string
	// Topics is the set of topics to subscribe to.
	Topics []string
	// DLQTopic receives messages that could not be processed. Empty means
	// "<topic>.dlq".
	DLQTopic string
	// MaxRetries is how many times one message is retried in-process before being
	// dead-lettered.
	MaxRetries int
	// RetryBackoff is the base delay between in-process retries; it doubles.
	RetryBackoff time.Duration
	// HandlerTimeout bounds a single Handle call.
	HandlerTimeout time.Duration
	// MinBytes/MaxBytes tune fetch sizing.
	MinBytes int
	MaxBytes int
	// MaxWait is how long a fetch waits for MinBytes before returning.
	MaxWait time.Duration
	// StartFromOldest reads a new group from the beginning of the topic. Financial
	// consumers want this: skipping history would silently drop money movements.
	StartFromOldest bool
}

func (c ConsumerConfig) withDefaults() ConsumerConfig {
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = 200 * time.Millisecond
	}
	if c.HandlerTimeout <= 0 {
		c.HandlerTimeout = 30 * time.Second
	}
	if c.MinBytes <= 0 {
		c.MinBytes = 1
	}
	if c.MaxBytes <= 0 {
		c.MaxBytes = 10 << 20 // 10 MiB
	}
	if c.MaxWait <= 0 {
		c.MaxWait = 500 * time.Millisecond
	}
	return c
}

// ConsumerMetrics is the hook the consumer reports through.
type ConsumerMetrics interface {
	// EventConsumed records a processed message and how long it took.
	EventConsumed(topic, eventType string, duration time.Duration, err error)
	// EventDeadLettered records a message routed to the DLQ.
	EventDeadLettered(topic, eventType, reason string)
	// ConsumerLag records the gap between the high-water mark and our offset.
	ConsumerLag(topic string, lag int64)
}

type nopConsumerMetrics struct{}

func (nopConsumerMetrics) EventConsumed(string, string, time.Duration, error) {}
func (nopConsumerMetrics) EventDeadLettered(string, string, string)           {}
func (nopConsumerMetrics) ConsumerLag(string, int64)                          {}

// Consumer reads one topic group and dispatches to a Handler.
type Consumer struct {
	reader   *kafka.Reader
	handler  Handler
	dlq      *Producer
	dlqTopic string
	cfg      ConsumerConfig
	logger   *slog.Logger
	metrics  ConsumerMetrics
}

// NewConsumer builds a Consumer. The dlq producer may be nil, in which case
// undeliverable messages are logged and the offset is committed — losing them,
// which is why every financial topic must be given a DLQ producer.
func NewConsumer(cfg ConsumerConfig, handler Handler, dlq *Producer, logger *slog.Logger, metrics ConsumerMetrics) (*Consumer, error) {
	cfg = cfg.withDefaults()
	switch {
	case len(cfg.Brokers) == 0:
		return nil, errors.New("kafkax: at least one broker is required")
	case cfg.GroupID == "":
		return nil, errors.New("kafkax: group id is required")
	case len(cfg.Topics) == 0:
		return nil, errors.New("kafkax: at least one topic is required")
	case handler == nil:
		return nil, errors.New("kafkax: handler is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = nopConsumerMetrics{}
	}

	startOffset := kafka.LastOffset
	if cfg.StartFromOldest {
		startOffset = kafka.FirstOffset
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:     cfg.Brokers,
		GroupID:     cfg.GroupID,
		GroupTopics: cfg.Topics,
		MinBytes:    cfg.MinBytes,
		MaxBytes:    cfg.MaxBytes,
		MaxWait:     cfg.MaxWait,
		StartOffset: startOffset,
		// Offsets are committed explicitly after a message is handled. Auto-commit
		// would acknowledge messages we have not processed yet, turning a crash into
		// silent data loss.
		CommitInterval: 0,
	})

	dlqTopic := cfg.DLQTopic
	if dlqTopic == "" && len(cfg.Topics) == 1 {
		dlqTopic = cfg.Topics[0] + ".dlq"
	}

	return &Consumer{
		reader:   reader,
		handler:  handler,
		dlq:      dlq,
		dlqTopic: dlqTopic,
		cfg:      cfg,
		logger: logger.With(
			slog.String("component", "kafka-consumer"),
			slog.String("group", cfg.GroupID),
			slog.String("topics", strings.Join(cfg.Topics, ",")),
		),
		metrics: metrics,
	}, nil
}

// Run consumes until ctx is canceled.
func (c *Consumer) Run(ctx context.Context) error {
	c.logger.Info("kafka consumer started")
	defer c.logger.Info("kafka consumer stopped")

	for {
		message, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ctx.Err()) {
				return nil
			}
			// A fetch failure is almost always a transient broker or rebalance issue.
			// Back off briefly rather than spinning.
			c.logger.Error("failed to fetch message", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}

		c.metrics.ConsumerLag(message.Topic, c.reader.Lag())
		c.process(ctx, message)

		// The offset is committed whether the message succeeded or was
		// dead-lettered: in both cases we are done with it. Not committing would
		// replay the message forever and stall the partition.
		if err := c.reader.CommitMessages(ctx, message); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			c.logger.Error("failed to commit offset",
				slog.String("topic", message.Topic),
				slog.Int("partition", message.Partition),
				slog.Int64("offset", message.Offset),
				slog.String("error", err.Error()),
			)
		}
	}
}

func (c *Consumer) process(ctx context.Context, message kafka.Message) {
	start := time.Now()

	envelope, err := event.Unmarshal(message.Value)
	if err != nil {
		// An undecodable message can never succeed. Dead-letter it immediately.
		c.logger.Error("dropping undecodable message",
			slog.String("topic", message.Topic),
			slog.Int64("offset", message.Offset),
			slog.String("error", err.Error()),
		)
		c.deadLetter(ctx, message, "", "malformed envelope: "+err.Error())
		return
	}

	ctx = c.enrichContext(ctx, envelope, message)
	logger := logx.FromContext(ctx).With(
		slog.String("event_id", envelope.EventID),
		slog.String("event_type", envelope.EventType),
		slog.String("topic", message.Topic),
	)

	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := c.cfg.RetryBackoff * (1 << (attempt - 1))
			logger.Warn("retrying event",
				slog.Int("attempt", attempt+1),
				slog.Duration("delay", delay),
				slog.String("error", lastErr.Error()),
			)
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		handlerCtx, cancel := context.WithTimeout(ctx, c.cfg.HandlerTimeout)
		lastErr = c.handler.Handle(handlerCtx, envelope)
		cancel()

		if lastErr == nil {
			c.metrics.EventConsumed(message.Topic, envelope.EventType, time.Since(start), nil)
			logger.Debug("event handled", slog.Duration("duration", time.Since(start)))
			return
		}

		if !errs.IsRetryable(lastErr) {
			// A business rejection will fail the same way on every replay. Park it for
			// an operator instead of burning retries.
			logger.Error("event permanently rejected",
				slog.String("error", lastErr.Error()),
				slog.String("code", string(errs.CodeOf(lastErr))),
			)
			break
		}
	}

	c.metrics.EventConsumed(message.Topic, envelope.EventType, time.Since(start), lastErr)
	c.deadLetter(ctx, message, envelope.EventType, lastErr.Error())
}

// enrichContext restores the correlation and trace identifiers that the producer
// attached, so that logs on this side of the broker join up with the ones that
// caused them.
func (c *Consumer) enrichContext(ctx context.Context, envelope event.Envelope, message kafka.Message) context.Context {
	correlationID := envelope.CorrelationID
	if correlationID == "" {
		correlationID = headerValue(message, "correlation_id")
	}
	if correlationID == "" {
		// Fall back to the event id so that every log line can still be tied to one
		// unit of work.
		correlationID = envelope.EventID
	}
	return logx.WithCorrelationID(ctx, correlationID)
}

func headerValue(message kafka.Message, key string) string {
	for _, header := range message.Headers {
		if strings.EqualFold(header.Key, key) {
			return string(header.Value)
		}
	}
	return ""
}

// deadLetter forwards a message to the DLQ topic, preserving the original
// payload and annotating it with the failure reason and provenance.
func (c *Consumer) deadLetter(ctx context.Context, message kafka.Message, eventType, reason string) {
	c.metrics.EventDeadLettered(message.Topic, eventType, reason)

	if c.dlq == nil || c.dlqTopic == "" {
		c.logger.Error("no dead-letter topic configured; message is being dropped",
			slog.String("topic", message.Topic),
			slog.Int64("offset", message.Offset),
			slog.String("reason", reason),
		)
		return
	}

	headers := map[string]string{
		"dlq_reason":          reason,
		"dlq_original_topic":  message.Topic,
		"dlq_original_offset": fmt.Sprintf("%d", message.Offset),
		"dlq_failed_at":       time.Now().UTC().Format(time.RFC3339),
		"event_type":          eventType,
	}

	// Use a fresh context: the caller's may already be canceled by shutdown, and
	// dropping the message would be worse than a slightly delayed exit.
	dlqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := c.dlq.Publish(dlqCtx, c.dlqTopic, string(message.Key), message.Value, headers); err != nil {
		c.logger.Error("failed to dead-letter message",
			slog.String("dlq_topic", c.dlqTopic),
			slog.String("original_topic", message.Topic),
			slog.Int64("offset", message.Offset),
			slog.String("error", err.Error()),
		)
		return
	}

	c.logger.Warn("message dead-lettered",
		slog.String("dlq_topic", c.dlqTopic),
		slog.String("original_topic", message.Topic),
		slog.Int64("offset", message.Offset),
		slog.String("reason", reason),
	)
}

// Close releases the reader.
func (c *Consumer) Close() error {
	if err := c.reader.Close(); err != nil {
		return fmt.Errorf("kafkax: close consumer: %w", err)
	}
	return nil
}

// Lag returns the current consumer lag, which feeds the "consumer lag < 1000"
// SLO alert.
func (c *Consumer) Lag() int64 { return c.reader.Lag() }
