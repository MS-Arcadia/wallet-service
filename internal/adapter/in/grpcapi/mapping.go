// Package grpcapi is the gRPC inbound adapter.
//
// It is the platform's primary transport. Every handler is a thin translation:
// proto request in, application command out, application result in, proto response
// out. No validation beyond what the wire format cannot express, and no business
// rules — those live in the domain, where the REST adapter reaches them too.
package grpcapi

import (
	"time"

	commonv1 "github.com/MS-Arcadia/arcadia-platform/gen/arcadia/common/v1"
	walletv1 "github.com/MS-Arcadia/arcadia-platform/gen/arcadia/wallet/v1"
	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// --- Inbound conversions --------------------------------------------------

// toMoney converts a proto Money, rejecting a missing or malformed one.
func toMoney(amount *commonv1.Money, defaultCurrency string) (money.Money, error) {
	if amount == nil {
		return money.Money{}, errs.InvalidArgument("an amount is required")
	}
	currency := amount.GetCurrency()
	if currency == "" {
		// Clients that only ever deal in the platform currency may omit it.
		currency = defaultCurrency
	}
	parsed, err := money.New(amount.GetAmountMinor(), currency)
	if err != nil {
		return money.Money{}, errs.InvalidArgument("invalid amount: %s", err.Error()).WithCause(err)
	}
	return parsed, nil
}

// toOptionalMoney converts a proto Money that may legitimately be absent.
func toOptionalMoney(amount *commonv1.Money, defaultCurrency string) (money.Money, error) {
	if amount == nil || amount.GetAmountMinor() == 0 {
		return money.Money{}, nil
	}
	return toMoney(amount, defaultCurrency)
}

// toReason converts the proto ledger reason to the domain one.
func toReason(reason walletv1.LedgerReason) (wallet.Reason, error) {
	switch reason {
	case walletv1.LedgerReason_LEDGER_REASON_PURCHASE:
		return wallet.ReasonPurchase, nil
	case walletv1.LedgerReason_LEDGER_REASON_REVENUE:
		return wallet.ReasonRevenue, nil
	case walletv1.LedgerReason_LEDGER_REASON_REFUND:
		return wallet.ReasonRefund, nil
	case walletv1.LedgerReason_LEDGER_REASON_REVERSAL:
		return wallet.ReasonReversal, nil
	case walletv1.LedgerReason_LEDGER_REASON_CHARGE:
		return wallet.ReasonCharge, nil
	case walletv1.LedgerReason_LEDGER_REASON_GIFTCARD:
		return wallet.ReasonGiftCard, nil
	case walletv1.LedgerReason_LEDGER_REASON_TRADE:
		return wallet.ReasonTrade, nil
	case walletv1.LedgerReason_LEDGER_REASON_DISCOUNT:
		return wallet.ReasonDiscount, nil
	case walletv1.LedgerReason_LEDGER_REASON_INTEREST:
		return wallet.ReasonInterest, nil
	case walletv1.LedgerReason_LEDGER_REASON_HOLD_CAPTURE:
		return wallet.ReasonHoldCapture, nil
	case walletv1.LedgerReason_LEDGER_REASON_ADJUSTMENT:
		return wallet.ReasonAdjustment, nil
	default:
		return "", errs.InvalidArgument("a ledger reason is required")
	}
}

// toDirection converts the proto direction to the domain one.
func toDirection(direction walletv1.LedgerDirection) (wallet.Direction, error) {
	switch direction {
	case walletv1.LedgerDirection_LEDGER_DIRECTION_DEBIT:
		return wallet.DirectionDebit, nil
	case walletv1.LedgerDirection_LEDGER_DIRECTION_CREDIT:
		return wallet.DirectionCredit, nil
	default:
		return "", errs.InvalidArgument("a direction of DEBIT or CREDIT is required")
	}
}

// toHoldStatus converts the proto hold status. The unspecified value means "any".
func toHoldStatus(status walletv1.HoldStatus) hold.Status {
	switch status {
	case walletv1.HoldStatus_HOLD_STATUS_ACTIVE:
		return hold.StatusActive
	case walletv1.HoldStatus_HOLD_STATUS_CAPTURED:
		return hold.StatusCaptured
	case walletv1.HoldStatus_HOLD_STATUS_RELEASED:
		return hold.StatusReleased
	case walletv1.HoldStatus_HOLD_STATUS_EXPIRED:
		return hold.StatusExpired
	default:
		return ""
	}
}

// toGiftCardStatus converts the proto gift-card status. Unspecified means "any".
func toGiftCardStatus(status walletv1.GiftCardStatus) giftcard.Status {
	switch status {
	case walletv1.GiftCardStatus_GIFT_CARD_STATUS_ACTIVE:
		return giftcard.StatusActive
	case walletv1.GiftCardStatus_GIFT_CARD_STATUS_USED:
		return giftcard.StatusUsed
	case walletv1.GiftCardStatus_GIFT_CARD_STATUS_REVOKED:
		return giftcard.StatusRevoked
	default:
		return ""
	}
}

// pageOf extracts a page request, applying defaults.
func pageOf(page *commonv1.PageRequest) (int, int) {
	if page == nil {
		return 1, 0
	}
	return int(page.GetPage()), int(page.GetPageSize())
}

// timeRangeOf parses an optional RFC3339 time range.
func timeRangeOf(rangeSpec *commonv1.TimeRange) (from, to *time.Time, err error) {
	if rangeSpec == nil {
		return nil, nil, nil
	}
	if raw := rangeSpec.GetFrom(); raw != "" {
		parsed, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return nil, nil, errs.InvalidArgument("`from` must be an RFC3339 timestamp, got %q", raw)
		}
		from = &parsed
	}
	if raw := rangeSpec.GetTo(); raw != "" {
		parsed, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			return nil, nil, errs.InvalidArgument("`to` must be an RFC3339 timestamp, got %q", raw)
		}
		to = &parsed
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, nil, errs.InvalidArgument("`from` must be earlier than `to`")
	}
	return from, to, nil
}

