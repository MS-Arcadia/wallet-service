package interest_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/domain/interest"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func irr(minor int64) money.Money { return money.MustNew(minor, "IRR") }

func policy(t *testing.T, rateBps int64, minBalance int64) interest.Policy {
	t.Helper()
	p, err := interest.NewPolicy(interest.Config{
		AnnualRateBps:  rateBps,
		MinimumBalance: irr(minBalance),
		Enabled:        true,
	})
	require.NoError(t, err)
	return p
}

func TestNewPolicyValidation(t *testing.T) {
	_, err := interest.NewPolicy(interest.Config{AnnualRateBps: -1})
	assert.Error(t, err)

	_, err = interest.NewPolicy(interest.Config{AnnualRateBps: 10_001})
	assert.Error(t, err, "a rate above 100% a year is almost certainly a units mistake")

	_, err = interest.NewPolicy(interest.Config{AnnualRateBps: 500, MinimumBalance: irr(-1)})
	assert.Error(t, err)

	p, err := interest.NewPolicy(interest.Config{AnnualRateBps: 10_000, Enabled: true})
	require.NoError(t, err, "exactly 100% is the boundary and is accepted")
	assert.EqualValues(t, 10_000, p.AnnualRateBps())
}

func TestCalculateDailyInterest(t *testing.T) {
	// 5% a year on 36,500,000 is 1,825,000 a year, so 5,000 a day.
	p := policy(t, 500, 0)

	accrual, err := p.Calculate(irr(36_500_000))
	require.NoError(t, err)
	assert.True(t, accrual.Eligible)
	assert.EqualValues(t, 5_000, accrual.Amount.Minor())
	assert.Empty(t, accrual.Skipped)
}

func TestCalculateRoundsDown(t *testing.T) {
	// 5% a year on 10,000 is 500 a year, which is 1.369... a day.
	p := policy(t, 500, 0)

	accrual, err := p.Calculate(irr(10_000))
	require.NoError(t, err)
	assert.EqualValues(t, 1, accrual.Amount.Minor(),
		"rounding down means the platform can never over-pay")
}

func TestAYearOfDailyAccrualNeverExceedsTheAnnualRate(t *testing.T) {
	// The important property: 365 daily credits must not add up to more than the
	// advertised annual return on the starting balance.
	p := policy(t, 500, 0)
	const startingBalance int64 = 36_500_000

	accrual, err := p.Calculate(irr(startingBalance))
	require.NoError(t, err)

	yearTotal := accrual.Amount.Minor() * interest.DaysPerYear
	annualEntitlement := startingBalance * 500 / 10_000

	assert.LessOrEqual(t, yearTotal, annualEntitlement,
		"365 days of accrual (%d) must not exceed one year at the headline rate (%d)",
		yearTotal, annualEntitlement)
}

func TestSubUnitInterestIsEligibleButZero(t *testing.T) {
	// 5% a year on 100 is 5 a year — under one minor unit per day.
	p := policy(t, 500, 0)

	accrual, err := p.Calculate(irr(100))
	require.NoError(t, err)
	assert.True(t, accrual.Amount.IsZero())
	assert.True(t, accrual.Eligible, "the wallet qualified; the daily amount simply rounded to nothing")
	assert.Contains(t, accrual.Skipped, "less than one minor unit")
}

func TestMinimumBalanceGate(t *testing.T) {
	p := policy(t, 500, 1_000_000)

	accrual, err := p.Calculate(irr(999_999))
	require.NoError(t, err)
	assert.True(t, accrual.Amount.IsZero())
	assert.False(t, accrual.Eligible)
	assert.Contains(t, accrual.Skipped, "minimum")

	accrual, err = p.Calculate(irr(1_000_000))
	require.NoError(t, err)
	assert.True(t, accrual.Eligible, "the minimum is inclusive")
}

func TestDisabledPolicyAccruesNothing(t *testing.T) {
	p, err := interest.NewPolicy(interest.Config{AnnualRateBps: 500, Enabled: false})
	require.NoError(t, err)

	accrual, err := p.Calculate(irr(100_000_000))
	require.NoError(t, err)
	assert.True(t, accrual.Amount.IsZero())
	assert.False(t, accrual.Eligible)
	assert.Contains(t, accrual.Skipped, "disabled")
}

