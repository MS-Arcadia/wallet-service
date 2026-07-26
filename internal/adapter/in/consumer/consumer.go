// Package consumer is the Kafka inbound adapter.
//
// It is the asynchronous half of the service's API, and the half the architecture
// document treats as the default: the Store service drives a purchase saga by
// publishing commands here, and the Payment Adapter reports a settled bank payment
// the same way.
//
// Two properties matter throughout. Every handler is idempotent, because Kafka
// delivers at least once and this service moves money. And every handler classifies
// its failures: a business rejection is permanent and goes to the dead-letter topic
// for an operator, while an infrastructure failure is retried.
package consumer

import (
	"context"
	"log/slog"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/authn"
	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/event"
	"github.com/MS-Arcadia/arcadia-platform/pkg/kafkax"
	"github.com/MS-Arcadia/arcadia-platform/pkg/logx"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// Inbound event and command types this service handles.
const (
	// From the Payment Adapter, on payment-events.
	EventBankPaymentConfirmed = "arcadia.payment.v1.BankPaymentConfirmed"
	EventBankPaymentFailed    = "arcadia.payment.v1.BankPaymentFailed"

	// From Auth, on user-events.
	EventUserRegistered = "arcadia.auth.v1.UserRegistered"

	// Commands from the Store saga orchestrator, on wallet-commands.
	CommandDebitWallet  = "arcadia.wallet.v1.DebitWalletCommand"
	CommandCreditWallet = "arcadia.wallet.v1.CreditWalletCommand"
	CommandRefundOrder  = "arcadia.wallet.v1.RefundOrderCommand"
	CommandReverseSplit = "arcadia.wallet.v1.ReverseSplitCommand"
	CommandHoldFunds    = "arcadia.wallet.v1.HoldFundsCommand"
	CommandCaptureHold  = "arcadia.wallet.v1.CaptureHoldCommand"
	CommandReleaseHold  = "arcadia.wallet.v1.ReleaseHoldCommand"

	// From the Marketplace matching engine, on trade-events.
	CommandSettleTrade = "arcadia.marketplace.v1.SettleTradeCommand"
)

// Handlers wires inbound messages to use cases.
type Handlers struct {
	wallets *app.WalletService
	charges *app.ChargeService
	logger  *slog.Logger
	// currency is used when a producer omits it, which internal services routinely do.
	currency string
}

// NewHandlers builds the handler set.
func NewHandlers(wallets *app.WalletService, charges *app.ChargeService, currency string, logger *slog.Logger) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{
		wallets:  wallets,
		charges:  charges,
		currency: currency,
		logger:   logger.With(slog.String("component", "kafka-handlers")),
	}
}

// --- Inbound payloads -----------------------------------------------------
//
// These mirror what the producing services publish. They are deliberately tolerant:
// an unknown extra field is ignored rather than rejected, so that another team adding
// a field to their event does not break this consumer.

// BankPaymentPayload is what the Payment Adapter publishes when a bank settles.
type BankPaymentPayload struct {
	IntentID      string      `json:"intent_id"`
	UserID        string      `json:"user_id"`
	Amount        money.Money `json:"amount"`
	BankReference string      `json:"bank_reference,omitempty"`
	FailureCode   string      `json:"failure_code,omitempty"`
	FailureReason string      `json:"failure_reason,omitempty"`
}

// UserRegisteredPayload is what Auth publishes for a new account.
type UserRegisteredPayload struct {
	UserID string `json:"user_id"`
	Email  string `json:"email,omitempty"`
	Role   string `json:"role,omitempty"`
}

