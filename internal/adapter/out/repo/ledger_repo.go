package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/arcadia-platform/pkg/postgres"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// LedgerRepo is the Postgres port.LedgerRepository.
//
// Note what this type does not have: an Update or a Delete. The ledger is
// append-only in the application because there is no code to change it, and
// append-only in the database because a trigger rejects the attempt.
type LedgerRepo struct{}

// NewLedgerRepo returns a LedgerRepo.
func NewLedgerRepo() *LedgerRepo { return &LedgerRepo{} }

const ledgerColumns = `
	id, sequence, wallet_id, user_id, direction, amount_minor, balance_after_minor,
	currency, reason, reference_id, description, correlation_id, idempotency_key, created_at`

// Append writes one entry and back-fills the sequence the database assigned.
func (r *LedgerRepo) Append(ctx context.Context, tx port.Tx, entry ledger.Entry) error {
	err := tx.QueryRow(ctx, `
		INSERT INTO ledger_entries (
			id, wallet_id, user_id, direction, amount_minor, balance_after_minor,
			currency, reason, reference_id, description, correlation_id, idempotency_key, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING sequence`,
		entry.ID, entry.WalletID, entry.UserID, string(entry.Direction),
		entry.Amount.Minor(), entry.BalanceAfter.Minor(), entry.Amount.Currency(),
		string(entry.Reason), entry.ReferenceID, entry.Description,
		entry.CorrelationID, entry.IdempotencyKey, entry.CreatedAt,
	).Scan(&entry.Sequence)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			// The (wallet_id, idempotency_key) unique index fired. The idempotency table
			// should have caught this first, so reaching here means the two disagree —
			// worth surfacing rather than swallowing.
			return errs.Conflict("a ledger entry already exists for this request").
				WithReason("DUPLICATE_LEDGER_ENTRY").
				WithCause(err)
		}
		return errs.Internal("failed to append ledger entry %s", entry.ID).WithCause(err)
	}
	return nil
}

// List returns a filtered, paginated slice of entries, newest first.
func (r *LedgerRepo) List(ctx context.Context, reader port.Reader, filter ledger.Filter) (ledger.Page, error) {
	where, args := ledgerFilterSQL(filter)

	var total int64
	countQuery := `SELECT count(*) FROM ledger_entries ` + where
	if err := reader.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return ledger.Page{}, errs.Internal("failed to count ledger entries").WithCause(err)
	}
	if total == 0 {
		return ledger.Page{Entries: nil, TotalItems: 0, Limit: filter.Limit, Offset: filter.Offset}, nil
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	// Order by sequence, not created_at: two entries written in the same transaction
	// share a timestamp, and an unstable order would make paging skip or repeat rows.
	query := fmt.Sprintf(`SELECT%s FROM ledger_entries %s ORDER BY sequence DESC LIMIT $%d OFFSET $%d`,
		ledgerColumns, where, len(args)+1, len(args)+2)
	args = append(args, limit, filter.Offset)

	rows, err := reader.Query(ctx, query, args...)
	if err != nil {
		return ledger.Page{}, errs.Internal("failed to list ledger entries").WithCause(err)
	}
	defer rows.Close()

	entries := make([]ledger.Entry, 0, limit)
	for rows.Next() {
		entry, err := scanLedgerEntry(rows)
		if err != nil {
			return ledger.Page{}, errs.Internal("failed to scan a ledger row").WithCause(err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return ledger.Page{}, errs.Internal("failed to iterate ledger entries").WithCause(err)
	}

	return ledger.Page{
		Entries:    entries,
		TotalItems: total,
		Limit:      limit,
		Offset:     filter.Offset,
	}, nil
}

// ledgerFilterSQL builds a parameterised WHERE clause.
//
// Every value is a bind parameter. Nothing from a caller is ever concatenated into
// the SQL text, which is what makes this immune to injection regardless of what a
// client puts in a reference id.
func ledgerFilterSQL(filter ledger.Filter) (string, []any) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)

	if filter.WalletID != "" {
		args = append(args, filter.WalletID)
		conditions = append(conditions, fmt.Sprintf("wallet_id = $%d", len(args)))
	}
	if filter.Direction != "" {
		args = append(args, string(filter.Direction))
		conditions = append(conditions, fmt.Sprintf("direction = $%d", len(args)))
	}
	if filter.ReferenceID != "" {
		args = append(args, filter.ReferenceID)
		conditions = append(conditions, fmt.Sprintf("reference_id = $%d", len(args)))
	}
	if len(filter.Reasons) > 0 {
		reasons := make([]string, 0, len(filter.Reasons))
		for _, reason := range filter.Reasons {
			reasons = append(reasons, string(reason))
		}
		args = append(args, reasons)
		conditions = append(conditions, fmt.Sprintf("reason = ANY($%d)", len(args)))
	}
	if filter.From != nil {
		args = append(args, *filter.From)
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if filter.To != nil {
		args = append(args, *filter.To)
		conditions = append(conditions, fmt.Sprintf("created_at < $%d", len(args)))
	}

	if len(conditions) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(conditions, " AND "), args
}

// FindByIdempotencyKey returns the entry a previous request produced.
func (r *LedgerRepo) FindByIdempotencyKey(ctx context.Context, reader port.Reader, walletID, key string) (*ledger.Entry, error) {
	row := reader.QueryRow(ctx, `SELECT`+ledgerColumns+`
		FROM ledger_entries
		WHERE wallet_id = $1 AND idempotency_key = $2
		ORDER BY sequence
		LIMIT 1`, walletID, key)

	entry, err := scanLedgerEntry(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, errs.NotFound("no ledger entry exists for idempotency key %s", key)
		}
		return nil, errs.Internal("failed to read the ledger entry for key %s", key).WithCause(err)
	}
	return &entry, nil
}