func TestZeroRateAccruesNothing(t *testing.T) {
	p, err := interest.NewPolicy(interest.Config{AnnualRateBps: 0, Enabled: true})
	require.NoError(t, err)

	accrual, err := p.Calculate(irr(100_000_000))
	require.NoError(t, err)
	assert.True(t, accrual.Amount.IsZero())
	assert.Contains(t, accrual.Skipped, "zero")
}

func TestNonPositiveBalanceAccruesNothing(t *testing.T) {
	p := policy(t, 500, 0)

	for _, balance := range []int64{0, -1_000_000} {
		accrual, err := p.Calculate(irr(balance))
		require.NoError(t, err)
		assert.True(t, accrual.Amount.IsZero(), "balance %d must earn nothing", balance)
		assert.False(t, accrual.Eligible)
	}
}

func TestInterestKeepsTheWalletCurrency(t *testing.T) {
	p := policy(t, 500, 0)

	accrual, err := p.Calculate(money.MustNew(36_500_000, "USD"))
	require.NoError(t, err)
	assert.Equal(t, "USD", accrual.Amount.Currency())
}

func TestHugeBalanceIsRefusedRatherThanOverflowing(t *testing.T) {
	p := policy(t, 10_000, 0)

	_, err := p.Calculate(irr(9_223_372_036_854_775_000))
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
}

func TestWithRateReplaysAtAHistoricRate(t *testing.T) {
	p := policy(t, 500, 1_000_000)

	replay, err := p.WithRate(300)
	require.NoError(t, err)
	assert.EqualValues(t, 300, replay.AnnualRateBps())
	assert.EqualValues(t, 1_000_000, replay.MinimumBalance().Minor(), "the other settings are preserved")
	assert.True(t, replay.Enabled())

	_, err = p.WithRate(-1)
	assert.Error(t, err)
}

func TestIdempotencyKeyIsStablePerWalletPerDay(t *testing.T) {
	morning := time.Date(2026, 7, 26, 2, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 7, 26, 23, 59, 0, 0, time.UTC)
	nextDay := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)

	// Re-running the same day must produce the same key, or a retried nightly job
	// would pay every user twice.
	assert.Equal(t, interest.IdempotencyKey("w-1", morning), interest.IdempotencyKey("w-1", evening))
	assert.NotEqual(t, interest.IdempotencyKey("w-1", morning), interest.IdempotencyKey("w-1", nextDay))
	assert.NotEqual(t, interest.IdempotencyKey("w-1", morning), interest.IdempotencyKey("w-2", morning))
	assert.Equal(t, "interest:w-1:2026-07-26", interest.IdempotencyKey("w-1", morning))
}

func TestIdempotencyKeyNormalisesTimezone(t *testing.T) {
	tehran := time.FixedZone("Asia/Tehran", int(3*time.Hour/time.Second)+1800)
	// 01:00 Tehran on the 27th is 21:30 UTC on the 26th; the key must follow UTC so
	// that the job's notion of "today" does not depend on the host's timezone.
	local := time.Date(2026, 7, 27, 1, 0, 0, 0, tehran)
	assert.Equal(t, "interest:w-1:2026-07-26", interest.IdempotencyKey("w-1", local))
}

func TestAccrualDate(t *testing.T) {
	at := time.Date(2026, 7, 26, 17, 45, 12, 999, time.UTC)
	date := interest.AccrualDate(at)

	assert.Equal(t, 2026, date.Year())
	assert.Equal(t, time.July, date.Month())
	assert.Equal(t, 26, date.Day())
	assert.Zero(t, date.Hour())
	assert.Zero(t, date.Minute())
	assert.Equal(t, time.UTC, date.Location())
}

func TestParseAccrualDate(t *testing.T) {
	date, err := interest.ParseAccrualDate("2026-07-26")
	require.NoError(t, err)
	assert.True(t, interest.AccrualDate(date).Equal(date))

	for _, bad := range []string{"26-07-2026", "2026/07/26", "yesterday", ""} {
		_, err := interest.ParseAccrualDate(bad)
		assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err), "input %q", bad)
	}
}
