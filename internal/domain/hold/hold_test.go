package hold_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func irr(minor int64) money.Money { return money.MustNew(minor, "IRR") }

func newHold(t *testing.T, amountMinor int64, ttl time.Duration) *hold.Hold {
	t.Helper()
	h, err := hold.New("h-1", "w-1", "u-1", irr(amountMinor), "preorder-1", "pre-order", ttl, now)
	require.NoError(t, err)
	return h
}

func TestNew(t *testing.T) {
	h := newHold(t, 300_000, 0)

	assert.Equal(t, "h-1", h.ID())
	assert.Equal(t, hold.StatusActive, h.Status())
	assert.EqualValues(t, 300_000, h.Amount().Minor())
	assert.True(t, h.CapturedAmount().IsZero())
	assert.EqualValues(t, 300_000, h.Remaining().Minor())
	assert.Nil(t, h.ExpiresAt(), "ttl of zero means the hold never lapses")
	assert.Nil(t, h.ResolvedAt())
}

func TestNewWithTTLSetsExpiry(t *testing.T) {
	h := newHold(t, 300_000, 48*time.Hour)
	require.NotNil(t, h.ExpiresAt())
	assert.True(t, now.Add(48*time.Hour).Equal(*h.ExpiresAt()))
	assert.False(t, h.IsExpired(now))
	assert.True(t, h.IsExpired(now.Add(48*time.Hour)))
}

func TestNewValidation(t *testing.T) {
	_, err := hold.New("", "w-1", "u-1", irr(100), "ref", "", 0, now)
	assert.Error(t, err)

	_, err = hold.New("h-1", "", "u-1", irr(100), "ref", "", 0, now)
	assert.Error(t, err)

	_, err = hold.New("h-1", "w-1", "u-1", irr(0), "ref", "", 0, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))

	_, err = hold.New("h-1", "w-1", "u-1", irr(-100), "ref", "", 0, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))

	_, err = hold.New("h-1", "w-1", "u-1", irr(100), "", "", 0, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err),
		"a hold with no reference could never be reconciled against its pre-order")
}

func TestCaptureAllClosesTheHold(t *testing.T) {
	h := newHold(t, 300_000, 0)

	captured, err := h.Capture(irr(300_000), now)
	require.NoError(t, err)
	assert.EqualValues(t, 300_000, captured.Minor())
	assert.Equal(t, hold.StatusCaptured, h.Status())
	assert.True(t, h.Remaining().IsZero())
	require.NotNil(t, h.ResolvedAt())
}

func TestZeroAmountCapturesEverythingRemaining(t *testing.T) {
	h := newHold(t, 300_000, 0)

	captured, err := h.Capture(money.Money{}, now)
	require.NoError(t, err)
	assert.EqualValues(t, 300_000, captured.Minor())
	assert.Equal(t, hold.StatusCaptured, h.Status())
}

func TestInstalmentPlanDrawsDownOneHold(t *testing.T) {
	// Three equal instalments against a single 300,000 reservation.
	h := newHold(t, 300_000, 0)

	for i := 1; i <= 3; i++ {
		captured, err := h.Capture(irr(100_000), now.Add(time.Duration(i)*24*time.Hour))
		require.NoError(t, err, "instalment %d must succeed", i)
		assert.EqualValues(t, 100_000, captured.Minor())

		if i < 3 {
			assert.Equal(t, hold.StatusActive, h.Status(), "the hold stays open between instalments")
			assert.EqualValues(t, int64(300_000-i*100_000), h.Remaining().Minor())
		}
	}

	assert.Equal(t, hold.StatusCaptured, h.Status())
	assert.EqualValues(t, 300_000, h.CapturedAmount().Minor())
	assert.True(t, h.Remaining().IsZero())
}

func TestCaptureBeyondRemainingIsRejected(t *testing.T) {
	h := newHold(t, 300_000, 0)
	_, err := h.Capture(irr(250_000), now)
	require.NoError(t, err)

	_, err = h.Capture(irr(50_001), now)
	require.Error(t, err)
	assert.Equal(t, hold.ReasonCodeExceedsRemaining, errs.ReasonOf(err))
	assert.EqualValues(t, 250_000, h.CapturedAmount().Minor(),
		"a rejected capture must not change the accumulated total")
}

