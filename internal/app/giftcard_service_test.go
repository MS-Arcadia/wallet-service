package app_test

import (
	"testing"

	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/abuse"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Issuance -------------------------------------------------------------

func TestIssueGiftCardsRequiresStaff(t *testing.T) {
	h := newHarness(t)

	cmd := app.IssueGiftCardsCommand{Value: irr(500_000), Quantity: 1, IdempotencyKey: "k-1"}

	_, err := h.GiftCards.IssueGiftCards(asUser("u-1"), cmd)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = h.GiftCards.IssueGiftCards(anonymous(), cmd)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))

	_, err = h.GiftCards.IssueGiftCards(asSupport("s-1"), cmd)
	assert.NoError(t, err)
}

func TestIssueGiftCardsReturnsCodesOnlyOnce(t *testing.T) {
	h := newHarness(t)

	cmd := app.IssueGiftCardsCommand{
		Value: irr(500_000), Quantity: 3, Note: "eid promotion", IdempotencyKey: "batch-1",
	}
	first, err := h.GiftCards.IssueGiftCards(asSupport("s-1"), cmd)
	require.NoError(t, err)
	require.Len(t, first.GiftCards, 3)

	codes := make(map[string]struct{}, 3)
	for _, card := range first.GiftCards {
		assert.NotEmpty(t, card.Code, "the issuing response is the only place a code appears")
		assert.Len(t, card.CodeHint, 4)
		codes[card.Code] = struct{}{}
	}
	assert.Len(t, codes, 3, "each card gets its own code")

	// A replayed issuance cannot re-reveal codes, because only their hashes exist.
	replayed, err := h.GiftCards.IssueGiftCards(asSupport("s-1"), cmd)
	require.NoError(t, err)
	assert.True(t, replayed.IdempotentReplay)
	assert.Equal(t, first.BatchID, replayed.BatchID)
	for _, card := range replayed.GiftCards {
		assert.Empty(t, card.Code, "a stored code would defeat the point of hashing it")
	}
	assert.Len(t, h.Store.GiftCards, 3, "the replay must not mint a second batch")
}

func TestIssueGiftCardsValidation(t *testing.T) {
	h := newHarness(t)
	ctx := asSupport("s-1")

	tests := []struct {
		name string
		cmd  app.IssueGiftCardsCommand
	}{
		{"no key", app.IssueGiftCardsCommand{Value: irr(100), Quantity: 1}},
		{"zero value", app.IssueGiftCardsCommand{Value: irr(0), Quantity: 1, IdempotencyKey: "k"}},
		{"negative value", app.IssueGiftCardsCommand{Value: irr(-100), Quantity: 1, IdempotencyKey: "k"}},
		{"zero quantity", app.IssueGiftCardsCommand{Value: irr(100), Quantity: 0, IdempotencyKey: "k"}},
		{"absurd quantity", app.IssueGiftCardsCommand{Value: irr(100), Quantity: 1_000_001, IdempotencyKey: "k"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.GiftCards.IssueGiftCards(ctx, tc.cmd)
			assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
		})
	}
}

// --- Redemption -----------------------------------------------------------

func TestRedeemGiftCardCreditsTheWallet(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, cardID := h.issueCard(500_000)

	result, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 500_000, result.Credited.Minor())
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"))
	assert.Equal(t, wallet.ReasonGiftCard, result.Entry.Reason)
	assert.Equal(t, cardID, result.Entry.ReferenceID)
	assert.True(t, h.Store.HasEvent(app.EventGiftCardRedeemed))
	assert.True(t, h.Store.HasEvent(app.EventWalletCredited))
}

func TestRedeemAcceptsAnyFormattingOfTheCode(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, _ := h.issueCard(500_000)

	// A user pasting the code in lower case, or with the dashes stripped, must still
	// be able to spend it.
	messy := "  " + code + "  "
	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: messy, IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"))
}

func TestGiftCardCannotBeRedeemedTwice(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	h.fund("u-2", 0)
	code, _ := h.issueCard(500_000)

	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err)

	// A second user with the same code — the leaked-code scenario.
	_, err = h.GiftCards.RedeemGiftCard(asUser("u-2"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-2",
	})
	require.Error(t, err)
	assert.Equal(t, giftcard.ReasonCodeAlreadyUsed, errs.ReasonOf(err))
	assert.EqualValues(t, 0, h.balanceOf("u-2"))
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"), "the first redeemer keeps the value")
}

