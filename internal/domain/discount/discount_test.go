package discount_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func irr(minor int64) money.Money { return money.MustNew(minor, "IRR") }

func percentCode(t *testing.T, bps int32) *discount.Code {
	t.Helper()
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "SUMMER20", PercentBps: bps, IssuedBy: "admin-1", MaxRedemptions: 1,
	}, now)
	require.NoError(t, err)
	return code
}

func TestIssuePercentageCode(t *testing.T) {
	code := percentCode(t, 2_000)

	assert.Equal(t, "SUMMER20", code.Code())
	assert.Equal(t, discount.StatusActive, code.Status())
	assert.EqualValues(t, 2_000, code.PercentBps())
	assert.EqualValues(t, 1, code.MaxRedemptions())
	assert.EqualValues(t, 0, code.RedemptionCount())
	assert.EqualValues(t, 1, code.RemainingRedemptions())
}

func TestIssueDefaultsToSingleUse(t *testing.T) {
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "ONE", PercentBps: 1_000, IssuedBy: "admin-1",
	}, now)
	require.NoError(t, err)
	assert.EqualValues(t, 1, code.MaxRedemptions(),
		"anything that gives money away must default to the safe limit")
}

func TestIssueValidation(t *testing.T) {
	tests := []struct {
		name string
		spec discount.Spec
	}{
		{"no id", discount.Spec{Code: "X", PercentBps: 100, IssuedBy: "a"}},
		{"no code string", discount.Spec{ID: "dc-1", PercentBps: 100, IssuedBy: "a"}},
		{"no issuer", discount.Spec{ID: "dc-1", Code: "X", PercentBps: 100}},
		{"neither percent nor amount", discount.Spec{ID: "dc-1", Code: "X", IssuedBy: "a"}},
		{"both percent and amount", discount.Spec{
			ID: "dc-1", Code: "X", PercentBps: 100, AmountOff: irr(500), IssuedBy: "a",
		}},
		{"percent over 100", discount.Spec{
			ID: "dc-1", Code: "X", PercentBps: 10_001, IssuedBy: "a",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := discount.Issue(tc.spec, now)
			assert.Error(t, err)
		})
	}
}

func TestIssueRejectsPastExpiry(t *testing.T) {
	past := now.Add(-time.Hour)
	_, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "X", PercentBps: 100, IssuedBy: "a", ExpiresAt: &past,
	}, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestPreviewPercentage(t *testing.T) {
	code := percentCode(t, 2_000) // 20%

	quote, err := code.Preview(irr(500_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 100_000, quote.Discount.Minor())
	assert.EqualValues(t, 400_000, quote.Payable.Minor())
}

func TestPreviewIsSideEffectFree(t *testing.T) {
	code := percentCode(t, 2_000)

	for i := 0; i < 5; i++ {
		_, err := code.Preview(irr(500_000), now)
		require.NoError(t, err)
	}
	assert.EqualValues(t, 0, code.RedemptionCount(),
		"the Store service previews on every checkout render; that must not burn redemptions")
	assert.Equal(t, discount.StatusActive, code.Status())
}

func TestPreviewFixedAmount(t *testing.T) {
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "FLAT50", AmountOff: irr(50_000), IssuedBy: "admin-1",
	}, now)
	require.NoError(t, err)

	quote, err := code.Preview(irr(200_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 50_000, quote.Discount.Minor())
	assert.EqualValues(t, 150_000, quote.Payable.Minor())
}

func TestFixedAmountNeverExceedsTheOrder(t *testing.T) {
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "FLAT50", AmountOff: irr(50_000), IssuedBy: "admin-1",
	}, now)
	require.NoError(t, err)

	// A 50,000 discount on a 30,000 order must not pay the buyer 20,000.
	quote, err := code.Preview(irr(30_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 30_000, quote.Discount.Minor())
	assert.True(t, quote.Payable.IsZero())
	assert.False(t, quote.Payable.IsNegative())
}

func TestMaxDiscountCapsAPercentage(t *testing.T) {
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "HALF", PercentBps: 5_000, MaxDiscount: irr(100_000), IssuedBy: "admin-1",
	}, now)
	require.NoError(t, err)

	// 50% of 1,000,000 would be 500,000, but the cap is 100,000.
	quote, err := code.Preview(irr(1_000_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 100_000, quote.Discount.Minor())
	assert.EqualValues(t, 900_000, quote.Payable.Minor())

	// Below the cap the percentage applies in full.
	quote, err = code.Preview(irr(100_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 50_000, quote.Discount.Minor())
}

func TestMinOrderAmount(t *testing.T) {
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "BIG", PercentBps: 1_000, MinOrderAmount: irr(200_000), IssuedBy: "admin-1",
	}, now)
	require.NoError(t, err)

	_, err = code.Preview(irr(199_999), now)
	require.Error(t, err)
	assert.Equal(t, discount.ReasonCodeBelowMinimum, errs.ReasonOf(err))

	quote, err := code.Preview(irr(200_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 20_000, quote.Discount.Minor())
}

