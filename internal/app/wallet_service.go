package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/clock"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// Paging defaults applied to every list endpoint.
const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// Operation names, used to namespace idempotency keys and label metrics. Two
// different operations may safely share a key value.
const (
	opDebit          = "debit"
	opCredit         = "credit"
	opTransfer       = "transfer"
	opHoldFunds      = "hold_funds"
	opCaptureHold    = "capture_hold"
	opReleaseHold    = "release_hold"
	opAdjust         = "adjust"
	opRedeemGiftCard = "redeem_gift_card"
	opIssueGiftCards = "issue_gift_cards"
	opRedeemDiscount = "redeem_discount"
	opInitiateCharge = "initiate_charge"
	opConfirmCharge  = "confirm_charge"
	opAccrueInterest = "accrue_interest"
)

// Deps is everything the use cases need. Passing one struct keeps the constructor
// signatures stable as the service grows.
type Deps struct {
	TxManager    port.TxManager
	Reader       port.Reader
	Wallets      port.WalletRepository
	Ledger       port.LedgerRepository
	GiftCards    port.GiftCardRepository
	Discounts    port.DiscountRepository
	Holds        port.HoldRepository
	Idempotency  port.IdempotencyStore
	Publisher    port.EventPublisher
	PaymentGW    port.PaymentGateway
	AbuseLimiter port.AbuseLimiter
	Metrics      port.Metrics
	Clock        clock.Clock
	IDs          idGenerator
	Logger       *slog.Logger
	// Currency is the platform's single operating currency. Multi-currency wallets
	// are out of scope: the money type is ready for them, the ledger schema is not.
	Currency string
	// Producer and SchemaVersion stamp published events.
	Producer      string
	SchemaVersion int
}

// WalletService implements the wallet use cases.
type WalletService struct {
	*core
}

// NewWalletService builds the service.
func NewWalletService(deps Deps) *WalletService {
	return &WalletService{core: newCore(deps)}
}

// --- Commands -------------------------------------------------------------

// DebitCommand asks for money to leave a wallet.
type DebitCommand struct {
	UserID         string
	Amount         money.Money
	Reason         wallet.Reason
	ReferenceID    string
	Description    string
	IdempotencyKey string
}

// CreditCommand asks for money to enter a wallet.
type CreditCommand struct {
	UserID         string
	Amount         money.Money
	Reason         wallet.Reason
	ReferenceID    string
	Description    string
	IdempotencyKey string
}

// TransferCommand moves money between two wallets atomically.
type TransferCommand struct {
	FromUserID     string
	ToUserID       string
	Amount         money.Money
	Reason         wallet.Reason
	ReferenceID    string
	Description    string
	IdempotencyKey string
}

// HoldFundsCommand reserves part of a balance.
type HoldFundsCommand struct {
	UserID         string
	Amount         money.Money
	ReferenceID    string
	Reason         string
	TTL            time.Duration
	IdempotencyKey string
}

// CaptureHoldCommand converts a reservation into a debit.
type CaptureHoldCommand struct {
	HoldID string
	// Amount of zero captures everything remaining on the hold.
	Amount         money.Money
	Reason         wallet.Reason
	IdempotencyKey string
}

// ReleaseHoldCommand returns a reservation to the available balance.
type ReleaseHoldCommand struct {
	HoldID         string
	IdempotencyKey string
}

// ListLedgerQuery filters a ledger read.
type ListLedgerQuery struct {
	UserID      string
	Reasons     []wallet.Reason
	Direction   wallet.Direction
	ReferenceID string
	From        *time.Time
	To          *time.Time
	Page        int
	PageSize    int
}

// ListHoldsQuery filters a hold read.
type ListHoldsQuery struct {
	UserID   string
	Status   hold.Status
	Page     int
	PageSize int
}

// --- Reads ----------------------------------------------------------------