func TestCaptureOnClosedHoldIsRejected(t *testing.T) {
	h := newHold(t, 100_000, 0)
	_, err := h.Capture(irr(100_000), now)
	require.NoError(t, err)

	_, err = h.Capture(irr(1), now)
	require.Error(t, err)
	assert.Equal(t, hold.ReasonCodeNotActive, errs.ReasonOf(err))
}

func TestCaptureOnExpiredHoldIsRejectedBeforeTheSweeperRuns(t *testing.T) {
	h := newHold(t, 100_000, time.Hour)

	// The status is still ACTIVE — nothing has swept it yet — but the reservation
	// has lapsed and the money belongs to the user again.
	assert.Equal(t, hold.StatusActive, h.Status())
	_, err := h.Capture(irr(100_000), now.Add(2*time.Hour))
	require.Error(t, err)
	assert.Equal(t, hold.ReasonCodeExpired, errs.ReasonOf(err))
}

func TestReleaseReturnsTheRemainder(t *testing.T) {
	h := newHold(t, 300_000, 0)
	_, err := h.Capture(irr(120_000), now)
	require.NoError(t, err)

	released, err := h.Release(now)
	require.NoError(t, err)
	assert.EqualValues(t, 180_000, released.Minor(), "only the uncaptured part comes back")
	assert.Equal(t, hold.StatusReleased, h.Status())
	require.NotNil(t, h.ResolvedAt())
}

func TestDoubleReleaseIsRejected(t *testing.T) {
	h := newHold(t, 100_000, 0)
	_, err := h.Release(now)
	require.NoError(t, err)

	_, err = h.Release(now)
	require.Error(t, err)
	assert.Equal(t, hold.ReasonCodeNotActive, errs.ReasonOf(err))
}

func TestExpireOnlyWorksAfterTheTTL(t *testing.T) {
	h := newHold(t, 100_000, time.Hour)

	_, err := h.Expire(now)
	require.Error(t, err, "expiring early would confiscate a live reservation")
	assert.Equal(t, hold.StatusActive, h.Status())

	released, err := h.Expire(now.Add(time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 100_000, released.Minor())
	assert.Equal(t, hold.StatusExpired, h.Status(),
		"a lapsed plan is distinguishable from a user-initiated cancellation")
}

func TestExpireOnHoldWithoutTTL(t *testing.T) {
	h := newHold(t, 100_000, 0)
	_, err := h.Expire(now.Add(10 * 365 * 24 * time.Hour))
	assert.Error(t, err, "a hold with no expiry never lapses")
}

func TestCaptureRejectsNegativeAmount(t *testing.T) {
	h := newHold(t, 100_000, 0)
	_, err := h.Capture(irr(-1_000), now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestStatusTerminal(t *testing.T) {
	assert.False(t, hold.StatusActive.Terminal())
	for _, status := range []hold.Status{hold.StatusCaptured, hold.StatusReleased, hold.StatusExpired} {
		assert.True(t, status.Terminal(), "%s must be terminal", status)
	}
}

func TestRehydrate(t *testing.T) {
	resolvedAt := now.Add(time.Hour)
	h, err := hold.Rehydrate("h-1", "w-1", "u-1", irr(300_000), irr(300_000),
		hold.StatusCaptured, "preorder-1", "pre-order", nil, now, &resolvedAt, 4)
	require.NoError(t, err)

	assert.Equal(t, hold.StatusCaptured, h.Status())
	assert.True(t, h.Remaining().IsZero())
	assert.EqualValues(t, 4, h.Version())
}

func TestRehydrateRejectsCorruptState(t *testing.T) {
	_, err := hold.Rehydrate("", "w-1", "u-1", irr(1), irr(0),
		hold.StatusActive, "ref", "", nil, now, nil, 1)
	assert.Error(t, err)

	_, err = hold.Rehydrate("h-1", "w-1", "u-1", irr(1), irr(0),
		hold.Status("FORGOTTEN"), "ref", "", nil, now, nil, 1)
	assert.Error(t, err)
}
