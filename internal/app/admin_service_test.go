package app_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Reconciliation -------------------------------------------------------

func TestReconcileIsCleanForANormalWallet(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID: "u-1", Amount: irr(120_000), Reason: wallet.ReasonPurchase,
		ReferenceID: "order-1", IdempotencyKey: "d-1",
	})
	require.NoError(t, err)

	result, err := h.Admin.Reconcile(asAdmin("a-1"), "")
	require.NoError(t, err)
	assert.Empty(t, result.Mismatches)
	assert.EqualValues(t, 0, h.Metrics.LedgerMismatches)
}

func TestReconcileDetectsATamperedBalance(t *testing.T) {
	h := newHarness(t)
	view := h.fund("u-1", 500_000)

	// Simulate the failure the whole design guards against: a balance that no longer
	// matches its ledger, whether from a bug or a manual database edit.
	record := h.Store.Wallets[view.ID]
	record.Balance = irr(999_999)
	h.Store.Wallets[view.ID] = record

	result, err := h.Admin.Reconcile(asAdmin("a-1"), "")
	require.NoError(t, err)
	require.Len(t, result.Mismatches, 1)

	mismatch := result.Mismatches[0]
	assert.Equal(t, view.ID, mismatch.WalletID)
	assert.EqualValues(t, 999_999, mismatch.StoredBalance.Minor())
	assert.EqualValues(t, 500_000, mismatch.LedgerBalance.Minor())
	assert.EqualValues(t, 499_999, mismatch.Delta.Minor())

	// The gauge is what the alerting rules page on, and the event is what makes the
	// incident investigable afterwards.
	assert.EqualValues(t, 1, h.Metrics.LedgerMismatches)
	assert.True(t, h.Store.HasEvent(app.EventLedgerMismatchDetected))
}

func TestReconcileRequiresAdmin(t *testing.T) {
	h := newHarness(t)

	_, err := h.Admin.Reconcile(asUser("u-1"), "")
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = h.Admin.Reconcile(asSupport("s-1"), "")
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = h.Admin.Reconcile(asStoreService(), "")
	assert.NoError(t, err, "the scheduled job runs as a service principal")
}

// --- Interest accrual -----------------------------------------------------

func TestAccrueInterestCreditsEligibleWallets(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000) // 5% a year on this is exactly 5,000 a day

	result, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)

	assert.EqualValues(t, 1, result.WalletsCredited)
	assert.EqualValues(t, 5_000, result.TotalInterest.Minor())
	assert.Equal(t, "2026-07-26", result.AccrualDate)
	assert.EqualValues(t, 36_505_000, h.balanceOf("saver"))
	assert.True(t, h.Store.HasEvent(app.EventInterestAccrued))
}

func TestAccrualIsRecordedAsInterestInTheLedger(t *testing.T) {
	h := newHarness(t)
	view := h.fund("saver", 36_500_000)

	_, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)

	entries := h.Store.LedgerFor(view.ID)
	require.Len(t, entries, 2)
	assert.Equal(t, wallet.ReasonInterest, entries[1].Reason)
	assert.Equal(t, wallet.DirectionCredit, entries[1].Direction)
	assert.Equal(t, "2026-07-26", entries[1].ReferenceID)
}

func TestRerunningADaysAccrualPaysNothingExtra(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000)

	first, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, first.WalletsCredited)

	// The nightly job crashed halfway and was re-run. This is the exact scenario the
	// per-wallet-per-date idempotency key exists for.
	second, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)
	assert.EqualValues(t, 0, second.WalletsCredited, "every wallet was already credited for this date")
	assert.EqualValues(t, 36_505_000, h.balanceOf("saver"), "paid exactly once")
}

func TestAccrualOnANewDayPaysAgain(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000)

	_, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)

	h.Clock.Advance(24 * time.Hour)
	result, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.WalletsCredited)
	assert.Equal(t, "2026-07-27", result.AccrualDate)
	assert.Greater(t, h.balanceOf("saver"), int64(36_505_000))
}

