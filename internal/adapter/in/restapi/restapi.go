// Package restapi is the REST inbound adapter.
//
// It exists so that switching a caller from gRPC to REST is a configuration change
// rather than a rewrite. Both adapters sit on the identical application layer: the
// same use cases, the same validation, the same authorisation, the same idempotency.
// Nothing here decides anything — it only translates JSON and HTTP into commands.
//
// Set SERVER_MODE=http to serve only this, grpc for only gRPC, or both to run them
// side by side.
package restapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/httpx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// API holds the handlers.
type API struct {
	wallets   *app.WalletService
	giftCards *app.GiftCardService
	discounts *app.DiscountService
	charges   *app.ChargeService
	admin     *app.AdminService
	currency  string
}

// New builds the API.
func New(
	wallets *app.WalletService,
	giftCards *app.GiftCardService,
	discounts *app.DiscountService,
	charges *app.ChargeService,
	admin *app.AdminService,
	currency string,
) *API {
	return &API{
		wallets:   wallets,
		giftCards: giftCards,
		discounts: discounts,
		charges:   charges,
		admin:     admin,
		currency:  currency,
	}
}

// Routes registers every endpoint on a mux.
//
// The paths are versioned (/v1/...) from day one. Contract versioning is one of the
// maintainability commitments in the architecture document, and retrofitting a
// version prefix after clients exist is a breaking change.
func (a *API) Routes(mux *http.ServeMux) {
	// User-facing wallet operations.
	mux.HandleFunc("GET /v1/wallets/me", a.getMyWallet)
	mux.HandleFunc("GET /v1/wallets/me/ledger", a.listMyLedger)
	mux.HandleFunc("GET /v1/wallets/me/holds", a.listMyHolds)
	mux.HandleFunc("POST /v1/wallets/me/charges", a.initiateCharge)
	mux.HandleFunc("POST /v1/wallets/me/gift-cards/redeem", a.redeemGiftCard)

	// Staff and service reads of another user's wallet.
	mux.HandleFunc("GET /v1/wallets/{userID}", a.getWallet)
	mux.HandleFunc("GET /v1/wallets/{userID}/ledger", a.listLedger)

	// Saga participants, called by the Store and Marketplace services.
	mux.HandleFunc("POST /v1/wallets/{userID}/debit", a.debit)
	mux.HandleFunc("POST /v1/wallets/{userID}/credit", a.credit)
	mux.HandleFunc("POST /v1/transfers", a.transfer)

	// Holds.
	mux.HandleFunc("POST /v1/wallets/{userID}/holds", a.holdFunds)
	mux.HandleFunc("POST /v1/holds/{holdID}/capture", a.captureHold)
	mux.HandleFunc("POST /v1/holds/{holdID}/release", a.releaseHold)

	// Gift cards, Support only.
	mux.HandleFunc("POST /v1/gift-cards", a.issueGiftCards)
	mux.HandleFunc("GET /v1/gift-cards", a.listGiftCards)
	mux.HandleFunc("GET /v1/gift-cards/{id}", a.getGiftCard)
	mux.HandleFunc("POST /v1/gift-cards/{id}/revoke", a.revokeGiftCard)

	// Discount codes.
	mux.HandleFunc("POST /v1/discount-codes", a.issueDiscountCode)
	mux.HandleFunc("GET /v1/discount-codes/{code}", a.getDiscountCode)
	mux.HandleFunc("POST /v1/discount-codes/{code}/preview", a.previewDiscount)
	mux.HandleFunc("POST /v1/discount-codes/{code}/redeem", a.redeemDiscount)

	// Operations, Admin only.
	mux.HandleFunc("POST /v1/admin/reconcile", a.reconcile)
	mux.HandleFunc("POST /v1/admin/interest/accrue", a.accrueInterest)
	mux.HandleFunc("POST /v1/admin/wallets/{userID}/freeze", a.freezeWallet)
	mux.HandleFunc("POST /v1/admin/wallets/{userID}/unfreeze", a.unfreezeWallet)
	mux.HandleFunc("POST /v1/admin/wallets/{userID}/adjust", a.adjust)
}

