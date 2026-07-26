// Package discount models promotional codes.
//
// A discount code never moves money by itself: it computes a reduction that the
// Store service applies to an order total. Keeping the calculation here — rather
// than in Store — means one implementation of the rounding and capping rules, and
// one place to test them.
package discount

import (
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
)

// Status is the code's lifecycle state.
type Status string

const (
	// StatusActive — redeemable.
	StatusActive Status = "ACTIVE"
	// StatusUsed — all redemptions consumed.
	StatusUsed Status = "USED"
	// StatusExpired — past its expiry date.
	StatusExpired Status = "EXPIRED"
	// StatusRevoked — cancelled by staff.
	StatusRevoked Status = "REVOKED"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusUsed, StatusExpired, StatusRevoked:
		return true
	default:
		return false
	}
}

// Reason codes returned by this package.
const (
	ReasonCodeNotFound         = "DISCOUNT_CODE_NOT_FOUND"
	ReasonCodeExpired          = "DISCOUNT_CODE_EXPIRED"
	ReasonCodeExhausted        = "DISCOUNT_CODE_EXHAUSTED"
	ReasonCodeRevoked          = "DISCOUNT_CODE_REVOKED"
	ReasonCodeBelowMinimum     = "ORDER_BELOW_MINIMUM"
	ReasonCodeCurrencyMismatch = "CURRENCY_MISMATCH"
)

// Code is the aggregate.
type Code struct {
	id   string
	code string
	// percentBps is the reduction in basis points. Exactly one of percentBps and
	// amountOff is set.
	percentBps int32
	amountOff  money.Money
	// maxDiscount caps a percentage discount in absolute terms, so that "20% off"
	// on a very expensive game cannot become an unbounded giveaway.
	maxDiscount money.Money
	// minOrderAmount is the smallest order the code applies to.
	minOrderAmount money.Money
	status         Status
	// maxRedemptions is how many times the code may be used in total; 1 makes it
	// single-use.
	maxRedemptions  int32
	redemptionCount int32
	issuedBy        string
	expiresAt       *time.Time
	createdAt       time.Time
	version         int64
}

// Spec describes a code to be issued.
type Spec struct {
	ID             string
	Code           string
	PercentBps     int32
	AmountOff      money.Money
	MaxDiscount    money.Money
	MinOrderAmount money.Money
	MaxRedemptions int32
	IssuedBy       string
	ExpiresAt      *time.Time
}

// Issue creates a discount code from a specification.
func Issue(spec Spec, now time.Time) (*Code, error) {
	switch {
	case spec.ID == "":
		return nil, errs.Internal("a discount code requires an id")
	case spec.Code == "":
		return nil, errs.InvalidArgument("the discount code string is required")
	case spec.IssuedBy == "":
		return nil, errs.InvalidArgument("the issuing user is required")
	}

	hasPercent := spec.PercentBps > 0
	hasAmount := spec.AmountOff.IsPositive()
	switch {
	case hasPercent && hasAmount:
		return nil, errs.InvalidArgument("a discount code sets either a percentage or a fixed amount, not both")
	case !hasPercent && !hasAmount:
		return nil, errs.InvalidArgument("a discount code needs either a percentage or a fixed amount")
	case spec.PercentBps > 10_000:
		return nil, errs.InvalidArgument("a percentage discount cannot exceed 100%% (10000 bps), got %d", spec.PercentBps)
	}

	maxRedemptions := spec.MaxRedemptions
	if maxRedemptions <= 0 {
		// A code with no explicit limit is single-use: the safe default for anything
		// that gives money away.
		maxRedemptions = 1
	}
	if spec.ExpiresAt != nil && !spec.ExpiresAt.After(now) {
		return nil, errs.InvalidArgument("the expiry date must be in the future")
	}

	return &Code{
		id:              spec.ID,
		code:            spec.Code,
		percentBps:      spec.PercentBps,
		amountOff:       spec.AmountOff,
		maxDiscount:     spec.MaxDiscount,
		minOrderAmount:  spec.MinOrderAmount,
		status:          StatusActive,
		maxRedemptions:  maxRedemptions,
		redemptionCount: 0,
		issuedBy:        spec.IssuedBy,
		expiresAt:       spec.ExpiresAt,
		createdAt:       now.UTC(),
		version:         1,
	}, nil
}