func TestAccrualSkipsWalletsBelowTheMinimum(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000)
	h.fund("pauper", 99_999) // the harness sets a 100,000 minimum

	result, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)

	assert.EqualValues(t, 2, result.WalletsProcessed)
	assert.EqualValues(t, 1, result.WalletsCredited)
	assert.EqualValues(t, 99_999, h.balanceOf("pauper"), "a near-empty wallet earns nothing")
}

func TestDryRunReportsWithoutPaying(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000)

	result, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{DryRun: true})
	require.NoError(t, err)

	assert.True(t, result.DryRun)
	assert.EqualValues(t, 1, result.WalletsCredited, "the report says what would have been paid")
	assert.EqualValues(t, 5_000, result.TotalInterest.Minor())
	assert.EqualValues(t, 36_500_000, h.balanceOf("saver"), "but nothing actually moved")
	assert.False(t, h.Store.HasEvent(app.EventInterestAccrued))
}

func TestAccrualRateOverrideReplaysAHistoricRun(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000)

	// 10% a year instead of the configured 5%.
	result, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{
		AnnualRateBps: 1_000,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 10_000, result.TotalInterest.Minor())
}

func TestAccrualRejectsAnAbsurdRate(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000)

	_, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{
		AnnualRateBps: 20_000,
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err),
		"a rate above 100% a year is almost certainly a units mistake")
}

func TestAccrualRejectsAMalformedDate(t *testing.T) {
	h := newHarness(t)

	_, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{
		AccrualDate: "yesterday",
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestAccrualSkipsFrozenWallets(t *testing.T) {
	h := newHarness(t)
	h.fund("saver", 36_500_000)

	_, err := h.Admin.FreezeWallet(asSupport("s-1"), "saver", "investigation")
	require.NoError(t, err)

	result, err := h.Admin.AccrueInterest(asAdmin("a-1"), app.AccrueInterestCommand{})
	require.NoError(t, err)
	assert.EqualValues(t, 0, result.WalletsProcessed,
		"only active wallets are scanned, so a frozen one cannot be credited")
	assert.EqualValues(t, 36_500_000, h.balanceOf("saver"))
}

func TestAccrualRequiresAdmin(t *testing.T) {
	h := newHarness(t)

	_, err := h.Admin.AccrueInterest(asUser("u-1"), app.AccrueInterestCommand{})
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
}

// --- Freeze and unfreeze --------------------------------------------------

func TestFreezeAndUnfreeze(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	frozen, err := h.Admin.FreezeWallet(asSupport("s-1"), "u-1", "gift card abuse review")
	require.NoError(t, err)
	assert.Equal(t, wallet.StatusFrozen, frozen.Status)
	assert.True(t, h.Store.HasEvent(app.EventWalletFrozen))

	thawed, err := h.Admin.UnfreezeWallet(asSupport("s-1"), "u-1", "cleared by support")
	require.NoError(t, err)
	assert.Equal(t, wallet.StatusActive, thawed.Status)
	assert.True(t, h.Store.HasEvent(app.EventWalletUnfrozen))

	_, err = h.Wallets.Debit(asStoreService(), app.DebitCommand{
		UserID: "u-1", Amount: irr(1_000), Reason: wallet.ReasonPurchase,
		ReferenceID: "order-1", IdempotencyKey: "d-1",
	})
	assert.NoError(t, err, "money moves again once the wallet is thawed")
}

func TestFreezeRequiresAReason(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Admin.FreezeWallet(asSupport("s-1"), "u-1", "")
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err),
		"locking a user out of their own money without a reason is not defensible")
}

func TestFreezeRequiresStaff(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Admin.FreezeWallet(asUser("u-2"), "u-1", "because I can")
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
}

func TestDoubleFreezeIsRejected(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 500_000)

	_, err := h.Admin.FreezeWallet(asSupport("s-1"), "u-1", "review")
	require.NoError(t, err)

	_, err = h.Admin.FreezeWallet(asSupport("s-1"), "u-1", "review again")
	assert.Equal(t, wallet.ReasonCodeAlreadyFrozen, errs.ReasonOf(err))
}

