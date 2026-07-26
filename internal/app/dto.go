// Package app contains the use cases: the orchestration layer that loads
// aggregates, calls their methods, records the resulting movements in the ledger,
// and publishes the events other services react to.
//
// Every use case here is transport-agnostic. The gRPC server, the REST handlers
// and the Kafka consumers are three thin adapters over this same layer, which is
// what makes switching a caller from gRPC to REST a configuration change rather
// than a rewrite.
package app

import (
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// The DTOs below are the application layer's own vocabulary. Aggregates are not
// returned directly for two reasons: their fields are unexported by design, and an
// idempotent replay has to be able to serialise a past result to JSON and hand it
// back verbatim.

// WalletView is a wallet as a caller sees it.
type WalletView struct {
	ID        string        `json:"id"`
	UserID    string        `json:"user_id"`
	Balance   money.Money   `json:"balance"`
	Held      money.Money   `json:"held"`
	Available money.Money   `json:"available"`
	Status    wallet.Status `json:"status"`
	Version   int64         `json:"version"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

func newWalletView(w *wallet.Wallet) WalletView {
	return WalletView{
		ID:        w.ID(),
		UserID:    w.UserID(),
		Balance:   w.Balance(),
		Held:      w.Held(),
		Available: w.Available(),
		Status:    w.Status(),
		Version:   w.Version(),
		CreatedAt: w.CreatedAt(),
		UpdatedAt: w.UpdatedAt(),
	}
}

// LedgerEntryView is one ledger line as a caller sees it.
type LedgerEntryView struct {
	ID            string           `json:"id"`
	Sequence      int64            `json:"sequence"`
	WalletID      string           `json:"wallet_id"`
	Direction     wallet.Direction `json:"direction"`
	Amount        money.Money      `json:"amount"`
	BalanceAfter  money.Money      `json:"balance_after"`
	Reason        wallet.Reason    `json:"reason"`
	ReferenceID   string           `json:"reference_id,omitempty"`
	Description   string           `json:"description,omitempty"`
	CorrelationID string           `json:"correlation_id,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
}

func newLedgerEntryView(entry ledger.Entry) LedgerEntryView {
	return LedgerEntryView{
		ID:            entry.ID,
		Sequence:      entry.Sequence,
		WalletID:      entry.WalletID,
		Direction:     entry.Direction,
		Amount:        entry.Amount,
		BalanceAfter:  entry.BalanceAfter,
		Reason:        entry.Reason,
		ReferenceID:   entry.ReferenceID,
		Description:   entry.Description,
		CorrelationID: entry.CorrelationID,
		CreatedAt:     entry.CreatedAt,
	}
}

// TransactionResult is what a debit, credit, capture or adjustment returns.
type TransactionResult struct {
	Entry  LedgerEntryView `json:"entry"`
	Wallet WalletView      `json:"wallet"`
	// IdempotentReplay reports that this response was reconstructed from an earlier
	// identical request and that no new money moved.
	IdempotentReplay bool `json:"idempotent_replay"`
}

// TransferResult is what a two-sided transfer returns.
type TransferResult struct {
	DebitEntry       LedgerEntryView `json:"debit_entry"`
	CreditEntry      LedgerEntryView `json:"credit_entry"`
	IdempotentReplay bool            `json:"idempotent_replay"`
}

// HoldView is a hold as a caller sees it.
type HoldView struct {
	ID             string      `json:"id"`
	WalletID       string      `json:"wallet_id"`
	UserID         string      `json:"user_id"`
	Amount         money.Money `json:"amount"`
	CapturedAmount money.Money `json:"captured_amount"`
	Remaining      money.Money `json:"remaining"`
	Status         hold.Status `json:"status"`
	ReferenceID    string      `json:"reference_id"`
	Reason         string      `json:"reason,omitempty"`
	ExpiresAt      *time.Time  `json:"expires_at,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	ResolvedAt     *time.Time  `json:"resolved_at,omitempty"`
}

func newHoldView(h *hold.Hold) HoldView {
	return HoldView{
		ID:             h.ID(),
		WalletID:       h.WalletID(),
		UserID:         h.UserID(),
		Amount:         h.Amount(),
		CapturedAmount: h.CapturedAmount(),
		Remaining:      h.Remaining(),
		Status:         h.Status(),
		ReferenceID:    h.ReferenceID(),
		Reason:         h.Reason(),
		ExpiresAt:      h.ExpiresAt(),
		CreatedAt:      h.CreatedAt(),
		ResolvedAt:     h.ResolvedAt(),
	}
}

// HoldResult is what placing or releasing a hold returns.
type HoldResult struct {
	Hold             HoldView   `json:"hold"`
	Wallet           WalletView `json:"wallet"`
	IdempotentReplay bool       `json:"idempotent_replay"`
}

// GiftCardView is a gift card as staff sees it.
//
// Code is populated only in the response to the issuance call that created the
// card. Every later read leaves it empty, because only the hash is stored.
type GiftCardView struct {
	ID         string          `json:"id"`
	Code       string          `json:"code,omitempty"`
	CodeHint   string          `json:"code_hint"`
	Value      money.Money     `json:"value"`
	Status     giftcard.Status `json:"status"`
	IssuedBy   string          `json:"issued_by"`
	BatchID    string          `json:"batch_id,omitempty"`
	Note       string          `json:"note,omitempty"`
	RedeemedBy string          `json:"redeemed_by,omitempty"`
	RedeemedAt *time.Time      `json:"redeemed_at,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

func newGiftCardView(card *giftcard.GiftCard, plaintextCode string) GiftCardView {
	return GiftCardView{
		ID:         card.ID(),
		Code:       plaintextCode,
		CodeHint:   card.CodeHint(),
		Value:      card.Value(),
		Status:     card.Status(),
		IssuedBy:   card.IssuedBy(),
		BatchID:    card.BatchID(),
		Note:       card.Note(),
		RedeemedBy: card.RedeemedBy(),
		RedeemedAt: card.RedeemedAt(),
		CreatedAt:  card.CreatedAt(),
	}
}

// IssueGiftCardsResult is the outcome of minting a batch.
type IssueGiftCardsResult struct {
	BatchID   string         `json:"batch_id"`
	GiftCards []GiftCardView `json:"gift_cards"`
	// IdempotentReplay is always accompanied by empty Code fields: a replayed
	// issuance cannot re-reveal codes that were never stored.
	IdempotentReplay bool `json:"idempotent_replay"`
}

// RedeemGiftCardResult is the outcome of redeeming a card.
type RedeemGiftCardResult struct {
	Credited         money.Money     `json:"credited"`
	Wallet           WalletView      `json:"wallet"`
	Entry            LedgerEntryView `json:"entry"`
	IdempotentReplay bool            `json:"idempotent_replay"`
}

// DiscountCodeView is a discount code as a caller sees it.
type DiscountCodeView struct {
	ID              string          `json:"id"`
	Code            string          `json:"code"`
	PercentBps      int32           `json:"percent_bps,omitempty"`
	AmountOff       money.Money     `json:"amount_off,omitempty"`
	MaxDiscount     money.Money     `json:"max_discount,omitempty"`
	MinOrderAmount  money.Money     `json:"min_order_amount,omitempty"`
	Status          discount.Status `json:"status"`
	MaxRedemptions  int32           `json:"max_redemptions"`
	RedemptionCount int32           `json:"redemption_count"`
	IssuedBy        string          `json:"issued_by"`
	ExpiresAt       *time.Time      `json:"expires_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

func newDiscountCodeView(code *discount.Code) DiscountCodeView {
	return DiscountCodeView{
		ID:              code.ID(),
		Code:            code.Code(),
		PercentBps:      code.PercentBps(),
		AmountOff:       code.AmountOff(),
		MaxDiscount:     code.MaxDiscount(),
		MinOrderAmount:  code.MinOrderAmount(),
		Status:          code.Status(),
		MaxRedemptions:  code.MaxRedemptions(),
		RedemptionCount: code.RedemptionCount(),
		IssuedBy:        code.IssuedBy(),
		ExpiresAt:       code.ExpiresAt(),
		CreatedAt:       code.CreatedAt(),
	}
}

// DiscountQuote is what previewing or redeeming a discount code returns.
type DiscountQuote struct {
	Discount         money.Money       `json:"discount"`
	Payable          money.Money       `json:"payable"`
	Code             *DiscountCodeView `json:"code,omitempty"`
	IdempotentReplay bool              `json:"idempotent_replay"`
}

// ChargeResult is what starting a bank top-up returns.
type ChargeResult struct {
	PaymentIntentID  string      `json:"payment_intent_id"`
	RedirectURL      string      `json:"redirect_url"`
	Amount           money.Money `json:"amount"`
	ExpiresAt        *time.Time  `json:"expires_at,omitempty"`
	IdempotentReplay bool        `json:"idempotent_replay"`
}

// LedgerPage is a paginated ledger read.
type LedgerPage struct {
	Entries    []LedgerEntryView `json:"entries"`
	TotalItems int64             `json:"total_items"`
	Page       int               `json:"page"`
	PageSize   int               `json:"page_size"`
	TotalPages int               `json:"total_pages"`
}

// HoldPage is a paginated hold read.
type HoldPage struct {
	Holds      []HoldView `json:"holds"`
	TotalItems int64      `json:"total_items"`
	Page       int        `json:"page"`
	PageSize   int        `json:"page_size"`
	TotalPages int        `json:"total_pages"`
}

// GiftCardPage is a paginated gift-card read.
type GiftCardPage struct {
	GiftCards  []GiftCardView `json:"gift_cards"`
	TotalItems int64          `json:"total_items"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	TotalPages int            `json:"total_pages"`
}

// ReconcileResult is the outcome of a reconciliation sweep.
type ReconcileResult struct {
	WalletsChecked int64               `json:"wallets_checked"`
	Mismatches     []ReconcileMismatch `json:"mismatches"`
}

// ReconcileMismatch is one wallet whose balance disagrees with its ledger.
type ReconcileMismatch struct {
	WalletID      string      `json:"wallet_id"`
	UserID        string      `json:"user_id"`
	StoredBalance money.Money `json:"stored_balance"`
	LedgerBalance money.Money `json:"ledger_balance"`
	Delta         money.Money `json:"delta"`
}

// AccrueInterestResult summarises an interest run.
type AccrueInterestResult struct {
	WalletsProcessed int64       `json:"wallets_processed"`
	WalletsCredited  int64       `json:"wallets_credited"`
	TotalInterest    money.Money `json:"total_interest"`
	AccrualDate      string      `json:"accrual_date"`
	DryRun           bool        `json:"dry_run"`
}

// pageInfo converts an offset/limit pair and a total into page metadata.
func pageInfo(totalItems int64, limit, offset int) (page, pageSize, totalPages int) {
	if limit <= 0 {
		limit = defaultPageSize
	}
	pageSize = limit
	page = offset/limit + 1
	totalPages = int((totalItems + int64(limit) - 1) / int64(limit))
	if totalPages == 0 {
		totalPages = 1
	}
	return page, pageSize, totalPages
}