// GetOrCreateWallet returns the caller's wallet, provisioning it on first access.
//
// Lazy provisioning is deliberate. A wallet is also created eagerly when Auth
// publishes UserRegistered, but that event can be delayed by a broker hiccup, and a
// user opening their wallet page must not see an error because of it. Both paths
// converge on the same unique constraint on user_id.
func (s *WalletService) GetOrCreateWallet(ctx context.Context, userID string) (WalletView, error) {
	if _, err := authn.RequireSelfOrStaff(ctx, userID); err != nil {
		return WalletView{}, err
	}
	if userID == "" {
		return WalletView{}, errs.InvalidArgument("a user id is required")
	}

	if existing, err := s.deps.Wallets.FindByUserID(ctx, s.deps.Reader, userID); err == nil {
		return newWalletView(existing), nil
	} else if !errs.Is(err, errs.CodeNotFound) {
		return WalletView{}, err
	}

	var view WalletView
	created := false

	err := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		// Re-check inside the transaction: another request for the same new user may
		// have won the race between the read above and this write.
		if existing, err := s.deps.Wallets.FindByUserID(ctx, tx, userID); err == nil {
			view = newWalletView(existing)
			return nil
		} else if !errs.Is(err, errs.CodeNotFound) {
			return err
		}

		now := s.deps.Clock.Now()
		w, err := wallet.New(s.deps.IDs.NewID(), userID, s.deps.Currency, now)
		if err != nil {
			return err
		}
		if err := s.deps.Wallets.Insert(ctx, tx, w); err != nil {
			// A concurrent creator beat us to the unique index. Load theirs instead of
			// failing a read that the user experiences as "show me my wallet".
			if errs.Is(err, errs.CodeAlreadyExists) {
				existing, findErr := s.deps.Wallets.FindByUserID(ctx, tx, userID)
				if findErr != nil {
					return findErr
				}
				view = newWalletView(existing)
				return nil
			}
			return err
		}

		if err := s.emit(ctx, tx, EventWalletCreated, aggregateWallet, w.ID(), now, WalletCreatedPayload{
			WalletID: w.ID(),
			UserID:   userID,
			Currency: w.Currency(),
		}); err != nil {
			return err
		}

		view = newWalletView(w)
		created = true
		return nil
	})
	if err != nil {
		return WalletView{}, err
	}

	if created {
		s.publisher.Notify()
		s.deps.Metrics.WalletOperation("provision", "success")
	}
	return view, nil
}

// EnsureWallet provisions a wallet without a request-scoped authorisation check.
//
// Only the UserRegistered consumer calls this: the event arrives from Auth over the
// broker, so there is no end user to authorise and the RBAC helpers would refuse a
// call carrying no principal.
func (s *WalletService) EnsureWallet(ctx context.Context, userID string) (WalletView, error) {
	if userID == "" {
		return WalletView{}, errs.InvalidArgument("a user id is required")
	}
	return s.GetOrCreateWallet(authn.WithPrincipal(ctx, authn.Principal{
		UserID: userID,
		Role:   authn.RoleService,
	}), userID)
}

// GetWallet reads a wallet without creating one.
func (s *WalletService) GetWallet(ctx context.Context, userID string) (WalletView, error) {
	if _, err := authn.RequireSelfOrStaff(ctx, userID); err != nil {
		return WalletView{}, err
	}
	w, err := s.deps.Wallets.FindByUserID(ctx, s.deps.Reader, userID)
	if err != nil {
		return WalletView{}, err
	}
	return newWalletView(w), nil
}