// --- Manual adjustment ----------------------------------------------------

func TestAdjustCreditsWithAnAuditTrail(t *testing.T) {
	h := newHarness(t)
	view := h.fund("u-1", 100_000)

	result, err := h.Admin.Adjust(asAdmin("admin-7"), app.AdjustCommand{
		UserID:         "u-1",
		Direction:      wallet.DirectionCredit,
		Amount:         irr(50_000),
		Reason:         "compensating a failed refund, ticket ARC-1234",
		IdempotencyKey: "adjust-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 150_000, h.balanceOf("u-1"))
	assert.Equal(t, wallet.ReasonAdjustment, result.Entry.Reason)
	assert.Contains(t, result.Entry.Description, "ARC-1234",
		"the justification is part of the permanent record")

	// The operator is named in the reference, so an auditor can see who did it.
	entries := h.Store.LedgerFor(view.ID)
	assert.Equal(t, "adjustment:admin-7", entries[len(entries)-1].ReferenceID)
}

func TestAdjustCanDebit(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 100_000)

	_, err := h.Admin.Adjust(asAdmin("admin-7"), app.AdjustCommand{
		UserID:         "u-1",
		Direction:      wallet.DirectionDebit,
		Amount:         irr(30_000),
		Reason:         "reversing a duplicated credit found in reconciliation",
		IdempotencyKey: "adjust-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 70_000, h.balanceOf("u-1"))
}

func TestAdjustCannotOverdraw(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 10_000)

	_, err := h.Admin.Adjust(asAdmin("admin-7"), app.AdjustCommand{
		UserID:         "u-1",
		Direction:      wallet.DirectionDebit,
		Amount:         irr(50_000),
		Reason:         "clawback",
		IdempotencyKey: "adjust-1",
	})
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeInsufficientFunds, errs.ReasonOf(err),
		"even an admin cannot drive a balance negative")
	assert.EqualValues(t, 10_000, h.balanceOf("u-1"))
}

func TestAdjustRequiresAJustification(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 100_000)

	_, err := h.Admin.Adjust(asAdmin("a-1"), app.AdjustCommand{
		UserID: "u-1", Direction: wallet.DirectionCredit, Amount: irr(1_000),
		IdempotencyKey: "adjust-1",
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestAdjustIsAdminOnly(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 100_000)

	cmd := app.AdjustCommand{
		UserID: "u-1", Direction: wallet.DirectionCredit, Amount: irr(1_000_000),
		Reason: "a gift to myself", IdempotencyKey: "adjust-1",
	}

	_, err := h.Admin.Adjust(asUser("u-1"), cmd)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	// Support can freeze and issue gift cards, but minting balance out of nothing is
	// reserved for Admin.
	_, err = h.Admin.Adjust(asSupport("s-1"), cmd)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
	assert.EqualValues(t, 100_000, h.balanceOf("u-1"))
}

func TestAdjustIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 100_000)

	cmd := app.AdjustCommand{
		UserID: "u-1", Direction: wallet.DirectionCredit, Amount: irr(50_000),
		Reason: "ticket ARC-1", IdempotencyKey: "adjust-1",
	}
	_, err := h.Admin.Adjust(asAdmin("a-1"), cmd)
	require.NoError(t, err)
	second, err := h.Admin.Adjust(asAdmin("a-1"), cmd)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay)
	assert.EqualValues(t, 150_000, h.balanceOf("u-1"))
}

func TestAdjustRejectsAnInvalidDirection(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 100_000)

	_, err := h.Admin.Adjust(asAdmin("a-1"), app.AdjustCommand{
		UserID: "u-1", Direction: wallet.Direction("SIDEWAYS"), Amount: irr(1_000),
		Reason: "?", IdempotencyKey: "adjust-1",
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}