// MovementCommandPayload is a debit or credit instruction from the Store saga.
type MovementCommandPayload struct {
	UserID      string      `json:"user_id"`
	Amount      money.Money `json:"amount"`
	Reason      string      `json:"reason"`
	ReferenceID string      `json:"reference_id"`
	Description string      `json:"description,omitempty"`
	// IdempotencyKey is optional. When absent the event id is used, which is the
	// stronger default: it is unique per delivery attempt group by construction.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// SettleTradePayload is the marketplace's instruction to move money between two
// users after a match.
type SettleTradePayload struct {
	TradeID     string      `json:"trade_id"`
	BuyerID     string      `json:"buyer_id"`
	SellerID    string      `json:"seller_id"`
	Amount      money.Money `json:"amount"`
	Description string      `json:"description,omitempty"`
}

// HoldCommandPayload places or resolves a reservation.
type HoldCommandPayload struct {
	UserID      string      `json:"user_id,omitempty"`
	HoldID      string      `json:"hold_id,omitempty"`
	Amount      money.Money `json:"amount"`
	ReferenceID string      `json:"reference_id,omitempty"`
	Reason      string      `json:"reason,omitempty"`
	TTLSeconds  int64       `json:"ttl_seconds,omitempty"`
}

// --- Routers --------------------------------------------------------------

// PaymentEventsRouter handles the payment-events topic.
func (h *Handlers) PaymentEventsRouter() *kafkax.Router {
	router := kafkax.NewRouter(h.logger, kafkax.IgnoreUnknown)
	router.OnFunc(EventBankPaymentConfirmed, h.handleBankPaymentConfirmed)
	router.OnFunc(EventBankPaymentFailed, h.handleBankPaymentFailed)
	return router
}

// UserEventsRouter handles the user-events topic.
func (h *Handlers) UserEventsRouter() *kafkax.Router {
	router := kafkax.NewRouter(h.logger, kafkax.IgnoreUnknown)
	router.OnFunc(EventUserRegistered, h.handleUserRegistered)
	return router
}

// WalletCommandsRouter handles the wallet-commands topic.
//
// Unknown messages here are dead-lettered rather than ignored: this is a dedicated
// command topic, so a message nobody handles means the Store service is issuing a
// command this version does not implement — a contract violation worth an operator's
// attention, not something to swallow.
func (h *Handlers) WalletCommandsRouter() *kafkax.Router {
	router := kafkax.NewRouter(h.logger, kafkax.DeadLetterUnknown)
	router.OnFunc(CommandDebitWallet, h.handleDebitCommand)
	router.OnFunc(CommandCreditWallet, h.handleCreditCommand)
	router.OnFunc(CommandRefundOrder, h.handleRefundCommand)
	router.OnFunc(CommandReverseSplit, h.handleReverseSplitCommand)
	router.OnFunc(CommandHoldFunds, h.handleHoldFundsCommand)
	router.OnFunc(CommandCaptureHold, h.handleCaptureHoldCommand)
	router.OnFunc(CommandReleaseHold, h.handleReleaseHoldCommand)
	return router
}

// TradeEventsRouter handles the trade-events topic.
func (h *Handlers) TradeEventsRouter() *kafkax.Router {
	router := kafkax.NewRouter(h.logger, kafkax.IgnoreUnknown)
	router.OnFunc(CommandSettleTrade, h.handleSettleTrade)
	return router
}

// --- Handlers -------------------------------------------------------------

func (h *Handlers) handleBankPaymentConfirmed(ctx context.Context, envelope event.Envelope) error {
	var payload BankPaymentPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		// A payload we cannot parse will never parse. Non-retryable, so the consumer
		// dead-letters it immediately instead of burning retries.
		return errs.InvalidArgument("malformed BankPaymentConfirmed payload: %s", err.Error()).WithCause(err)
	}

	amount, err := h.amountOf(payload.Amount)
	if err != nil {
		return err
	}

	logx.FromContext(ctx).Info("crediting a confirmed bank payment",
		slog.String("user_id", payload.UserID),
		slog.String("intent_id", payload.IntentID),
		slog.String("amount", amount.String()),
	)

	// The event id is the idempotency key: a redelivered confirmation credits nothing
	// extra.
	_, err = h.charges.ConfirmCharge(ctx, app.ConfirmChargeCommand{
		UserID:          payload.UserID,
		Amount:          amount,
		PaymentIntentID: payload.IntentID,
		BankReference:   payload.BankReference,
		EventID:         envelope.EventID,
	})
	return err
}

