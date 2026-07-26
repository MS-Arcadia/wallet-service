// Package paymentgw is the outbound adapter to the Payment Adapter service.
//
// The wallet never speaks to a bank. It speaks to this port, which speaks gRPC to
// the Payment Adapter, which in turn holds the Anti-Corruption Layer for whichever
// provider is in use. Two layers of indirection sounds like a lot until the bank
// changes its API and nothing in the wallet has to move.
package paymentgw

import (
	"context"
	"log/slog"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	commonv1 "github.com/MS-Arcadia/wallet-service/internal/pb/arcadia/common/v1"
	paymentv1 "github.com/MS-Arcadia/wallet-service/internal/pb/arcadia/payment/v1"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
	"google.golang.org/grpc"
)

// Client is the gRPC-backed port.PaymentGateway.
type Client struct {
	client paymentv1.PaymentServiceClient
	logger *slog.Logger
}

// New builds a Client over an established connection.
func New(conn *grpc.ClientConn, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		client: paymentv1.NewPaymentServiceClient(conn),
		logger: logger.With(slog.String("component", "payment-gateway-client")),
	}
}

// InitiatePayment asks the adapter for a bank authorisation URL.
func (c *Client) InitiatePayment(ctx context.Context, req port.PaymentRequest) (port.PaymentIntent, error) {
	response, err := c.client.InitiatePayment(ctx, &paymentv1.InitiatePaymentRequest{
		UserId: req.UserID,
		Amount: &commonv1.Money{
			AmountMinor: req.Amount.Minor(),
			Currency:    req.Amount.Currency(),
		},
		Purpose:   paymentv1.PaymentPurpose_PAYMENT_PURPOSE_WALLET_TOPUP,
		ReturnUrl: req.ReturnURL,
		Metadata:  req.Metadata,
		// Forwarding the key is what stops a retried top-up from opening a second
		// payment at the bank.
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		// Translate the transport error back into the platform taxonomy so that the use
		// case sees "the dependency is unavailable" rather than a gRPC status it would
		// have to know how to interpret.
		return port.PaymentIntent{}, errs.Wrap(errs.FromGRPC(err),
			"the payment adapter could not start a top-up for user %s", req.UserID)
	}
	if response.GetIntent() == nil {
		return port.PaymentIntent{}, errs.Internal("the payment adapter returned no intent")
	}
	return toPortIntent(response.GetIntent())
}

// GetPaymentIntent reads an intent's current state, used by reconciliation to settle
// a top-up whose callback never arrived.
func (c *Client) GetPaymentIntent(ctx context.Context, id string) (port.PaymentIntent, error) {
	response, err := c.client.GetPaymentIntent(ctx, &paymentv1.GetPaymentIntentRequest{Id: id})
	if err != nil {
		return port.PaymentIntent{}, errs.Wrap(errs.FromGRPC(err),
			"could not read payment intent %s", id)
	}
	if response.GetIntent() == nil {
		return port.PaymentIntent{}, errs.NotFound("no payment intent exists with id %s", id)
	}
	return toPortIntent(response.GetIntent())
}

func toPortIntent(intent *paymentv1.PaymentIntent) (port.PaymentIntent, error) {
	result := port.PaymentIntent{
		ID:          intent.GetId(),
		RedirectURL: intent.GetRedirectUrl(),
		State:       intent.GetState().String(),
	}

	if amount := intent.GetAmount(); amount != nil {
		parsed, err := money.New(amount.GetAmountMinor(), amount.GetCurrency())
		if err != nil {
			return port.PaymentIntent{}, errs.Internal(
				"the payment adapter returned an invalid currency for intent %s", intent.GetId()).WithCause(err)
		}
		result.Amount = parsed
	}

	if raw := intent.GetExpiresAt(); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			result.ExpiresAt = &parsed
		}
	}
	return result, nil
}

var _ port.PaymentGateway = (*Client)(nil)
