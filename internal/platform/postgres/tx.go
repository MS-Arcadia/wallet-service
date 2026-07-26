package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TxFn is a unit of work executed inside a transaction.
//
// It is an alias rather than a defined type so that a service can declare its own
// TxManager port with the literal signature and have *PoolTxManager satisfy it.
// Interface satisfaction in Go compares parameter types exactly, and a defined
// function type is not identical to the equivalent function literal.
type TxFn = func(ctx context.Context, tx pgx.Tx) error

// TxManager runs units of work transactionally. Application code depends on this
// interface rather than on pgx, which is what lets a use case coordinate an
// aggregate write and an outbox insert without knowing which driver is beneath.
type TxManager interface {
	// WithinTx runs fn in a transaction, committing on success and rolling back on
	// any error or panic.
	WithinTx(ctx context.Context, fn TxFn) error
	// WithinSerializableTx runs fn at SERIALIZABLE isolation with bounded retries
	// on serialization failures.
	WithinSerializableTx(ctx context.Context, fn TxFn) error
}

// PoolTxManager is the pgx-backed TxManager.
type PoolTxManager struct {
	pool *pgxpool.Pool
	// maxRetries bounds serialization-failure retries.
	maxRetries int
	// retryBackoff is the base delay between retries; it grows linearly.
	retryBackoff time.Duration
}

// NewTxManager returns a TxManager over pool.
func NewTxManager(pool *pgxpool.Pool) *PoolTxManager {
	return &PoolTxManager{pool: pool, maxRetries: 3, retryBackoff: 10 * time.Millisecond}
}

// Pool exposes the underlying pool for read-only queries that need no transaction.
func (m *PoolTxManager) Pool() *pgxpool.Pool { return m.pool }

// WithinTx implements TxManager using the default READ COMMITTED isolation.
func (m *PoolTxManager) WithinTx(ctx context.Context, fn TxFn) error {
	return m.run(ctx, pgx.TxOptions{}, fn)
}

// WithinSerializableTx implements TxManager at SERIALIZABLE isolation, retrying a
// bounded number of times when Postgres reports a serialization failure.
//
// This is reserved for the few operations where READ COMMITTED plus row locking
// is not enough — a balance read that feeds a decision made outside the row's
// own UPDATE, for instance.
func (m *PoolTxManager) WithinSerializableTx(ctx context.Context, fn TxFn) error {
	opts := pgx.TxOptions{IsoLevel: pgx.Serializable}

	var lastErr error
	for attempt := 0; attempt <= m.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * m.retryBackoff):
			}
		}
		lastErr = m.run(ctx, opts, fn)
		if lastErr == nil {
			return nil
		}
		if !IsRetryableTxError(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("postgres: transaction failed after %d retries: %w", m.maxRetries, lastErr)
}

func (m *PoolTxManager) run(ctx context.Context, opts pgx.TxOptions, fn TxFn) error {
	tx, err := m.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("postgres: begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		// Roll back with a fresh context: if the caller's context is already
		// canceled, the rollback would be dropped and the connection returned to
		// the pool still inside a transaction.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if err := runGuarded(ctx, tx, fn); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		if errors.Is(err, pgx.ErrTxClosed) {
			// fn already committed or rolled back explicitly; honour that.
			committed = true
			return nil
		}
		return fmt.Errorf("postgres: commit: %w", err)
	}
	committed = true
	return nil
}

// runGuarded turns a panic inside fn into an error. The transaction is then
// rolled back by run's deferred cleanup and the request fails with a 500 —
// preferable to letting one malformed request unwind the whole process.
func runGuarded(ctx context.Context, tx pgx.Tx, fn TxFn) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("postgres: panic in transaction: %v", r)
		}
	}()
	return fn(ctx, tx)
}
