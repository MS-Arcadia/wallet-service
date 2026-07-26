package app_test

import (
	"testing"

	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Initiating a top-up --------------------------------------------------

func TestInitiateChargeDoesNotTouchTheBalance(t *testing.T) {
	h := newHarness(t)

	result, err := h.Charges.InitiateCharge(asUser("u-1"), app.InitiateChargeCommand{
		UserID:         "u-1",
		Amount:         irr(500_000),
		ReturnURL:      "https://arcadia.example/wallet",
		IdempotencyKey: "charge-1",
	})
	require.NoError(t, err)

	assert.Equal(t, "intent-1", result.PaymentIntentID)
	assert.Contains(t, result.RedirectURL, "bank.example")

	// No money has moved yet: the user has not paid the bank. Crediting here would
	// hand balance to everyone who merely opened the payment page.
	assert.EqualValues(t, 0, h.balanceOf("u-1"))
	assert.False(t, h.Store.HasEvent(app.EventWalletCredited))
	assert.True(t, h.Store.HasEvent(app.EventChargeInitiated))
}

func TestInitiateChargeProvisionsTheWalletFirst(t *testing.T) {
	h := newHarness(t)

	// A brand new user with no wallet. One must exist before the payment starts, or
	// the confirmation consumer would have nowhere to put the money.
	_, err := h.Charges.InitiateCharge(asUser("new-user"), app.InitiateChargeCommand{
		Amount: irr(500_000), IdempotencyKey: "charge-1",
	})
	require.NoError(t, err)

	_, found := h.Store.WalletOf("new-user")
	assert.True(t, found)
}

func TestInitiateChargeForwardsTheIdempotencyKeyToTheBank(t *testing.T) {
	h := newHarness(t)

	_, err := h.Charges.InitiateCharge(asUser("u-1"), app.InitiateChargeCommand{
		Amount: irr(500_000), IdempotencyKey: "charge-1",
	})
	require.NoError(t, err)

	require.Len(t, h.Gateway.Requests, 1)
	assert.Equal(t, "charge-1", h.Gateway.Requests[0].IdempotencyKey,
		"the gateway needs the key so a retry does not create a second intent at the bank")
	assert.Equal(t, "WALLET_TOPUP", h.Gateway.Requests[0].Metadata["purpose"])
}

func TestRetriedInitiateChargeCallsTheBankOnce(t *testing.T) {
	h := newHarness(t)

	cmd := app.InitiateChargeCommand{Amount: irr(500_000), IdempotencyKey: "charge-1"}
	first, err := h.Charges.InitiateCharge(asUser("u-1"), cmd)
	require.NoError(t, err)

	second, err := h.Charges.InitiateCharge(asUser("u-1"), cmd)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay)
	assert.Equal(t, first.PaymentIntentID, second.PaymentIntentID)
	assert.Equal(t, 1, h.Gateway.CallCount(),
		"a double-clicked top-up button must not open two bank payments")
}

func TestUsersCannotStartATopUpForSomebodyElse(t *testing.T) {
	h := newHarness(t)

	_, err := h.Charges.InitiateCharge(asUser("u-1"), app.InitiateChargeCommand{
		UserID: "u-2", Amount: irr(500_000), IdempotencyKey: "charge-1",
	})
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	// Not even staff: a top-up ends with a real person at a bank page.
	_, err = h.Charges.InitiateCharge(asSupport("s-1"), app.InitiateChargeCommand{
		UserID: "u-2", Amount: irr(500_000), IdempotencyKey: "charge-2",
	})
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
}

func TestInitiateChargeValidation(t *testing.T) {
	h := newHarness(t)
	ctx := asUser("u-1")

	tests := []struct {
		name string
		cmd  app.InitiateChargeCommand
	}{
		{"no key", app.InitiateChargeCommand{Amount: irr(500_000)}},
		{"zero amount", app.InitiateChargeCommand{Amount: irr(0), IdempotencyKey: "k"}},
		{"negative amount", app.InitiateChargeCommand{Amount: irr(-500_000), IdempotencyKey: "k"}},
		{"below the minimum", app.InitiateChargeCommand{Amount: irr(999), IdempotencyKey: "k"}},
		{"foreign currency", app.InitiateChargeCommand{
			Amount: money.MustNew(500_000, "USD"), IdempotencyKey: "k",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Charges.InitiateCharge(ctx, tc.cmd)
			assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
		})
	}
	assert.Zero(t, h.Gateway.CallCount(), "an invalid request must never reach the bank")
}

