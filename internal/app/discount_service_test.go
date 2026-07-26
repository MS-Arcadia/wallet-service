package app_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func issueDiscount(t *testing.T, h *harness, cmd app.IssueDiscountCodeCommand) app.DiscountCodeView {
	t.Helper()
	if cmd.IdempotencyKey == "" {
		cmd.IdempotencyKey = "issue-" + cmd.Code
	}
	view, err := h.Discounts.IssueDiscountCode(asAdmin("a-1"), cmd)
	require.NoError(t, err)
	return view
}

func TestIssueDiscountCodeRequiresStaff(t *testing.T) {
	h := newHarness(t)
	cmd := app.IssueDiscountCodeCommand{Code: "SUMMER20", PercentBps: 2_000, IdempotencyKey: "k-1"}

	_, err := h.Discounts.IssueDiscountCode(asUser("u-1"), cmd)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = h.Discounts.IssueDiscountCode(asSupport("s-1"), cmd)
	assert.NoError(t, err)
}

func TestIssueDiscountCodeGeneratesACodeWhenNoneIsGiven(t *testing.T) {
	h := newHarness(t)

	view := issueDiscount(t, h, app.IssueDiscountCodeCommand{
		PercentBps: 1_000, IdempotencyKey: "k-1",
	})
	assert.NotEmpty(t, view.Code)
	assert.Equal(t, discount.StatusActive, view.Status)
}

func TestPreviewComputesADiscountWithoutConsumingIt(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{Code: "SUMMER20", PercentBps: 2_000})

	for i := 0; i < 5; i++ {
		quote, err := h.Discounts.PreviewDiscount(asUser("u-1"), "SUMMER20", irr(500_000))
		require.NoError(t, err)
		assert.EqualValues(t, 100_000, quote.Discount.Minor())
		assert.EqualValues(t, 400_000, quote.Payable.Minor())
	}

	stored, err := h.Discounts.GetDiscountCode(asUser("u-1"), "SUMMER20")
	require.NoError(t, err)
	assert.EqualValues(t, 0, stored.RedemptionCount,
		"the Store service previews on every checkout render; that must be free")
}

func TestPreviewAcceptsAnyFormattingOfTheCode(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{Code: "SUMMER20", PercentBps: 2_000})

	quote, err := h.Discounts.PreviewDiscount(asUser("u-1"), "summer-20", irr(500_000))
	require.NoError(t, err)
	assert.EqualValues(t, 100_000, quote.Discount.Minor())
}

func TestRedeemConsumesAnAllowance(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{
		Code: "SUMMER20", PercentBps: 2_000, MaxRedemptions: 2,
	})

	quote, err := h.Discounts.RedeemDiscountCode(asUser("u-1"), app.RedeemDiscountCodeCommand{
		Code: "SUMMER20", OrderAmount: irr(500_000),
		ReferenceID: "order-1", IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 100_000, quote.Discount.Minor())
	assert.EqualValues(t, 400_000, quote.Payable.Minor())
	assert.True(t, h.Store.HasEvent(app.EventDiscountCodeRedeemed))

	stored, err := h.Discounts.GetDiscountCode(asUser("u-1"), "SUMMER20")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stored.RedemptionCount)
}

func TestSingleUseCodeCannotBeRedeemedTwice(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{Code: "ONCE", PercentBps: 5_000})

	_, err := h.Discounts.RedeemDiscountCode(asUser("u-1"), app.RedeemDiscountCodeCommand{
		Code: "ONCE", OrderAmount: irr(500_000), ReferenceID: "order-1", IdempotencyKey: "r-1",
	})
	require.NoError(t, err)

	// A second, genuinely different order.
	_, err = h.Discounts.RedeemDiscountCode(asUser("u-2"), app.RedeemDiscountCodeCommand{
		Code: "ONCE", OrderAmount: irr(500_000), ReferenceID: "order-2", IdempotencyKey: "r-2",
	})
	require.Error(t, err)
	assert.Equal(t, discount.ReasonCodeExhausted, errs.ReasonOf(err))
}

