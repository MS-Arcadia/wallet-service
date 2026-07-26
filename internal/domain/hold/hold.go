// Package hold models a reservation against a wallet balance.
//
// Holds are what make pre-orders and instalment plans possible without inventing
// a second balance. The money stays in the wallet and stays in the ledger's sum,
// but it can no longer be spent on anything else. A hold either turns into a real
// debit (capture), goes back to the user (release), or lapses (expiry).
package hold

import (
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// Status is the hold's lifecycle state.
type Status string

const (
	// StatusActive — reserving funds.
	StatusActive Status = "ACTIVE"
	// StatusCaptured — fully converted into a debit.
	StatusCaptured Status = "CAPTURED"
	// StatusReleased — returned to the available balance.
	StatusReleased Status = "RELEASED"
	// StatusExpired — released automatically after its TTL elapsed.
	StatusExpired Status = "EXPIRED"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusCaptured, StatusReleased, StatusExpired:
		return true
	default:
		return false
	}
}

// Terminal reports whether no further transition is possible.
func (s Status) Terminal() bool { return s != StatusActive }

// Reason codes returned by this package.
const (
	ReasonCodeNotFound         = "HOLD_NOT_FOUND"
	ReasonCodeNotActive        = "HOLD_NOT_ACTIVE"
	ReasonCodeExpired          = "HOLD_EXPIRED"
	ReasonCodeExceedsRemaining = "CAPTURE_EXCEEDS_REMAINING"
)

// Hold is the aggregate.
type Hold struct {
	id       string
	walletID string
	userID   string
	// amount is the original reservation.
	amount money.Money
	// capturedAmount accumulates partial captures, which is how a three-instalment
	// plan draws down one hold.
	capturedAmount money.Money
	status         Status
	// referenceID points at the pre-order or instalment plan.
	referenceID string
	reason      string
	expiresAt   *time.Time
	createdAt   time.Time
	resolvedAt  *time.Time
	version     int64
}

// New creates an active hold.
func New(
	id, walletID, userID string,
	amount money.Money,
	referenceID, reason string,
	ttl time.Duration,
	now time.Time,
) (*Hold, error) {
	switch {
	case id == "":
		return nil, errs.Internal("a hold requires an id")
	case walletID == "":
		return nil, errs.Internal("a hold requires a wallet id")
	case !amount.IsPositive():
		return nil, errs.InvalidArgument("a hold amount must be greater than zero, got %s", amount)
	case referenceID == "":
		return nil, errs.InvalidArgument("a hold requires a reference id so it can be reconciled later")
	}

	var expiresAt *time.Time
	if ttl > 0 {
		expiry := now.UTC().Add(ttl)
		expiresAt = &expiry
	}

	return &Hold{
		id:             id,
		walletID:       walletID,
		userID:         userID,
		amount:         amount,
		capturedAmount: money.Zero(amount.Currency()),
		status:         StatusActive,
		referenceID:    referenceID,
		reason:         reason,
		expiresAt:      expiresAt,
		createdAt:      now.UTC(),
		version:        1,
	}, nil
}

// Rehydrate reconstructs a Hold from stored state.
func Rehydrate(
	id, walletID, userID string,
	amount, capturedAmount money.Money,
	status Status,
	referenceID, reason string,
	expiresAt *time.Time,
	createdAt time.Time,
	resolvedAt *time.Time,
	version int64,
) (*Hold, error) {
	if id == "" {
		return nil, errs.Internal("cannot rehydrate a hold without an id")
	}
	if !status.Valid() {
		return nil, errs.Internal("cannot rehydrate hold %s with unknown status %q", id, status)
	}
	return &Hold{
		id:             id,
		walletID:       walletID,
		userID:         userID,
		amount:         amount,
		capturedAmount: capturedAmount,
		status:         status,
		referenceID:    referenceID,
		reason:         reason,
		expiresAt:      expiresAt,
		createdAt:      createdAt.UTC(),
		resolvedAt:     resolvedAt,
		version:        version,
	}, nil
}

// Accessors.

// ID returns the identifier.
func (h *Hold) ID() string { return h.id }

// WalletID returns the affected wallet.
func (h *Hold) WalletID() string { return h.walletID }

// UserID returns the owning user.
func (h *Hold) UserID() string { return h.userID }

// Amount returns the original reservation.
func (h *Hold) Amount() money.Money { return h.amount }

// CapturedAmount returns how much has been captured so far.
func (h *Hold) CapturedAmount() money.Money { return h.capturedAmount }

// Status returns the lifecycle state.
func (h *Hold) Status() Status { return h.status }

// ReferenceID returns the originating entity.
func (h *Hold) ReferenceID() string { return h.referenceID }

// Reason returns the human-readable purpose.
func (h *Hold) Reason() string { return h.reason }

