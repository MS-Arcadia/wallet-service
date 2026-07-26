package giftcard_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/idgen"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPepper = "a-gift-card-pepper-long-enough-to-be-accepted"

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func newHasher(t *testing.T) *giftcard.Hasher {
	t.Helper()
	hasher, err := giftcard.NewHasher(testPepper)
	require.NoError(t, err)
	return hasher
}

func issue(t *testing.T, hasher *giftcard.Hasher) (*giftcard.GiftCard, string) {
	t.Helper()
	card, code, err := giftcard.Issue("gc-1", money.MustNew(500_000, "IRR"),
		"support-1", "batch-1", "promo campaign", hasher, now)
	require.NoError(t, err)
	return card, code
}

func TestNewHasherRejectsShortPepper(t *testing.T) {
	_, err := giftcard.NewHasher("too-short")
	assert.Error(t, err)
}

func TestIssue(t *testing.T) {
	hasher := newHasher(t)
	card, code := issue(t, hasher)

	assert.Equal(t, "gc-1", card.ID())
	assert.Equal(t, giftcard.StatusActive, card.Status())
	assert.EqualValues(t, 500_000, card.Value().Minor())
	assert.Equal(t, "support-1", card.IssuedBy())
	assert.Equal(t, "batch-1", card.BatchID())
	assert.NotEmpty(t, code)
	assert.Nil(t, card.RedeemedAt())
}

func TestIssuedCodeIsNotRecoverableFromTheAggregate(t *testing.T) {
	hasher := newHasher(t)
	card, code := issue(t, hasher)

	// Whatever the aggregate exposes must not let anybody reconstruct the code.
	assert.NotContains(t, card.CodeHash(), idgen.NormalizeCode(code))
	assert.Len(t, card.CodeHash(), 64, "hex-encoded HMAC-SHA256")
	assert.Len(t, card.CodeHint(), 4)
	assert.Equal(t, idgen.NormalizeCode(code)[12:], card.CodeHint())
}

func TestHashIsStableAndFormatInsensitive(t *testing.T) {
	hasher := newHasher(t)
	_, code := issue(t, hasher)

	assert.Equal(t, hasher.Hash(code), hasher.Hash(code), "hashing must be deterministic")
	// A user pasting the code in lower case or with spaces must still redeem it.
	assert.Equal(t, hasher.Hash(code), hasher.Hash(idgen.NormalizeCode(code)))
	assert.Equal(t, hasher.Hash(code), hasher.Hash(" "+code+" "))
}

func TestDifferentPepperProducesDifferentHash(t *testing.T) {
	first := newHasher(t)
	second, err := giftcard.NewHasher("a-completely-different-pepper-also-long-enough")
	require.NoError(t, err)

	_, code := issue(t, first)
	assert.NotEqual(t, first.Hash(code), second.Hash(code),
		"the pepper must actually participate in the hash")
}

func TestIssueValidation(t *testing.T) {
	hasher := newHasher(t)

	_, _, err := giftcard.Issue("", money.MustNew(100, "IRR"), "support-1", "b", "", hasher, now)
	assert.Error(t, err)

	_, _, err = giftcard.Issue("gc-1", money.MustNew(0, "IRR"), "support-1", "b", "", hasher, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))

	_, _, err = giftcard.Issue("gc-1", money.MustNew(-100, "IRR"), "support-1", "b", "", hasher, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))

	_, _, err = giftcard.Issue("gc-1", money.MustNew(100, "IRR"), "", "b", "", hasher, now)
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestRedeem(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)

	require.NoError(t, card.Redeem("user-1", now))
	assert.Equal(t, giftcard.StatusUsed, card.Status())
	assert.Equal(t, "user-1", card.RedeemedBy())
	require.NotNil(t, card.RedeemedAt())
	assert.True(t, now.Equal(*card.RedeemedAt()))
}