// parseOptionalTime parses an optional RFC3339 timestamp.
func parseOptionalTime(raw, field string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, errs.InvalidArgument("%s must be an RFC3339 timestamp, got %q", field, raw)
	}
	return &parsed, nil
}

// --- Outbound conversions -------------------------------------------------

func fromMoney(amount money.Money) *commonv1.Money {
	return &commonv1.Money{
		AmountMinor: amount.Minor(),
		Currency:    amount.Currency(),
	}
}

func fromWalletStatus(status wallet.Status) walletv1.WalletStatus {
	switch status {
	case wallet.StatusActive:
		return walletv1.WalletStatus_WALLET_STATUS_ACTIVE
	case wallet.StatusFrozen:
		return walletv1.WalletStatus_WALLET_STATUS_FROZEN
	case wallet.StatusClosed:
		return walletv1.WalletStatus_WALLET_STATUS_CLOSED
	default:
		return walletv1.WalletStatus_WALLET_STATUS_UNSPECIFIED
	}
}

func fromDirection(direction wallet.Direction) walletv1.LedgerDirection {
	if direction == wallet.DirectionDebit {
		return walletv1.LedgerDirection_LEDGER_DIRECTION_DEBIT
	}
	return walletv1.LedgerDirection_LEDGER_DIRECTION_CREDIT
}

func fromReason(reason wallet.Reason) walletv1.LedgerReason {
	switch reason {
	case wallet.ReasonPurchase:
		return walletv1.LedgerReason_LEDGER_REASON_PURCHASE
	case wallet.ReasonRevenue:
		return walletv1.LedgerReason_LEDGER_REASON_REVENUE
	case wallet.ReasonRefund:
		return walletv1.LedgerReason_LEDGER_REASON_REFUND
	case wallet.ReasonReversal:
		return walletv1.LedgerReason_LEDGER_REASON_REVERSAL
	case wallet.ReasonCharge:
		return walletv1.LedgerReason_LEDGER_REASON_CHARGE
	case wallet.ReasonGiftCard:
		return walletv1.LedgerReason_LEDGER_REASON_GIFTCARD
	case wallet.ReasonTrade:
		return walletv1.LedgerReason_LEDGER_REASON_TRADE
	case wallet.ReasonDiscount:
		return walletv1.LedgerReason_LEDGER_REASON_DISCOUNT
	case wallet.ReasonInterest:
		return walletv1.LedgerReason_LEDGER_REASON_INTEREST
	case wallet.ReasonHoldCapture:
		return walletv1.LedgerReason_LEDGER_REASON_HOLD_CAPTURE
	case wallet.ReasonAdjustment:
		return walletv1.LedgerReason_LEDGER_REASON_ADJUSTMENT
	default:
		return walletv1.LedgerReason_LEDGER_REASON_UNSPECIFIED
	}
}

func fromWallet(view app.WalletView) *walletv1.Wallet {
	return &walletv1.Wallet{
		Id:        view.ID,
		UserId:    view.UserID,
		Balance:   fromMoney(view.Balance),
		Held:      fromMoney(view.Held),
		Available: fromMoney(view.Available),
		Status:    fromWalletStatus(view.Status),
		Version:   view.Version,
		CreatedAt: formatTime(view.CreatedAt),
		UpdatedAt: formatTime(view.UpdatedAt),
	}
}

func fromLedgerEntry(view app.LedgerEntryView) *walletv1.LedgerEntry {
	return &walletv1.LedgerEntry{
		Id:            view.ID,
		WalletId:      view.WalletID,
		Direction:     fromDirection(view.Direction),
		Amount:        fromMoney(view.Amount),
		BalanceAfter:  fromMoney(view.BalanceAfter),
		Reason:        fromReason(view.Reason),
		ReferenceId:   view.ReferenceID,
		Description:   view.Description,
		CorrelationId: view.CorrelationID,
		CreatedAt:     formatTime(view.CreatedAt),
	}
}

