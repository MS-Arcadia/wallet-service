package app_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Provisioning ---------------------------------------------------------

func TestGetOrCreateWalletProvisionsOnFirstAccess(t *testing.T) {
	h := newHarness(t)

	view, err := h.Wallets.GetOrCreateWallet(asUser("u-1"), "u-1")
	require.NoError(t, err)

	assert.Equal(t, "u-1", view.UserID)
	assert.Equal(t, wallet.StatusActive, view.Status)
	assert.True(t, view.Balance.IsZero())
	assert.True(t, h.Store.HasEvent(app.EventWalletCreated))
}

func TestGetOrCreateWalletIsIdempotent(t *testing.T) {
	h := newHarness(t)

	first, err := h.Wallets.GetOrCreateWallet(asUser("u-1"), "u-1")
	require.NoError(t, err)
	second, err := h.Wallets.GetOrCreateWallet(asUser("u-1"), "u-1")
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	assert.Len(t, h.Store.EventsOfType(app.EventWalletCreated), 1,
		"a second read must not announce a second wallet")
	assert.Len(t, h.Store.Wallets, 1)
}

func TestUsersCannotReadEachOthersWallets(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 100_000)

	_, err := h.Wallets.GetOrCreateWallet(asUser("u-2"), "u-1")
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = h.Wallets.GetWallet(asSupport("s-1"), "u-1")
	assert.NoError(t, err, "support may inspect any wallet")

	_, err = h.Wallets.GetWallet(anonymous(), "u-1")
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

func TestEnsureWalletWorksWithoutAPrincipal(t *testing.T) {
	h := newHarness(t)

	// This is the UserRegistered consumer's path: an event from Auth, no end user in
	// the context to authorise.
	view, err := h.Wallets.EnsureWallet(anonymous(), "u-1")
	require.NoError(t, err)
	assert.Equal(t, "u-1", view.UserID)
}

// --- Debit ----------------------------------------------------------------

func TestDebitMovesMoneyAndRecordsEverything(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	result, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(120_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		Description:    "Elden Ring",
		IdempotencyKey: "debit-order-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 380_000, result.Wallet.Balance.Minor())
	assert.EqualValues(t, 380_000, h.balanceOf("u-1"))
	assert.False(t, result.IdempotentReplay)

	// The ledger entry.
	assert.Equal(t, wallet.DirectionDebit, result.Entry.Direction)
	assert.EqualValues(t, 120_000, result.Entry.Amount.Minor())
	assert.EqualValues(t, 380_000, result.Entry.BalanceAfter.Minor(),
		"balance_after lets an auditor replay the chain without recomputing totals")
	assert.Equal(t, "order-1", result.Entry.ReferenceID)

	// The domain event the saga listens for, and the audit record.
	assert.True(t, h.Store.HasEvent(app.EventWalletDebited))
	assert.True(t, h.Store.HasEvent(app.EventAuditRecorded))
	assert.Positive(t, h.Store.Notified, "the outbox dispatcher must be nudged after committing")
}

func TestDebitBeyondBalanceChangesNothing(t *testing.T) {
	h := newHarness(t)
	view := h.fund("u-1", 50_000)

	_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(50_001),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		IdempotencyKey: "debit-order-1",
	})
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeInsufficientFunds, errs.ReasonOf(err))

	// The transaction rolled back completely: no balance change, no ledger entry
	// beyond the fixture credit, and no event announcing a movement that never
	// happened.
	assert.EqualValues(t, 50_000, h.balanceOf("u-1"))
	entries := h.Store.LedgerFor(view.ID)
	require.Len(t, entries, 1, "a rejected debit must not append to the ledger")
	assert.Equal(t, wallet.DirectionCredit, entries[0].Direction, "only the fixture credit remains")
	assert.False(t, h.Store.HasEvent(app.EventWalletDebited))
}

func TestARejectedDebitDoesNotBurnItsIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 50_000)

	cmd := app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(80_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		IdempotencyKey: "debit-order-1",
	}
	_, err := h.Wallets.Debit(asStoreService(), cmd)
	require.Error(t, err)

	// The rollback took the idempotency claim with it, so once the user tops up, the
	// saga can retry the same command and it will succeed. A key stranded by a
	// business rejection would deadlock the order permanently.
	_, err = h.Wallets.Credit(asStoreService(), app.CreditCommand{
		UserID: "u-1", Amount: irr(50_000), Reason: wallet.ReasonCharge,
		ReferenceID: "topup", IdempotencyKey: "topup-1",
	})
	require.NoError(t, err)

	result, err := h.Wallets.Debit(asStoreService(), cmd)
	require.NoError(t, err)
	assert.False(t, result.IdempotentReplay)
	assert.EqualValues(t, 20_000, h.balanceOf("u-1"))
}

func TestInsufficientFundsPublishesPaymentFailedForTheSaga(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 10_000)

	_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(999_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-42",
		IdempotencyKey: "debit-order-42",
	})
	require.Error(t, err)

	// The Store orchestrator waits on the broker, not on the RPC, so the rejection
	// has to reach it as an event or the saga stalls forever.
	events := h.Store.EventsOfType(app.EventPaymentFailed)
	require.Len(t, events, 1)

	var payload app.PaymentFailedPayload
	require.NoError(t, events[0].DecodePayload(&payload))
	assert.Equal(t, "u-1", payload.UserID)
	assert.Equal(t, "order-42", payload.ReferenceID)
	assert.Equal(t, wallet.ReasonCodeInsufficientFunds, payload.Reason)
	assert.EqualValues(t, 999_000, payload.Requested.Minor())
	assert.EqualValues(t, 10_000, payload.Available.Minor())
}

func TestFrozenWalletDebitAlsoPublishesPaymentFailed(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Admin.FreezeWallet(asSupport("s-1"), "u-1", "gift card abuse investigation")
	require.NoError(t, err)

	_, err = h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(1_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		IdempotencyKey: "debit-order-1",
	})
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeWalletFrozen, errs.ReasonOf(err))

	events := h.Store.EventsOfType(app.EventPaymentFailed)
	require.Len(t, events, 1)

	var payload app.PaymentFailedPayload
	require.NoError(t, events[0].DecodePayload(&payload))
	assert.Equal(t, wallet.ReasonCodeWalletFrozen, payload.Reason)
}

func TestDebitRequiresAnIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID:      "u-1",
		Amount:      irr(1_000),
		Reason:      wallet.ReasonPurchase,
		ReferenceID: "order-1",
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err),
		"without a key a network retry would move money twice")
}

func TestUsersCannotDebitOtherWallets(t *testing.T) {
	h := newHarness(t)
	h.fund("victim", 500_000)

	_, err := h.Wallets.Debit(asUser("attacker"), app.DebitCommand{
		UserID:         "victim",
		Amount:         irr(500_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		IdempotencyKey: "steal-1",
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
	assert.EqualValues(t, 500_000, h.balanceOf("victim"))
}

// --- Idempotency ----------------------------------------------------------

func TestRetriedDebitMovesMoneyOnce(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	cmd := app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(120_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		IdempotencyKey: "debit-order-1",
	}

	first, err := h.Wallets.Debit(asStoreService(), cmd)
	require.NoError(t, err)
	assert.False(t, first.IdempotentReplay)

	// The client's connection dropped and it retried the identical request.
	second, err := h.Wallets.Debit(asStoreService(), cmd)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay, "the retry must be recognised, not re-executed")
	assert.Equal(t, first.Entry.ID, second.Entry.ID, "the original entry is replayed verbatim")
	assert.EqualValues(t, 380_000, h.balanceOf("u-1"), "the money moved exactly once")
	assert.Len(t, h.Store.EventsOfType(app.EventWalletDebited), 1,
		"a replay must not announce a second movement")
	assert.Equal(t, 1, h.Metrics.Replays["debit"])
}

func TestManyRetriesStillMoveMoneyOnce(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	cmd := app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(100_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		IdempotencyKey: "debit-order-1",
	}
	for i := 0; i < 10; i++ {
		_, err := h.Wallets.Debit(asStoreService(), cmd)
		require.NoError(t, err, "attempt %d", i+1)
	}

	assert.EqualValues(t, 400_000, h.balanceOf("u-1"))
	assert.Len(t, h.Store.LedgerFor(h.fund("u-1", 0).ID), 2,
		"the fixture credit plus one debit — not ten debits")
}

func TestReusingAKeyForADifferentRequestIsRejected(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(100_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-1",
		IdempotencyKey: "shared-key",
	})
	require.NoError(t, err)

	// Same key, different amount. That is a client bug, and silently returning the
	// first answer would hide it.
	_, err = h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID:         "u-1",
		Amount:         irr(250_000),
		Reason:         wallet.ReasonPurchase,
		ReferenceID:    "order-2",
		IdempotencyKey: "shared-key",
	})
	require.Error(t, err)
	assert.Equal(t, "IDEMPOTENCY_KEY_REUSED", errs.ReasonOf(err))
	assert.EqualValues(t, 400_000, h.balanceOf("u-1"))
}