func TestDoubleRedemptionIsRejected(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)
	require.NoError(t, card.Redeem("user-1", now))

	err := card.Redeem("user-2", now.Add(time.Second))
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.Equal(t, giftcard.ReasonCodeAlreadyUsed, errs.ReasonOf(err))
	assert.Equal(t, "user-1", card.RedeemedBy(), "the first redeemer must be preserved")
}

func TestIssuerCannotRedeemOwnCard(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)

	err := card.Redeem("support-1", now)
	require.Error(t, err)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
	assert.Equal(t, giftcard.ReasonCodeSelfIssued, errs.ReasonOf(err))
	assert.Equal(t, giftcard.StatusActive, card.Status())
}

func TestRedeemRequiresAUser(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)
	assert.Error(t, card.Redeem("", now))
}

func TestRevoke(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)

	require.NoError(t, card.Revoke("printed with the wrong value", now))
	assert.Equal(t, giftcard.StatusRevoked, card.Status())
	assert.Equal(t, "printed with the wrong value", card.RevokeNote())
	require.NotNil(t, card.RevokedAt())
}

func TestRevokedCardCannotBeRedeemed(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)
	require.NoError(t, card.Revoke("fraud", now))

	err := card.Redeem("user-1", now)
	require.Error(t, err)
	assert.Equal(t, giftcard.ReasonCodeRevoked, errs.ReasonOf(err))
}

func TestRedeemedCardCannotBeRevoked(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)
	require.NoError(t, card.Redeem("user-1", now))

	err := card.Revoke("changed our mind", now)
	require.Error(t, err)
	assert.Equal(t, giftcard.ReasonCodeAlreadyUsed, errs.ReasonOf(err),
		"clawing money back out of a wallet is an adjustment, not a revocation")
}

func TestDoubleRevokeIsRejected(t *testing.T) {
	hasher := newHasher(t)
	card, _ := issue(t, hasher)
	require.NoError(t, card.Revoke("first", now))
	assert.Error(t, card.Revoke("second", now))
}

func TestNotFoundErrorLeaksNothing(t *testing.T) {
	err := giftcard.ErrNotFound()
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	assert.Equal(t, giftcard.ReasonCodeNotFound, errs.ReasonOf(err))
	// The message must be identical whether the code was unknown or malformed, so
	// that the endpoint cannot be used to enumerate live codes.
	assert.Equal(t, "NOT_FOUND: this gift card code is not valid", err.Error())
}

func TestRehydrate(t *testing.T) {
	redeemedAt := now.Add(-time.Hour)
	card, err := giftcard.Rehydrate("gc-1", "deadbeef", "VW8N",
		money.MustNew(500_000, "IRR"), giftcard.StatusUsed,
		"support-1", "batch-1", "note", "user-1",
		&redeemedAt, nil, "", now.Add(-2*time.Hour), 2)
	require.NoError(t, err)

	assert.Equal(t, giftcard.StatusUsed, card.Status())
	assert.Equal(t, "user-1", card.RedeemedBy())
	assert.EqualValues(t, 2, card.Version())
}

func TestRehydrateRejectsCorruptState(t *testing.T) {
	_, err := giftcard.Rehydrate("", "h", "1234", money.MustNew(1, "IRR"),
		giftcard.StatusActive, "s", "b", "", "", nil, nil, "", now, 1)
	assert.Error(t, err)

	_, err = giftcard.Rehydrate("gc-1", "h", "1234", money.MustNew(1, "IRR"),
		giftcard.Status("SHREDDED"), "s", "b", "", "", nil, nil, "", now, 1)
	assert.Error(t, err)
}

func TestIssuedCodesAreUnique(t *testing.T) {
	hasher := newHasher(t)
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		_, code, err := giftcard.Issue("gc", money.MustNew(100, "IRR"), "support-1", "b", "", hasher, now)
		require.NoError(t, err)
		_, dup := seen[code]
		require.False(t, dup, "issued a duplicate code %q", code)
		seen[code] = struct{}{}
	}
}