func TestRetriedRedemptionCreditsOnce(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, _ := h.issueCard(500_000)

	cmd := app.RedeemGiftCardCommand{Code: code, IdempotencyKey: "redeem-1"}
	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), cmd)
	require.NoError(t, err)

	second, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), cmd)
	require.NoError(t, err, "a retry of the same request must succeed, not report ALREADY_USED")
	assert.True(t, second.IdempotentReplay)
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"), "credited exactly once")
}

func TestUnknownCodeIsRejectedWithoutLeakingAnything(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)

	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: "ZZZZ-ZZZZ-ZZZZ-ZZZZ", IdempotencyKey: "redeem-1",
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	assert.Equal(t, giftcard.ReasonCodeNotFound, errs.ReasonOf(err))
	assert.EqualValues(t, 0, h.balanceOf("u-1"))
}

func TestRevokedCardCannotBeRedeemed(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, cardID := h.issueCard(500_000)

	_, err := h.GiftCards.RevokeGiftCard(asSupport("s-1"), cardID, "batch printed with the wrong value")
	require.NoError(t, err)

	_, err = h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.Error(t, err)
	assert.Equal(t, giftcard.ReasonCodeRevoked, errs.ReasonOf(err))
	assert.EqualValues(t, 0, h.balanceOf("u-1"))
}

