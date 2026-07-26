// Package wallet holds the Wallet aggregate — the heart of the service.
//
// Nothing in this package imports a database driver, a broker client or a
// transport. Every rule that protects a user's money is expressed here as plain
// Go, which is what makes those rules cheap to test and impossible to bypass by
// reaching around the persistence layer.
package wallet

import (
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// Status is the wallet's lifecycle state.
type Status string

const (
	// StatusActive — normal operation.
	StatusActive Status = "ACTIVE"
	// StatusFrozen — no money may move in or out. Support freezes a wallet while
	// investigating suspected gift-card abuse or a disputed transaction. Freezing
	// blocks credits as well as debits: letting money in but not out would leave a
	// user's balance hostage with no way to reconcile it.
	StatusFrozen Status = "FROZEN"
	// StatusClosed — terminal; the account is gone.
	StatusClosed Status = "CLOSED"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusFrozen, StatusClosed:
		return true
	default:
		return false
	}
}

// Reason explains why money moved. It mirrors the ledger reason enum in the
// contract and is part of the permanent audit record.
type Reason string

// The complete set of ledger reasons.
const (
	ReasonPurchase    Reason = "PURCHASE"
	ReasonRevenue     Reason = "REVENUE"
	ReasonRefund      Reason = "REFUND"
	ReasonReversal    Reason = "REVERSAL"
	ReasonCharge      Reason = "CHARGE"
	ReasonGiftCard    Reason = "GIFTCARD"
	ReasonTrade       Reason = "TRADE"
	ReasonDiscount    Reason = "DISCOUNT"
	ReasonInterest    Reason = "INTEREST"
	ReasonHoldCapture Reason = "HOLD_CAPTURE"
	ReasonAdjustment  Reason = "ADJUSTMENT"
)

