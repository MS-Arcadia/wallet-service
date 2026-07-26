// Package repo holds the Postgres implementations of the application's repository
// ports.
//
// Every method here does three things and nothing else: build SQL, translate rows
// into aggregates, and translate driver errors into the platform's error taxonomy.
// No business rule lives in this package — that would put it out of reach of the
// domain tests.
package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/MS-Arcadia/wallet-service/internal/platform/postgres"
)

// WalletRepo is the Postgres port.WalletRepository.
type WalletRepo struct{}

// NewWalletRepo returns a WalletRepo.
func NewWalletRepo() *WalletRepo { return &WalletRepo{} }

const walletColumns = `
	id, user_id, balance_minor, held_minor, currency, status, version, created_at, updated_at`

// Insert stores a new wallet.
func (r *WalletRepo) Insert(ctx context.Context, tx port.Tx, w *wallet.Wallet) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO wallets (id, user_id, balance_minor, held_minor, currency, status, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		w.ID(), w.UserID(), w.Balance().Minor(), w.Held().Minor(), w.Currency(),
		string(w.Status()), w.Version(), w.CreatedAt(), w.UpdatedAt(),
	)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			// The unique index on user_id fired: a concurrent request provisioned this
			// user's wallet first. The caller loads theirs instead of failing.
			return errs.AlreadyExists("a wallet already exists for user %s", w.UserID()).WithCause(err)
		}
		return errs.Internal("failed to insert wallet %s", w.ID()).WithCause(err)
	}
	return nil
}

// FindByUserID reads a wallet by owner without locking it.
func (r *WalletRepo) FindByUserID(ctx context.Context, reader port.Reader, userID string) (*wallet.Wallet, error) {
	row := reader.QueryRow(ctx, `SELECT`+walletColumns+` FROM wallets WHERE user_id = $1`, userID)
	w, err := scanWallet(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, wallet.ErrNotFound(userID)
		}
		return nil, errs.Internal("failed to read the wallet for user %s", userID).WithCause(err)
	}
	return w, nil
}

// FindByID reads a wallet by identifier.
func (r *WalletRepo) FindByID(ctx context.Context, reader port.Reader, id string) (*wallet.Wallet, error) {
	row := reader.QueryRow(ctx, `SELECT`+walletColumns+` FROM wallets WHERE id = $1`, id)
	w, err := scanWallet(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, errs.NotFound("no wallet exists with id %s", id).
				WithReason(wallet.ReasonCodeWalletNotFound)
		}
		return nil, errs.Internal("failed to read wallet %s", id).WithCause(err)
	}
	return w, nil
}

// LockByUserID reads a wallet FOR UPDATE.
//
// This is the single most important line of SQL in the service. Without the row
// lock, two concurrent debits both read the same balance, both decide there are
// sufficient funds, and both commit — and the account is overdrawn. The lock forces
// the second transaction to wait and then re-read.
func (r *WalletRepo) LockByUserID(ctx context.Context, tx port.Tx, userID string) (*wallet.Wallet, error) {
	row := tx.QueryRow(ctx, `SELECT`+walletColumns+` FROM wallets WHERE user_id = $1 FOR UPDATE`, userID)
	w, err := scanWallet(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, wallet.ErrNotFound(userID)
		}
		return nil, errs.Internal("failed to lock the wallet for user %s", userID).WithCause(err)
	}
	return w, nil
}

// LockByID reads a wallet FOR UPDATE by identifier.
func (r *WalletRepo) LockByID(ctx context.Context, tx port.Tx, id string) (*wallet.Wallet, error) {
	row := tx.QueryRow(ctx, `SELECT`+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id)
	w, err := scanWallet(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, errs.NotFound("no wallet exists with id %s", id).
				WithReason(wallet.ReasonCodeWalletNotFound)
		}
		return nil, errs.Internal("failed to lock wallet %s", id).WithCause(err)
	}
	return w, nil
}

