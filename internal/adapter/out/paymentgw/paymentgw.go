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
		return port.PaymentIntent{}, c.translate(err,
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
		return port.PaymentIntent{}, c.translate(err, "could not read payment intent %s", id)
	}
	if response.GetIntent() == nil {
		return port.PaymentIntent{}, errs.NotFound("no payment intent exists with id %s", id)
	}
	return toPortIntent(response.GetIntent())
}

// translate turns a gRPC error from the adapter into a platform error, reclassifying the two
// statuses that must not be passed through.
//
// UNAUTHENTICATED and PERMISSION_DENIED here mean *this service* was refused — a missing or
// rejected service credential, which is a deployment fault. Forwarded unchanged they reach the
// end user as 401 or 403, telling them to log in again for something their own session had
// nothing to do with. That is exactly what happened: every bank top-up failed as a 401 because
// the wallet sent no service token at all, and the error blamed the buyer.
//
// Reclassified as UNAVAILABLE — 503 — because from the caller's side that is the truth: the
// feature is temporarily not working and nothing they can do affects it. Logged at error,
// because unlike a genuine outage this one needs somebody to fix configuration.
//
// Every other status is passed through. A NOT_FOUND for an intent really is not found, and an
// INVALID_ARGUMENT really was a bad request.
func (c *Client) translate(err error, format string, args ...any) error {
	translated := errs.FromGRPC(err)

	switch errs.CodeOf(translated) {
	case errs.CodeUnauthenticated, errs.CodePermissionDenied:
		c.logger.Error("the payment adapter refused this service's credentials",
			slog.String("error", err.Error()))
		return errs.Wrap(
			errs.Unavailable("the payment adapter is not accepting requests from this service").
				WithReason("PAYMENT_GATEWAY_UNAUTHORIZED"),
			format, args...)
	default:
		return errs.Wrap(translated, format, args...)
	}
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
