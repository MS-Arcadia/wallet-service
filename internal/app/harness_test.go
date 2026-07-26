package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/authn"
	"github.com/MS-Arcadia/arcadia-platform/pkg/clock"
	"github.com/MS-Arcadia/arcadia-platform/pkg/idgen"
	"github.com/MS-Arcadia/arcadia-platform/pkg/logx"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/app/apptest"
	"github.com/MS-Arcadia/wallet-service/internal/domain/abuse"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/interest"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/stretchr/testify/require"
)

const (
	currency   = "IRR"
	testPepper = "a-gift-card-pepper-that-is-long-enough-for-hmac"
)

var testNow = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func irr(minor int64) money.Money { return money.MustNew(minor, currency) }

// harness wires the use cases up against in-memory fakes. Every test starts from
// one of these, so a change to the dependency set is a one-line edit here rather
// than a sweep through fifty tests.
type harness struct {
	t *testing.T

	Store   *apptest.Store
	Clock   *clock.Fixed
	Gateway *apptest.PaymentGateway
	Limiter *apptest.AbuseLimiter
	Metrics *apptest.Metrics
	Hasher  *giftcard.Hasher

	Wallets   *app.WalletService
	GiftCards *app.GiftCardService
	Discounts *app.DiscountService
	Charges   *app.ChargeService
	Admin     *app.AdminService
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	store := apptest.NewStore()
	fixedClock := clock.NewFixed(testNow)
	gateway := apptest.NewPaymentGateway()
	limiter := apptest.NewAbuseLimiter()
	metrics := apptest.NewMetrics()

	hasher, err := giftcard.NewHasher(testPepper)
	require.NoError(t, err)

	deps := app.Deps{
		TxManager:     apptest.NewTxManager(store),
		Reader:        nil, // the fakes read straight from the in-memory store
		Wallets:       apptest.NewWalletRepo(store),
		Ledger:        apptest.NewLedgerRepo(store),
		GiftCards:     apptest.NewGiftCardRepo(store),
		Discounts:     apptest.NewDiscountRepo(store),
		Holds:         apptest.NewHoldRepo(store),
		Idempotency:   apptest.NewIdempotencyStore(store),
		Publisher:     apptest.NewPublisher(store),
		PaymentGW:     gateway,
		AbuseLimiter:  limiter,
		Metrics:       metrics,
		Clock:         fixedClock,
		IDs:           &idgen.Sequence{Prefix: "id"},
		Logger:        logx.NewNop(),
		Currency:      currency,
		Producer:      "wallet-service",
		SchemaVersion: 1,
	}

	interestPolicy, err := interest.NewPolicy(interest.Config{
		AnnualRateBps:  500, // 5% a year
		MinimumBalance: irr(100_000),
		Enabled:        true,
	})
	require.NoError(t, err)

	return &harness{
		t:         t,
		Store:     store,
		Clock:     fixedClock,
		Gateway:   gateway,
		Limiter:   limiter,
		Metrics:   metrics,
		Hasher:    hasher,
		Wallets:   app.NewWalletService(deps),
		GiftCards: app.NewGiftCardService(deps, hasher, abuse.DefaultPolicy()),
		Discounts: app.NewDiscountService(deps),
		Charges:   app.NewChargeService(deps),
		Admin:     app.NewAdminService(deps, interestPolicy),
	}
}

// --- Context builders -----------------------------------------------------

// quietContext carries a discarding logger, so that a passing test run stays
// readable instead of streaming every use case's log lines to stderr.
func quietContext() context.Context {
	return logx.WithLogger(context.Background(), logx.NewNop())
}

func asUser(userID string) context.Context {
	return authn.WithPrincipal(quietContext(), authn.Principal{
		UserID: userID, Role: authn.RoleBasicUser,
	})
}

func asSupport(userID string) context.Context {
	return authn.WithPrincipal(quietContext(), authn.Principal{
		UserID: userID, Role: authn.RoleSupport,
	})
}

func asAdmin(userID string) context.Context {
	return authn.WithPrincipal(quietContext(), authn.Principal{
		UserID: userID, Role: authn.RoleAdmin,
	})
}

func asStoreService() context.Context {
	return authn.WithPrincipal(quietContext(), authn.Principal{
		UserID: "store-service", Role: authn.RoleService,
	})
}

func anonymous() context.Context { return quietContext() }

// --- Fixtures -------------------------------------------------------------

// fund provisions a wallet for userID and credits it with balanceMinor.
func (h *harness) fund(userID string, balanceMinor int64) app.WalletView {
	h.t.Helper()

	view, err := h.Wallets.GetOrCreateWallet(asUser(userID), userID)
	require.NoError(h.t, err)

	if balanceMinor > 0 {
		result, err := h.Wallets.Credit(asStoreService(), app.CreditCommand{
			UserID:         userID,
			Amount:         irr(balanceMinor),
			Reason:         wallet.ReasonCharge,
			ReferenceID:    "fixture",
			IdempotencyKey: "fixture-credit-" + userID,
		})
		require.NoError(h.t, err)
		view = result.Wallet
	}
	return view
}

// balanceOf reads a user's current balance in minor units.
func (h *harness) balanceOf(userID string) int64 {
	h.t.Helper()
	w, ok := h.Store.WalletOf(userID)
	require.True(h.t, ok, "no wallet stored for %s", userID)
	return w.Balance().Minor()
}

// availableOf reads a user's spendable balance in minor units.
func (h *harness) availableOf(userID string) int64 {
	h.t.Helper()
	w, ok := h.Store.WalletOf(userID)
	require.True(h.t, ok, "no wallet stored for %s", userID)
	return w.Available().Minor()
}

// issueCard mints one gift card and returns its plaintext code.
func (h *harness) issueCard(valueMinor int64) (string, string) {
	h.t.Helper()

	result, err := h.GiftCards.IssueGiftCards(asSupport("support-1"), app.IssueGiftCardsCommand{
		Value:          irr(valueMinor),
		Quantity:       1,
		Note:           "test batch",
		IdempotencyKey: "issue-" + idgen.NormalizeCode(time.Now().Format("150405.000000000")),
	})
	require.NoError(h.t, err)
	require.Len(h.t, result.GiftCards, 1)
	require.NotEmpty(h.t, result.GiftCards[0].Code)
	return result.GiftCards[0].Code, result.GiftCards[0].ID
}