func TestGatewayFailureIsSurfaced(t *testing.T) {
	h := newHarness(t)
	h.Gateway.Err = errs.Unavailable("the payment adapter is unreachable")

	_, err := h.Charges.InitiateCharge(asUser("u-1"), app.InitiateChargeCommand{
		Amount: irr(500_000), IdempotencyKey: "charge-1",
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnavailable, errs.CodeOf(err))
	assert.True(t, errs.IsRetryable(err), "an unreachable dependency is worth retrying")
}

func TestRetryAfterAGatewayFailureIsNotSilentlyDropped(t *testing.T) {
	h := newHarness(t)
	h.Gateway.Err = errs.Unavailable("the payment adapter is unreachable")

	cmd := app.InitiateChargeCommand{Amount: irr(500_000), IdempotencyKey: "charge-1"}
	_, err := h.Charges.InitiateCharge(asUser("u-1"), cmd)
	require.Error(t, err)

	// The key was claimed before the gateway call and no response was stored. A retry
	// must be told the request is in flight rather than being handed a stale success
	// or quietly opening a second bank payment.
	h.Gateway.Err = nil
	_, err = h.Charges.InitiateCharge(asUser("u-1"), cmd)
	require.Error(t, err)
	assert.Equal(t, "IDEMPOTENCY_IN_PROGRESS", errs.ReasonOf(err))
	assert.True(t, errs.IsRetryable(err))
}

// --- Confirming a top-up --------------------------------------------------

func TestConfirmChargeCreditsTheWallet(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)

	result, err := h.Charges.ConfirmCharge(anonymous(), app.ConfirmChargeCommand{
		UserID:          "u-1",
		Amount:          irr(500_000),
		PaymentIntentID: "intent-1",
		BankReference:   "BNK-998877",
		EventID:         "evt-1",
	})
	require.NoError(t, err)

	assert.EqualValues(t, 500_000, h.balanceOf("u-1"))
	assert.Equal(t, wallet.ReasonCharge, result.Entry.Reason)
	assert.Equal(t, "intent-1", result.Entry.ReferenceID)
	assert.Contains(t, result.Entry.Description, "BNK-998877",
		"the bank reference belongs in the audit trail")
	assert.True(t, h.Store.HasEvent(app.EventWalletCredited))
}

func TestRedeliveredConfirmationCreditsOnce(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)

	cmd := app.ConfirmChargeCommand{
		UserID: "u-1", Amount: irr(500_000), PaymentIntentID: "intent-1", EventID: "evt-1",
	}

	// Kafka delivers at least once, so this genuinely happens in production.
	_, err := h.Charges.ConfirmCharge(anonymous(), cmd)
	require.NoError(t, err)
	second, err := h.Charges.ConfirmCharge(anonymous(), cmd)
	require.NoError(t, err)

	assert.True(t, second.IdempotentReplay)
	assert.EqualValues(t, 500_000, h.balanceOf("u-1"), "a redelivered event must not pay twice")
	assert.Len(t, h.Store.EventsOfType(app.EventWalletCredited), 1)
}

func TestConfirmChargeProvisionsAMissingWallet(t *testing.T) {
	h := newHarness(t)

	// The user's registration event has not arrived yet, but their money has. It must
	// not be dropped.
	_, err := h.Charges.ConfirmCharge(anonymous(), app.ConfirmChargeCommand{
		UserID: "brand-new", Amount: irr(500_000), PaymentIntentID: "intent-1", EventID: "evt-1",
	})
	require.NoError(t, err)
	assert.EqualValues(t, 500_000, h.balanceOf("brand-new"))
}

func TestConfirmChargeValidation(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)

	_, err := h.Charges.ConfirmCharge(anonymous(), app.ConfirmChargeCommand{
		Amount: irr(500_000), EventID: "evt-1",
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))

	_, err = h.Charges.ConfirmCharge(anonymous(), app.ConfirmChargeCommand{
		UserID: "u-1", Amount: irr(500_000),
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err),
		"without the event id the credit could not be made idempotent")

	_, err = h.Charges.ConfirmCharge(anonymous(), app.ConfirmChargeCommand{
		UserID: "u-1", Amount: irr(0), EventID: "evt-1",
	})
	assert.Equal(t, errs.CodeInvalidArgument, errs.CodeOf(err))
}

func TestConfirmChargeIntoAFrozenWalletIsRefused(t *testing.T) {
	h := newHarness(t)
	h.fund("u-1", 0)

	_, err := h.Admin.FreezeWallet(asSupport("s-1"), "u-1", "investigation")
	require.NoError(t, err)

	_, err = h.Charges.ConfirmCharge(anonymous(), app.ConfirmChargeCommand{
		UserID: "u-1", Amount: irr(500_000), PaymentIntentID: "intent-1", EventID: "evt-1",
	})
	require.Error(t, err)
	assert.Equal(t, wallet.ReasonCodeWalletFrozen, errs.ReasonOf(err))
	assert.False(t, errs.IsRetryable(err),
		"a frozen wallet is a permanent rejection, so the consumer dead-letters it for an operator")
}
