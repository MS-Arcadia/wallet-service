package app

import (
	"context"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/event"
	"github.com/MS-Arcadia/arcadia-platform/pkg/logx"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// Event types published by this service. These strings are a public contract:
// the Store saga, the Catalog service, Auth and Notification all match on them, so
// a rename is a breaking change and needs a new schema version, not an edit.
const (
	EventWalletCreated          = "arcadia.wallet.v1.WalletCreated"
	EventWalletDebited          = "arcadia.wallet.v1.WalletDebited"
	EventWalletCredited         = "arcadia.wallet.v1.WalletCredited"
	EventPaymentFailed          = "arcadia.wallet.v1.PaymentFailed"
	EventFundsTransferred       = "arcadia.wallet.v1.FundsTransferred"
	EventGiftCardIssued         = "arcadia.wallet.v1.GiftCardIssued"
	EventGiftCardRedeemed       = "arcadia.wallet.v1.GiftCardRedeemed"
	EventGiftCardAbuseDetected  = "arcadia.wallet.v1.GiftCardAbuseDetected"
	EventHoldPlaced             = "arcadia.wallet.v1.HoldPlaced"
	EventHoldCaptured           = "arcadia.wallet.v1.HoldCaptured"
	EventHoldReleased           = "arcadia.wallet.v1.HoldReleased"
	EventInterestAccrued        = "arcadia.wallet.v1.InterestAccrued"
	EventWalletFrozen           = "arcadia.wallet.v1.WalletFrozen"
	EventWalletUnfrozen         = "arcadia.wallet.v1.WalletUnfrozen"
	EventDiscountCodeRedeemed   = "arcadia.wallet.v1.DiscountCodeRedeemed"
	EventChargeInitiated        = "arcadia.wallet.v1.ChargeInitiated"
	EventLedgerMismatchDetected = "arcadia.wallet.v1.LedgerMismatchDetected"
	// EventAuditRecorded mirrors every money movement onto the audit topic, which
	// feeds the immutable WORM sink the architecture document requires for
	// non-repudiation.
	EventAuditRecorded = "arcadia.wallet.v1.AuditRecorded"
)

// Aggregate type names used in envelopes.
const (
	aggregateWallet   = "wallet"
	aggregateGiftCard = "gift_card"
	aggregateHold     = "hold"
	aggregateDiscount = "discount_code"
)

// --- Event payloads -------------------------------------------------------

// WalletCreatedPayload announces a new wallet.
type WalletCreatedPayload struct {
	WalletID string `json:"wallet_id"`
	UserID   string `json:"user_id"`
	Currency string `json:"currency"`
}

// MovementPayload announces a balance change. It is the shape the Store saga
// consumes to advance a purchase.
type MovementPayload struct {
	WalletID     string      `json:"wallet_id"`
	UserID       string      `json:"user_id"`
	EntryID      string      `json:"entry_id"`
	Amount       money.Money `json:"amount"`
	BalanceAfter money.Money `json:"balance_after"`
	Reason       string      `json:"reason"`
	// ReferenceID is the order, trade or intent this movement belongs to. The saga
	// correlates on it.
	ReferenceID string `json:"reference_id,omitempty"`
	Description string `json:"description,omitempty"`
	// IdempotencyKey lets a consumer recognise a movement it already saw.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// PaymentFailedPayload announces that a debit could not be satisfied.
//
// This is the reply that makes the purchase saga terminate cleanly: no ownership
// was granted and no compensation is needed, so the Store service can mark the
// order FAILED and tell the buyer why.
type PaymentFailedPayload struct {
	UserID      string      `json:"user_id"`
	WalletID    string      `json:"wallet_id,omitempty"`
	ReferenceID string      `json:"reference_id"`
	Requested   money.Money `json:"requested"`
	Available   money.Money `json:"available"`
	// Reason is a stable code such as INSUFFICIENT_FUNDS or WALLET_FROZEN.
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// TransferPayload announces a two-sided settlement.
type TransferPayload struct {
	FromUserID    string      `json:"from_user_id"`
	ToUserID      string      `json:"to_user_id"`
	Amount        money.Money `json:"amount"`
	Reason        string      `json:"reason"`
	ReferenceID   string      `json:"reference_id"`
	DebitEntryID  string      `json:"debit_entry_id"`
	CreditEntryID string      `json:"credit_entry_id"`
}

// GiftCardIssuedPayload announces a minted batch. It deliberately carries no code.
type GiftCardIssuedPayload struct {
	BatchID  string      `json:"batch_id"`
	Quantity int         `json:"quantity"`
	Value    money.Money `json:"value"`
	IssuedBy string      `json:"issued_by"`
}

// GiftCardRedeemedPayload announces a redemption.
type GiftCardRedeemedPayload struct {
	GiftCardID string      `json:"gift_card_id"`
	UserID     string      `json:"user_id"`
	WalletID   string      `json:"wallet_id"`
	Value      money.Money `json:"value"`
	CodeHint   string      `json:"code_hint"`
}

// GiftCardAbuseDetectedPayload flags a user for Support review.
//
// The wallet service never bans anybody. It reports a pattern; Auth queues the
// user and a human decides, which is what the requirements ask for.
type GiftCardAbuseDetectedPayload struct {
	UserID         string `json:"user_id"`
	FailedAttempts int64  `json:"failed_attempts"`
	WindowRule     string `json:"window_rule"`
	DetectedAt     string `json:"detected_at"`
	// RecommendedAction is advisory only.
	RecommendedAction string `json:"recommended_action"`
}

// HoldPayload announces a hold transition.
type HoldPayload struct {
	HoldID      string      `json:"hold_id"`
	WalletID    string      `json:"wallet_id"`
	UserID      string      `json:"user_id"`
	Amount      money.Money `json:"amount"`
	ReferenceID string      `json:"reference_id"`
	Status      string      `json:"status"`
}

// InterestAccruedPayload announces credited interest.
type InterestAccruedPayload struct {
	WalletID      string      `json:"wallet_id"`
	UserID        string      `json:"user_id"`
	Amount        money.Money `json:"amount"`
	AnnualRateBps int64       `json:"annual_rate_bps"`
	AccrualDate   string      `json:"accrual_date"`
	BalanceBefore money.Money `json:"balance_before"`
}

// WalletStatusPayload announces a freeze or unfreeze.
type WalletStatusPayload struct {
	WalletID string `json:"wallet_id"`
	UserID   string `json:"user_id"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	ActorID  string `json:"actor_id"`
}

// DiscountRedeemedPayload announces a consumed discount code.
type DiscountRedeemedPayload struct {
	CodeID      string      `json:"code_id"`
	Code        string      `json:"code"`
	UserID      string      `json:"user_id"`
	OrderAmount money.Money `json:"order_amount"`
	Discount    money.Money `json:"discount"`
	ReferenceID string      `json:"reference_id"`
}

// ChargeInitiatedPayload announces a started bank top-up. The balance has not
// changed yet; it changes when BankPaymentConfirmed arrives.
type ChargeInitiatedPayload struct {
	UserID          string      `json:"user_id"`
	WalletID        string      `json:"wallet_id"`
	PaymentIntentID string      `json:"payment_intent_id"`
	Amount          money.Money `json:"amount"`
}

// LedgerMismatchPayload announces a reconciliation failure. This is a P1 signal.
type LedgerMismatchPayload struct {
	WalletID      string      `json:"wallet_id"`
	UserID        string      `json:"user_id"`
	StoredBalance money.Money `json:"stored_balance"`
	LedgerBalance money.Money `json:"ledger_balance"`
	Delta         money.Money `json:"delta"`
	DetectedAt    string      `json:"detected_at"`
}

// AuditPayload is the immutable audit record for one money movement.
type AuditPayload struct {
	EntryID        string      `json:"entry_id"`
	Sequence       int64       `json:"sequence"`
	WalletID       string      `json:"wallet_id"`
	UserID         string      `json:"user_id"`
	Direction      string      `json:"direction"`
	Amount         money.Money `json:"amount"`
	BalanceAfter   money.Money `json:"balance_after"`
	Reason         string      `json:"reason"`
	ReferenceID    string      `json:"reference_id,omitempty"`
	IdempotencyKey string      `json:"idempotency_key,omitempty"`
	CorrelationID  string      `json:"correlation_id,omitempty"`
	// ActorID is who caused the movement: the wallet owner, a Support user, or a
	// service acting inside a saga.
	ActorID    string `json:"actor_id,omitempty"`
	OccurredAt string `json:"occurred_at"`
}

// --- Emitter --------------------------------------------------------------

// emitter builds and queues envelopes. It is embedded in every use-case service so
// that publishing is a one-liner at the call site.
//
// Note what is absent: any notion of a Kafka topic. The use cases name the event;
// the outbox adapter decides which topic carries it. Moving a rename or a
// re-routing decision into infrastructure keeps the application layer free of
// broker vocabulary.
type emitter struct {
	publisher port.EventPublisher
	builder   *event.Builder
	ids       idGenerator
}

// idGenerator is the narrow slice of platform's idgen.Generator that this layer
// needs.
type idGenerator interface {
	NewID() string
}

func newEmitter(publisher port.EventPublisher, producer string, schemaVersion int, ids idGenerator) *emitter {
	return &emitter{
		publisher: publisher,
		builder:   event.NewBuilder(producer, schemaVersion),
		ids:       ids,
	}
}

// emit queues one domain event on the events topic.
func (e *emitter) emit(ctx context.Context, tx port.Tx, eventType, aggregateType, aggregateID string, at time.Time, payload any) error {
	envelope, err := e.envelope(ctx, eventType, aggregateType, aggregateID, at, payload)
	if err != nil {
		return err
	}
	if err := e.publisher.Publish(ctx, tx, envelope); err != nil {
		return errs.Wrap(err, "failed to queue %s", eventType)
	}
	return nil
}

// emitAudit queues an audit record for a ledger entry on the audit topic.
func (e *emitter) emitAudit(ctx context.Context, tx port.Tx, entry ledger.Entry, actorID string) error {
	payload := AuditPayload{
		EntryID:        entry.ID,
		Sequence:       entry.Sequence,
		WalletID:       entry.WalletID,
		UserID:         entry.UserID,
		Direction:      entry.Direction.String(),
		Amount:         entry.Amount,
		BalanceAfter:   entry.BalanceAfter,
		Reason:         entry.Reason.String(),
		ReferenceID:    entry.ReferenceID,
		IdempotencyKey: entry.IdempotencyKey,
		CorrelationID:  entry.CorrelationID,
		ActorID:        actorID,
		OccurredAt:     entry.CreatedAt.UTC().Format(time.RFC3339Nano),
	}

	envelope, err := e.envelope(ctx, EventAuditRecorded, aggregateWallet, entry.WalletID, entry.CreatedAt, payload)
	if err != nil {
		return err
	}
	if err := e.publisher.Publish(ctx, tx, envelope); err != nil {
		return errs.Wrap(err, "failed to queue the audit record for entry %s", entry.ID)
	}
	return nil
}

// envelope builds an envelope, stamping the correlation and trace identifiers from
// the request context so that a consumer can join its logs to ours.
func (e *emitter) envelope(ctx context.Context, eventType, aggregateType, aggregateID string, at time.Time, payload any) (event.Envelope, error) {
	envelope, err := e.builder.Build(e.ids.NewID(), eventType, aggregateType, aggregateID, at, payload)
	if err != nil {
		return event.Envelope{}, errs.Wrap(err, "failed to build %s", eventType)
	}
	envelope.CorrelationID = logx.CorrelationID(ctx)
	envelope.TraceID = logx.TraceID(ctx)
	return envelope, nil
}

// movementPayload builds the standard balance-change payload from a ledger entry.
func movementPayload(entry ledger.Entry) MovementPayload {
	return MovementPayload{
		WalletID:       entry.WalletID,
		UserID:         entry.UserID,
		EntryID:        entry.ID,
		Amount:         entry.Amount,
		BalanceAfter:   entry.BalanceAfter,
		Reason:         entry.Reason.String(),
		ReferenceID:    entry.ReferenceID,
		Description:    entry.Description,
		IdempotencyKey: entry.IdempotencyKey,
	}
}

// movementEventType maps a direction onto the event that announces it.
func movementEventType(direction wallet.Direction) string {
	if direction == wallet.DirectionDebit {
		return EventWalletDebited
	}
	return EventWalletCredited
}