func TestIssuerCannotRedeemTheirOwnCard(t *testing.T) {
	h := newHarness(t)
	h.fund("support-1", 0)
	code, _ := h.issueCard(500_000)

	// Insider fraud: the Support user who minted the card tries to spend it.
	_, err := h.GiftCards.RedeemGiftCard(asSupport("support-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.Error(t, err)
	assert.Equal(t, giftcard.ReasonCodeSelfIssued, errs.ReasonOf(err))
	assert.EqualValues(t, 0, h.balanceOf("support-1"))
}

func TestRedeemRequiresAuthentication(t *testing.T) {
	h := newHarness(t)
	code, _ := h.issueCard(500_000)

	_, err := h.GiftCards.RedeemGiftCard(anonymous(), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

// --- Abuse detection ------------------------------------------------------

func TestFailedAttemptsAreCountedAgainstTheUser(t *testing.T) {
	h := newHarness(t)
	h.fund("prober", 0)

	for i := 0; i < 3; i++ {
		_, err := h.GiftCards.RedeemGiftCard(asUser("prober"), app.RedeemGiftCardCommand{
			Code: "ZZZZ-ZZZZ-ZZZZ-ZZZ" + string(rune('A'+i)), IdempotencyKey: "guess-" + string(rune('a'+i)),
		})
		require.Error(t, err)
	}
	assert.EqualValues(t, 3, h.Limiter.Failures["prober"])
}

func TestASuccessfulRedemptionCostsNoAllowance(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, _ := h.issueCard(500_000)

	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err)
	assert.Zero(t, h.Limiter.Failures["u-1"],
		"only genuine code failures count; a legitimate user must not be throttled")
}

func TestAFrozenWalletDoesNotCountAsAGuess(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, _ := h.issueCard(500_000)

	_, err := h.Admin.FreezeWallet(asSupport("s-1"), "u-1", "under investigation")
	require.NoError(t, err)

	_, err = h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeWalletFrozen, errs.ReasonOf(err))
	assert.Zero(t, h.Limiter.Failures["u-1"],
		"the code was valid; the user's own wallet state is not evidence of probing")
}

func TestBlockedUserIsRefusedBeforeAnyLookup(t *testing.T) {
	h := newHarness(t)
	h.fund("prober", 0)
	code, _ := h.issueCard(500_000)

	// Drive the limiter past its threshold.
	h.Limiter.Failures["prober"] = 5

	_, err := h.GiftCards.RedeemGiftCard(asUser("prober"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeResourceExhausted, errs.CodeOf(err))
	assert.Equal(t, abuse.ReasonCodeTooManyAttempts, errs.ReasonOf(err))

	// Even though the code was real, the throttle refused it. Otherwise the endpoint
	// would still be usable to confirm which guesses were valid.
	assert.EqualValues(t, 0, h.balanceOf("prober"))
	assert.Equal(t, 1, h.Metrics.RateLimitBlocks["giftcard-attempt"])
}

func TestSustainedProbingFlagsTheUserForSupport(t *testing.T) {
	h := newHarness(t)
	h.fund("prober", 0)

	// The default policy flags at ten failures in the hourly window. Raise the
	// limiter's block threshold so that the attempts keep landing and the flag
	// threshold is what fires.
	h.Limiter.BlockAfter = 1_000

	for i := 0; i < 10; i++ {
		_, err := h.GiftCards.RedeemGiftCard(asUser("prober"), app.RedeemGiftCardCommand{
			Code:           "ZZZZ-ZZZZ-ZZZZ-ZZZZ",
			IdempotencyKey: "guess-" + string(rune('a'+i)),
		})
		require.Error(t, err)
	}

	events := h.Store.EventsOfType(app.EventGiftCardAbuseDetected)
	require.NotEmpty(t, events, "Support must be told about a pattern of guessing")

	var payload app.GiftCardAbuseDetectedPayload
	require.NoError(t, events[0].DecodePayload(&payload))
	assert.Equal(t, "prober", payload.UserID)
	assert.GreaterOrEqual(t, payload.FailedAttempts, int64(10))
	assert.Equal(t, "REVIEW_FOR_BAN", payload.RecommendedAction,
		"the wallet service recommends; a human decides")
}

func TestNoFlagBeforeTheReviewThreshold(t *testing.T) {
	h := newHarness(t)
	h.fund("clumsy", 0)
	h.Limiter.BlockAfter = 1_000

	// Nine mistyped codes is a clumsy user, not an attacker.
	for i := 0; i < 9; i++ {
		_, err := h.GiftCards.RedeemGiftCard(asUser("clumsy"), app.RedeemGiftCardCommand{
			Code:           "ZZZZ-ZZZZ-ZZZZ-ZZZZ",
			IdempotencyKey: "typo-" + string(rune('a'+i)),
		})
		require.Error(t, err)
	}
	assert.False(t, h.Store.HasEvent(app.EventGiftCardAbuseDetected))
}

func TestRedemptionSurvivesAnUnreachableLimiter(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, _ := h.issueCard(500_000)

	// Redis is down. The abuse rule stops being enforced, but a legitimate user must
	// still be able to spend their gift card.
	h.Limiter.Err = assert.AnError

	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err, "the limiter fails open by design")
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"))
}

// --- Staff reads ----------------------------------------------------------

func TestGetGiftCardNeverEchoesTheCode(t *testing.T) {
	h := newHarness(t)
	code, cardID := h.issueCard(500_000)

	byID, err := h.GiftCards.GetGiftCard(asSupport("s-1"), cardID, "")
	require.NoError(t, err)
	assert.Empty(t, byID.Code, "a support screen is a poor place to display a bearer instrument")
	assert.NotEmpty(t, byID.CodeHint)

	byCode, err := h.GiftCards.GetGiftCard(asSupport("s-1"), "", code)
	require.NoError(t, err)
	assert.Equal(t, cardID, byCode.ID, "lookup by code resolves through the hash")
	assert.Empty(t, byCode.Code)
}

func TestGetGiftCardRequiresStaff(t *testing.T) {
	h := newHarness(t)
	_, cardID := h.issueCard(500_000)

	_, err := h.GiftCards.GetGiftCard(asUser("u-1"), cardID, "")
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
}

func TestListGiftCardsFiltersByStatus(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, _ := h.issueCard(100_000)
	h.issueCard(200_000)

	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err)

	active, err := h.GiftCards.ListGiftCards(asSupport("s-1"), giftcard.StatusActive, "", 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, active.TotalItems)

	used, err := h.GiftCards.ListGiftCards(asSupport("s-1"), giftcard.StatusUsed, "", 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, used.TotalItems)
	assert.Equal(t, "u-1", used.GiftCards[0].RedeemedBy)
}

func TestRevokeRequiresAJustification(t *testing.T) {
	h := newHarness(t)
	_, cardID := h.issueCard(500_000)

	_, err := h.GiftCards.RevokeGiftCard(asSupport("s-1"), cardID, "")
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err),
		"destroying promised value without a reason is not auditable")

	_, err = h.GiftCards.RevokeGiftCard(asUser("u-1"), cardID, "because")
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
}

func TestRedeemedCardCannotBeRevoked(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)
	code, cardID := h.issueCard(500_000)

	_, err := h.GiftCards.RedeemGiftCard(asUser("u-1"), app.RedeemGiftCardCommand{
		Code: code, IdempotencyKey: "redeem-1",
	})
	require.NoError(t, err)

	_, err = h.GiftCards.RevokeGiftCard(asSupport("s-1"), cardID, "changed our mind")
	require.Error(t, err)
	assert.Equal(t, giftcard.ReasonCodeAlreadyUsed, errs.ReasonOf(err),
		"clawing money back out of a wallet is an adjustment, not a revocation")
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"))
}
