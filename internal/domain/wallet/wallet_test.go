package wallet_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const currency = "IRR"

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func irr(minor int64) money.Money { return money.MustNew(minor, currency) }

// newFunded returns an active wallet holding the given balance.
func newFunded(t *testing.T, balanceMinor int64) *wallet.Wallet {
	t.Helper()
	w, err := wallet.New("w-1", "u-1", currency, now)
	require.NoError(t, err)
	if balanceMinor > 0 {
		_, err = w.Credit(irr(balanceMinor), wallet.ReasonCharge, "pay-1", "initial top-up", now)
		require.NoError(t, err)
	}
	return w
}

func TestNewWallet(t *testing.T) {
	w, err := wallet.New("w-1", "u-1", currency, now)
	require.NoError(t, err)

	assert.Equal(t, "w-1", w.ID())
	assert.Equal(t, "u-1", w.UserID())
	assert.Equal(t, wallet.StatusActive, w.Status())
	assert.True(t, w.Balance().IsZero())
	assert.True(t, w.Held().IsZero())
	assert.True(t, w.Available().IsZero())
	assert.Equal(t, currency, w.Currency())
	assert.EqualValues(t, 1, w.Version())
}

func TestNewWalletValidation(t *testing.T) {
	_, err := wallet.New("", "u-1", currency, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))

	_, err = wallet.New("w-1", "", currency, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))

	_, err = wallet.New("w-1", "u-1", "NOTACURRENCY", now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestCredit(t *testing.T) {
	w := newFunded(t, 0)

	movement, err := w.Credit(irr(50_000), wallet.ReasonCharge, "pay-1", "bank top-up", now)
	require.NoError(t, err)

	assert.Equal(t, wallet.DirectionCredit, movement.Direction)
	assert.EqualValues(t, 50_000, movement.Amount.Minor())
	assert.EqualValues(t, 50_000, movement.BalanceAfter.Minor())
	assert.Equal(t, wallet.ReasonCharge, movement.Reason)
	assert.Equal(t, "pay-1", movement.ReferenceID)
	assert.EqualValues(t, 50_000, w.Balance().Minor())
	assert.EqualValues(t, 2, w.Version(), "a balance change must bump the version")
}

func TestDebit(t *testing.T) {
	w := newFunded(t, 100_000)

	movement, err := w.Debit(irr(30_000), wallet.ReasonPurchase, "order-1", "bought a game", now)
	require.NoError(t, err)

	assert.Equal(t, wallet.DirectionDebit, movement.Direction)
	assert.EqualValues(t, 30_000, movement.Amount.Minor())
	assert.EqualValues(t, 70_000, movement.BalanceAfter.Minor())
	assert.EqualValues(t, 70_000, w.Balance().Minor())
}

func TestDebitExactBalanceIsAllowed(t *testing.T) {
	w := newFunded(t, 100_000)

	_, err := w.Debit(irr(100_000), wallet.ReasonPurchase, "order-1", "", now)
	require.NoError(t, err)
	assert.True(t, w.Balance().IsZero())
}

func TestDebitInsufficientFunds(t *testing.T) {
	w := newFunded(t, 10_000)

	_, err := w.Debit(irr(10_001), wallet.ReasonPurchase, "order-1", "", now)
	require.Error(t, err)

	assert.Equal(t, errs.CodeFailedPrecondition, errs.CodeOf(err),
		"the Store saga distinguishes a business rejection from an infrastructure failure by this code")
	assert.Equal(t, wallet.ReasonCodeInsufficientFunds, errs.ReasonOf(err))
	assert.False(t, errs.IsRetryable(err), "retrying will never make the money appear")
	assert.EqualValues(t, 10_000, w.Balance().Minor(), "a rejected debit must not change the balance")
	assert.EqualValues(t, 2, w.Version(), "a rejected debit must not bump the version")
}

func TestDebitRejectsNonPositiveAmounts(t *testing.T) {
	w := newFunded(t, 100_000)

	for _, amount := range []int64{0, -1, -50_000} {
		_, err := w.Debit(irr(amount), wallet.ReasonPurchase, "order-1", "", now)
		require.Error(t, err, "amount %d must be rejected", amount)
		assert.Equal(t, wallet.ReasonCodeAmountNotPositive, errs.ReasonOf(err))
	}
	assert.EqualValues(t, 100_000, w.Balance().Minor())
}

func TestCreditRejectsNonPositiveAmounts(t *testing.T) {
	w := newFunded(t, 0)

	// A negative credit would be a debit that skips the balance check entirely.
	_, err := w.Credit(irr(-100), wallet.ReasonGiftCard, "gc-1", "", now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeAmountNotPositive, errs.ReasonOf(err))
	assert.True(t, w.Balance().IsZero())
}

func TestMovementRejectsForeignCurrency(t *testing.T) {
	w := newFunded(t, 100_000)

	_, err := w.Debit(money.MustNew(100, "USD"), wallet.ReasonPurchase, "order-1", "", now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeCurrencyMismatch, errs.ReasonOf(err))

	_, err = w.Credit(money.MustNew(100, "USD"), wallet.ReasonCharge, "pay-1", "", now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeCurrencyMismatch, errs.ReasonOf(err))
}

func TestMovementRejectsUnknownReason(t *testing.T) {
	w := newFunded(t, 100_000)

	_, err := w.Debit(irr(100), wallet.Reason("EMBEZZLEMENT"), "ref", "", now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestFrozenWalletRejectsAllMovement(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.Freeze(now))
	assert.Equal(t, wallet.StatusFrozen, w.Status())

	_, err := w.Debit(irr(100), wallet.ReasonPurchase, "order-1", "", now)
	assert.Equal(t, wallet.ReasonCodeWalletFrozen, errs.ReasonOf(err))

	// A frozen wallet blocks credits too: money must not accumulate somewhere the
	// owner cannot spend it while an investigation runs.
	_, err = w.Credit(irr(100), wallet.ReasonCharge, "pay-1", "", now)
	assert.Equal(t, wallet.ReasonCodeWalletFrozen, errs.ReasonOf(err))

	assert.Error(t, w.PlaceHold(irr(100), now))
}

func TestFreezeAndUnfreezeTransitions(t *testing.T) {
	w := newFunded(t, 100_000)

	assert.Error(t, w.Unfreeze(now), "an active wallet cannot be unfrozen")

	require.NoError(t, w.Freeze(now))
	err := w.Freeze(now)
	assert.Equal(t, wallet.ReasonCodeAlreadyFrozen, errs.ReasonOf(err))

	require.NoError(t, w.Unfreeze(now))
	assert.Equal(t, wallet.StatusActive, w.Status())

	// Movement works again afterwards.
	_, err = w.Debit(irr(100), wallet.ReasonPurchase, "order-1", "", now)
	assert.NoError(t, err)
}

func TestClosedWalletRejectsEverything(t *testing.T) {
	w, err := wallet.Rehydrate("w-1", "u-1", irr(100_000), irr(0),
		wallet.StatusClosed, 5, now, now)
	require.NoError(t, err)

	_, err = w.Debit(irr(100), wallet.ReasonPurchase, "order-1", "", now)
	assert.Equal(t, wallet.ReasonCodeWalletClosed, errs.ReasonOf(err))

	assert.Equal(t, wallet.ReasonCodeWalletClosed, errs.ReasonOf(w.Freeze(now)))
}

func TestHoldReservesAvailableBalance(t *testing.T) {
	w := newFunded(t, 100_000)

	require.NoError(t, w.PlaceHold(irr(40_000), now))
	assert.EqualValues(t, 100_000, w.Balance().Minor(), "a hold does not move money")
	assert.EqualValues(t, 40_000, w.Held().Minor())
	assert.EqualValues(t, 60_000, w.Available().Minor())
}

func TestHeldMoneyCannotBeSpent(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.PlaceHold(irr(70_000), now))

	// The total balance would cover this, but the available balance would not — the
	// held amount is already committed to a pre-order.
	_, err := w.Debit(irr(50_000), wallet.ReasonPurchase, "order-1", "", now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeInsufficientFunds, errs.ReasonOf(err))

	_, err = w.Debit(irr(30_000), wallet.ReasonPurchase, "order-1", "", now)
	assert.NoError(t, err, "spending within the available balance is fine")
}

func TestHoldCannotExceedAvailable(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.PlaceHold(irr(60_000), now))

	err := w.PlaceHold(irr(50_000), now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeInsufficientFunds, errs.ReasonOf(err))
	assert.EqualValues(t, 60_000, w.Held().Minor(), "the rejected hold must not be recorded")
}