func TestTheSameKeyIsUsableAcrossDifferentOperations(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID: "u-1", Amount: irr(100_000), Reason: wallet.ReasonPurchase,
		ReferenceID: "order-1", IdempotencyKey: "key-1",
	})
	require.NoError(t, err)

	// Operations namespace their keys, so a debit and a credit under the same key
	// value do not collide.
	_, err = h.Wallets.Credit(asStoreService(), app.CreditCommand{
		UserID: "u-1", Amount: irr(50_000), Reason: wallet.ReasonRefund,
		ReferenceID: "order-1", IdempotencyKey: "key-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 450_000, h.balanceOf("u-1"))
}

// --- Credit ---------------------------------------------------------------

func TestCreditRevenueShare(t *testing.T) {
	h := newHarness(t)
	h.fund("developer-1", 0)

	// The 70% developer share of a 100,000 sale, as computed by Store.
	result, err := h.Wallets.Credit(asStoreService(), app.CreditCommand{
		UserID:         "developer-1",
		Amount:         irr(70_000),
		Reason:         wallet.ReasonRevenue,
		ReferenceID:    "order-1",
		Description:    "70% revenue share",
		IdempotencyKey: "credit-dev-order-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 70_000, result.Wallet.Balance.Minor())
	assert.Equal(t, wallet.ReasonRevenue, result.Entry.Reason)
	assert.True(t, h.Store.HasEvent(app.EventWalletCredited))
}

// --- Transfer -------------------------------------------------------------

func TestTransferSettlesBothSidesAtomically(t *testing.T) {
	h := newHarness(t)
	h.fund("buyer", 500_000)
	h.fund("seller", 10_000)

	result, err := h.Wallets.Transfer(asStoreService(), app.TransferCommand{
		FromUserID:     "buyer",
		ToUserID:       "seller",
		Amount:         irr(120_000),
		Reason:         wallet.ReasonTrade,
		ReferenceID:    "trade-1",
		Description:    "marketplace item settlement",
		IdempotencyKey: "settle-trade-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 380_000, h.balanceOf("buyer"))
	assert.EqualValues(t, 130_000, h.balanceOf("seller"))
	assert.Equal(t, wallet.DirectionDebit, result.DebitEntry.Direction)
	assert.Equal(t, wallet.DirectionCredit, result.CreditEntry.Direction)
	assert.True(t, h.Store.HasEvent(app.EventFundsTransferred))
}

func TestTransferWithInsufficientFundsMovesNothing(t *testing.T) {
	h := newHarness(t)
	h.fund("buyer", 1_000)
	h.fund("seller", 10_000)

	_, err := h.Wallets.Transfer(asStoreService(), app.TransferCommand{
		FromUserID:     "buyer",
		ToUserID:       "seller",
		Amount:         irr(120_000),
		Reason:         wallet.ReasonTrade,
		ReferenceID:    "trade-1",
		IdempotencyKey: "settle-trade-1",
	})
	require.Error(t, err)

	// The seller must not be credited for a payment the buyer could not make.
	assert.EqualValues(t, 1_000, h.balanceOf("buyer"))
	assert.EqualValues(t, 10_000, h.balanceOf("seller"))
	assert.False(t, h.Store.HasEvent(app.EventFundsTransferred))
}

func TestTransferWorksInEitherDirectionBetweenTheSamePair(t *testing.T) {
	h := newHarness(t)
	h.fund("alpha", 500_000)
	h.fund("omega", 500_000)

	// Wallets are locked in a canonical order regardless of who is paying whom,
	// which is what stops two opposite trades from deadlocking each other.
	_, err := h.Wallets.Transfer(asStoreService(), app.TransferCommand{
		FromUserID: "alpha", ToUserID: "omega", Amount: irr(100_000),
		Reason: wallet.ReasonTrade, ReferenceID: "t-1", IdempotencyKey: "k-1",
	})
	require.NoError(t, err)

	_, err = h.Wallets.Transfer(asStoreService(), app.TransferCommand{
		FromUserID: "omega", ToUserID: "alpha", Amount: irr(40_000),
		Reason: wallet.ReasonTrade, ReferenceID: "t-2", IdempotencyKey: "k-2",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 440_000, h.balanceOf("alpha"))
	assert.EqualValues(t, 560_000, h.balanceOf("omega"))
}

func TestRetriedTransferSettlesOnce(t *testing.T) {
	h := newHarness(t)
	h.fund("buyer", 500_000)
	h.fund("seller", 0)

	cmd := app.TransferCommand{
		FromUserID: "buyer", ToUserID: "seller", Amount: irr(100_000),
		Reason: wallet.ReasonTrade, ReferenceID: "trade-1", IdempotencyKey: "settle-1",
	}
	_, err := h.Wallets.Transfer(asStoreService(), cmd)
	require.NoError(t, err)
	second, err := h.Wallets.Transfer(asStoreService(), cmd)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay)
	assert.EqualValues(t, 400_000, h.balanceOf("buyer"))
	assert.EqualValues(t, 100_000, h.balanceOf("seller"))
}

func TestUsersCannotInitiateTransfers(t *testing.T) {
	h := newHarness(t)
	h.fund("attacker", 0)
	h.fund("victim", 500_000)

	_, err := h.Wallets.Transfer(asUser("attacker"), app.TransferCommand{
		FromUserID: "victim", ToUserID: "attacker", Amount: irr(500_000),
		Reason: wallet.ReasonTrade, ReferenceID: "t-1", IdempotencyKey: "k-1",
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
	assert.EqualValues(t, 500_000, h.balanceOf("victim"))
}

func TestTransferToSelfIsRejected(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.Transfer(asStoreService(), app.TransferCommand{
		FromUserID: "u-1", ToUserID: "u-1", Amount: irr(1_000),
		Reason: wallet.ReasonTrade, ReferenceID: "t-1", IdempotencyKey: "k-1",
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

// --- Holds ----------------------------------------------------------------

func TestHoldReservesWithoutMovingMoney(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	result, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID:         "u-1",
		Amount:         irr(300_000),
		ReferenceID:    "preorder-1",
		Reason:         "pre-order: Silksong",
		TTL:            48 * time.Hour,
		IdempotencyKey: "hold-preorder-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 500_000, h.balanceOf("u-1"), "a hold does not move money")
	assert.EqualValues(t, 200_000, h.availableOf("u-1"))
	assert.Equal(t, hold.StatusActive, result.Hold.Status)
	assert.True(t, h.Store.HasEvent(app.EventHoldPlaced))

	// No ledger entry: the balance never changed, only how much was spendable.
	entries := h.Store.LedgerFor(result.Wallet.ID)
	assert.Len(t, entries, 1, "only the fixture credit")
}

func TestHeldFundsCannotBeSpentElsewhere(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID: "u-1", Amount: irr(400_000), ReferenceID: "preorder-1",
		IdempotencyKey: "hold-1",
	})
	require.NoError(t, err)

	_, err = h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID: "u-1", Amount: irr(200_000), Reason: wallet.ReasonPurchase,
		ReferenceID: "order-1", IdempotencyKey: "debit-1",
	})
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeInsufficientFunds, errs.ReasonOf(err))

	_, err = h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID: "u-1", Amount: irr(100_000), Reason: wallet.ReasonPurchase,
		ReferenceID: "order-2", IdempotencyKey: "debit-2",
	})
	assert.NoError(t, err, "spending within the available balance still works")
}

func TestCaptureHoldTurnsAReservationIntoADebit(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	held, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID: "u-1", Amount: irr(300_000), ReferenceID: "preorder-1",
		IdempotencyKey: "hold-1",
	})
	require.NoError(t, err)

	result, err := h.Wallets.CaptureHold(asStoreService(), app.CaptureHoldCommand{
		HoldID:         held.Hold.ID,
		Reason:         wallet.ReasonPurchase,
		IdempotencyKey: "capture-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 200_000, h.balanceOf("u-1"))
	assert.EqualValues(t, 200_000, h.availableOf("u-1"))
	assert.Equal(t, wallet.ReasonPurchase, result.Entry.Reason,
		"an explicit reason overrides HOLD_CAPTURE so the history reads naturally")
	assert.True(t, h.Store.HasEvent(app.EventHoldCaptured))
}

func TestPartialCapturesDrawDownOneHold(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	held, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID: "u-1", Amount: irr(300_000), ReferenceID: "instalments-1",
		IdempotencyKey: "hold-1",
	})
	require.NoError(t, err)

	for i := 1; i <= 3; i++ {
		_, err := h.Wallets.CaptureHold(asStoreService(), app.CaptureHoldCommand{
			HoldID:         held.Hold.ID,
			Amount:         irr(100_000),
			Reason:         wallet.ReasonPurchase,
			IdempotencyKey: "instalment-" + string(rune('0'+i)),
		})
		require.NoError(t, err, "instalment %d", i)
	}

	assert.EqualValues(t, 200_000, h.balanceOf("u-1"))
	assert.EqualValues(t, 200_000, h.availableOf("u-1"), "the hold is fully drawn down")
}

func TestReleaseHoldReturnsSpendingPower(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	held, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID: "u-1", Amount: irr(300_000), ReferenceID: "preorder-1",
		IdempotencyKey: "hold-1",
	})
	require.NoError(t, err)

	result, err := h.Wallets.ReleaseHold(asUser("u-1"), app.ReleaseHoldCommand{
		HoldID: held.Hold.ID, IdempotencyKey: "release-1",
	})
	require.NoError(t, err)

	assert.Equal(t, hold.StatusReleased, result.Hold.Status)
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"))
	assert.EqualValues(t, 500_000, h.availableOf("u-1"))
	assert.True(t, h.Store.HasEvent(app.EventHoldReleased))
}

