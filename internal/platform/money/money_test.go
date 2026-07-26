package money_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsBadCurrency(t *testing.T) {
	for _, cur := range []string{"", "IR", "IRRR", "ir1", "12$"} {
		_, err := money.New(100, cur)
		assert.ErrorIs(t, err, money.ErrInvalidCurrency, "currency %q must be rejected", cur)
	}
}

func TestNewNormalisesCurrency(t *testing.T) {
	m, err := money.New(100, " irr ")
	require.NoError(t, err)
	assert.Equal(t, "IRR", m.Currency())
	assert.EqualValues(t, 100, m.Minor())
}

func TestAddAndSub(t *testing.T) {
	a := money.MustNew(1500, "IRR")
	b := money.MustNew(250, "IRR")

	sum, err := a.Add(b)
	require.NoError(t, err)
	assert.EqualValues(t, 1750, sum.Minor())

	diff, err := a.Sub(b)
	require.NoError(t, err)
	assert.EqualValues(t, 1250, diff.Minor())
}

func TestSubCanGoNegative(t *testing.T) {
	// Money itself allows negatives; the non-negative balance invariant lives in
	// the Wallet aggregate, not here.
	res, err := money.MustNew(100, "IRR").Sub(money.MustNew(300, "IRR"))
	require.NoError(t, err)
	assert.EqualValues(t, -200, res.Minor())
	assert.True(t, res.IsNegative())
}

func TestCurrencyMismatch(t *testing.T) {
	_, err := money.MustNew(100, "IRR").Add(money.MustNew(100, "USD"))
	assert.ErrorIs(t, err, money.ErrCurrencyMismatch)

	_, err = money.MustNew(100, "IRR").Cmp(money.MustNew(100, "USD"))
	assert.ErrorIs(t, err, money.ErrCurrencyMismatch)
}

func TestZeroValueIsCurrencyAgnostic(t *testing.T) {
	var acc money.Money
	sum, err := acc.Add(money.MustNew(500, "IRR"))
	require.NoError(t, err)
	assert.Equal(t, "IRR", sum.Currency())
	assert.EqualValues(t, 500, sum.Minor())
}

func TestAddOverflow(t *testing.T) {
	_, err := money.MustNew(math.MaxInt64, "IRR").Add(money.MustNew(1, "IRR"))
	assert.ErrorIs(t, err, money.ErrOverflow)
}

func TestSubOverflow(t *testing.T) {
	_, err := money.MustNew(math.MinInt64, "IRR").Sub(money.MustNew(1, "IRR"))
	assert.ErrorIs(t, err, money.ErrOverflow)
}

func TestPercentRoundsHalfUp(t *testing.T) {
	tests := []struct {
		name  string
		minor int64
		bps   int64
		want  int64
	}{
		{"2 percent of 1000", 1000, 200, 20},
		{"30 percent of 999", 999, 3000, 300}, // 299.7 -> 300
		{"70 percent of 999", 999, 7000, 699}, // 699.3 -> 699
		{"exact half rounds up", 5, 5000, 3},  // 2.5 -> 3
		{"negative half rounds away", -5, 5000, -3},
		{"zero rate", 1000, 0, 0},
		{"zero amount", 0, 5000, 0},
		{"full rate is identity", 12345, 10000, 12345},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := money.MustNew(tc.minor, "IRR").Percent(tc.bps)
			require.NoError(t, err)
			assert.EqualValues(t, tc.want, got.Minor())
			assert.Equal(t, "IRR", got.Currency())
		})
	}
}

func TestPercentOverflow(t *testing.T) {
	_, err := money.MustNew(math.MaxInt64/100, "IRR").Percent(10_000)
	assert.ErrorIs(t, err, money.ErrOverflow)
}

func TestAllocateNeverLosesAUnit(t *testing.T) {
	// The 70/30 revenue split from the requirements, over amounts that do not
	// divide evenly. The parts must always sum back to the original.
	for _, amount := range []int64{1, 2, 3, 7, 99, 101, 999, 1001, 123457} {
		parts, err := money.MustNew(amount, "IRR").Allocate(70, 30)
		require.NoError(t, err)
		require.Len(t, parts, 2)

		total, err := money.Sum(parts...)
		require.NoError(t, err)
		assert.EqualValues(t, amount, total.Minor(),
			"70/30 split of %d lost or invented units: %v", amount, parts)
	}
}

