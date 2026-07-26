package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/MS-Arcadia/wallet-service/internal/platform/postgres"
)

// HoldRepo is the Postgres port.HoldRepository.
type HoldRepo struct{}

// NewHoldRepo returns a HoldRepo.
func NewHoldRepo() *HoldRepo { return &HoldRepo{} }

const holdColumns = `
	id, wallet_id, user_id, amount_minor, captured_amount_minor, currency, status,
	reference_id, reason, expires_at, created_at, resolved_at, version`

// Insert stores a hold.
func (r *HoldRepo) Insert(ctx context.Context, tx port.Tx, h *hold.Hold) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO holds (
			id, wallet_id, user_id, amount_minor, captured_amount_minor, currency,
			status, reference_id, reason, expires_at, created_at, version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		h.ID(), h.WalletID(), h.UserID(), h.Amount().Minor(), h.CapturedAmount().Minor(),
		h.Amount().Currency(), string(h.Status()), h.ReferenceID(), h.Reason(),
		h.ExpiresAt(), h.CreatedAt(), h.Version(),
	)
	if err != nil {
		return errs.Internal("failed to insert hold %s", h.ID()).WithCause(err)
	}
	return nil
}

// FindByID looks a hold up.
func (r *HoldRepo) FindByID(ctx context.Context, reader port.Reader, id string) (*hold.Hold, error) {
	row := reader.QueryRow(ctx, `SELECT`+holdColumns+` FROM holds WHERE id = $1`, id)
	h, err := scanHold(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, hold.ErrNotFound(id)
		}
		return nil, errs.Internal("failed to read hold %s", id).WithCause(err)
	}
	return h, nil
}

// LockByID reads a hold FOR UPDATE, so two concurrent instalment captures cannot
// both draw down the same remaining amount.
func (r *HoldRepo) LockByID(ctx context.Context, tx port.Tx, id string) (*hold.Hold, error) {
	row := tx.QueryRow(ctx, `SELECT`+holdColumns+` FROM holds WHERE id = $1 FOR UPDATE`, id)
	h, err := scanHold(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, hold.ErrNotFound(id)
		}
		return nil, errs.Internal("failed to lock hold %s", id).WithCause(err)
	}
	return h, nil
}

// Update persists a hold transition.
func (r *HoldRepo) Update(ctx context.Context, tx port.Tx, h *hold.Hold, expectedVersion int64) error {
	tag, err := tx.Exec(ctx, `
		UPDATE holds
		SET captured_amount_minor = $1, status = $2, resolved_at = $3, version = $4
		WHERE id = $5 AND version = $6`,
		h.CapturedAmount().Minor(), string(h.Status()), h.ResolvedAt(), h.Version(),
		h.ID(), expectedVersion,
	)
	if err != nil {
		if postgres.IsCheckViolation(err) {
			return errs.FailedPrecondition("the capture would exceed the reserved amount").
				WithReason(hold.ReasonCodeExceedsRemaining).
				WithCause(err)
		}
		return errs.Internal("failed to update hold %s", h.ID()).WithCause(err)
	}
	if tag.RowsAffected() == 0 {
		return errs.Aborted("hold %s was modified concurrently", h.ID()).
			WithReason("VERSION_CONFLICT")
	}
	return nil
}

// List returns a filtered page of holds.
func (r *HoldRepo) List(ctx context.Context, reader port.Reader, filter hold.Filter) ([]*hold.Hold, int64, error) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 5)

	if filter.WalletID != "" {
		args = append(args, filter.WalletID)
		conditions = append(conditions, fmt.Sprintf("wallet_id = $%d", len(args)))
	}
	if filter.UserID != "" {
		args = append(args, filter.UserID)
		conditions = append(conditions, fmt.Sprintf("user_id = $%d", len(args)))
	}
	if filter.Status != "" {
		args = append(args, string(filter.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := reader.QueryRow(ctx, `SELECT count(*) FROM holds `+where, args...).Scan(&total); err != nil {
		return nil, 0, errs.Internal("failed to count holds").WithCause(err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	query := fmt.Sprintf(`SELECT%s FROM holds %s ORDER BY created_at DESC, id LIMIT $%d OFFSET $%d`,
		holdColumns, where, len(args)+1, len(args)+2)
	args = append(args, limit, filter.Offset)

	rows, err := reader.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, errs.Internal("failed to list holds").WithCause(err)
	}
	defer rows.Close()

	holds := make([]*hold.Hold, 0, limit)
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, 0, errs.Internal("failed to scan a hold row").WithCause(err)
		}
		holds = append(holds, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, errs.Internal("failed to iterate holds").WithCause(err)
	}
	return holds, total, nil
}

// ListExpired returns active holds past their TTL, oldest first.
func (r *HoldRepo) ListExpired(ctx context.Context, reader port.Reader, before time.Time, limit int) ([]*hold.Hold, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := reader.Query(ctx, `SELECT`+holdColumns+`
		FROM holds
		WHERE status = 'ACTIVE' AND expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`, before, limit)
	if err != nil {
		return nil, errs.Internal("failed to list expired holds").WithCause(err)
	}
	defer rows.Close()

	holds := make([]*hold.Hold, 0, limit)
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, errs.Internal("failed to scan an expired hold row").WithCause(err)
		}
		holds = append(holds, h)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Internal("failed to iterate expired holds").WithCause(err)
	}
	return holds, nil
}

func scanHold(row scanner) (*hold.Hold, error) {
	var (
		id, walletID, userID, currency, status, referenceID, reason string
		amountMinor, capturedMinor                                  int64
		version                                                     int64
		expiresAt, resolvedAt                                       *time.Time
		createdAt                                                   time.Time
	)
	if err := row.Scan(&id, &walletID, &userID, &amountMinor, &capturedMinor,
		&currency, &status, &referenceID, &reason, &expiresAt, &createdAt,
		&resolvedAt, &version); err != nil {
		return nil, err
	}

	amount, err := money.New(amountMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("hold %s has an invalid currency: %w", id, err)
	}
	captured, err := money.New(capturedMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("hold %s has an invalid currency: %w", id, err)
	}

	h, err := hold.Rehydrate(id, walletID, userID, amount, captured,
		hold.Status(status), referenceID, reason, expiresAt, createdAt, resolvedAt, version)
	if err != nil {
		return nil, err
	}
	return h, nil
}

var _ port.HoldRepository = (*HoldRepo)(nil)
