package grpcapi

import (
	"context"
	"time"

	walletv1 "github.com/MS-Arcadia/arcadia-platform/gen/arcadia/wallet/v1"
	"github.com/MS-Arcadia/arcadia-platform/pkg/authn"
	"github.com/MS-Arcadia/arcadia-platform/pkg/grpcx"
	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"google.golang.org/grpc"
)

// WalletServer implements walletv1.WalletServiceServer.
type WalletServer struct {
	wallets   *app.WalletService
	charges   *app.ChargeService
	giftCards *app.GiftCardService
	currency  string
}

// NewWalletServer builds the server.
func NewWalletServer(
	wallets *app.WalletService,
	charges *app.ChargeService,
	giftCards *app.GiftCardService,
	currency string,
) *WalletServer {
	return &WalletServer{wallets: wallets, charges: charges, giftCards: giftCards, currency: currency}
}

// Register attaches the server to a gRPC registrar.
func (s *WalletServer) Register(registrar grpc.ServiceRegistrar) {
	walletv1.RegisterWalletServiceServer(registrar, s)
}

// GetMyWallet returns the caller's own wallet, provisioning it on first access.
func (s *WalletServer) GetMyWallet(ctx context.Context, _ *walletv1.GetMyWalletRequest) (*walletv1.GetMyWalletResponse, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	view, err := s.wallets.GetOrCreateWallet(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	return &walletv1.GetMyWalletResponse{Wallet: fromWallet(view)}, nil
}

// GetWallet returns any user's wallet. Support and Admin only.
func (s *WalletServer) GetWallet(ctx context.Context, req *walletv1.GetWalletRequest) (*walletv1.GetWalletResponse, error) {
	view, err := s.wallets.GetWallet(ctx, req.GetUserId())
	if err != nil {
		return nil, err
	}
	return &walletv1.GetWalletResponse{Wallet: fromWallet(view)}, nil
}

// InitiateCharge starts a bank top-up.
func (s *WalletServer) InitiateCharge(ctx context.Context, req *walletv1.InitiateChargeRequest) (*walletv1.InitiateChargeResponse, error) {
	amount, err := toMoney(req.GetAmount(), s.currency)
	if err != nil {
		return nil, err
	}

	result, err := s.charges.InitiateCharge(ctx, app.InitiateChargeCommand{
		Amount:         amount,
		ReturnURL:      req.GetReturnUrl(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}

	return &walletv1.InitiateChargeResponse{
		PaymentIntentId: result.PaymentIntentID,
		RedirectUrl:     result.RedirectURL,
		Amount:          fromMoney(result.Amount),
		ExpiresAt:       formatOptionalTime(result.ExpiresAt),
	}, nil
}

// RedeemGiftCard credits a gift card to the caller's wallet.
//
// It lives on WalletService rather than GiftCardService because that is how a user
// thinks of it — topping up their balance — while GiftCardService stays the
// staff-facing surface. Both route into the same use case.
func (s *WalletServer) RedeemGiftCard(ctx context.Context, req *walletv1.RedeemGiftCardRequest) (*walletv1.RedeemGiftCardResponse, error) {
	result, err := s.giftCards.RedeemGiftCard(ctx, app.RedeemGiftCardCommand{
		Code:           req.GetCode(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return &walletv1.RedeemGiftCardResponse{
		Credited: fromMoney(result.Credited),
		Wallet:   fromWallet(result.Wallet),
		Entry:    fromLedgerEntry(result.Entry),
	}, nil
}

// ListLedger returns a page of a wallet's transaction history.
func (s *WalletServer) ListLedger(ctx context.Context, req *walletv1.ListLedgerRequest) (*walletv1.ListLedgerResponse, error) {
	page, pageSize := pageOf(req.GetPage())
	from, to, err := timeRangeOf(req.GetRange())
	if err != nil {
		return nil, err
	}

	reasons := make([]wallet.Reason, 0, len(req.GetReasons()))
	for _, raw := range req.GetReasons() {
		reason, err := toReason(raw)
		if err != nil {
			return nil, err
		}
		reasons = append(reasons, reason)
	}

	// An unspecified direction means "both", which is not an error.
	direction := wallet.Direction("")
	if req.GetDirection() != walletv1.LedgerDirection_LEDGER_DIRECTION_UNSPECIFIED {
		direction, err = toDirection(req.GetDirection())
		if err != nil {
			return nil, err
		}
	}

	result, err := s.wallets.ListLedger(ctx, app.ListLedgerQuery{
		UserID:    req.GetUserId(),
		Reasons:   reasons,
		Direction: direction,
		From:      from,
		To:        to,
		Page:      page,
		PageSize:  pageSize,
	})
	if err != nil {
		return nil, err
	}

	entries := make([]*walletv1.LedgerEntry, 0, len(result.Entries))
	for _, entry := range result.Entries {
		entries = append(entries, fromLedgerEntry(entry))
	}
	return &walletv1.ListLedgerResponse{
		Entries: entries,
		Page:    fromPage(result.TotalItems, result.Page, result.PageSize, result.TotalPages),
	}, nil
}

// Debit removes money from a wallet. Step one of the purchase saga.
func (s *WalletServer) Debit(ctx context.Context, req *walletv1.DebitRequest) (*walletv1.TransactionResponse, error) {
	amount, err := toMoney(req.GetAmount(), s.currency)
	if err != nil {
		return nil, err
	}
	reason, err := toReason(req.GetReason())
	if err != nil {
		return nil, err
	}

	result, err := s.wallets.Debit(ctx, app.DebitCommand{
		UserID:         req.GetUserId(),
		Amount:         amount,
		Reason:         reason,
		ReferenceID:    req.GetReferenceId(),
		Description:    req.GetDescription(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return fromTransaction(result), nil
}

// Credit adds money to a wallet.
func (s *WalletServer) Credit(ctx context.Context, req *walletv1.CreditRequest) (*walletv1.TransactionResponse, error) {
	amount, err := toMoney(req.GetAmount(), s.currency)
	if err != nil {
		return nil, err
	}
	reason, err := toReason(req.GetReason())
	if err != nil {
		return nil, err
	}

	result, err := s.wallets.Credit(ctx, app.CreditCommand{
		UserID:         req.GetUserId(),
		Amount:         amount,
		Reason:         reason,
		ReferenceID:    req.GetReferenceId(),
		Description:    req.GetDescription(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return fromTransaction(result), nil
}

// Transfer settles a marketplace trade: both sides move, or neither does.
func (s *WalletServer) Transfer(ctx context.Context, req *walletv1.TransferRequest) (*walletv1.TransferResponse, error) {
	amount, err := toMoney(req.GetAmount(), s.currency)
	if err != nil {
		return nil, err
	}
	reason, err := toReason(req.GetReason())
	if err != nil {
		return nil, err
	}

	result, err := s.wallets.Transfer(ctx, app.TransferCommand{
		FromUserID:     req.GetFromUserId(),
		ToUserID:       req.GetToUserId(),
		Amount:         amount,
		Reason:         reason,
		ReferenceID:    req.GetReferenceId(),
		Description:    req.GetDescription(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}

	return &walletv1.TransferResponse{
		DebitEntry:       fromLedgerEntry(result.DebitEntry),
		CreditEntry:      fromLedgerEntry(result.CreditEntry),
		IdempotentReplay: result.IdempotentReplay,
	}, nil
}

// HoldFunds reserves part of a balance for a pre-order or an instalment plan.
func (s *WalletServer) HoldFunds(ctx context.Context, req *walletv1.HoldFundsRequest) (*walletv1.HoldFundsResponse, error) {
	amount, err := toMoney(req.GetAmount(), s.currency)
	if err != nil {
		return nil, err
	}

	result, err := s.wallets.HoldFunds(ctx, app.HoldFundsCommand{
		UserID:         req.GetUserId(),
		Amount:         amount,
		ReferenceID:    req.GetReferenceId(),
		Reason:         req.GetReason(),
		TTL:            time.Duration(req.GetTtlSeconds()) * time.Second,
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}

	return &walletv1.HoldFundsResponse{
		Hold:             fromHold(result.Hold),
		Wallet:           fromWallet(result.Wallet),
		IdempotentReplay: result.IdempotentReplay,
	}, nil
}

// CaptureHold turns a reservation into a real debit.
func (s *WalletServer) CaptureHold(ctx context.Context, req *walletv1.CaptureHoldRequest) (*walletv1.TransactionResponse, error) {
	// A zero amount means "capture everything remaining", so the amount is optional.
	amount, err := toOptionalMoney(req.GetAmount(), s.currency)
	if err != nil {
		return nil, err
	}

	reason := wallet.Reason("")
	if req.GetReason() != walletv1.LedgerReason_LEDGER_REASON_UNSPECIFIED {
		reason, err = toReason(req.GetReason())
		if err != nil {
			return nil, err
		}
	}

	result, err := s.wallets.CaptureHold(ctx, app.CaptureHoldCommand{
		HoldID:         req.GetHoldId(),
		Amount:         amount,
		Reason:         reason,
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return fromTransaction(result), nil
}

// ReleaseHold returns a reservation to the available balance.
func (s *WalletServer) ReleaseHold(ctx context.Context, req *walletv1.ReleaseHoldRequest) (*walletv1.ReleaseHoldResponse, error) {
	result, err := s.wallets.ReleaseHold(ctx, app.ReleaseHoldCommand{
		HoldID:         req.GetHoldId(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return &walletv1.ReleaseHoldResponse{
		Hold:             fromHold(result.Hold),
		Wallet:           fromWallet(result.Wallet),
		IdempotentReplay: result.IdempotentReplay,
	}, nil
}

// ListHolds returns a page of a wallet's holds.
func (s *WalletServer) ListHolds(ctx context.Context, req *walletv1.ListHoldsRequest) (*walletv1.ListHoldsResponse, error) {
	page, pageSize := pageOf(req.GetPage())

	result, err := s.wallets.ListHolds(ctx, app.ListHoldsQuery{
		UserID:   req.GetUserId(),
		Status:   toHoldStatus(req.GetStatus()),
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		return nil, err
	}

	holds := make([]*walletv1.Hold, 0, len(result.Holds))
	for _, h := range result.Holds {
		holds = append(holds, fromHold(h))
	}
	return &walletv1.ListHoldsResponse{
		Holds: holds,
		Page:  fromPage(result.TotalItems, result.Page, result.PageSize, result.TotalPages),
	}, nil
}

// idempotencyKey prefers the key in the request body and falls back to the
// Idempotency-Key metadata header, so that a client may supply it either way.
func idempotencyKey(ctx context.Context, fromBody string) string {
	if fromBody != "" {
		return fromBody
	}
	return grpcx.IdempotencyKeyFrom(ctx)
}

var _ walletv1.WalletServiceServer = (*WalletServer)(nil)
