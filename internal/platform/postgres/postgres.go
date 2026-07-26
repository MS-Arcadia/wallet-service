// Package postgres wires up a pgx connection pool and the transaction helper
// that the repositories build on.
//
// The unit-of-work abstraction here is what makes the Transactional Outbox
// pattern work: a use case writes its aggregate and its outbox row through the
// same pgx.Tx, so the database's own ACID guarantee — not a distributed
// transaction — is what keeps state and events in step.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config describes how to reach and size the pool.
type Config struct {
	// DSN is the libpq connection string.
	DSN string
	// MaxConns caps the pool. Keep it well under the server's max_connections
	// divided by the number of replicas.
	MaxConns int32
	// MinConns keeps warm connections ready so that a burst does not pay TCP and
	// TLS setup costs.
	MinConns int32
	// MaxConnLifetime recycles connections so that a long-lived pool cannot pin a
	// stale prepared-statement plan forever.
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// ConnectTimeout bounds the initial dial.
	ConnectTimeout time.Duration
	// ApplicationName shows up in pg_stat_activity, which makes it obvious which
	// service is holding a lock.
	ApplicationName string
	// StatementTimeout is applied per session; a runaway query is killed by the
	// server rather than holding a connection forever.
	StatementTimeout time.Duration
}

// Pool is the concrete connection pool type used across the services.
type Pool = pgxpool.Pool

// Connect opens and verifies a pool.
func Connect(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if cfg.DSN == "" {
		return nil, errors.New("postgres: DSN is required")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}

	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.ConnectTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout
	}

	runtimeParams := poolCfg.ConnConfig.RuntimeParams
	if runtimeParams == nil {
		runtimeParams = map[string]string{}
		poolCfg.ConnConfig.RuntimeParams = runtimeParams
	}
	if cfg.ApplicationName != "" {
		runtimeParams["application_name"] = cfg.ApplicationName
	}
	if cfg.StatementTimeout > 0 {
		runtimeParams["statement_timeout"] = fmt.Sprintf("%d", cfg.StatementTimeout.Milliseconds())
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// Querier is the read/write surface shared by *pgxpool.Pool and pgx.Tx.
// Repositories accept a Querier so that the same method works inside and outside
// a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Ensure both pgx types satisfy Querier at compile time.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// PostgreSQL error codes this package reacts to.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeSerializationFail   = "40001"
	codeDeadlockDetected    = "40P01"
	codeLockNotAvailable    = "55P03"
)

// IsUniqueViolation reports whether err is a duplicate-key error. Repositories
// translate this into a domain "already exists" or, for idempotency keys, into
// "this request was already processed".
func IsUniqueViolation(err error) bool { return hasSQLState(err, codeUniqueViolation) }

// IsForeignKeyViolation reports a referential-integrity failure.
func IsForeignKeyViolation(err error) bool { return hasSQLState(err, codeForeignKeyViolation) }

// IsCheckViolation reports that a CHECK constraint rejected the write. The
// non-negative-balance constraint on wallets is the important one: it is the
// database enforcing an invariant the domain also enforces, as a backstop.
func IsCheckViolation(err error) bool { return hasSQLState(err, codeCheckViolation) }

// IsRetryableTxError reports whether a failed transaction can simply be replayed.
func IsRetryableTxError(err error) bool {
	return hasSQLState(err, codeSerializationFail) ||
		hasSQLState(err, codeDeadlockDetected) ||
		hasSQLState(err, codeLockNotAvailable)
}

// ConstraintName returns the violated constraint's name, or "".
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func hasSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == code
	}
	return false
}

// IsNoRows reports whether a query returned no rows.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
