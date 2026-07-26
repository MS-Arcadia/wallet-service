// Package kafkax wraps segmentio/kafka-go with the service's conventions:
// acks=all producers, per-topic dead-letter routing, bounded retries with backoff,
// and the correlation id carried through message headers.
package kafkax

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/segmentio/kafka-go"
)

// ProducerConfig configures a Producer.
type ProducerConfig struct {
	// Brokers is the bootstrap list.
	Brokers []string
	// ClientID identifies this producer in broker logs.
	ClientID string
	// WriteTimeout bounds a single produce request.
	WriteTimeout time.Duration
	// BatchTimeout is how long the writer waits to fill a batch. Financial events
	// want this small; throughput-oriented topics can afford more.
	BatchTimeout time.Duration
	// MaxAttempts is how many times kafka-go retries a produce internally.
	MaxAttempts int
	// Compression enables snappy compression when true.
	Compression bool
}

func (c ProducerConfig) withDefaults() ProducerConfig {
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.BatchTimeout <= 0 {
		c.BatchTimeout = 50 * time.Millisecond
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	return c
}

// Producer publishes messages to Kafka. It satisfies outbox.Publisher.
type Producer struct {
	writer *kafka.Writer
	logger *slog.Logger
}

// NewProducer builds a Producer.
//
// RequireAll is not negotiable for this platform: a financial event that only
// reached the partition leader is an event that a leader failover can lose.
func NewProducer(cfg ProducerConfig, logger *slog.Logger) (*Producer, error) {
	cfg = cfg.withDefaults()
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("kafkax: at least one broker is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	writer := &kafka.Writer{
		Addr:         kafka.TCP(cfg.Brokers...),
		Balancer:     &kafka.Hash{}, // hash the key so one aggregate keeps one partition
		RequiredAcks: kafka.RequireAll,
		MaxAttempts:  cfg.MaxAttempts,
		WriteTimeout: cfg.WriteTimeout,
		BatchTimeout: cfg.BatchTimeout,
		// Async would return before the broker acknowledges, which would defeat the
		// outbox: the dispatcher must not mark a row PUBLISHED until it truly is.
		Async:     false,
		Transport: &kafka.Transport{ClientID: cfg.ClientID},
	}
	if cfg.Compression {
		writer.Compression = kafka.Snappy
	}

	return &Producer{writer: writer, logger: logger.With(slog.String("component", "kafka-producer"))}, nil
}

// Publish writes one message and waits for the broker's acknowledgement.
func (p *Producer) Publish(ctx context.Context, topic, key string, payload []byte, headers map[string]string) error {
	if topic == "" {
		return errors.New("kafkax: topic is required")
	}

	msg := kafka.Message{
		Topic: topic,
		Key:   []byte(key),
		Value: payload,
	}
	if len(headers) > 0 {
		msg.Headers = make([]kafka.Header, 0, len(headers))
		for k, v := range headers {
			msg.Headers = append(msg.Headers, kafka.Header{Key: k, Value: []byte(v)})
		}
	}

	if err := p.writer.WriteMessages(ctx, msg); err != nil {
		return fmt.Errorf("kafkax: publish to %s: %w", topic, err)
	}
	return nil
}

// PublishBatch writes several messages, which may span topics.
func (p *Producer) PublishBatch(ctx context.Context, messages []kafka.Message) error {
	if len(messages) == 0 {
		return nil
	}
	if err := p.writer.WriteMessages(ctx, messages...); err != nil {
		return fmt.Errorf("kafkax: publish batch of %d: %w", len(messages), err)
	}
	return nil
}

// Close flushes and releases the writer.
func (p *Producer) Close() error {
	if err := p.writer.Close(); err != nil {
		return fmt.Errorf("kafkax: close producer: %w", err)
	}
	return nil
}

// Stats exposes writer counters for the metrics endpoint.
func (p *Producer) Stats() kafka.WriterStats { return p.writer.Stats() }