// ListLedger returns a page of a wallet's ledger: the transaction history a user
// sees, and the audit trail a support agent reads when a purchase is disputed.
func (s *WalletService) ListLedger(ctx context.Context, query ListLedgerQuery) (LedgerPage, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return LedgerPage{}, err
	}
	// An empty user id means "my own ledger".
	targetUserID := query.UserID
	if targetUserID == "" {
		targetUserID = principal.UserID
	}
	if _, err := authn.RequireSelfOrStaff(ctx, targetUserID); err != nil {
		return LedgerPage{}, err
	}

	w, err := s.deps.Wallets.FindByUserID(ctx, s.deps.Reader, targetUserID)
	if err != nil {
		return LedgerPage{}, err
	}

	limit, offset := paging(query.Page, query.PageSize)
	page, err := s.deps.Ledger.List(ctx, s.deps.Reader, ledger.Filter{
		WalletID:    w.ID(),
		Reasons:     query.Reasons,
		Direction:   query.Direction,
		ReferenceID: query.ReferenceID,
		From:        query.From,
		To:          query.To,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		return LedgerPage{}, err
	}

	views := make([]LedgerEntryView, 0, len(page.Entries))
	for _, entry := range page.Entries {
		views = append(views, newLedgerEntryView(entry))
	}
	pageNum, pageSize, totalPages := pageInfo(page.TotalItems, limit, offset)
	return LedgerPage{
		Entries:    views,
		TotalItems: page.TotalItems,
		Page:       pageNum,
		PageSize:   pageSize,
		TotalPages: totalPages,
	}, nil
}

// ListHolds returns a page of a wallet's holds.
func (s *WalletService) ListHolds(ctx context.Context, query ListHoldsQuery) (HoldPage, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return HoldPage{}, err
	}
	targetUserID := query.UserID
	if targetUserID == "" {
		targetUserID = principal.UserID
	}
	if _, err := authn.RequireSelfOrStaff(ctx, targetUserID); err != nil {
		return HoldPage{}, err
	}

	limit, offset := paging(query.Page, query.PageSize)
	holds, total, err := s.deps.Holds.List(ctx, s.deps.Reader, hold.Filter{
		UserID: targetUserID,
		Status: query.Status,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return HoldPage{}, err
	}

	views := make([]HoldView, 0, len(holds))
	for _, h := range holds {
		views = append(views, newHoldView(h))
	}
	pageNum, pageSize, totalPages := pageInfo(total, limit, offset)
	return HoldPage{
		Holds:      views,
		TotalItems: total,
		Page:       pageNum,
		PageSize:   pageSize,
		TotalPages: totalPages,
	}, nil
}

// --- Money movement -------------------------------------------------------

// Debit removes money from a wallet.
//
// This is step one of the purchase saga. It is idempotent, it locks the wallet row
// for the duration, and on a business rejection it publishes PaymentFailed so the
// orchestrator can terminate the saga without a compensation pass.
func (s *WalletService) Debit(ctx context.Context, cmd DebitCommand) (TransactionResult, error) {
	if err := s.authoriseMovement(ctx, cmd.UserID); err != nil {
		return TransactionResult{}, err
	}
	if err := validateMovement(cmd.UserID, cmd.Amount, cmd.Reason, cmd.IdempotencyKey); err != nil {
		return TransactionResult{}, err
	}

	result, err := s.applyMovement(ctx, movementRequest{
		Operation:      opDebit,
		UserID:         cmd.UserID,
		Amount:         cmd.Amount,
		Reason:         cmd.Reason,
		ReferenceID:    cmd.ReferenceID,
		Description:    cmd.Description,
		IdempotencyKey: cmd.IdempotencyKey,
		Direction:      wallet.DirectionDebit,
	})
	if err != nil {
		// A business rejection — not enough money, or a frozen wallet — is reported to
		// the saga as an event as well as an error, because the orchestrator listens on
		// the broker rather than holding the RPC open.
		if !errs.IsRetryable(err) {
			s.publishPaymentFailed(ctx, cmd, err)
		}
		return TransactionResult{}, err
	}
	return result, nil
}