// Rehydrate reconstructs a Code from stored state.
func Rehydrate(
	id, code string,
	percentBps int32,
	amountOff, maxDiscount, minOrderAmount money.Money,
	status Status,
	maxRedemptions, redemptionCount int32,
	issuedBy string,
	expiresAt *time.Time,
	createdAt time.Time,
	version int64,
) (*Code, error) {
	if id == "" {
		return nil, errs.Internal("cannot rehydrate a discount code without an id")
	}
	if !status.Valid() {
		return nil, errs.Internal("cannot rehydrate discount code %s with unknown status %q", id, status)
	}
	return &Code{
		id:              id,
		code:            code,
		percentBps:      percentBps,
		amountOff:       amountOff,
		maxDiscount:     maxDiscount,
		minOrderAmount:  minOrderAmount,
		status:          status,
		maxRedemptions:  maxRedemptions,
		redemptionCount: redemptionCount,
		issuedBy:        issuedBy,
		expiresAt:       expiresAt,
		createdAt:       createdAt.UTC(),
		version:         version,
	}, nil
}

// Accessors.

// ID returns the identifier.
func (c *Code) ID() string { return c.id }

// Code returns the redeemable string.
func (c *Code) Code() string { return c.code }

// PercentBps returns the percentage reduction in basis points.
func (c *Code) PercentBps() int32 { return c.percentBps }

// AmountOff returns the fixed reduction.
func (c *Code) AmountOff() money.Money { return c.amountOff }

// MaxDiscount returns the absolute cap on a percentage discount.
func (c *Code) MaxDiscount() money.Money { return c.maxDiscount }

// MinOrderAmount returns the minimum qualifying order.
func (c *Code) MinOrderAmount() money.Money { return c.minOrderAmount }

// Status returns the lifecycle state.
func (c *Code) Status() Status { return c.status }

// MaxRedemptions returns the total allowance.
func (c *Code) MaxRedemptions() int32 { return c.maxRedemptions }

// RedemptionCount returns how many times the code has been used.
func (c *Code) RedemptionCount() int32 { return c.redemptionCount }

// IssuedBy returns the issuing user.
func (c *Code) IssuedBy() string { return c.issuedBy }

// ExpiresAt returns the expiry, or nil for a code that never expires.
func (c *Code) ExpiresAt() *time.Time { return c.expiresAt }

// CreatedAt returns the issuance time.
func (c *Code) CreatedAt() time.Time { return c.createdAt }

// Version returns the optimistic-concurrency version.
func (c *Code) Version() int64 { return c.version }