func (h *Handlers) handleBankPaymentFailed(ctx context.Context, envelope event.Envelope) error {
	var payload BankPaymentPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		return errs.InvalidArgument("malformed BankPaymentFailed payload: %s", err.Error()).WithCause(err)
	}

	// Nothing to undo: the balance was never touched, because a top-up only credits on
	// confirmation. The event is recorded so that a support agent can explain to the
	// user why their money did not arrive.
	logx.FromContext(ctx).Info("a bank payment failed; no balance change was needed",
		slog.String("user_id", payload.UserID),
		slog.String("intent_id", payload.IntentID),
		slog.String("failure_code", payload.FailureCode),
		slog.String("failure_reason", payload.FailureReason),
	)
	return nil
}

func (h *Handlers) handleUserRegistered(ctx context.Context, envelope event.Envelope) error {
	var payload UserRegisteredPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		return errs.InvalidArgument("malformed UserRegistered payload: %s", err.Error()).WithCause(err)
	}
	if payload.UserID == "" {
		return errs.InvalidArgument("UserRegistered carried no user id")
	}

	// Eager provisioning. A user opening their wallet page before this event arrives
	// gets one created lazily instead, and both paths converge on the same unique
	// constraint, so a duplicate is harmless.
	if _, err := h.wallets.EnsureWallet(ctx, payload.UserID); err != nil {
		return err
	}
	logx.FromContext(ctx).Debug("provisioned a wallet for a new user",
		slog.String("user_id", payload.UserID))
	return nil
}

func (h *Handlers) handleDebitCommand(ctx context.Context, envelope event.Envelope) error {
	payload, amount, reason, err := h.decodeMovement(envelope, wallet.ReasonPurchase)
	if err != nil {
		return err
	}

	_, err = h.wallets.DebitInternal(ctx, app.DebitCommand{
		UserID:         payload.UserID,
		Amount:         amount,
		Reason:         reason,
		ReferenceID:    payload.ReferenceID,
		Description:    payload.Description,
		IdempotencyKey: idempotencyKeyFor(payload.IdempotencyKey, envelope),
	})
	// Insufficient funds is not an error to retry or to dead-letter: it is the saga's
	// expected negative outcome, and the use case has already published PaymentFailed
	// so the orchestrator can proceed. Swallowing it here keeps the DLQ meaningful.
	if err != nil && errs.ReasonOf(err) == wallet.ReasonCodeInsufficientFunds {
		logx.FromContext(ctx).Info("a saga debit was declined for insufficient funds",
			slog.String("user_id", payload.UserID),
			slog.String("reference_id", payload.ReferenceID),
		)
		return nil
	}
	return err
}

func (h *Handlers) handleCreditCommand(ctx context.Context, envelope event.Envelope) error {
	payload, amount, reason, err := h.decodeMovement(envelope, wallet.ReasonRevenue)
	if err != nil {
		return err
	}

	_, err = h.wallets.CreditInternal(ctx, app.CreditCommand{
		UserID:         payload.UserID,
		Amount:         amount,
		Reason:         reason,
		ReferenceID:    payload.ReferenceID,
		Description:    payload.Description,
		IdempotencyKey: idempotencyKeyFor(payload.IdempotencyKey, envelope),
	})
	return err
}

// handleRefundCommand returns money to a buyer.
//
// This is the compensating step of the purchase saga, and the twelve-hour refund
// window from the requirements. The window itself is the Store service's rule — it
// owns the order and its timestamp; the wallet's job is to move the money reliably
// when told to.
func (h *Handlers) handleRefundCommand(ctx context.Context, envelope event.Envelope) error {
	payload, amount, _, err := h.decodeMovement(envelope, wallet.ReasonRefund)
	if err != nil {
		return err
	}

	_, err = h.wallets.CreditInternal(ctx, app.CreditCommand{
		UserID:         payload.UserID,
		Amount:         amount,
		Reason:         wallet.ReasonRefund,
		ReferenceID:    payload.ReferenceID,
		Description:    payload.Description,
		IdempotencyKey: idempotencyKeyFor(payload.IdempotencyKey, envelope),
	})
	return err
}