func TestUsersCannotTouchAnotherUsersHold(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	held, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID: "u-1", Amount: irr(300_000), ReferenceID: "preorder-1",
		IdempotencyKey: "hold-1",
	})
	require.NoError(t, err)

	_, err = h.Wallets.ReleaseHold(asUser("u-2"), app.ReleaseHoldCommand{
		HoldID: held.Hold.ID, IdempotencyKey: "release-1",
	})
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = h.Wallets.CaptureHold(asUser("u-2"), app.CaptureHoldCommand{
		HoldID: held.Hold.ID, IdempotencyKey: "capture-1",
	})
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
}

func TestExpireHoldsReleasesLapsedReservations(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID: "u-1", Amount: irr(300_000), ReferenceID: "preorder-1",
		TTL: time.Hour, IdempotencyKey: "hold-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 200_000, h.availableOf("u-1"))

	// Nothing has expired yet.
	released, err := h.Wallets.ExpireHolds(asStoreService(), 100)
	require.NoError(t, err)
	assert.Zero(t, released)

	h.Clock.Advance(2 * time.Hour)
	released, err = h.Wallets.ExpireHolds(asStoreService(), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, released)
	assert.EqualValues(t, 500_000, h.availableOf("u-1"),
		"an abandoned pre-order must not reserve money forever")
}

