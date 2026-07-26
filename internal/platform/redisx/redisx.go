// Package redisx provides the Redis client, a sliding-window rate limiter and a
// distributed lock.
//
// The rate limiter is what implements the gift-card abuse rule from the
// requirements — repeated wrong codes in a short window flag the user for Support
// review. A sliding window is used rather than a fixed one because a fixed window
// lets an attacker fire twice the quota by straddling the boundary.
package redisx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Config configures the client.
type Config struct {
	// Addr is host:port.
	Addr string
	// Password may be empty for a local instance.
	Password string
	// DB selects the logical database.
	DB int
	// PoolSize caps connections.
	PoolSize int
	// DialTimeout, ReadTimeout and WriteTimeout bound operations. They are kept
	// short: Redis is on the fast path, and a slow Redis must degrade rather than
	// stall a wallet operation.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// Client wraps a redis.Client.
type Client struct {
	rdb *redis.Client
}

// Connect opens and verifies a Redis client.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, errors.New("redisx: addr is required")
	}
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 20
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 2 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 2 * time.Second
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redisx: ping %s: %w", cfg.Addr, err)
	}
	return &Client{rdb: rdb}, nil
}

// Raw exposes the underlying client.
func (c *Client) Raw() *redis.Client { return c.rdb }

// Ping checks connectivity, for the health registry.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redisx: ping: %w", err)
	}
	return nil
}

// Close releases the client.
func (c *Client) Close() error {
	if err := c.rdb.Close(); err != nil {
		return fmt.Errorf("redisx: close: %w", err)
	}
	return nil
}