func TestReleaseHold(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.PlaceHold(irr(40_000), now))
	require.NoError(t, w.ReleaseHold(irr(40_000), now))

	assert.True(t, w.Held().IsZero())
	assert.EqualValues(t, 100_000, w.Available().Minor())
	assert.EqualValues(t, 100_000, w.Balance().Minor())
}

func TestReleaseMoreThanHeldIsRejected(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.PlaceHold(irr(20_000), now))

	err := w.ReleaseHold(irr(20_001), now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeHoldExceedsHeld, errs.ReasonOf(err))
	assert.EqualValues(t, 20_000, w.Held().Minor())
}

func TestCaptureHoldMovesMoneyAndClearsReservation(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.PlaceHold(irr(40_000), now))

	movement, err := w.CaptureHold(irr(40_000), "preorder-1", "instalment 1 of 3", now)
	require.NoError(t, err)

	assert.Equal(t, wallet.DirectionDebit, movement.Direction)
	assert.Equal(t, wallet.ReasonHoldCapture, movement.Reason)
	assert.EqualValues(t, 60_000, w.Balance().Minor())
	assert.True(t, w.Held().IsZero())
	assert.EqualValues(t, 60_000, w.Available().Minor())
}

func TestPartialCapture(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.PlaceHold(irr(40_000), now))

	_, err := w.CaptureHold(irr(15_000), "preorder-1", "", now)
	require.NoError(t, err)
	assert.EqualValues(t, 85_000, w.Balance().Minor())
	assert.EqualValues(t, 25_000, w.Held().Minor(), "the uncaptured remainder stays held")
	assert.EqualValues(t, 60_000, w.Available().Minor())
}

