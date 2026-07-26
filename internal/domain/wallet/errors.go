package wallet

import (
	"errors"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
)

// Reason codes are stable, machine-readable discriminators. Clients — the Store
// service's saga in particular — branch on these strings, so they are part of the
// service's public contract and must not be renamed casually.
const (
	ReasonCodeInsufficientFunds = "INSUFFICIENT_FUNDS"
	ReasonCodeWalletFrozen      = "WALLET_FROZEN"
	ReasonCodeWalletClosed      = "WALLET_CLOSED"
	ReasonCodeAlreadyFrozen     = "WALLET_ALREADY_FROZEN"
	ReasonCodeNotFrozen         = "WALLET_NOT_FROZEN"
	ReasonCodeCurrencyMismatch  = "CURRENCY_MISMATCH"
	ReasonCodeAmountNotPositive = "AMOUNT_NOT_POSITIVE"
	ReasonCodeBalanceOverflow   = "BALANCE_OVERFLOW"
	ReasonCodeHoldExceedsHeld   = "HOLD_EXCEEDS_HELD"
	ReasonCodeWalletNotFound    = "WALLET_NOT_FOUND"
)

// ErrInsufficientFunds reports that the available balance cannot cover a debit.
//
// This is the single most important error the service produces: the Store saga
// reads it to decide that a purchase has failed for a business reason and that no
// compensation is needed, as opposed to an infrastructure failure that must be
// retried.
func ErrInsufficientFunds(available, requested money.Money) error {
	return errs.FailedPrecondition("insufficient funds: %s available, %s requested", available, requested).
		WithReason(ReasonCodeInsufficientFunds).
		WithDetail("available_minor", available.Minor()).
		WithDetail("requested_minor", requested.Minor()).
		WithDetail("currency", requested.Currency())
}

// ErrAmountNotPositive reports a zero or negative amount.
func ErrAmountNotPositive(amount money.Money) error {
	return errs.InvalidArgument("the amount must be greater than zero, got %s", amount).
		WithReason(ReasonCodeAmountNotPositive)
}

// ErrNotFound reports a missing wallet.
func ErrNotFound(userID string) error {
	return errs.NotFound("no wallet exists for user %s", userID).
		WithReason(ReasonCodeWalletNotFound)
}

// currencyMismatch converts a money package currency error into a domain error.
func currencyMismatch(err error) error {
	if errors.Is(err, money.ErrCurrencyMismatch) {
		return errs.InvalidArgument("currency mismatch: %s", err.Error()).
			WithReason(ReasonCodeCurrencyMismatch).
			WithCause(err)
	}
	return errs.Internal("money arithmetic failed").WithCause(err)
}

func isOverflow(err error) bool { return errors.Is(err, money.ErrOverflow) }