// RemainingRedemptions returns how many uses are left.
func (c *Code) RemainingRedemptions() int32 {
	remaining := c.maxRedemptions - c.redemptionCount
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Quote is the outcome of applying a code to an order.
type Quote struct {
	// Discount is the reduction to apply.
	Discount money.Money
	// Payable is what the buyer still owes: order minus discount.
	Payable money.Money
}

// Preview computes the discount for an order without consuming a redemption.
//
// The Store service calls this on every checkout preview, so it must be free of
// side effects.
func (c *Code) Preview(orderAmount money.Money, now time.Time) (Quote, error) {
	if err := c.checkUsable(now); err != nil {
		return Quote{}, err
	}
	if !orderAmount.IsPositive() {
		return Quote{}, errs.InvalidArgument("the order amount must be greater than zero, got %s", orderAmount)
	}

	if c.minOrderAmount.IsPositive() {
		if !orderAmount.SameCurrency(c.minOrderAmount) {
			return Quote{}, c.currencyError(orderAmount)
		}
		below, err := orderAmount.LessThan(c.minOrderAmount)
		if err != nil {
			return Quote{}, c.currencyError(orderAmount)
		}
		if below {
			return Quote{}, errs.FailedPrecondition("this code applies to orders of %s or more", c.minOrderAmount).
				WithReason(ReasonCodeBelowMinimum).
				WithDetail("min_order_minor", c.minOrderAmount.Minor())
		}
	}

	discount, err := c.rawDiscount(orderAmount)
	if err != nil {
		return Quote{}, err
	}

	// Cap a percentage discount at maxDiscount, when one is configured.
	if c.maxDiscount.IsPositive() {
		if !discount.SameCurrency(c.maxDiscount) {
			return Quote{}, c.currencyError(orderAmount)
		}
		exceeds, err := discount.GreaterThan(c.maxDiscount)
		if err != nil {
			return Quote{}, c.currencyError(orderAmount)
		}
		if exceeds {
			discount = c.maxDiscount
		}
	}

	// A fixed-amount code larger than the order must not produce a negative
	// payable, which would turn a discount into a payout.
	overshoots, err := discount.GreaterThan(orderAmount)
	if err != nil {
		return Quote{}, c.currencyError(orderAmount)
	}
	if overshoots {
		discount = orderAmount
	}

	payable, err := orderAmount.Sub(discount)
	if err != nil {
		return Quote{}, errs.Internal("failed to compute the payable amount").WithCause(err)
	}
	return Quote{Discount: discount, Payable: payable}, nil
}

func (c *Code) rawDiscount(orderAmount money.Money) (money.Money, error) {
	if c.percentBps > 0 {
		discount, err := orderAmount.Percent(int64(c.percentBps))
		if err != nil {
			return money.Money{}, errs.Internal("failed to compute a percentage discount").WithCause(err)
		}
		return discount, nil
	}
	if !orderAmount.SameCurrency(c.amountOff) {
		return money.Money{}, c.currencyError(orderAmount)
	}
	return c.amountOff, nil
}

// Redeem consumes one redemption and returns the quote.
func (c *Code) Redeem(orderAmount money.Money, now time.Time) (Quote, error) {
	quote, err := c.Preview(orderAmount, now)
	if err != nil {
		return Quote{}, err
	}

	c.redemptionCount++
	if c.redemptionCount >= c.maxRedemptions {
		c.status = StatusUsed
	}
	c.version++
	return quote, nil
}

// Revoke cancels the code.
func (c *Code) Revoke(now time.Time) error {
	if c.status == StatusRevoked {
		return errs.Conflict("this discount code is already revoked").WithReason(ReasonCodeRevoked)
	}
	c.status = StatusRevoked
	c.version++
	return nil
}

func (c *Code) checkUsable(now time.Time) error {
	switch c.status {
	case StatusUsed:
		return errs.Conflict("this discount code has been fully redeemed").WithReason(ReasonCodeExhausted)
	case StatusRevoked:
		return errs.Conflict("this discount code has been revoked").WithReason(ReasonCodeRevoked)
	case StatusExpired:
		return errs.FailedPrecondition("this discount code has expired").WithReason(ReasonCodeExpired)
	}
	// Expiry is evaluated live rather than trusting a background job to have flipped
	// the status: a code must stop working the moment it expires, not whenever the
	// sweeper next runs.
	if c.expiresAt != nil && !now.Before(*c.expiresAt) {
		return errs.FailedPrecondition("this discount code expired on %s", c.expiresAt.Format(time.RFC3339)).
			WithReason(ReasonCodeExpired)
	}
	if c.redemptionCount >= c.maxRedemptions {
		return errs.Conflict("this discount code has been fully redeemed").WithReason(ReasonCodeExhausted)
	}
	return nil
}

func (c *Code) currencyError(orderAmount money.Money) error {
	return errs.InvalidArgument("this code is not valid for orders in %s", orderAmount.Currency()).
		WithReason(ReasonCodeCurrencyMismatch)
}

// ErrNotFound reports an unknown code.
func ErrNotFound() error {
	return errs.NotFound("this discount code is not valid").WithReason(ReasonCodeNotFound)
}