// Credit adds money to a wallet: revenue share, refunds and gift-card value all
// arrive this way.
func (s *WalletService) Credit(ctx context.Context, cmd CreditCommand) (TransactionResult, error) {
	if err := s.authoriseMovement(ctx, cmd.UserID); err != nil {
		return TransactionResult{}, err
	}
	if err := validateMovement(cmd.UserID, cmd.Amount, cmd.Reason, cmd.IdempotencyKey); err != nil {
		return TransactionResult{}, err
	}

	return s.applyMovement(ctx, movementRequest{
		Operation:      opCredit,
		UserID:         cmd.UserID,
		Amount:         cmd.Amount,
		Reason:         cmd.Reason,
		ReferenceID:    cmd.ReferenceID,
		Description:    cmd.Description,
		IdempotencyKey: cmd.IdempotencyKey,
		Direction:      wallet.DirectionCredit,
	})
}

// DebitInternal performs a debit on behalf of an inbound Kafka command, attaching a
// service principal. Only consumers call it.
func (s *WalletService) DebitInternal(ctx context.Context, cmd DebitCommand) (TransactionResult, error) {
	return s.Debit(asService(ctx), cmd)
}

// CreditInternal is Credit for an inbound Kafka command.
func (s *WalletService) CreditInternal(ctx context.Context, cmd CreditCommand) (TransactionResult, error) {
	return s.Credit(asService(ctx), cmd)
}

// TransferInternal is Transfer for an inbound Kafka command.
func (s *WalletService) TransferInternal(ctx context.Context, cmd TransferCommand) (TransferResult, error) {
	return s.Transfer(asService(ctx), cmd)
}

// movementRequest is the internal shape shared by Debit, Credit and Adjust.
type movementRequest struct {
	Operation      string
	UserID         string
	Amount         money.Money
	Reason         wallet.Reason
	ReferenceID    string
	Description    string
	IdempotencyKey string
	Direction      wallet.Direction
}

// applyMovement is the single code path through which money enters or leaves one
// wallet.
func (s *WalletService) applyMovement(ctx context.Context, req movementRequest) (TransactionResult, error) {
	actorID := s.actorFrom(ctx)

	var (
		result TransactionResult
		replay bool
	)

	err := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, req.Operation, req.IdempotencyKey, req.UserID, req)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		// FOR UPDATE. Two concurrent debits on one wallet must not both read the same
		// balance, both pass the sufficiency check, and together overdraw the account.
		w, err := s.deps.Wallets.LockByUserID(ctx, tx, req.UserID)
		if err != nil {
			return err
		}
		versionAtLoad := w.Version()
		now := s.deps.Clock.Now()

		var movement wallet.Movement
		if req.Direction == wallet.DirectionDebit {
			movement, err = w.Debit(req.Amount, req.Reason, req.ReferenceID, req.Description, now)
		} else {
			movement, err = w.Credit(req.Amount, req.Reason, req.ReferenceID, req.Description, now)
		}
		if err != nil {
			return err
		}

		entry, err := s.recordMovement(ctx, tx, w, movement, versionAtLoad, req.IdempotencyKey, actorID, now)
		if err != nil {
			return err
		}

		result = TransactionResult{
			Entry:  newLedgerEntryView(entry),
			Wallet: newWalletView(w),
		}
		return s.saveResponse(ctx, tx, req.Operation, req.IdempotencyKey, result)
	})
	if err != nil {
		s.recordFailure(req.Operation, err)
		return TransactionResult{}, err
	}

	if replay {
		s.recordReplay(ctx, req.Operation, req.IdempotencyKey)
		result.IdempotentReplay = true
		return result, nil
	}

	s.publisher.Notify()
	s.recordSuccess(req.Operation, req.Direction, req.Reason, req.Amount)
	return result, nil
}