func TestCaptureMoreThanHeldIsRejected(t *testing.T) {
	w := newFunded(t, 100_000)
	require.NoError(t, w.PlaceHold(irr(10_000), now))

	_, err := w.CaptureHold(irr(20_000), "preorder-1", "", now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeHoldExceedsHeld, errs.ReasonOf(err))
	assert.EqualValues(t, 100_000, w.Balance().Minor(), "a rejected capture must not move money")
	assert.EqualValues(t, 10_000, w.Held().Minor())
}

func TestCreditOverflowIsRejected(t *testing.T) {
	w, err := wallet.Rehydrate("w-1", "u-1", irr(9_223_372_036_854_775_000), irr(0),
		wallet.StatusActive, 1, now, now)
	require.NoError(t, err)

	_, err = w.Credit(irr(1_000), wallet.ReasonCharge, "pay-1", "", now)
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeBalanceOverflow, errs.ReasonOf(err))
}

func TestRehydrateRejectsCorruptState(t *testing.T) {
	_, err := wallet.Rehydrate("", "u-1", irr(0), irr(0), wallet.StatusActive, 1, now, now)
	assert.Error(t, err)

	_, err = wallet.Rehydrate("w-1", "u-1", irr(0), irr(0), wallet.Status("MELTED"), 1, now, now)
	assert.Error(t, err)
}

func TestBalanceNeverGoesNegativeUnderRepeatedDebits(t *testing.T) {
	// A property-style check: no sequence of debits can drive the balance below
	// zero, whatever order the amounts arrive in.
	w := newFunded(t, 1_000)
	amounts := []int64{300, 400, 500, 100, 200, 700, 50}

	for _, amount := range amounts {
		_, err := w.Debit(irr(amount), wallet.ReasonPurchase, "order", "", now)
		if err != nil {
			assert.Equal(t, wallet.ReasonCodeInsufficientFunds, errs.ReasonOf(err))
		}
		assert.False(t, w.Balance().IsNegative(), "the balance went negative after debiting %d", amount)
	}
	assert.EqualValues(t, 0, w.Balance().Minor(), "300+400+100+200 exactly exhausts 1000")
}

func TestReasonValidity(t *testing.T) {
	valid := []wallet.Reason{
		wallet.ReasonPurchase, wallet.ReasonRevenue, wallet.ReasonRefund, wallet.ReasonReversal,
		wallet.ReasonCharge, wallet.ReasonGiftCard, wallet.ReasonTrade, wallet.ReasonDiscount,
		wallet.ReasonInterest, wallet.ReasonHoldCapture, wallet.ReasonAdjustment,
	}
	for _, reason := range valid {
		assert.True(t, reason.Valid(), "%s must be a valid reason", reason)
	}
	assert.False(t, wallet.Reason("").Valid())
	assert.False(t, wallet.Reason("NONSENSE").Valid())
}