// handleReverseSplitCommand claws a revenue share back from a developer or the
// platform after a refund.
func (h *Handlers) handleReverseSplitCommand(ctx context.Context, envelope event.Envelope) error {
	payload, amount, _, err := h.decodeMovement(envelope, wallet.ReasonReversal)
	if err != nil {
		return err
	}

	_, err = h.wallets.DebitInternal(ctx, app.DebitCommand{
		UserID:         payload.UserID,
		Amount:         amount,
		Reason:         wallet.ReasonReversal,
		ReferenceID:    payload.ReferenceID,
		Description:    payload.Description,
		IdempotencyKey: idempotencyKeyFor(payload.IdempotencyKey, envelope),
	})
	// A developer who has already spent their share cannot be debited. That is a real
	// business situation, not a bug: it is recorded and escalated rather than retried
	// forever, and the architecture document's note about holding the shortfall as a
	// debt applies.
	if err != nil && errs.ReasonOf(err) == wallet.ReasonCodeInsufficientFunds {
		logx.FromContext(ctx).Warn("could not reverse a revenue share; the recipient has already spent it",
			slog.String("user_id", payload.UserID),
			slog.String("reference_id", payload.ReferenceID),
			slog.String("amount", amount.String()),
		)
		// Returned as a permanent failure so the message lands in the DLQ and a human
		// decides how to recover the shortfall.
		return errs.FailedPrecondition(
			"cannot reverse %s from user %s: the funds have already been spent", amount, payload.UserID).
			WithReason("REVERSAL_SHORTFALL").
			WithDetail("reference_id", payload.ReferenceID)
	}
	return err
}

func (h *Handlers) handleSettleTrade(ctx context.Context, envelope event.Envelope) error {
	var payload SettleTradePayload
	if err := envelope.DecodePayload(&payload); err != nil {
		return errs.InvalidArgument("malformed SettleTradeCommand payload: %s", err.Error()).WithCause(err)
	}
	switch {
	case payload.BuyerID == "" || payload.SellerID == "":
		return errs.InvalidArgument("SettleTradeCommand needs both a buyer and a seller")
	case payload.TradeID == "":
		return errs.InvalidArgument("SettleTradeCommand needs a trade id")
	}

	amount, err := h.amountOf(payload.Amount)
	if err != nil {
		return err
	}

	description := payload.Description
	if description == "" {
		description = "marketplace trade " + payload.TradeID
	}

	// One transaction, both sides. The matching engine must never end up with a buyer
	// who paid and a seller who was not paid.
	_, err = h.wallets.TransferInternal(ctx, app.TransferCommand{
		FromUserID:     payload.BuyerID,
		ToUserID:       payload.SellerID,
		Amount:         amount,
		Reason:         wallet.ReasonTrade,
		ReferenceID:    payload.TradeID,
		Description:    description,
		IdempotencyKey: envelope.EventID,
	})
	return err
}

func (h *Handlers) handleHoldFundsCommand(ctx context.Context, envelope event.Envelope) error {
	var payload HoldCommandPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		return errs.InvalidArgument("malformed HoldFundsCommand payload: %s", err.Error()).WithCause(err)
	}
	amount, err := h.amountOf(payload.Amount)
	if err != nil {
		return err
	}

	_, err = h.wallets.HoldFunds(asService(ctx), app.HoldFundsCommand{
		UserID:         payload.UserID,
		Amount:         amount,
		ReferenceID:    payload.ReferenceID,
		Reason:         payload.Reason,
		TTL:            time.Duration(payload.TTLSeconds) * time.Second,
		IdempotencyKey: envelope.EventID,
	})
	return err
}

