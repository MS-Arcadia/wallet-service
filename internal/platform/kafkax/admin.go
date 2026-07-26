package kafkax

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
)

// unknownEventError marks an event type with no registered handler. It carries no
// retryable classification, so errs.IsRetryable reports false and the consumer
// dead-letters it.
type unknownEventError struct{ eventType string }

func (e unknownEventError) Error() string {
	return fmt.Sprintf("kafkax: no handler registered for event type %q", e.eventType)
}

// TopicSpec describes a topic to create.
type TopicSpec struct {
	Name string
	// Partitions determines the maximum consumer parallelism for the topic.
	Partitions int
	// ReplicationFactor must not exceed the number of brokers.
	ReplicationFactor int
	// RetentionMs is how long messages are kept. Financial topics keep them long
	// enough to replay a full reconciliation.
	RetentionMs int64
	// Compact turns on log compaction, for topics that behave like a keyed table.
	Compact bool
}

// EnsureTopics creates any missing topics.
//
// Auto-creation is disabled on the broker on purpose: a typo in a topic name
// should fail loudly rather than silently create a topic nobody reads. Services
// therefore declare the topics they own at boot, which also documents ownership
// in code.
func EnsureTopics(ctx context.Context, brokers []string, specs []TopicSpec, logger *slog.Logger) error {
	if len(brokers) == 0 {
		return errors.New("kafkax: at least one broker is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return fmt.Errorf("kafkax: dial broker %s: %w", brokers[0], err)
	}
	defer func() { _ = conn.Close() }()

	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("kafkax: locate controller: %w", err)
	}

	controllerAddr := fmt.Sprintf("%s:%d", controller.Host, controller.Port)
	controllerConn, err := kafka.DialContext(ctx, "tcp", controllerAddr)
	if err != nil {
		return fmt.Errorf("kafkax: dial controller %s: %w", controllerAddr, err)
	}
	defer func() { _ = controllerConn.Close() }()

	configs := make([]kafka.TopicConfig, 0, len(specs))
	for _, spec := range specs {
		if spec.Name == "" {
			return errors.New("kafkax: topic name is required")
		}
		partitions := spec.Partitions
		if partitions <= 0 {
			partitions = 3
		}
		replication := spec.ReplicationFactor
		if replication <= 0 {
			replication = 1
		}

		entries := []kafka.ConfigEntry{}
		if spec.RetentionMs > 0 {
			entries = append(entries, kafka.ConfigEntry{
				ConfigName:  "retention.ms",
				ConfigValue: fmt.Sprintf("%d", spec.RetentionMs),
			})
		}
		if spec.Compact {
			entries = append(entries, kafka.ConfigEntry{
				ConfigName:  "cleanup.policy",
				ConfigValue: "compact",
			})
		}

		configs = append(configs, kafka.TopicConfig{
			Topic:             spec.Name,
			NumPartitions:     partitions,
			ReplicationFactor: replication,
			ConfigEntries:     entries,
		})
	}

	if err := controllerConn.CreateTopics(configs...); err != nil {
		// Kafka answers with this when the topic is already there, which is the
		// normal case on every restart after the first.
		if strings.Contains(err.Error(), "already exists") ||
			errors.Is(err, kafka.TopicAlreadyExists) {
			logger.Debug("kafka topics already exist")
			return nil
		}
		return fmt.Errorf("kafkax: create topics: %w", err)
	}

	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	logger.Info("ensured kafka topics", slog.String("topics", strings.Join(names, ",")))
	return nil
}

// WaitForBrokers blocks until a broker answers or the timeout elapses. Used at
// boot so that a service started alongside Kafka by compose does not crash-loop
// while the broker is still electing a controller.
func WaitForBrokers(ctx context.Context, brokers []string, timeout time.Duration, logger *slog.Logger) error {
	if len(brokers) == 0 {
		return errors.New("kafkax: at least one broker is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	deadline := time.Now().Add(timeout)
	attempt := 0
	for {
		attempt++
		conn, err := kafka.DialContext(ctx, "tcp", brokers[0])
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kafkax: broker %s unreachable after %s: %w", brokers[0], timeout, err)
		}

		logger.Warn("waiting for kafka broker",
			slog.String("broker", brokers[0]),
			slog.Int("attempt", attempt),
			slog.String("error", err.Error()),
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