// SumByWallet returns the net of every entry for a wallet.
func (r *LedgerRepo) SumByWallet(ctx context.Context, reader port.Reader, walletID string) (money.Money, error) {
	var (
		sum      int64
		currency *string
	)
	// The CASE expression applies the sign that direction implies. Summing in the
	// database rather than in Go avoids streaming a lifetime of entries over the wire
	// just to add them up.
	err := reader.QueryRow(ctx, `
		SELECT
			coalesce(sum(CASE WHEN direction = 'DEBIT' THEN -amount_minor ELSE amount_minor END), 0),
			max(currency)
		FROM ledger_entries
		WHERE wallet_id = $1`, walletID).Scan(&sum, &currency)
	if err != nil {
		return money.Money{}, errs.Internal("failed to sum the ledger for wallet %s", walletID).WithCause(err)
	}
	if currency == nil {
		// A wallet with no entries yet. Zero, currency unknown, is the honest answer.
		return money.Money{}, nil
	}

	total, err := money.New(sum, *currency)
	if err != nil {
		return money.Money{}, errs.Internal("wallet %s has an invalid ledger currency", walletID).WithCause(err)
	}
	return total, nil
}

// FindMismatches returns wallets whose stored balance disagrees with their ledger.
//
// The comparison runs entirely in the database: a lateral join sums each wallet's
// entries and keeps only the rows that differ. Doing it in Go would mean fetching
// every wallet and every entry on every reconciliation pass.
func (r *LedgerRepo) FindMismatches(ctx context.Context, reader port.Reader, userID string, limit int) ([]ledger.Mismatch, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := reader.Query(ctx, `
		SELECT w.id, w.user_id, w.currency, w.balance_minor, coalesce(l.ledger_sum, 0)
		FROM wallets w
		LEFT JOIN LATERAL (
			SELECT sum(CASE WHEN direction = 'DEBIT' THEN -amount_minor ELSE amount_minor END) AS ledger_sum
			FROM ledger_entries
			WHERE wallet_id = w.id
		) l ON true
		WHERE ($1 = '' OR w.user_id = $1::uuid)
		  AND w.balance_minor <> coalesce(l.ledger_sum, 0)
		ORDER BY w.id
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, errs.Internal("failed to reconcile wallets against the ledger").WithCause(err)
	}
	defer rows.Close()

	mismatches := make([]ledger.Mismatch, 0)
	for rows.Next() {
		var (
			walletID, ownerID, currency string
			storedMinor, ledgerMinor    int64
		)
		if err := rows.Scan(&walletID, &ownerID, &currency, &storedMinor, &ledgerMinor); err != nil {
			return nil, errs.Internal("failed to scan a reconciliation row").WithCause(err)
		}

		stored, err := money.New(storedMinor, currency)
		if err != nil {
			return nil, errs.Internal("wallet %s has an invalid currency", walletID).WithCause(err)
		}
		ledgerBalance, err := money.New(ledgerMinor, currency)
		if err != nil {
			return nil, errs.Internal("wallet %s has an invalid currency", walletID).WithCause(err)
		}
		delta, err := stored.Sub(ledgerBalance)
		if err != nil {
			return nil, errs.Internal("failed to compute the reconciliation delta").WithCause(err)
		}

		mismatches = append(mismatches, ledger.Mismatch{
			WalletID:      walletID,
			UserID:        ownerID,
			StoredBalance: stored,
			LedgerBalance: ledgerBalance,
			Delta:         delta,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Internal("failed to iterate reconciliation rows").WithCause(err)
	}
	return mismatches, nil
}

func scanLedgerEntry(row scanner) (ledger.Entry, error) {
	var (
		entry                          ledger.Entry
		direction, currency, reason    string
		amountMinor, balanceAfterMinor int64
		createdAt                      time.Time
	)
	if err := row.Scan(
		&entry.ID, &entry.Sequence, &entry.WalletID, &entry.UserID, &direction,
		&amountMinor, &balanceAfterMinor, &currency, &reason,
		&entry.ReferenceID, &entry.Description, &entry.CorrelationID,
		&entry.IdempotencyKey, &createdAt,
	); err != nil {
		return ledger.Entry{}, err
	}

	amount, err := money.New(amountMinor, currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("ledger entry %s has an invalid currency: %w", entry.ID, err)
	}
	balanceAfter, err := money.New(balanceAfterMinor, currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("ledger entry %s has an invalid currency: %w", entry.ID, err)
	}

	entry.Direction = wallet.Direction(direction)
	entry.Amount = amount
	entry.BalanceAfter = balanceAfter
	entry.Reason = wallet.Reason(reason)
	entry.CreatedAt = createdAt
	return entry, nil
}

var _ port.LedgerRepository = (*LedgerRepo)(nil)
