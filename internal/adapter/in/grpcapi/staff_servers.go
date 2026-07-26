package grpcapi

import (
	"context"

	"github.com/MS-Arcadia/wallet-service/internal/app"
	walletv1 "github.com/MS-Arcadia/wallet-service/internal/pb/arcadia/wallet/v1"
	"google.golang.org/grpc"
)

// --- GiftCardService ------------------------------------------------------

// GiftCardServer implements walletv1.GiftCardServiceServer.
type GiftCardServer struct {
	giftCards *app.GiftCardService
	currency  string
}

// NewGiftCardServer builds the server.
func NewGiftCardServer(giftCards *app.GiftCardService, currency string) *GiftCardServer {
	return &GiftCardServer{giftCards: giftCards, currency: currency}
}

// Register attaches the server to a gRPC registrar.
func (s *GiftCardServer) Register(registrar grpc.ServiceRegistrar) {
	walletv1.RegisterGiftCardServiceServer(registrar, s)
}

// IssueGiftCards mints a batch and returns the plaintext codes exactly once.
func (s *GiftCardServer) IssueGiftCards(ctx context.Context, req *walletv1.IssueGiftCardsRequest) (*walletv1.IssueGiftCardsResponse, error) {
	value, err := toMoney(req.GetValue(), s.currency)
	if err != nil {
		return nil, err
	}

	quantity := int(req.GetQuantity())
	if quantity == 0 {
		// A single card is the overwhelmingly common case, so an omitted quantity means
		// one rather than an error.
		quantity = 1
	}

	result, err := s.giftCards.IssueGiftCards(ctx, app.IssueGiftCardsCommand{
		Value:          value,
		Quantity:       quantity,
		Note:           req.GetNote(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}

	cards := make([]*walletv1.GiftCard, 0, len(result.GiftCards))
	for _, card := range result.GiftCards {
		cards = append(cards, fromGiftCard(card))
	}
	return &walletv1.IssueGiftCardsResponse{BatchId: result.BatchID, GiftCards: cards}, nil
}

// GetGiftCard reads a card by id or by code.
func (s *GiftCardServer) GetGiftCard(ctx context.Context, req *walletv1.GetGiftCardRequest) (*walletv1.GetGiftCardResponse, error) {
	view, err := s.giftCards.GetGiftCard(ctx, req.GetId(), req.GetCode())
	if err != nil {
		return nil, err
	}
	return &walletv1.GetGiftCardResponse{GiftCard: fromGiftCard(view)}, nil
}

// ListGiftCards returns a filtered page of cards.
func (s *GiftCardServer) ListGiftCards(ctx context.Context, req *walletv1.ListGiftCardsRequest) (*walletv1.ListGiftCardsResponse, error) {
	page, pageSize := pageOf(req.GetPage())

	result, err := s.giftCards.ListGiftCards(ctx, toGiftCardStatus(req.GetStatus()), req.GetBatchId(), page, pageSize)
	if err != nil {
		return nil, err
	}

	cards := make([]*walletv1.GiftCard, 0, len(result.GiftCards))
	for _, card := range result.GiftCards {
		cards = append(cards, fromGiftCard(card))
	}
	return &walletv1.ListGiftCardsResponse{
		GiftCards: cards,
		Page:      fromPage(result.TotalItems, result.Page, result.PageSize, result.TotalPages),
	}, nil
}

// RevokeGiftCard cancels an unredeemed card.
func (s *GiftCardServer) RevokeGiftCard(ctx context.Context, req *walletv1.RevokeGiftCardRequest) (*walletv1.RevokeGiftCardResponse, error) {
	view, err := s.giftCards.RevokeGiftCard(ctx, req.GetId(), req.GetReason())
	if err != nil {
		return nil, err
	}
	return &walletv1.RevokeGiftCardResponse{GiftCard: fromGiftCard(view)}, nil
}

// --- DiscountService ------------------------------------------------------

// DiscountServer implements walletv1.DiscountServiceServer.
type DiscountServer struct {
	discounts *app.DiscountService
	currency  string
}

// NewDiscountServer builds the server.
func NewDiscountServer(discounts *app.DiscountService, currency string) *DiscountServer {
	return &DiscountServer{discounts: discounts, currency: currency}
}

// Register attaches the server to a gRPC registrar.
func (s *DiscountServer) Register(registrar grpc.ServiceRegistrar) {
	walletv1.RegisterDiscountServiceServer(registrar, s)
}

// IssueDiscountCode mints a promotional code.
func (s *DiscountServer) IssueDiscountCode(ctx context.Context, req *walletv1.IssueDiscountCodeRequest) (*walletv1.IssueDiscountCodeResponse, error) {
	amountOff, err := toOptionalMoney(req.GetAmountOff(), s.currency)
	if err != nil {
		return nil, err
	}
	maxDiscount, err := toOptionalMoney(req.GetMaxDiscount(), s.currency)
	if err != nil {
		return nil, err
	}
	minOrder, err := toOptionalMoney(req.GetMinOrderAmount(), s.currency)
	if err != nil {
		return nil, err
	}
	expiresAt, err := parseOptionalTime(req.GetExpiresAt(), "expires_at")
	if err != nil {
		return nil, err
	}

	view, err := s.discounts.IssueDiscountCode(ctx, app.IssueDiscountCodeCommand{
		Code:           req.GetCode(),
		PercentBps:     req.GetPercentBps(),
		AmountOff:      amountOff,
		MaxDiscount:    maxDiscount,
		MinOrderAmount: minOrder,
		MaxRedemptions: req.GetMaxRedemptions(),
		ExpiresAt:      expiresAt,
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return &walletv1.IssueDiscountCodeResponse{DiscountCode: fromDiscountCode(view)}, nil
}

// PreviewDiscount computes a discount without consuming a redemption.
func (s *DiscountServer) PreviewDiscount(ctx context.Context, req *walletv1.PreviewDiscountRequest) (*walletv1.PreviewDiscountResponse, error) {
	orderAmount, err := toMoney(req.GetOrderAmount(), s.currency)
	if err != nil {
		return nil, err
	}

	quote, err := s.discounts.PreviewDiscount(ctx, req.GetCode(), orderAmount)
	if err != nil {
		return nil, err
	}

	response := &walletv1.PreviewDiscountResponse{
		Discount: fromMoney(quote.Discount),
		Payable:  fromMoney(quote.Payable),
	}
	if quote.Code != nil {
		response.DiscountCode = fromDiscountCode(*quote.Code)
	}
	return response, nil
}

// RedeemDiscountCode consumes one redemption.
func (s *DiscountServer) RedeemDiscountCode(ctx context.Context, req *walletv1.RedeemDiscountCodeRequest) (*walletv1.RedeemDiscountCodeResponse, error) {
	orderAmount, err := toMoney(req.GetOrderAmount(), s.currency)
	if err != nil {
		return nil, err
	}

	quote, err := s.discounts.RedeemDiscountCode(ctx, app.RedeemDiscountCodeCommand{
		Code:           req.GetCode(),
		OrderAmount:    orderAmount,
		ReferenceID:    req.GetReferenceId(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return &walletv1.RedeemDiscountCodeResponse{
		Discount:         fromMoney(quote.Discount),
		Payable:          fromMoney(quote.Payable),
		IdempotentReplay: quote.IdempotentReplay,
	}, nil
}

// GetDiscountCode reads a code.
func (s *DiscountServer) GetDiscountCode(ctx context.Context, req *walletv1.GetDiscountCodeRequest) (*walletv1.GetDiscountCodeResponse, error) {
	view, err := s.discounts.GetDiscountCode(ctx, req.GetCode())
	if err != nil {
		return nil, err
	}
	return &walletv1.GetDiscountCodeResponse{DiscountCode: fromDiscountCode(view)}, nil
}

// --- WalletAdminService ---------------------------------------------------

// AdminServer implements walletv1.WalletAdminServiceServer.
type AdminServer struct {
	admin    *app.AdminService
	currency string
}

// NewAdminServer builds the server.
func NewAdminServer(admin *app.AdminService, currency string) *AdminServer {
	return &AdminServer{admin: admin, currency: currency}
}

// Register attaches the server to a gRPC registrar.
func (s *AdminServer) Register(registrar grpc.ServiceRegistrar) {
	walletv1.RegisterWalletAdminServiceServer(registrar, s)
}

// Reconcile verifies balances against the ledger.
func (s *AdminServer) Reconcile(ctx context.Context, req *walletv1.ReconcileRequest) (*walletv1.ReconcileResponse, error) {
	result, err := s.admin.Reconcile(ctx, req.GetUserId())
	if err != nil {
		return nil, err
	}

	mismatches := make([]*walletv1.ReconcileMismatch, 0, len(result.Mismatches))
	for _, mismatch := range result.Mismatches {
		mismatches = append(mismatches, &walletv1.ReconcileMismatch{
			WalletId:      mismatch.WalletID,
			UserId:        mismatch.UserID,
			StoredBalance: fromMoney(mismatch.StoredBalance),
			LedgerBalance: fromMoney(mismatch.LedgerBalance),
			Delta:         fromMoney(mismatch.Delta),
		})
	}
	return &walletv1.ReconcileResponse{
		WalletsChecked: result.WalletsChecked,
		Mismatches:     mismatches,
	}, nil
}

// AccrueInterest runs an interest cycle.
func (s *AdminServer) AccrueInterest(ctx context.Context, req *walletv1.AccrueInterestRequest) (*walletv1.AccrueInterestResponse, error) {
	result, err := s.admin.AccrueInterest(ctx, app.AccrueInterestCommand{
		AccrualDate:   req.GetAccrualDate(),
		AnnualRateBps: req.GetAnnualRateBps(),
		DryRun:        req.GetDryRun(),
	})
	if err != nil {
		return nil, err
	}
	return &walletv1.AccrueInterestResponse{
		WalletsProcessed: result.WalletsProcessed,
		WalletsCredited:  result.WalletsCredited,
		TotalInterest:    fromMoney(result.TotalInterest),
		AccrualDate:      result.AccrualDate,
		DryRun:           result.DryRun,
	}, nil
}

// FreezeWallet suspends movement on a wallet.
func (s *AdminServer) FreezeWallet(ctx context.Context, req *walletv1.FreezeWalletRequest) (*walletv1.FreezeWalletResponse, error) {
	view, err := s.admin.FreezeWallet(ctx, req.GetUserId(), req.GetReason())
	if err != nil {
		return nil, err
	}
	return &walletv1.FreezeWalletResponse{Wallet: fromWallet(view)}, nil
}

// UnfreezeWallet restores normal operation.
func (s *AdminServer) UnfreezeWallet(ctx context.Context, req *walletv1.UnfreezeWalletRequest) (*walletv1.UnfreezeWalletResponse, error) {
	view, err := s.admin.UnfreezeWallet(ctx, req.GetUserId(), req.GetReason())
	if err != nil {
		return nil, err
	}
	return &walletv1.UnfreezeWalletResponse{Wallet: fromWallet(view)}, nil
}

// Adjust applies a manual balance correction.
func (s *AdminServer) Adjust(ctx context.Context, req *walletv1.AdjustRequest) (*walletv1.TransactionResponse, error) {
	amount, err := toMoney(req.GetAmount(), s.currency)
	if err != nil {
		return nil, err
	}
	direction, err := toDirection(req.GetDirection())
	if err != nil {
		return nil, err
	}

	result, err := s.admin.Adjust(ctx, app.AdjustCommand{
		UserID:         req.GetUserId(),
		Direction:      direction,
		Amount:         amount,
		Reason:         req.GetReason(),
		IdempotencyKey: idempotencyKey(ctx, req.GetIdempotencyKey()),
	})
	if err != nil {
		return nil, err
	}
	return fromTransaction(result), nil
}

var (
	_ walletv1.GiftCardServiceServer    = (*GiftCardServer)(nil)
	_ walletv1.DiscountServiceServer    = (*DiscountServer)(nil)
	_ walletv1.WalletAdminServiceServer = (*AdminServer)(nil)
)