func TestAllocateFavoursLargestRemainder(t *testing.T) {
	// 10 split 70/30 is exactly 7/3.
	parts, err := money.MustNew(10, "IRR").Allocate(70, 30)
	require.NoError(t, err)
	assert.EqualValues(t, 7, parts[0].Minor())
	assert.EqualValues(t, 3, parts[1].Minor())

	// 1 split 70/30: the developer's 0.7 remainder beats the platform's 0.3.
	parts, err = money.MustNew(1, "IRR").Allocate(70, 30)
	require.NoError(t, err)
	assert.EqualValues(t, 1, parts[0].Minor())
	assert.EqualValues(t, 0, parts[1].Minor())
}

func TestAllocateThreeWay(t *testing.T) {
	parts, err := money.MustNew(100, "IRR").Allocate(1, 1, 1)
	require.NoError(t, err)
	total, err := money.Sum(parts...)
	require.NoError(t, err)
	assert.EqualValues(t, 100, total.Minor())
	assert.EqualValues(t, 34, parts[0].Minor())
	assert.EqualValues(t, 33, parts[1].Minor())
	assert.EqualValues(t, 33, parts[2].Minor())
}

func TestAllocateNegativeAmount(t *testing.T) {
	// Reversing a split (a refund clawing back the 70/30 revenue share) must
	// mirror the original allocation exactly, or the two ledger entries would not
	// cancel out.
	positive, err := money.MustNew(999, "IRR").Allocate(70, 30)
	require.NoError(t, err)
	assert.EqualValues(t, 699, positive[0].Minor())
	assert.EqualValues(t, 300, positive[1].Minor())

	negative, err := money.MustNew(-999, "IRR").Allocate(70, 30)
	require.NoError(t, err)
	total, err := money.Sum(negative...)
	require.NoError(t, err)
	assert.EqualValues(t, -999, total.Minor())
	for i := range positive {
		assert.EqualValues(t, -positive[i].Minor(), negative[i].Minor(),
			"reversing the split must negate each part exactly")
	}
}

func TestAllocateRejectsBadWeights(t *testing.T) {
	_, err := money.MustNew(100, "IRR").Allocate()
	assert.Error(t, err)

	_, err = money.MustNew(100, "IRR").Allocate(0, 0)
	assert.Error(t, err)

	_, err = money.MustNew(100, "IRR").Allocate(70, -30)
	assert.Error(t, err)
}

func TestMulInt(t *testing.T) {
	got, err := money.MustNew(250, "IRR").MulInt(4)
	require.NoError(t, err)
	assert.EqualValues(t, 1000, got.Minor())

	_, err = money.MustNew(math.MaxInt64/2, "IRR").MulInt(4)
	assert.ErrorIs(t, err, money.ErrOverflow)
}

func TestComparisons(t *testing.T) {
	a := money.MustNew(100, "IRR")
	b := money.MustNew(200, "IRR")

	gt, err := b.GreaterThan(a)
	require.NoError(t, err)
	assert.True(t, gt)

	lt, err := a.LessThan(b)
	require.NoError(t, err)
	assert.True(t, lt)

	assert.True(t, a.Equal(money.MustNew(100, "IRR")))
	assert.False(t, a.Equal(money.MustNew(100, "USD")))
}

func TestNegAndAbs(t *testing.T) {
	m := money.MustNew(-500, "IRR")
	assert.EqualValues(t, 500, m.Abs().Minor())
	assert.EqualValues(t, 500, m.Neg().Minor())
	assert.EqualValues(t, -500, money.MustNew(500, "IRR").Neg().Minor())
}

func TestJSONRoundTrip(t *testing.T) {
	original := money.MustNew(9_007_199_254_740_993, "IRR") // > 2^53
	data, err := json.Marshal(original)
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount_minor":"9007199254740993","currency":"IRR"}`, string(data))

	var decoded money.Money
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.True(t, original.Equal(decoded))
}

func TestJSONAcceptsNumericAmount(t *testing.T) {
	var m money.Money
	require.NoError(t, json.Unmarshal([]byte(`{"amount_minor":1500,"currency":"irr"}`), &m))
	assert.EqualValues(t, 1500, m.Minor())
	assert.Equal(t, "IRR", m.Currency())
}

func TestJSONRejectsBadPayloads(t *testing.T) {
	var m money.Money
	assert.Error(t, json.Unmarshal([]byte(`{"amount_minor":"abc","currency":"IRR"}`), &m))
	assert.Error(t, json.Unmarshal([]byte(`{"amount_minor":100,"currency":"XX"}`), &m))
}

func TestString(t *testing.T) {
	assert.Equal(t, "12.34 IRR", money.MustNew(1234, "IRR").String())
	assert.Equal(t, "-12.34 IRR", money.MustNew(-1234, "IRR").String())
	assert.Equal(t, "0.00 ???", money.Money{}.String())
}

func TestSumEmpty(t *testing.T) {
	sum, err := money.Sum()
	require.NoError(t, err)
	assert.True(t, sum.IsZero())
}