func fromHoldStatus(status hold.Status) walletv1.HoldStatus {
	switch status {
	case hold.StatusActive:
		return walletv1.HoldStatus_HOLD_STATUS_ACTIVE
	case hold.StatusCaptured:
		return walletv1.HoldStatus_HOLD_STATUS_CAPTURED
	case hold.StatusReleased:
		return walletv1.HoldStatus_HOLD_STATUS_RELEASED
	case hold.StatusExpired:
		return walletv1.HoldStatus_HOLD_STATUS_EXPIRED
	default:
		return walletv1.HoldStatus_HOLD_STATUS_UNSPECIFIED
	}
}

func fromHold(view app.HoldView) *walletv1.Hold {
	return &walletv1.Hold{
		Id:          view.ID,
		WalletId:    view.WalletID,
		Amount:      fromMoney(view.Amount),
		Status:      fromHoldStatus(view.Status),
		ReferenceId: view.ReferenceID,
		Reason:      view.Reason,
		ExpiresAt:   formatOptionalTime(view.ExpiresAt),
		CreatedAt:   formatTime(view.CreatedAt),
		ResolvedAt:  formatOptionalTime(view.ResolvedAt),
	}
}

func fromGiftCardStatus(status giftcard.Status) walletv1.GiftCardStatus {
	switch status {
	case giftcard.StatusActive:
		return walletv1.GiftCardStatus_GIFT_CARD_STATUS_ACTIVE
	case giftcard.StatusUsed:
		return walletv1.GiftCardStatus_GIFT_CARD_STATUS_USED
	case giftcard.StatusRevoked:
		return walletv1.GiftCardStatus_GIFT_CARD_STATUS_REVOKED
	default:
		return walletv1.GiftCardStatus_GIFT_CARD_STATUS_UNSPECIFIED
	}
}

func fromGiftCard(view app.GiftCardView) *walletv1.GiftCard {
	return &walletv1.GiftCard{
		// Code is only ever non-empty in the response to the call that minted the card.
		Code:       view.Code,
		Id:         view.ID,
		Value:      fromMoney(view.Value),
		Status:     fromGiftCardStatus(view.Status),
		IssuedBy:   view.IssuedBy,
		RedeemedBy: view.RedeemedBy,
		BatchId:    view.BatchID,
		Note:       view.Note,
		CreatedAt:  formatTime(view.CreatedAt),
		RedeemedAt: formatOptionalTime(view.RedeemedAt),
	}
}

func fromDiscountStatus(status discount.Status) walletv1.DiscountCodeStatus {
	switch status {
	case discount.StatusActive:
		return walletv1.DiscountCodeStatus_DISCOUNT_CODE_STATUS_ACTIVE
	case discount.StatusUsed:
		return walletv1.DiscountCodeStatus_DISCOUNT_CODE_STATUS_USED
	case discount.StatusExpired:
		return walletv1.DiscountCodeStatus_DISCOUNT_CODE_STATUS_EXPIRED
	case discount.StatusRevoked:
		return walletv1.DiscountCodeStatus_DISCOUNT_CODE_STATUS_REVOKED
	default:
		return walletv1.DiscountCodeStatus_DISCOUNT_CODE_STATUS_UNSPECIFIED
	}
}

func fromDiscountCode(view app.DiscountCodeView) *walletv1.DiscountCode {
	return &walletv1.DiscountCode{
		Id:              view.ID,
		Code:            view.Code,
		PercentBps:      view.PercentBps,
		AmountOff:       fromMoney(view.AmountOff),
		MaxDiscount:     fromMoney(view.MaxDiscount),
		MinOrderAmount:  fromMoney(view.MinOrderAmount),
		Status:          fromDiscountStatus(view.Status),
		MaxRedemptions:  view.MaxRedemptions,
		RedemptionCount: view.RedemptionCount,
		IssuedBy:        view.IssuedBy,
		ExpiresAt:       formatOptionalTime(view.ExpiresAt),
		CreatedAt:       formatTime(view.CreatedAt),
	}
}

func fromTransaction(result app.TransactionResult) *walletv1.TransactionResponse {
	return &walletv1.TransactionResponse{
		Entry:            fromLedgerEntry(result.Entry),
		Wallet:           fromWallet(result.Wallet),
		IdempotentReplay: result.IdempotentReplay,
	}
}

func fromPage(totalItems int64, page, pageSize, totalPages int) *commonv1.PageResponse {
	return &commonv1.PageResponse{
		Page:       int32(page),
		PageSize:   int32(pageSize),
		TotalItems: totalItems,
		TotalPages: int32(totalPages),
	}
}

// formatTime renders a timestamp as RFC3339 with nanosecond precision, or "" for a
// zero value.
func formatTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func formatOptionalTime(at *time.Time) string {
	if at == nil {
		return ""
	}
	return formatTime(*at)
}