// Update persists a mutated wallet, asserting the version it was loaded at.
func (r *WalletRepo) Update(ctx context.Context, tx port.Tx, w *wallet.Wallet, expectedVersion int64) error {
	tag, err := tx.Exec(ctx, `
		UPDATE wallets
		SET balance_minor = $1, held_minor = $2, status = $3, version = $4, updated_at = $5
		WHERE id = $6 AND version = $7`,
		w.Balance().Minor(), w.Held().Minor(), string(w.Status()), w.Version(), w.UpdatedAt(),
		w.ID(), expectedVersion,
	)
	if err != nil {
		// The database's own CHECK constraints are the last line of defence for the
		// non-negative-balance invariant. Hitting one means the domain let something
		// through, so it is reported as a precondition failure rather than a 500 —
		// the caller's request genuinely was invalid.
		if postgres.IsCheckViolation(err) {
			return errs.FailedPrecondition("the update would violate a balance invariant (%s)",
				postgres.ConstraintName(err)).
				WithReason("BALANCE_INVARIANT_VIOLATED").
				WithCause(err)
		}
		return errs.Internal("failed to update wallet %s", w.ID()).WithCause(err)
	}
	if tag.RowsAffected() == 0 {
		// Either the row vanished or its version moved on. Both mean somebody else got
		// there first, and the caller should retry against fresh state.
		return errs.Aborted("wallet %s was modified concurrently; retry the operation", w.ID()).
			WithReason("VERSION_CONFLICT")
	}
	return nil
}

// ListActivePage returns a page of active wallets ordered by id.
//
// Keyset pagination, not OFFSET. A nightly job over a million wallets with OFFSET
// would re-scan everything it had already read on every page; ordering by id and
// asking for "the next N after this one" stays flat.
func (r *WalletRepo) ListActivePage(ctx context.Context, reader port.Reader, afterID string, limit int) ([]*wallet.Wallet, error) {
	if limit <= 0 {
		limit = 100
	}

	query := `SELECT` + walletColumns + `
		FROM wallets
		WHERE status = 'ACTIVE' AND ($1 = '' OR id > $1::uuid)
		ORDER BY id
		LIMIT $2`

	rows, err := reader.Query(ctx, query, afterID, limit)
	if err != nil {
		return nil, errs.Internal("failed to list active wallets").WithCause(err)
	}
	defer rows.Close()

	wallets := make([]*wallet.Wallet, 0, limit)
	for rows.Next() {
		w, err := scanWallet(rows)
		if err != nil {
			return nil, errs.Internal("failed to scan a wallet row").WithCause(err)
		}
		wallets = append(wallets, w)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Internal("failed to iterate active wallets").WithCause(err)
	}
	return wallets, nil
}

// Count returns the number of wallets.
func (r *WalletRepo) Count(ctx context.Context, reader port.Reader) (int64, error) {
	var count int64
	if err := reader.QueryRow(ctx, `SELECT count(*) FROM wallets`).Scan(&count); err != nil {
		return 0, errs.Internal("failed to count wallets").WithCause(err)
	}
	return count, nil
}

// scanner is satisfied by both pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanWallet(row scanner) (*wallet.Wallet, error) {
	var (
		id, userID, currency, status string
		balanceMinor, heldMinor      int64
		version                      int64
		createdAt, updatedAt         time.Time
	)
	if err := row.Scan(&id, &userID, &balanceMinor, &heldMinor, &currency, &status,
		&version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	balance, err := money.New(balanceMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("wallet %s has an invalid currency: %w", id, err)
	}
	held, err := money.New(heldMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("wallet %s has an invalid currency: %w", id, err)
	}

	w, err := wallet.Rehydrate(id, userID, balance, held, wallet.Status(status),
		version, createdAt, updatedAt)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// Compile-time proof that the repository still satisfies its port.
var _ port.WalletRepository = (*WalletRepo)(nil)
