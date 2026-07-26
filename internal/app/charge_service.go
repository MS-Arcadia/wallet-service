package app

import (
	"context"
	"log/slog"

	"github.com/MS-Arcadia/arcadia-platform/pkg/authn"
	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/logx"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// ChargeService implements topping a wallet up from the bank.
//
// The flow is deliberately split in two. InitiateCharge only asks the Payment
// Adapter for a redirect URL — it does not touch the balance, because at that point
// no money has moved. The balance changes when BankPaymentConfirmed arrives over
// Kafka and ConfirmCharge runs. Crediting optimistically at initiation would credit
// users who never complete the payment.
type ChargeService struct {
	*core
}

// NewChargeService builds the service.
func NewChargeService(deps Deps) *ChargeService {
	return &ChargeService{core: newCore(deps)}
}

// InitiateChargeCommand starts a bank top-up.
type InitiateChargeCommand struct {
	UserID         string
	Amount         money.Money
	ReturnURL      string
	IdempotencyKey string
}

// ConfirmChargeCommand credits a confirmed bank payment. It originates from the
// Payment Adapter's BankPaymentConfirmed event, never from a user request.
type ConfirmChargeCommand struct {
	UserID          string
	Amount          money.Money
	PaymentIntentID string
	BankReference   string
	// EventID is the identifier of the event being processed. It doubles as the
	// idempotency key, so a redelivered event credits nothing extra.
	EventID string
}

// minimumCharge stops a top-up so small that the payment provider's own fee would
// exceed it.
const minimumChargeMinor = 1_000

// InitiateCharge asks the Payment Adapter for a bank redirect URL.
func (s *ChargeService) InitiateCharge(ctx context.Context, cmd InitiateChargeCommand) (ChargeResult, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return ChargeResult{}, err
	}
	// A user tops up their own wallet. Staff and services must not be able to make
	// somebody else start a bank payment.
	if cmd.UserID == "" {
		cmd.UserID = principal.UserID
	}
	if cmd.UserID != principal.UserID {
		return ChargeResult{}, errs.PermissionDenied("you may only top up your own wallet")
	}
	switch {
	case cmd.IdempotencyKey == "":
		return ChargeResult{}, errs.InvalidArgument("an idempotency key is required to start a top-up")
	case !cmd.Amount.IsPositive():
		return ChargeResult{}, wallet.ErrAmountNotPositive(cmd.Amount)
	case cmd.Amount.Currency() != s.deps.Currency:
		return ChargeResult{}, errs.InvalidArgument(
			"wallets are held in %s, not %s", s.deps.Currency, cmd.Amount.Currency())
	case cmd.Amount.Minor() < minimumChargeMinor:
		return ChargeResult{}, errs.InvalidArgument(
			"the minimum top-up is %s", money.MustNew(minimumChargeMinor, s.deps.Currency)).
			WithReason("AMOUNT_BELOW_MINIMUM")
	}

	// The wallet must exist before a payment is started, so that the confirmation
	// consumer has somewhere to put the money.
	w, err := s.walletFor(ctx, cmd.UserID)
	if err != nil {
		return ChargeResult{}, err
	}

	var (
		result ChargeResult
		replay bool
	)

	// The idempotency claim is committed before the gateway is called. If the call
	// then fails, the key is left claimed with no stored response, and the retry gets
	// ABORTED with "still in progress" rather than silently creating a second payment
	// intent at the bank.
	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opInitiateCharge, cmd.IdempotencyKey, cmd.UserID, cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}
		return nil
	})
	if err != nil {
		return ChargeResult{}, err
	}
	if replay {
		s.deps.Metrics.IdempotentReplay(opInitiateCharge)
		result.IdempotentReplay = true
		return result, nil
	}

	intent, err := s.deps.PaymentGW.InitiatePayment(ctx, port.PaymentRequest{
		UserID:         cmd.UserID,
		Amount:         cmd.Amount,
		ReturnURL:      cmd.ReturnURL,
		IdempotencyKey: cmd.IdempotencyKey,
		Metadata: map[string]string{
			"wallet_id": w.ID,
			"purpose":   "WALLET_TOPUP",
		},
	})
	if err != nil {
		return ChargeResult{}, err
	}

	result = ChargeResult{
		PaymentIntentID: intent.ID,
		RedirectURL:     intent.RedirectURL,
		Amount:          cmd.Amount,
		ExpiresAt:       intent.ExpiresAt,
	}

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		if err := s.emit(ctx, tx, EventChargeInitiated, aggregateWallet, w.ID, s.deps.Clock.Now(),
			ChargeInitiatedPayload{
				UserID:          cmd.UserID,
				WalletID:        w.ID,
				PaymentIntentID: intent.ID,
				Amount:          cmd.Amount,
			}); err != nil {
			return err
		}
		return s.saveResponse(ctx, tx, opInitiateCharge, cmd.IdempotencyKey, result)
	})
	if err != nil {
		// The intent exists at the gateway but we failed to record it. Log loudly:
		// the reconciliation job will find the orphaned intent and settle it.
		logx.FromContext(ctx).Error("failed to record an initiated charge; reconciliation will pick it up",
			slog.String("payment_intent_id", intent.ID),
			slog.String("user_id", cmd.UserID),
			slog.String("error", err.Error()),
		)
		return ChargeResult{}, err
	}

	s.publisher.Notify()
	s.deps.Metrics.WalletOperation(opInitiateCharge, "success")
	return result, nil
}

// ConfirmCharge credits a wallet for a bank payment the adapter has confirmed.
//
// The event id is the idempotency key. Kafka delivers at least once, so this method
// will genuinely be called twice for the same payment, and the second call must
// credit nothing.
func (s *ChargeService) ConfirmCharge(ctx context.Context, cmd ConfirmChargeCommand) (TransactionResult, error) {
	switch {
	case cmd.UserID == "":
		return TransactionResult{}, errs.InvalidArgument("a user id is required")
	case cmd.EventID == "":
		return TransactionResult{}, errs.InvalidArgument("an event id is required to make the credit idempotent")
	case !cmd.Amount.IsPositive():
		return TransactionResult{}, wallet.ErrAmountNotPositive(cmd.Amount)
	}

	// The wallet may not exist yet if the user's registration event is still in
	// flight; provisioning here means a confirmed payment is never dropped.
	if _, err := s.walletFor(ctx, cmd.UserID); err != nil {
		return TransactionResult{}, err
	}

	description := "bank top-up"
	if cmd.BankReference != "" {
		description = "bank top-up " + cmd.BankReference
	}

	walletService := &WalletService{core: s.core}
	return walletService.CreditInternal(ctx, CreditCommand{
		UserID:         cmd.UserID,
		Amount:         cmd.Amount,
		Reason:         wallet.ReasonCharge,
		ReferenceID:    cmd.PaymentIntentID,
		Description:    description,
		IdempotencyKey: cmd.EventID,
	})
}

// walletFor returns the user's wallet, provisioning one if necessary.
func (s *ChargeService) walletFor(ctx context.Context, userID string) (WalletView, error) {
	walletService := &WalletService{core: s.core}
	return walletService.EnsureWallet(ctx, userID)
}
