package repo

import (
	"context"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/postgres"
)

// IdempotencyStore is the Postgres port.IdempotencyStore.
type IdempotencyStore struct{}

// NewIdempotencyStore returns an IdempotencyStore.
func NewIdempotencyStore() *IdempotencyStore { return &IdempotencyStore{} }

// Claim reserves a key inside tx.
//
// The implementation leans on the primary key doing the concurrency control. The
// INSERT ... ON CONFLICT DO NOTHING either creates the row — in which case this
// caller owns the operation — or affects nothing, in which case somebody else got
// there first and their stored response is what must be returned. There is no
// read-then-write window for two callers to slip through.
func (s *IdempotencyStore) Claim(ctx context.Context, tx port.Tx, record port.IdempotencyRecord) (*port.IdempotencyRecord, bool, error) {
	if record.Key == "" {
		return nil, false, errs.InvalidArgument("an idempotency key is required")
	}
	if record.Operation == "" {
		return nil, false, errs.Internal("an idempotency operation name is required")
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (key, operation, request_hash, wallet_id, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (key, operation) DO NOTHING`,
		record.Key, record.Operation, record.RequestHash, record.WalletID, record.CreatedAt,
	)
	if err != nil {
		return nil, false, errs.Internal("failed to claim idempotency key %s", record.Key).WithCause(err)
	}
	if tag.RowsAffected() == 1 {
		return nil, true, nil
	}

	// Somebody else owns this key. Load their record so the caller can compare the
	// request hash and replay the response.
	existing, err := s.find(ctx, tx, record.Key, record.Operation)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (s *IdempotencyStore) find(ctx context.Context, tx port.Tx, key, operation string) (*port.IdempotencyRecord, error) {
	var (
		record   port.IdempotencyRecord
		response []byte
	)
	err := tx.QueryRow(ctx, `
		SELECT key, operation, request_hash, response, wallet_id, created_at
		FROM idempotency_keys
		WHERE key = $1 AND operation = $2`, key, operation,
	).Scan(&record.Key, &record.Operation, &record.RequestHash, &response,
		&record.WalletID, &record.CreatedAt)
	if err != nil {
		if postgres.IsNoRows(err) {
			// The row was there a microsecond ago — the ON CONFLICT fired — so it can only
			// have been swept between the two statements. Treating it as retryable is
			// safer than pretending the operation completed.
			return nil, errs.Aborted("the idempotency record for key %s vanished; retry the request", key).
				WithReason("IDEMPOTENCY_RACE")
		}
		return nil, errs.Internal("failed to read idempotency key %s", key).WithCause(err)
	}
	record.Response = response
	return &record, nil
}

// SaveResponse attaches the result to a claimed key.
func (s *IdempotencyStore) SaveResponse(ctx context.Context, tx port.Tx, key, operation string, response []byte) error {
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET response = $1, completed_at = now()
		WHERE key = $2 AND operation = $3`,
		response, key, operation,
	)
	if err != nil {
		return errs.Internal("failed to store the idempotent response for key %s", key).WithCause(err)
	}
	if tag.RowsAffected() == 0 {
		return errs.Internal("idempotency key %s was never claimed", key)
	}
	return nil
}

// Purge deletes records older than the retention window.
//
// The window has to comfortably outlive any client's retry behaviour. Deleting a key
// too early would let a very late retry execute a second time, which is the exact
// failure the table exists to prevent.
func (s *IdempotencyStore) Purge(ctx context.Context, tx port.Tx, before time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, before)
	if err != nil {
		return 0, errs.Internal("failed to purge idempotency keys").WithCause(err)
	}
	return tag.RowsAffected(), nil
}

var _ port.IdempotencyStore = (*IdempotencyStore)(nil)