// Valid reports whether r is a known reason.
func (r Reason) Valid() bool {
	switch r {
	case ReasonPurchase, ReasonRevenue, ReasonRefund, ReasonReversal, ReasonCharge,
		ReasonGiftCard, ReasonTrade, ReasonDiscount, ReasonInterest, ReasonHoldCapture,
		ReasonAdjustment:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (r Reason) String() string { return string(r) }

// Direction is the side of a ledger entry.
type Direction string

const (
	// DirectionDebit takes money out of a wallet.
	DirectionDebit Direction = "DEBIT"
	// DirectionCredit puts money into a wallet.
	DirectionCredit Direction = "CREDIT"
)

// String implements fmt.Stringer.
func (d Direction) String() string { return string(d) }

// Movement describes a single balance change that the aggregate has just applied.
//
// The aggregate returns one of these instead of writing to the ledger itself: the
// domain decides *what* happened, the repository decides *where* it is recorded.
type Movement struct {
	Direction    Direction
	Amount       money.Money
	BalanceAfter money.Money
	Reason       Reason
	ReferenceID  string
	Description  string
}

// Wallet is the aggregate root guarding one user's balance.
type Wallet struct {
	id        string
	userID    string
	balance   money.Money
	held      money.Money
	status    Status
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// New creates an active wallet with a zero balance.
func New(id, userID, currency string, now time.Time) (*Wallet, error) {
	if id == "" {
		return nil, errs.InvalidArgument("wallet id is required")
	}
	if userID == "" {
		return nil, errs.InvalidArgument("user id is required")
	}
	zero, err := money.New(0, currency)
	if err != nil {
		return nil, errs.InvalidArgument("invalid currency: %s", err.Error()).WithCause(err)
	}

	return &Wallet{
		id:        id,
		userID:    userID,
		balance:   zero,
		held:      zero,
		status:    StatusActive,
		version:   1,
		createdAt: now.UTC(),
		updatedAt: now.UTC(),
	}, nil
}

// Rehydrate reconstructs a Wallet from stored state. Only repositories call it.
//
// It deliberately performs no validation beyond structural sanity: data already in
// the database is history, and refusing to load a row because it offends a rule
// added later would make the wallet unreachable rather than fixable.
func Rehydrate(
	id, userID string,
	balance, held money.Money,
	status Status,
	version int64,
	createdAt, updatedAt time.Time,
) (*Wallet, error) {
	if id == "" || userID == "" {
		return nil, errs.Internal("cannot rehydrate a wallet without an id and a user id")
	}
	if !status.Valid() {
		return nil, errs.Internal("cannot rehydrate wallet %s with unknown status %q", id, status)
	}
	return &Wallet{
		id:        id,
		userID:    userID,
		balance:   balance,
		held:      held,
		status:    status,
		version:   version,
		createdAt: createdAt.UTC(),
		updatedAt: updatedAt.UTC(),
	}, nil
}

// Accessors. The fields stay unexported so that no caller can move money without
// going through a method that enforces the invariants.

// ID returns the wallet identifier.
func (w *Wallet) ID() string { return w.id }

// UserID returns the owning user.
func (w *Wallet) UserID() string { return w.userID }

// Balance returns the total balance, including any held amount.
func (w *Wallet) Balance() money.Money { return w.balance }

// Held returns the sum of active holds.
func (w *Wallet) Held() money.Money { return w.held }

// Status returns the lifecycle state.
func (w *Wallet) Status() Status { return w.status }

// Version returns the optimistic-concurrency version.
func (w *Wallet) Version() int64 { return w.version }

// Currency returns the wallet's ISO currency code.
func (w *Wallet) Currency() string { return w.balance.Currency() }

// CreatedAt returns the creation timestamp.
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }

// UpdatedAt returns the last mutation timestamp.
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// Available returns the spendable balance: total minus what is held for pending
// pre-orders and instalments.
func (w *Wallet) Available() money.Money {
	available, err := w.balance.Sub(w.held)
	if err != nil {
		// Both amounts are this wallet's own currency, so Sub cannot fail here. A
		// zero is still safer than a panic on the money path.
		return money.Zero(w.balance.Currency())
	}
	return available
}

// Debit removes money from the wallet.
//
// The available balance — not the total — is what must cover the debit, otherwise
// a purchase could spend money already committed to a pre-order.
func (w *Wallet) Debit(amount money.Money, reason Reason, referenceID, description string, now time.Time) (Movement, error) {
	if err := w.checkMovement(amount, reason); err != nil {
		return Movement{}, err
	}

	sufficient, err := w.Available().Cmp(amount)
	if err != nil {
		return Movement{}, currencyMismatch(err)
	}
	if sufficient < 0 {
		return Movement{}, ErrInsufficientFunds(w.Available(), amount)
	}

	newBalance, err := w.balance.Sub(amount)
	if err != nil {
		return Movement{}, currencyMismatch(err)
	}
	// Defence in depth: the available-balance check above already guarantees this,
	// but a negative balance is the one state this aggregate must never reach.
	if newBalance.IsNegative() {
		return Movement{}, ErrInsufficientFunds(w.Available(), amount)
	}

	w.balance = newBalance
	w.touch(now)

	return Movement{
		Direction:    DirectionDebit,
		Amount:       amount,
		BalanceAfter: newBalance,
		Reason:       reason,
		ReferenceID:  referenceID,
		Description:  description,
	}, nil
}

// Credit adds money to the wallet.
func (w *Wallet) Credit(amount money.Money, reason Reason, referenceID, description string, now time.Time) (Movement, error) {
	if err := w.checkMovement(amount, reason); err != nil {
		return Movement{}, err
	}

	newBalance, err := w.balance.Add(amount)
	if err != nil {
		if isOverflow(err) {
			return Movement{}, errs.FailedPrecondition("crediting %s would overflow the balance", amount).
				WithReason(ReasonCodeBalanceOverflow)
		}
		return Movement{}, currencyMismatch(err)
	}

	w.balance = newBalance
	w.touch(now)

	return Movement{
		Direction:    DirectionCredit,
		Amount:       amount,
		BalanceAfter: newBalance,
		Reason:       reason,
		ReferenceID:  referenceID,
		Description:  description,
	}, nil
}

// PlaceHold reserves part of the available balance without moving it.
//
// A hold is how a pre-order or an instalment plan claims money it has not spent
// yet: the balance is untouched, so the ledger stays clean, but the amount can no
// longer be spent on something else.
func (w *Wallet) PlaceHold(amount money.Money, now time.Time) error {
	if err := w.checkActive(); err != nil {
		return err
	}
	if !amount.IsPositive() {
		return ErrAmountNotPositive(amount)
	}

	sufficient, err := w.Available().Cmp(amount)
	if err != nil {
		return currencyMismatch(err)
	}
	if sufficient < 0 {
		return ErrInsufficientFunds(w.Available(), amount)
	}

	newHeld, err := w.held.Add(amount)
	if err != nil {
		return currencyMismatch(err)
	}
	w.held = newHeld
	w.touch(now)
	return nil
}

// ReleaseHold gives a reserved amount back to the available balance.
func (w *Wallet) ReleaseHold(amount money.Money, now time.Time) error {
	if !amount.IsPositive() {
		return ErrAmountNotPositive(amount)
	}

	newHeld, err := w.held.Sub(amount)
	if err != nil {
		return currencyMismatch(err)
	}
	if newHeld.IsNegative() {
		// Releasing more than is held means the caller lost track of a hold. Failing
		// loudly beats silently inventing available balance.
		return errs.Conflict("cannot release %s: only %s is held", amount, w.held).
			WithReason(ReasonCodeHoldExceedsHeld)
	}

	w.held = newHeld
	w.touch(now)
	return nil
}

// CaptureHold converts a hold into a real debit: the reservation is released and
// the same amount leaves the balance, atomically from the aggregate's point of
// view.
func (w *Wallet) CaptureHold(amount money.Money, referenceID, description string, now time.Time) (Movement, error) {
	if err := w.checkActive(); err != nil {
		return Movement{}, err
	}
	if err := w.ReleaseHold(amount, now); err != nil {
		return Movement{}, err
	}
	return w.Debit(amount, ReasonHoldCapture, referenceID, description, now)
}

// Freeze suspends all movement.
func (w *Wallet) Freeze(now time.Time) error {
	switch w.status {
	case StatusFrozen:
		return errs.Conflict("wallet %s is already frozen", w.id).WithReason(ReasonCodeAlreadyFrozen)
	case StatusClosed:
		return errs.Conflict("wallet %s is closed", w.id).WithReason(ReasonCodeWalletClosed)
	}
	w.status = StatusFrozen
	w.touch(now)
	return nil
}

// Unfreeze restores normal operation.
func (w *Wallet) Unfreeze(now time.Time) error {
	if w.status != StatusFrozen {
		return errs.Conflict("wallet %s is not frozen", w.id).WithReason(ReasonCodeNotFrozen)
	}
	w.status = StatusActive
	w.touch(now)
	return nil
}

// checkMovement validates the preconditions shared by Debit and Credit.
func (w *Wallet) checkMovement(amount money.Money, reason Reason) error {
	if err := w.checkActive(); err != nil {
		return err
	}
	if !reason.Valid() {
		return errs.InvalidArgument("unknown ledger reason %q", reason)
	}
	if !amount.IsPositive() {
		// Direction carries the sign. A signed amount would allow a "debit of minus
		// one hundred" that silently credits the wallet.
		return ErrAmountNotPositive(amount)
	}
	if amount.Currency() != w.balance.Currency() {
		return errs.InvalidArgument("wallet %s holds %s but the amount is in %s",
			w.id, w.balance.Currency(), amount.Currency()).
			WithReason(ReasonCodeCurrencyMismatch)
	}
	return nil
}

func (w *Wallet) checkActive() error {
	switch w.status {
	case StatusFrozen:
		return errs.FailedPrecondition("wallet %s is frozen; contact support", w.id).
			WithReason(ReasonCodeWalletFrozen)
	case StatusClosed:
		return errs.FailedPrecondition("wallet %s is closed", w.id).
			WithReason(ReasonCodeWalletClosed)
	default:
		return nil
	}
}

func (w *Wallet) touch(now time.Time) {
	w.version++
	w.updatedAt = now.UTC()
}