// Transfer moves money between two wallets in one transaction.
//
// The marketplace matching engine settles a trade with this: the buyer is debited
// and the seller credited, or neither happens. Two separate RPCs would leave a
// window in which the buyer has paid and the seller has not been paid.
func (s *WalletService) Transfer(ctx context.Context, cmd TransferCommand) (TransferResult, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return TransferResult{}, err
	}
	// Only a service or staff may move money between two accounts. A user must never
	// be able to pull funds out of somebody else's wallet.
	if !principal.IsService() && !principal.IsStaff() {
		return TransferResult{}, errs.PermissionDenied("transfers may only be initiated by the platform")
	}
	switch {
	case cmd.FromUserID == "" || cmd.ToUserID == "":
		return TransferResult{}, errs.InvalidArgument("both the source and destination users are required")
	case cmd.FromUserID == cmd.ToUserID:
		return TransferResult{}, errs.InvalidArgument("the source and destination wallets must differ")
	case cmd.IdempotencyKey == "":
		return TransferResult{}, errs.InvalidArgument("an idempotency key is required for a transfer")
	case !cmd.Amount.IsPositive():
		return TransferResult{}, wallet.ErrAmountNotPositive(cmd.Amount)
	case !cmd.Reason.Valid():
		return TransferResult{}, errs.InvalidArgument("unknown ledger reason %q", cmd.Reason)
	}

	var (
		result TransferResult
		replay bool
	)

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opTransfer, cmd.IdempotencyKey, cmd.FromUserID, cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		source, destination, err := s.lockPair(ctx, tx, cmd.FromUserID, cmd.ToUserID)
		if err != nil {
			return err
		}
		sourceVersion, destinationVersion := source.Version(), destination.Version()

		now := s.deps.Clock.Now()
		debit, err := source.Debit(cmd.Amount, cmd.Reason, cmd.ReferenceID, cmd.Description, now)
		if err != nil {
			return err
		}
		credit, err := destination.Credit(cmd.Amount, cmd.Reason, cmd.ReferenceID, cmd.Description, now)
		if err != nil {
			return err
		}

		debitEntry, err := s.recordMovement(ctx, tx, source, debit, sourceVersion,
			cmd.IdempotencyKey, principal.UserID, now)
		if err != nil {
			return err
		}
		creditEntry, err := s.recordMovement(ctx, tx, destination, credit, destinationVersion,
			cmd.IdempotencyKey, principal.UserID, now)
		if err != nil {
			return err
		}

		if err := s.emit(ctx, tx, EventFundsTransferred, aggregateWallet, source.ID(), now, TransferPayload{
			FromUserID:    cmd.FromUserID,
			ToUserID:      cmd.ToUserID,
			Amount:        cmd.Amount,
			Reason:        cmd.Reason.String(),
			ReferenceID:   cmd.ReferenceID,
			DebitEntryID:  debitEntry.ID,
			CreditEntryID: creditEntry.ID,
		}); err != nil {
			return err
		}

		result = TransferResult{
			DebitEntry:  newLedgerEntryView(debitEntry),
			CreditEntry: newLedgerEntryView(creditEntry),
		}
		return s.saveResponse(ctx, tx, opTransfer, cmd.IdempotencyKey, result)
	})
	if err != nil {
		s.recordFailure(opTransfer, err)
		return TransferResult{}, err
	}

	if replay {
		s.recordReplay(ctx, opTransfer, cmd.IdempotencyKey)
		result.IdempotentReplay = true
		return result, nil
	}

	s.publisher.Notify()
	s.recordSuccess(opTransfer, wallet.DirectionDebit, cmd.Reason, cmd.Amount)
	return result, nil
}

// lockPair locks two wallets in a deterministic order and returns them in the
// caller's requested order.
//
// The ordering is the whole point. Locking in whichever order the caller named the
// users would deadlock the instant two trades settle in opposite directions between
// the same pair of users at the same moment: each transaction would hold the row the
// other needs.
func (s *WalletService) lockPair(ctx context.Context, tx port.Tx, fromUserID, toUserID string) (source, destination *wallet.Wallet, err error) {
	first, second := fromUserID, toUserID
	if first > second {
		first, second = second, first
	}

	lockedFirst, err := s.deps.Wallets.LockByUserID(ctx, tx, first)
	if err != nil {
		return nil, nil, err
	}
	lockedSecond, err := s.deps.Wallets.LockByUserID(ctx, tx, second)
	if err != nil {
		return nil, nil, err
	}

	if lockedFirst.UserID() == fromUserID {
		return lockedFirst, lockedSecond, nil
	}
	return lockedSecond, lockedFirst, nil
}