func TestPercentageRoundingIsHalfUp(t *testing.T) {
	code := percentCode(t, 200) // 2%, the gift-message fee rate from the requirements

	quote, err := code.Preview(irr(1_025), now)
	require.NoError(t, err)
	// 2% of 1025 is 20.5, which rounds to 21.
	assert.EqualValues(t, 21, quote.Discount.Minor())
	assert.EqualValues(t, 1_004, quote.Payable.Minor())
}

func TestRedeemConsumesAnAllowance(t *testing.T) {
	code := percentCode(t, 1_000)

	quote, err := code.Redeem(irr(100_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 10_000, quote.Discount.Minor())
	assert.EqualValues(t, 1, code.RedemptionCount())
	assert.Equal(t, discount.StatusUsed, code.Status(), "a single-use code is spent after one redemption")
	assert.EqualValues(t, 0, code.RemainingRedemptions())
}

func TestRedeemBeyondTheLimitIsRejected(t *testing.T) {
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "TRIPLE", PercentBps: 1_000, MaxRedemptions: 3, IssuedBy: "admin-1",
	}, now)
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		_, err := code.Redeem(irr(100_000), now)
		require.NoError(t, err, "redemption %d must succeed", i+1)
	}

	_, err = code.Redeem(irr(100_000), now)
	require.Error(t, err)
	assert.Equal(t, discount.ReasonCodeExhausted, errs.ReasonOf(err))
	assert.EqualValues(t, 3, code.RedemptionCount(), "a rejected redemption must not increment the counter")
}

func TestExpiryIsEvaluatedLive(t *testing.T) {
	expiry := now.Add(time.Hour)
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "SOON", PercentBps: 1_000, IssuedBy: "admin-1", ExpiresAt: &expiry,
	}, now)
	require.NoError(t, err)

	_, err = code.Preview(irr(100_000), now)
	assert.NoError(t, err, "still valid before expiry")

	// No sweeper has run; the status is still ACTIVE. The code must nonetheless
	// stop working the instant it expires.
	assert.Equal(t, discount.StatusActive, code.Status())
	_, err = code.Preview(irr(100_000), expiry)
	require.Error(t, err)
	assert.Equal(t, discount.ReasonCodeExpired, errs.ReasonOf(err))

	_, err = code.Preview(irr(100_000), expiry.Add(time.Hour))
	assert.Equal(t, discount.ReasonCodeExpired, errs.ReasonOf(err))
}

func TestRevoke(t *testing.T) {
	code := percentCode(t, 1_000)
	require.NoError(t, code.Revoke(now))
	assert.Equal(t, discount.StatusRevoked, code.Status())

	_, err := code.Preview(irr(100_000), now)
	assert.Equal(t, discount.ReasonCodeRevoked, errs.ReasonOf(err))

	assert.Error(t, code.Revoke(now), "double revoke is rejected")
}

func TestPreviewRejectsNonPositiveOrder(t *testing.T) {
	code := percentCode(t, 1_000)

	for _, amount := range []int64{0, -1_000} {
		_, err := code.Preview(irr(amount), now)
		assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err), "order amount %d", amount)
	}
}

func TestForeignCurrencyIsRejected(t *testing.T) {
	code, err := discount.Issue(discount.Spec{
		ID: "dc-1", Code: "FLAT", AmountOff: irr(50_000), IssuedBy: "admin-1",
	}, now)
	require.NoError(t, err)

	_, err = code.Preview(money.MustNew(100_000, "USD"), now)
	require.Error(t, err)
	assert.Equal(t, discount.ReasonCodeCurrencyMismatch, errs.ReasonOf(err))
}

func TestRehydrate(t *testing.T) {
	code, err := discount.Rehydrate("dc-1", "SUMMER20", 2_000,
		money.Money{}, irr(100_000), irr(50_000),
		discount.StatusActive, 5, 2, "admin-1", nil, now, 3)
	require.NoError(t, err)

	assert.EqualValues(t, 3, code.RemainingRedemptions())
	assert.EqualValues(t, 3, code.Version())
}

func TestRehydrateRejectsCorruptState(t *testing.T) {
	_, err := discount.Rehydrate("", "X", 100, money.Money{}, money.Money{}, money.Money{},
		discount.StatusActive, 1, 0, "a", nil, now, 1)
	assert.Error(t, err)

	_, err = discount.Rehydrate("dc-1", "X", 100, money.Money{}, money.Money{}, money.Money{},
		discount.Status("TORN_UP"), 1, 0, "a", nil, now, 1)
	assert.Error(t, err)
}

func TestErrNotFound(t *testing.T) {
	err := discount.ErrNotFound()
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	assert.Equal(t, discount.ReasonCodeNotFound, errs.ReasonOf(err))
}