func TestExpiredHoldCannotBeCaptured(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	held, err := h.Wallets.HoldFunds(asUser("u-1"), app.HoldFundsCommand{
		UserID: "u-1", Amount: irr(300_000), ReferenceID: "preorder-1",
		TTL: time.Hour, IdempotencyKey: "hold-1",
	})
	require.NoError(t, err)

	h.Clock.Advance(2 * time.Hour)
	_, err = h.Wallets.CaptureHold(asStoreService(), app.CaptureHoldCommand{
		HoldID: held.Hold.ID, IdempotencyKey: "capture-1",
	})
	require.Error(t, err)
	assert.Equal(t, hold.ReasonCodeExpired, errs.ReasonOf(err))
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"))
}

// --- Ledger reads ---------------------------------------------------------

func TestListLedgerReturnsNewestFirst(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	for i := 1; i <= 3; i++ {
		_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
			UserID: "u-1", Amount: irr(int64(i) * 10_000), Reason: wallet.ReasonPurchase,
			ReferenceID:    "order-" + string(rune('0'+i)),
			IdempotencyKey: "debit-" + string(rune('0'+i)),
		})
		require.NoError(t, err)
	}

	page, err := h.Wallets.ListLedger(asUser("u-1"), app.ListLedgerQuery{PageSize: 10})
	require.NoError(t, err)

	require.Len(t, page.Entries, 4, "one fixture credit plus three debits")
	assert.EqualValues(t, 4, page.TotalItems)
	assert.EqualValues(t, 30_000, page.Entries[0].Amount.Minor(), "newest entry first")
}