// ExpiresAt returns the expiry, or nil for a hold that never lapses.
func (h *Hold) ExpiresAt() *time.Time { return h.expiresAt }

// CreatedAt returns the creation time.
func (h *Hold) CreatedAt() time.Time { return h.createdAt }

// ResolvedAt returns when the hold reached a terminal state, or nil.
func (h *Hold) ResolvedAt() *time.Time { return h.resolvedAt }

// Version returns the optimistic-concurrency version.
func (h *Hold) Version() int64 { return h.version }

// Remaining returns the amount still reserved: the original minus what has been
// captured.
func (h *Hold) Remaining() money.Money {
	remaining, err := h.amount.Sub(h.capturedAmount)
	if err != nil {
		return money.Zero(h.amount.Currency())
	}
	if remaining.IsNegative() {
		return money.Zero(h.amount.Currency())
	}
	return remaining
}

// IsExpired reports whether the TTL has elapsed.
func (h *Hold) IsExpired(now time.Time) bool {
	return h.expiresAt != nil && !now.Before(*h.expiresAt)
}

// Capture draws amount down from the hold.
//
// Passing a zero amount captures everything remaining, which is the common case
// for a single-shot pre-order.
func (h *Hold) Capture(amount money.Money, now time.Time) (money.Money, error) {
	if err := h.checkActive(now); err != nil {
		return money.Money{}, err
	}

	if amount.IsZero() {
		amount = h.Remaining()
	}
	if !amount.IsPositive() {
		return money.Money{}, errs.InvalidArgument("the capture amount must be greater than zero, got %s", amount)
	}

	exceeds, err := amount.GreaterThan(h.Remaining())
	if err != nil {
		return money.Money{}, errs.InvalidArgument("currency mismatch capturing hold %s", h.id).WithCause(err)
	}
	if exceeds {
		return money.Money{}, errs.FailedPrecondition("cannot capture %s: only %s remains on this hold", amount, h.Remaining()).
			WithReason(ReasonCodeExceedsRemaining).
			WithDetail("remaining_minor", h.Remaining().Minor())
	}

	captured, err := h.capturedAmount.Add(amount)
	if err != nil {
		return money.Money{}, errs.Internal("failed to accumulate the captured amount").WithCause(err)
	}
	h.capturedAmount = captured

	// Only a full draw-down closes the hold; a partial capture leaves the remainder
	// reserved for the next instalment.
	if h.Remaining().IsZero() {
		resolvedAt := now.UTC()
		h.status = StatusCaptured
		h.resolvedAt = &resolvedAt
	}
	h.version++
	return amount, nil
}

// Release returns the remaining reservation to the available balance and closes
// the hold.
func (h *Hold) Release(now time.Time) (money.Money, error) {
	if h.status.Terminal() {
		return money.Money{}, errs.Conflict("hold %s is already %s", h.id, h.status).
			WithReason(ReasonCodeNotActive)
	}

	released := h.Remaining()
	resolvedAt := now.UTC()
	h.status = StatusReleased
	h.resolvedAt = &resolvedAt
	h.version++
	return released, nil
}

// Expire releases a lapsed hold. It is Release with a different terminal status,
// so that a report can distinguish a user cancelling from a plan timing out.
func (h *Hold) Expire(now time.Time) (money.Money, error) {
	if h.status.Terminal() {
		return money.Money{}, errs.Conflict("hold %s is already %s", h.id, h.status).
			WithReason(ReasonCodeNotActive)
	}
	if !h.IsExpired(now) {
		return money.Money{}, errs.FailedPrecondition("hold %s has not expired yet", h.id)
	}

	released := h.Remaining()
	resolvedAt := now.UTC()
	h.status = StatusExpired
	h.resolvedAt = &resolvedAt
	h.version++
	return released, nil
}

func (h *Hold) checkActive(now time.Time) error {
	if h.status.Terminal() {
		return errs.Conflict("hold %s is already %s", h.id, h.status).WithReason(ReasonCodeNotActive)
	}
	// An expired hold is refused even before the sweeper has flipped its status: the
	// reservation has lapsed, and capturing it would take money the user is entitled
	// to have back.
	if h.IsExpired(now) {
		return errs.FailedPrecondition("hold %s expired on %s", h.id, h.expiresAt.Format(time.RFC3339)).
			WithReason(ReasonCodeExpired)
	}
	return nil
}

// ErrNotFound reports a missing hold.
func ErrNotFound(id string) error {
	return errs.NotFound("no hold exists with id %s", id).WithReason(ReasonCodeNotFound)
}

// Filter narrows a hold query.
type Filter struct {
	WalletID string
	UserID   string
	Status   Status
	Limit    int
	Offset   int
}