// --- Holds ----------------------------------------------------------------

// HoldFunds reserves part of a balance for a pre-order or an instalment plan.
func (s *WalletService) HoldFunds(ctx context.Context, cmd HoldFundsCommand) (HoldResult, error) {
	if err := s.authoriseMovement(ctx, cmd.UserID); err != nil {
		return HoldResult{}, err
	}
	switch {
	case cmd.IdempotencyKey == "":
		return HoldResult{}, errs.InvalidArgument("an idempotency key is required to place a hold")
	case !cmd.Amount.IsPositive():
		return HoldResult{}, wallet.ErrAmountNotPositive(cmd.Amount)
	case cmd.ReferenceID == "":
		return HoldResult{}, errs.InvalidArgument("a reference id is required so the hold can be reconciled")
	}

	var (
		result HoldResult
		replay bool
	)

	err := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opHoldFunds, cmd.IdempotencyKey, cmd.UserID, cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		w, err := s.deps.Wallets.LockByUserID(ctx, tx, cmd.UserID)
		if err != nil {
			return err
		}
		versionAtLoad := w.Version()
		now := s.deps.Clock.Now()

		if err := w.PlaceHold(cmd.Amount, now); err != nil {
			return err
		}
		h, err := hold.New(s.deps.IDs.NewID(), w.ID(), w.UserID(), cmd.Amount,
			cmd.ReferenceID, cmd.Reason, cmd.TTL, now)
		if err != nil {
			return err
		}

		if err := s.deps.Wallets.Update(ctx, tx, w, versionAtLoad); err != nil {
			return err
		}
		if err := s.deps.Holds.Insert(ctx, tx, h); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventHoldPlaced, aggregateHold, h.ID(), now, HoldPayload{
			HoldID:      h.ID(),
			WalletID:    w.ID(),
			UserID:      w.UserID(),
			Amount:      cmd.Amount,
			ReferenceID: cmd.ReferenceID,
			Status:      string(h.Status()),
		}); err != nil {
			return err
		}

		result = HoldResult{Hold: newHoldView(h), Wallet: newWalletView(w)}
		return s.saveResponse(ctx, tx, opHoldFunds, cmd.IdempotencyKey, result)
	})
	if err != nil {
		s.recordFailure(opHoldFunds, err)
		return HoldResult{}, err
	}

	if replay {
		s.recordReplay(ctx, opHoldFunds, cmd.IdempotencyKey)
		result.IdempotentReplay = true
		return result, nil
	}

	s.publisher.Notify()
	s.deps.Metrics.WalletOperation(opHoldFunds, "success")
	return result, nil
}