func (h *Handlers) handleCaptureHoldCommand(ctx context.Context, envelope event.Envelope) error {
	var payload HoldCommandPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		return errs.InvalidArgument("malformed CaptureHoldCommand payload: %s", err.Error()).WithCause(err)
	}
	if payload.HoldID == "" {
		return errs.InvalidArgument("CaptureHoldCommand needs a hold id")
	}

	// A zero amount captures everything remaining, which is the common case.
	amount := money.Money{}
	if payload.Amount.Minor() != 0 {
		parsed, err := h.amountOf(payload.Amount)
		if err != nil {
			return err
		}
		amount = parsed
	}

	_, err := h.wallets.CaptureHold(asService(ctx), app.CaptureHoldCommand{
		HoldID:         payload.HoldID,
		Amount:         amount,
		IdempotencyKey: envelope.EventID,
	})
	return err
}

func (h *Handlers) handleReleaseHoldCommand(ctx context.Context, envelope event.Envelope) error {
	var payload HoldCommandPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		return errs.InvalidArgument("malformed ReleaseHoldCommand payload: %s", err.Error()).WithCause(err)
	}
	if payload.HoldID == "" {
		return errs.InvalidArgument("ReleaseHoldCommand needs a hold id")
	}

	_, err := h.wallets.ReleaseHold(asService(ctx), app.ReleaseHoldCommand{
		HoldID:         payload.HoldID,
		IdempotencyKey: envelope.EventID,
	})
	return err
}

// --- Helpers --------------------------------------------------------------

// decodeMovement parses a movement command and resolves its amount and reason.
func (h *Handlers) decodeMovement(envelope event.Envelope, defaultReason wallet.Reason) (MovementCommandPayload, money.Money, wallet.Reason, error) {
	var payload MovementCommandPayload
	if err := envelope.DecodePayload(&payload); err != nil {
		return payload, money.Money{}, "", errs.InvalidArgument(
			"malformed %s payload: %s", envelope.EventType, err.Error()).WithCause(err)
	}
	if payload.UserID == "" {
		return payload, money.Money{}, "", errs.InvalidArgument("%s carried no user id", envelope.EventType)
	}

	amount, err := h.amountOf(payload.Amount)
	if err != nil {
		return payload, money.Money{}, "", err
	}

	reason := defaultReason
	if payload.Reason != "" {
		candidate := wallet.Reason(payload.Reason)
		if !candidate.Valid() {
			return payload, money.Money{}, "", errs.InvalidArgument(
				"%s carried an unknown reason %q", envelope.EventType, payload.Reason)
		}
		reason = candidate
	}
	return payload, amount, reason, nil
}

// amountOf normalises an inbound amount, filling in the platform currency when the
// producer omitted it.
func (h *Handlers) amountOf(amount money.Money) (money.Money, error) {
	if amount.Currency() == "" {
		normalised, err := money.New(amount.Minor(), h.currency)
		if err != nil {
			return money.Money{}, errs.Internal("the configured currency %q is invalid", h.currency).WithCause(err)
		}
		return normalised, nil
	}
	if amount.Currency() != h.currency {
		return money.Money{}, errs.InvalidArgument(
			"this service holds %s wallets; the message carried %s", h.currency, amount.Currency())
	}
	return amount, nil
}

// idempotencyKeyFor prefers an explicit key and falls back to the event id.
func idempotencyKeyFor(explicit string, envelope event.Envelope) string {
	if explicit != "" {
		return explicit
	}
	return envelope.EventID
}

// asService attaches a service principal to work that arrived over the broker.
//
// An inbound command has no end user in its context, and the use cases' RBAC helpers
// would refuse a call with no principal. Marking the caller as the platform itself is
// accurate: the Store saga really is acting on the user's behalf.
func asService(ctx context.Context) context.Context {
	if _, ok := authn.PrincipalFrom(ctx); ok {
		return ctx
	}
	return authn.WithPrincipal(ctx, authn.Principal{
		UserID: "wallet-service",
		Role:   authn.RoleService,
	})
}