func TestRetriedRedemptionConsumesOneAllowance(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{
		Code: "SUMMER20", PercentBps: 2_000, MaxRedemptions: 5,
	})

	cmd := app.RedeemDiscountCodeCommand{
		Code: "SUMMER20", OrderAmount: irr(500_000), ReferenceID: "order-1", IdempotencyKey: "r-1",
	}
	_, err := h.Discounts.RedeemDiscountCode(asUser("u-1"), cmd)
	require.NoError(t, err)
	second, err := h.Discounts.RedeemDiscountCode(asUser("u-1"), cmd)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay)
	stored, err := h.Discounts.GetDiscountCode(asUser("u-1"), "SUMMER20")
	require.NoError(t, err)
	assert.EqualValues(t, 1, stored.RedemptionCount, "a retry must not burn a second allowance")
}

func TestMaxDiscountCapIsEnforced(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{
		Code: "HALF", PercentBps: 5_000, MaxDiscount: irr(100_000),
	})

	quote, err := h.Discounts.PreviewDiscount(asUser("u-1"), "HALF", irr(1_000_000))
	require.NoError(t, err)
	assert.EqualValues(t, 100_000, quote.Discount.Minor(),
		"50% of a million is capped at the configured maximum")
}

func TestMinimumOrderAmountIsEnforced(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{
		Code: "BIGSPEND", PercentBps: 1_000, MinOrderAmount: irr(200_000),
	})

	_, err := h.Discounts.PreviewDiscount(asUser("u-1"), "BIGSPEND", irr(199_999))
	require.Error(t, err)
	assert.Equal(t, discount.ReasonCodeBelowMinimum, errs.ReasonOf(err))

	_, err = h.Discounts.PreviewDiscount(asUser("u-1"), "BIGSPEND", irr(200_000))
	assert.NoError(t, err)
}

func TestFixedAmountCannotExceedTheOrder(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{Code: "FLAT", AmountOff: irr(50_000)})

	quote, err := h.Discounts.PreviewDiscount(asUser("u-1"), "FLAT", irr(30_000))
	require.NoError(t, err)
	assert.EqualValues(t, 30_000, quote.Discount.Minor())
	assert.True(t, quote.Payable.IsZero())
	assert.False(t, quote.Payable.IsNegative(), "a discount must never become a payout")
}

func TestExpiredCodeIsRefused(t *testing.T) {
	h := newHarness(t)
	expiry := testNow.Add(time.Hour)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{
		Code: "FLASH", PercentBps: 3_000, ExpiresAt: &expiry,
	})

	_, err := h.Discounts.PreviewDiscount(asUser("u-1"), "FLASH", irr(500_000))
	require.NoError(t, err)

	h.Clock.Advance(2 * time.Hour)
	_, err = h.Discounts.PreviewDiscount(asUser("u-1"), "FLASH", irr(500_000))
	require.Error(t, err)
	assert.Equal(t, discount.ReasonCodeExpired, errs.ReasonOf(err),
		"expiry is evaluated live, not when a sweeper next runs")
}

func TestUnknownCodeIsNotFound(t *testing.T) {
	h := newHarness(t)

	_, err := h.Discounts.PreviewDiscount(asUser("u-1"), "NOSUCHCODE", irr(500_000))
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestRedeemRequiresAReference(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{Code: "SUMMER20", PercentBps: 2_000})

	_, err := h.Discounts.RedeemDiscountCode(asUser("u-1"), app.RedeemDiscountCodeCommand{
		Code: "SUMMER20", OrderAmount: irr(500_000), IdempotencyKey: "r-1",
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestDiscountReadsRequireAuthentication(t *testing.T) {
	h := newHarness(t)
	issueDiscount(t, h, app.IssueDiscountCodeCommand{Code: "SUMMER20", PercentBps: 2_000})

	_, err := h.Discounts.PreviewDiscount(anonymous(), "SUMMER20", irr(500_000))
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))

	_, err = h.Discounts.GetDiscountCode(anonymous(), "SUMMER20")
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}