// CaptureHold converts a reservation into a real debit.
func (s *WalletService) CaptureHold(ctx context.Context, cmd CaptureHoldCommand) (TransactionResult, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return TransactionResult{}, err
	}
	if cmd.IdempotencyKey == "" {
		return TransactionResult{}, errs.InvalidArgument("an idempotency key is required to capture a hold")
	}
	reason := cmd.Reason
	if reason == "" {
		reason = wallet.ReasonHoldCapture
	}
	if !reason.Valid() {
		return TransactionResult{}, errs.InvalidArgument("unknown ledger reason %q", cmd.Reason)
	}

	var (
		result TransactionResult
		replay bool
	)

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opCaptureHold, cmd.IdempotencyKey, "", cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		h, err := s.deps.Holds.LockByID(ctx, tx, cmd.HoldID)
		if err != nil {
			return err
		}
		// The hold names its owner, so authorisation can only happen after the load.
		if !principal.IsService() && !principal.IsStaff() && principal.UserID != h.UserID() {
			return errs.PermissionDenied("you may only capture a hold on your own wallet")
		}

		w, err := s.deps.Wallets.LockByID(ctx, tx, h.WalletID())
		if err != nil {
			return err
		}
		versionAtLoad, holdVersion := w.Version(), h.Version()
		now := s.deps.Clock.Now()

		captured, err := h.Capture(cmd.Amount, now)
		if err != nil {
			return err
		}
		movement, err := w.CaptureHold(captured, h.ReferenceID(), h.Reason(), now)
		if err != nil {
			return err
		}
		// The aggregate stamps HOLD_CAPTURE; honour an explicit override such as
		// PURCHASE so that an instalment reads naturally in the user's history.
		movement.Reason = reason

		if err := s.deps.Holds.Update(ctx, tx, h, holdVersion); err != nil {
			return err
		}
		entry, err := s.recordMovement(ctx, tx, w, movement, versionAtLoad,
			cmd.IdempotencyKey, principal.UserID, now)
		if err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventHoldCaptured, aggregateHold, h.ID(), now, HoldPayload{
			HoldID:      h.ID(),
			WalletID:    w.ID(),
			UserID:      w.UserID(),
			Amount:      captured,
			ReferenceID: h.ReferenceID(),
			Status:      string(h.Status()),
		}); err != nil {
			return err
		}

		result = TransactionResult{Entry: newLedgerEntryView(entry), Wallet: newWalletView(w)}
		return s.saveResponse(ctx, tx, opCaptureHold, cmd.IdempotencyKey, result)
	})
	if err != nil {
		s.recordFailure(opCaptureHold, err)
		return TransactionResult{}, err
	}

	if replay {
		s.recordReplay(ctx, opCaptureHold, cmd.IdempotencyKey)
		result.IdempotentReplay = true
		return result, nil
	}

	s.publisher.Notify()
	s.deps.Metrics.WalletOperation(opCaptureHold, "success")
	return result, nil
}

// ReleaseHold returns a reservation to the available balance.
func (s *WalletService) ReleaseHold(ctx context.Context, cmd ReleaseHoldCommand) (HoldResult, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return HoldResult{}, err
	}
	if cmd.IdempotencyKey == "" {
		return HoldResult{}, errs.InvalidArgument("an idempotency key is required to release a hold")
	}

	var (
		result HoldResult
		replay bool
	)

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opReleaseHold, cmd.IdempotencyKey, "", cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		h, err := s.deps.Holds.LockByID(ctx, tx, cmd.HoldID)
		if err != nil {
			return err
		}
		if !principal.IsService() && !principal.IsStaff() && principal.UserID != h.UserID() {
			return errs.PermissionDenied("you may only release a hold on your own wallet")
		}

		w, err := s.deps.Wallets.LockByID(ctx, tx, h.WalletID())
		if err != nil {
			return err
		}
		versionAtLoad, holdVersion := w.Version(), h.Version()
		now := s.deps.Clock.Now()

		released, err := h.Release(now)
		if err != nil {
			return err
		}
		if err := w.ReleaseHold(released, now); err != nil {
			return err
		}

		if err := s.deps.Holds.Update(ctx, tx, h, holdVersion); err != nil {
			return err
		}
		if err := s.deps.Wallets.Update(ctx, tx, w, versionAtLoad); err != nil {
			return err
		}
		// Releasing a hold writes no ledger entry: the balance never changed, only how
		// much of it was spendable. Inventing an entry would break reconciliation.
		if err := s.emit(ctx, tx, EventHoldReleased, aggregateHold, h.ID(), now, HoldPayload{
			HoldID:      h.ID(),
			WalletID:    w.ID(),
			UserID:      w.UserID(),
			Amount:      released,
			ReferenceID: h.ReferenceID(),
			Status:      string(h.Status()),
		}); err != nil {
			return err
		}

		result = HoldResult{Hold: newHoldView(h), Wallet: newWalletView(w)}
		return s.saveResponse(ctx, tx, opReleaseHold, cmd.IdempotencyKey, result)
	})
	if err != nil {
		s.recordFailure(opReleaseHold, err)
		return HoldResult{}, err
	}

	if replay {
		s.recordReplay(ctx, opReleaseHold, cmd.IdempotencyKey)
		result.IdempotentReplay = true
		return result, nil
	}

	s.publisher.Notify()
	s.deps.Metrics.WalletOperation(opReleaseHold, "success")
	return result, nil
}