func TestListLedgerFiltersByReason(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID: "u-1", Amount: irr(50_000), Reason: wallet.ReasonPurchase,
		ReferenceID: "order-1", IdempotencyKey: "d-1",
	})
	require.NoError(t, err)

	page, err := h.Wallets.ListLedger(asUser("u-1"), app.ListLedgerQuery{
		Reasons: []wallet.Reason{wallet.ReasonPurchase},
	})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	assert.Equal(t, wallet.ReasonPurchase, page.Entries[0].Reason)
}

func TestListLedgerPageSizeIsClamped(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	page, err := h.Wallets.ListLedger(asUser("u-1"), app.ListLedgerQuery{PageSize: 100_000})
	require.NoError(t, err)
	assert.LessOrEqual(t, page.PageSize, 100, "a client must not be able to ask for the whole ledger")
}

func TestUsersCannotReadAnotherUsersLedger(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.ListLedger(asUser("u-2"), app.ListLedgerQuery{UserID: "u-1"})
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = h.Wallets.ListLedger(asSupport("s-1"), app.ListLedgerQuery{UserID: "u-1"})
	assert.NoError(t, err, "support investigating a dispute may read any ledger")
}

// --- Ledger integrity -----------------------------------------------------

func TestLedgerAlwaysSumsToTheBalance(t *testing.T) {
	h := newHarness(t)
	view := h.fund("u-1", 1_000_000)

	// A realistic mixture of movements, including rejected ones.
	operations := []struct {
		debit  bool
		amount int64
		reason wallet.Reason
	}{
		{true, 250_000, wallet.ReasonPurchase},
		{false, 70_000, wallet.ReasonRevenue},
		{true, 900_000, wallet.ReasonPurchase}, // will be rejected
		{true, 120_000, wallet.ReasonPurchase},
		{false, 250_000, wallet.ReasonRefund},
		{true, 33_333, wallet.ReasonPurchase},
	}

	for i, op := range operations {
		key := "op-" + string(rune('a'+i))
		if op.debit {
			_, _ = h.Wallets.Debit(asStoreService(), app.DebitCommand{
				UserID: "u-1", Amount: irr(op.amount), Reason: op.reason,
				ReferenceID: key, IdempotencyKey: key,
			})
			continue
		}
		_, _ = h.Wallets.Credit(asStoreService(), app.CreditCommand{
			UserID: "u-1", Amount: irr(op.amount), Reason: op.reason,
			ReferenceID: key, IdempotencyKey: key,
		})
	}

	// The invariant the whole design protects: replaying the ledger must reproduce
	// the stored balance exactly.
	var sum int64
	for _, entry := range h.Store.LedgerFor(view.ID) {
		sum += entry.SignedAmount().Minor()
	}
	assert.Equal(t, h.balanceOf("u-1"), sum,
		"the ledger is the source of truth; the balance column is only a projection of it")
}