// --- Request bodies -------------------------------------------------------
//
// These are separate from the application commands on purpose. A wire format is a
// contract with clients and changes on their schedule; a command is internal and
// changes on ours. Coupling them would mean every internal rename is a breaking API
// change.

type movementRequest struct {
	Amount      money.Money `json:"amount"`
	Reason      string      `json:"reason"`
	ReferenceID string      `json:"reference_id"`
	Description string      `json:"description,omitempty"`
}

type transferRequest struct {
	FromUserID  string      `json:"from_user_id"`
	ToUserID    string      `json:"to_user_id"`
	Amount      money.Money `json:"amount"`
	Reason      string      `json:"reason"`
	ReferenceID string      `json:"reference_id"`
	Description string      `json:"description,omitempty"`
}

type chargeRequest struct {
	Amount    money.Money `json:"amount"`
	ReturnURL string      `json:"return_url,omitempty"`
}

type redeemGiftCardRequest struct {
	Code string `json:"code"`
}

type holdRequest struct {
	Amount      money.Money `json:"amount"`
	ReferenceID string      `json:"reference_id"`
	Reason      string      `json:"reason,omitempty"`
	TTLSeconds  int64       `json:"ttl_seconds,omitempty"`
}

type captureHoldRequest struct {
	// Amount is optional; omitting it captures everything remaining.
	Amount *money.Money `json:"amount,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

type issueGiftCardsRequest struct {
	Value    money.Money `json:"value"`
	Quantity int         `json:"quantity,omitempty"`
	Note     string      `json:"note,omitempty"`
}

type revokeRequest struct {
	Reason string `json:"reason"`
}

type issueDiscountRequest struct {
	Code           string       `json:"code,omitempty"`
	PercentBps     int32        `json:"percent_bps,omitempty"`
	AmountOff      *money.Money `json:"amount_off,omitempty"`
	MaxDiscount    *money.Money `json:"max_discount,omitempty"`
	MinOrderAmount *money.Money `json:"min_order_amount,omitempty"`
	MaxRedemptions int32        `json:"max_redemptions,omitempty"`
	ExpiresAt      string       `json:"expires_at,omitempty"`
}

type discountQuoteRequest struct {
	OrderAmount money.Money `json:"order_amount"`
	ReferenceID string      `json:"reference_id,omitempty"`
}

type accrueInterestRequest struct {
	AccrualDate   string `json:"accrual_date,omitempty"`
	AnnualRateBps int32  `json:"annual_rate_bps,omitempty"`
	DryRun        bool   `json:"dry_run,omitempty"`
}

type statusChangeRequest struct {
	Reason string `json:"reason,omitempty"`
}

type adjustRequest struct {
	Direction string      `json:"direction"`
	Amount    money.Money `json:"amount"`
	Reason    string      `json:"reason"`
}

// --- Wallet handlers ------------------------------------------------------

func (a *API) getMyWallet(w http.ResponseWriter, r *http.Request) {
	principal, err := requirePrincipal(r)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	view, err := a.wallets.GetOrCreateWallet(r.Context(), principal)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *API) getWallet(w http.ResponseWriter, r *http.Request) {
	userID, err := httpx.PathValue(r, "userID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	view, err := a.wallets.GetWallet(r.Context(), userID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *API) listMyLedger(w http.ResponseWriter, r *http.Request) {
	a.writeLedger(w, r, "")
}

func (a *API) listLedger(w http.ResponseWriter, r *http.Request) {
	userID, err := httpx.PathValue(r, "userID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	a.writeLedger(w, r, userID)
}

func (a *API) writeLedger(w http.ResponseWriter, r *http.Request, userID string) {
	query, err := a.ledgerQuery(r, userID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	page, err := a.wallets.ListLedger(r.Context(), query)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (a *API) ledgerQuery(r *http.Request, userID string) (app.ListLedgerQuery, error) {
	page, err := httpx.QueryInt(r, "page", 1)
	if err != nil {
		return app.ListLedgerQuery{}, err
	}
	pageSize, err := httpx.QueryInt(r, "page_size", 0)
	if err != nil {
		return app.ListLedgerQuery{}, err
	}

	query := app.ListLedgerQuery{
		UserID:      userID,
		ReferenceID: r.URL.Query().Get("reference_id"),
		Page:        page,
		PageSize:    pageSize,
	}

	if raw := r.URL.Query().Get("direction"); raw != "" {
		direction := wallet.Direction(strings.ToUpper(raw))
		if direction != wallet.DirectionDebit && direction != wallet.DirectionCredit {
			return app.ListLedgerQuery{}, errs.InvalidArgument("direction must be DEBIT or CREDIT, got %q", raw)
		}
		query.Direction = direction
	}

	for _, raw := range r.URL.Query()["reason"] {
		reason := wallet.Reason(strings.ToUpper(strings.TrimSpace(raw)))
		if !reason.Valid() {
			return app.ListLedgerQuery{}, errs.InvalidArgument("unknown reason %q", raw)
		}
		query.Reasons = append(query.Reasons, reason)
	}

	from, err := parseQueryTime(r, "from")
	if err != nil {
		return app.ListLedgerQuery{}, err
	}
	to, err := parseQueryTime(r, "to")
	if err != nil {
		return app.ListLedgerQuery{}, err
	}
	query.From, query.To = from, to
	return query, nil
}

func (a *API) listMyHolds(w http.ResponseWriter, r *http.Request) {
	page, err := httpx.QueryInt(r, "page", 1)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pageSize, err := httpx.QueryInt(r, "page_size", 0)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	query := app.ListHoldsQuery{Page: page, PageSize: pageSize}
	if raw := r.URL.Query().Get("status"); raw != "" {
		status := hold.Status(strings.ToUpper(raw))
		if !status.Valid() {
			httpx.WriteError(w, r, errs.InvalidArgument("unknown hold status %q", raw))
			return
		}
		query.Status = status
	}

	result, err := a.wallets.ListHolds(r.Context(), query)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) debit(w http.ResponseWriter, r *http.Request) {
	userID, err := httpx.PathValue(r, "userID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body movementRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	reason, err := parseReason(body.Reason)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	result, err := a.wallets.Debit(r.Context(), app.DebitCommand{
		UserID:         userID,
		Amount:         body.Amount,
		Reason:         reason,
		ReferenceID:    body.ReferenceID,
		Description:    body.Description,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) credit(w http.ResponseWriter, r *http.Request) {
	userID, err := httpx.PathValue(r, "userID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body movementRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	reason, err := parseReason(body.Reason)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	result, err := a.wallets.Credit(r.Context(), app.CreditCommand{
		UserID:         userID,
		Amount:         body.Amount,
		Reason:         reason,
		ReferenceID:    body.ReferenceID,
		Description:    body.Description,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) transfer(w http.ResponseWriter, r *http.Request) {
	var body transferRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	reason, err := parseReason(body.Reason)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	result, err := a.wallets.Transfer(r.Context(), app.TransferCommand{
		FromUserID:     body.FromUserID,
		ToUserID:       body.ToUserID,
		Amount:         body.Amount,
		Reason:         reason,
		ReferenceID:    body.ReferenceID,
		Description:    body.Description,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) initiateCharge(w http.ResponseWriter, r *http.Request) {
	var body chargeRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	result, err := a.charges.InitiateCharge(r.Context(), app.InitiateChargeCommand{
		Amount:         body.Amount,
		ReturnURL:      body.ReturnURL,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// 201: a payment intent was created at the gateway as a side effect.
	httpx.WriteJSON(w, http.StatusCreated, result)
}

// --- Hold handlers --------------------------------------------------------

func (a *API) holdFunds(w http.ResponseWriter, r *http.Request) {
	userID, err := httpx.PathValue(r, "userID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body holdRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	result, err := a.wallets.HoldFunds(r.Context(), app.HoldFundsCommand{
		UserID:         userID,
		Amount:         body.Amount,
		ReferenceID:    body.ReferenceID,
		Reason:         body.Reason,
		TTL:            time.Duration(body.TTLSeconds) * time.Second,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, result)
}

func (a *API) captureHold(w http.ResponseWriter, r *http.Request) {
	holdID, err := httpx.PathValue(r, "holdID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body captureHoldRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	cmd := app.CaptureHoldCommand{
		HoldID:         holdID,
		IdempotencyKey: httpx.IdempotencyKey(r),
	}
	if body.Amount != nil {
		cmd.Amount = *body.Amount
	}
	if body.Reason != "" {
		reason, err := parseReason(body.Reason)
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		cmd.Reason = reason
	}

	result, err := a.wallets.CaptureHold(r.Context(), cmd)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) releaseHold(w http.ResponseWriter, r *http.Request) {
	holdID, err := httpx.PathValue(r, "holdID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	result, err := a.wallets.ReleaseHold(r.Context(), app.ReleaseHoldCommand{
		HoldID:         holdID,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

// --- Gift card handlers ---------------------------------------------------

func (a *API) issueGiftCards(w http.ResponseWriter, r *http.Request) {
	var body issueGiftCardsRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if body.Quantity == 0 {
		body.Quantity = 1
	}

	result, err := a.giftCards.IssueGiftCards(r.Context(), app.IssueGiftCardsCommand{
		Value:          body.Value,
		Quantity:       body.Quantity,
		Note:           body.Note,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, result)
}

func (a *API) redeemGiftCard(w http.ResponseWriter, r *http.Request) {
	var body redeemGiftCardRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	result, err := a.giftCards.RedeemGiftCard(r.Context(), app.RedeemGiftCardCommand{
		Code:           body.Code,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) getGiftCard(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathValue(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	view, err := a.giftCards.GetGiftCard(r.Context(), id, "")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *API) listGiftCards(w http.ResponseWriter, r *http.Request) {
	page, err := httpx.QueryInt(r, "page", 1)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pageSize, err := httpx.QueryInt(r, "page_size", 0)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	status := giftcard.Status("")
	if raw := r.URL.Query().Get("status"); raw != "" {
		status = giftcard.Status(strings.ToUpper(raw))
		if !status.Valid() {
			httpx.WriteError(w, r, errs.InvalidArgument("unknown gift card status %q", raw))
			return
		}
	}

	result, err := a.giftCards.ListGiftCards(r.Context(), status, r.URL.Query().Get("batch_id"), page, pageSize)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) revokeGiftCard(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathValue(r, "id")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body revokeRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	view, err := a.giftCards.RevokeGiftCard(r.Context(), id, body.Reason)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

// --- Discount handlers ----------------------------------------------------

func (a *API) issueDiscountCode(w http.ResponseWriter, r *http.Request) {
	var body issueDiscountRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	expiresAt, err := parseOptionalRFC3339(body.ExpiresAt, "expires_at")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	cmd := app.IssueDiscountCodeCommand{
		Code:           body.Code,
		PercentBps:     body.PercentBps,
		MaxRedemptions: body.MaxRedemptions,
		ExpiresAt:      expiresAt,
		IdempotencyKey: httpx.IdempotencyKey(r),
	}
	if body.AmountOff != nil {
		cmd.AmountOff = *body.AmountOff
	}
	if body.MaxDiscount != nil {
		cmd.MaxDiscount = *body.MaxDiscount
	}
	if body.MinOrderAmount != nil {
		cmd.MinOrderAmount = *body.MinOrderAmount
	}

	view, err := a.discounts.IssueDiscountCode(r.Context(), cmd)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (a *API) getDiscountCode(w http.ResponseWriter, r *http.Request) {
	code, err := httpx.PathValue(r, "code")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	view, err := a.discounts.GetDiscountCode(r.Context(), code)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *API) previewDiscount(w http.ResponseWriter, r *http.Request) {
	code, err := httpx.PathValue(r, "code")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body discountQuoteRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	quote, err := a.discounts.PreviewDiscount(r.Context(), code, body.OrderAmount)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, quote)
}

func (a *API) redeemDiscount(w http.ResponseWriter, r *http.Request) {
	code, err := httpx.PathValue(r, "code")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body discountQuoteRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	quote, err := a.discounts.RedeemDiscountCode(r.Context(), app.RedeemDiscountCodeCommand{
		Code:           code,
		OrderAmount:    body.OrderAmount,
		ReferenceID:    body.ReferenceID,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, quote)
}

// --- Admin handlers -------------------------------------------------------

func (a *API) reconcile(w http.ResponseWriter, r *http.Request) {
	result, err := a.admin.Reconcile(r.Context(), r.URL.Query().Get("user_id"))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// A report of mismatches is still a successful report. The alert comes from the
	// metric, not from the status code.
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) accrueInterest(w http.ResponseWriter, r *http.Request) {
	var body accrueInterestRequest
	// An empty body is valid: accrue today at the configured rate.
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	result, err := a.admin.AccrueInterest(r.Context(), app.AccrueInterestCommand{
		AccrualDate:   body.AccrualDate,
		AnnualRateBps: body.AnnualRateBps,
		DryRun:        body.DryRun,
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

func (a *API) freezeWallet(w http.ResponseWriter, r *http.Request) {
	a.changeStatus(w, r, true)
}

func (a *API) unfreezeWallet(w http.ResponseWriter, r *http.Request) {
	a.changeStatus(w, r, false)
}

func (a *API) changeStatus(w http.ResponseWriter, r *http.Request, freeze bool) {
	userID, err := httpx.PathValue(r, "userID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body statusChangeRequest
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
	}

	var view app.WalletView
	if freeze {
		view, err = a.admin.FreezeWallet(r.Context(), userID, body.Reason)
	} else {
		view, err = a.admin.UnfreezeWallet(r.Context(), userID, body.Reason)
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (a *API) adjust(w http.ResponseWriter, r *http.Request) {
	userID, err := httpx.PathValue(r, "userID")
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var body adjustRequest
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	direction := wallet.Direction(strings.ToUpper(strings.TrimSpace(body.Direction)))
	if direction != wallet.DirectionDebit && direction != wallet.DirectionCredit {
		httpx.WriteError(w, r, errs.InvalidArgument("direction must be DEBIT or CREDIT, got %q", body.Direction))
		return
	}

	result, err := a.admin.Adjust(r.Context(), app.AdjustCommand{
		UserID:         userID,
		Direction:      direction,
		Amount:         body.Amount,
		Reason:         body.Reason,
		IdempotencyKey: httpx.IdempotencyKey(r),
	})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, result)
}

// --- Helpers --------------------------------------------------------------

func parseReason(raw string) (wallet.Reason, error) {
	reason := wallet.Reason(strings.ToUpper(strings.TrimSpace(raw)))
	if !reason.Valid() {
		return "", errs.InvalidArgument("unknown ledger reason %q", raw)
	}
	return reason, nil
}

func parseQueryTime(r *http.Request, name string) (*time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// Accept a bare date too, which is what somebody hand-writing a query reaches for.
		parsed, err = time.Parse("2006-01-02", raw)
		if err != nil {
			return nil, errs.InvalidArgument("%s must be an RFC3339 timestamp or a YYYY-MM-DD date, got %q", name, raw)
		}
	}
	return &parsed, nil
}

func parseOptionalRFC3339(raw, field string) (*time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, errs.InvalidArgument("%s must be an RFC3339 timestamp, got %q", field, raw)
	}
	return &parsed, nil
}

// requirePrincipal returns the authenticated caller's user id.
func requirePrincipal(r *http.Request) (string, error) {
	principal, err := authn.RequirePrincipal(r.Context())
	if err != nil {
		return "", err
	}
	return principal.UserID, nil
}