// ExpireHolds releases every hold whose TTL has elapsed.
//
// Run by the scheduler. Without it, an abandoned pre-order would reserve a user's
// money indefinitely.
func (s *WalletService) ExpireHolds(ctx context.Context, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	now := s.deps.Clock.Now()

	expired, err := s.deps.Holds.ListExpired(ctx, s.deps.Reader, now, batchSize)
	if err != nil {
		return 0, err
	}

	released := 0
	for _, candidate := range expired {
		// Each hold gets its own transaction: one wallet in a strange state must not
		// block the rest of the sweep.
		err := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
			h, err := s.deps.Holds.LockByID(ctx, tx, candidate.ID())
			if err != nil {
				return err
			}
			if h.Status().Terminal() {
				// Somebody captured or released it between our read and this lock.
				return nil
			}

			w, err := s.deps.Wallets.LockByID(ctx, tx, h.WalletID())
			if err != nil {
				return err
			}
			versionAtLoad, holdVersion := w.Version(), h.Version()

			amount, err := h.Expire(now)
			if err != nil {
				return err
			}
			if err := w.ReleaseHold(amount, now); err != nil {
				return err
			}
			if err := s.deps.Holds.Update(ctx, tx, h, holdVersion); err != nil {
				return err
			}
			if err := s.deps.Wallets.Update(ctx, tx, w, versionAtLoad); err != nil {
				return err
			}
			return s.emit(ctx, tx, EventHoldReleased, aggregateHold, h.ID(), now, HoldPayload{
				HoldID:      h.ID(),
				WalletID:    w.ID(),
				UserID:      w.UserID(),
				Amount:      amount,
				ReferenceID: h.ReferenceID(),
				Status:      string(h.Status()),
			})
		})
		if err != nil {
			logx.FromContext(ctx).Error("failed to expire a hold",
				slog.String("hold_id", candidate.ID()),
				slog.String("error", err.Error()),
			)
			continue
		}
		released++
	}

	if released > 0 {
		s.publisher.Notify()
		logx.FromContext(ctx).Info("expired holds released", slog.Int("count", released))
	}
	return released, nil
}

// publishPaymentFailed announces a rejected debit on the event stream.
//
// It runs in its own transaction, because the debit's transaction has already rolled
// back. A failure to publish is logged rather than returned: the caller already has
// the real error, and losing a notification must not turn a clean business rejection
// into a 500.
func (s *WalletService) publishPaymentFailed(ctx context.Context, cmd DebitCommand, cause error) {
	available := money.Zero(cmd.Amount.Currency())
	walletID := ""
	if w, err := s.deps.Wallets.FindByUserID(ctx, s.deps.Reader, cmd.UserID); err == nil {
		available = w.Available()
		walletID = w.ID()
	}

	payload := PaymentFailedPayload{
		UserID:      cmd.UserID,
		WalletID:    walletID,
		ReferenceID: cmd.ReferenceID,
		Requested:   cmd.Amount,
		Available:   available,
		Reason:      errs.ReasonOf(cause),
		Message:     cause.Error(),
	}
	if payload.Reason == "" {
		payload.Reason = string(errs.CodeOf(cause))
	}

	err := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		return s.emit(ctx, tx, EventPaymentFailed, aggregateWallet, cmd.UserID,
			s.deps.Clock.Now(), payload)
	})
	if err != nil {
		logx.FromContext(ctx).Error("failed to publish PaymentFailed; the purchase saga may stall",
			slog.String("user_id", cmd.UserID),
			slog.String("reference_id", cmd.ReferenceID),
			slog.String("error", err.Error()),
		)
		return
	}
	s.publisher.Notify()
}
