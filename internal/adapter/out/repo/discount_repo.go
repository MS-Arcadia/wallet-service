package repo

import (
	"context"
	"fmt"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/MS-Arcadia/wallet-service/internal/platform/postgres"
)

// DiscountRepo is the Postgres port.DiscountRepository.
type DiscountRepo struct{}

// NewDiscountRepo returns a DiscountRepo.
func NewDiscountRepo() *DiscountRepo { return &DiscountRepo{} }

const discountColumns = `
	id, code, percent_bps, amount_off_minor, max_discount_minor, min_order_amount_minor,
	currency, status, max_redemptions, redemption_count, issued_by, expires_at, created_at, version`

// Insert stores a code.
func (r *DiscountRepo) Insert(ctx context.Context, tx port.Tx, code *discount.Code) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO discount_codes (
			id, code, percent_bps, amount_off_minor, max_discount_minor, min_order_amount_minor,
			currency, status, max_redemptions, redemption_count, issued_by, expires_at, created_at, version
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		code.ID(), code.Code(), code.PercentBps(), code.AmountOff().Minor(),
		code.MaxDiscount().Minor(), code.MinOrderAmount().Minor(),
		discountCurrency(code), string(code.Status()), code.MaxRedemptions(),
		code.RedemptionCount(), code.IssuedBy(), code.ExpiresAt(), code.CreatedAt(), code.Version(),
	)
	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return errs.AlreadyExists("discount code %s already exists", code.Code()).WithCause(err)
		}
		return errs.Internal("failed to insert discount code %s", code.Code()).WithCause(err)
	}
	return nil
}

// discountCurrency picks the currency to store.
//
// A percentage code carries no amount of its own, so its money fields are zero
// values with an empty currency. The column is NOT NULL, so a placeholder is needed;
// the platform's operating currency is the honest choice, because that is the only
// currency the code can ever apply to.
func discountCurrency(code *discount.Code) string {
	for _, amount := range []money.Money{code.AmountOff(), code.MaxDiscount(), code.MinOrderAmount()} {
		if amount.Currency() != "" {
			return amount.Currency()
		}
	}
	return defaultCurrency
}

// defaultCurrency is set once at boot by SetDefaultCurrency.
var defaultCurrency = "IRR"

// SetDefaultCurrency tells the repositories which currency to stamp on rows whose
// aggregate carries no amount. Called once during wiring.
func SetDefaultCurrency(currency string) {
	if currency != "" {
		defaultCurrency = currency
	}
}

// FindByCode looks a code up.
func (r *DiscountRepo) FindByCode(ctx context.Context, reader port.Reader, code string) (*discount.Code, error) {
	row := reader.QueryRow(ctx, `SELECT`+discountColumns+` FROM discount_codes WHERE code = $1`, code)
	stored, err := scanDiscountCode(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, discount.ErrNotFound()
		}
		return nil, errs.Internal("failed to read discount code %s", code).WithCause(err)
	}
	return stored, nil
}

// LockByCode reads a code FOR UPDATE, so two concurrent checkouts cannot both
// consume the last allowance of a single-use code.
func (r *DiscountRepo) LockByCode(ctx context.Context, tx port.Tx, code string) (*discount.Code, error) {
	row := tx.QueryRow(ctx, `SELECT`+discountColumns+` FROM discount_codes WHERE code = $1 FOR UPDATE`, code)
	stored, err := scanDiscountCode(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, discount.ErrNotFound()
		}
		return nil, errs.Internal("failed to lock discount code %s", code).WithCause(err)
	}
	return stored, nil
}

// Update persists a redemption or a revocation.
func (r *DiscountRepo) Update(ctx context.Context, tx port.Tx, code *discount.Code, expectedVersion int64) error {
	tag, err := tx.Exec(ctx, `
		UPDATE discount_codes
		SET status = $1, redemption_count = $2, version = $3
		WHERE id = $4 AND version = $5`,
		string(code.Status()), code.RedemptionCount(), code.Version(), code.ID(), expectedVersion,
	)
	if err != nil {
		if postgres.IsCheckViolation(err) {
			return errs.FailedPrecondition("this discount code has been fully redeemed").
				WithReason(discount.ReasonCodeExhausted).
				WithCause(err)
		}
		return errs.Internal("failed to update discount code %s", code.Code()).WithCause(err)
	}
	if tag.RowsAffected() == 0 {
		return errs.Aborted("discount code %s was modified concurrently", code.Code()).
			WithReason("VERSION_CONFLICT")
	}
	return nil
}

// ExpireLapsed flips ACTIVE codes past their expiry to EXPIRED.
//
// This is cosmetic housekeeping so that a staff listing reads correctly. Redemption
// checks expiry live and never relies on this sweep having run.
func (r *DiscountRepo) ExpireLapsed(ctx context.Context, tx port.Tx, now time.Time) (int64, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE discount_codes
		SET status = 'EXPIRED', version = version + 1
		WHERE status = 'ACTIVE' AND expires_at IS NOT NULL AND expires_at <= $1`, now)
	if err != nil {
		return 0, errs.Internal("failed to expire lapsed discount codes").WithCause(err)
	}
	return tag.RowsAffected(), nil
}

func scanDiscountCode(row scanner) (*discount.Code, error) {
	var (
		id, code, currency, status, issuedBy            string
		percentBps, maxRedemptions, redemptionCount     int32
		amountOffMinor, maxDiscountMinor, minOrderMinor int64
		version                                         int64
		expiresAt                                       *time.Time
		createdAt                                       time.Time
	)
	if err := row.Scan(&id, &code, &percentBps, &amountOffMinor, &maxDiscountMinor,
		&minOrderMinor, &currency, &status, &maxRedemptions, &redemptionCount,
		&issuedBy, &expiresAt, &createdAt, &version); err != nil {
		return nil, err
	}

	// A zero amount stays a zero Money value rather than becoming "0 IRR", so that a
	// percentage code is not mistaken for a fixed-amount one on the way back out.
	amountOff, err := optionalMoney(amountOffMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("discount code %s has an invalid currency: %w", code, err)
	}
	maxDiscount, err := optionalMoney(maxDiscountMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("discount code %s has an invalid currency: %w", code, err)
	}
	minOrder, err := optionalMoney(minOrderMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("discount code %s has an invalid currency: %w", code, err)
	}

	stored, err := discount.Rehydrate(id, code, percentBps, amountOff, maxDiscount,
		minOrder, discount.Status(status), maxRedemptions, redemptionCount,
		issuedBy, expiresAt, createdAt, version)
	if err != nil {
		return nil, err
	}
	return stored, nil
}

func optionalMoney(minor int64, currency string) (money.Money, error) {
	if minor == 0 {
		return money.Money{}, nil
	}
	return money.New(minor, currency)
}

var _ port.DiscountRepository = (*DiscountRepo)(nil)
