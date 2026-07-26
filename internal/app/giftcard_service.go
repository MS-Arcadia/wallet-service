package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/abuse"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/idgen"
	"github.com/MS-Arcadia/wallet-service/internal/platform/logx"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// maxGiftCardBatch caps one issuance request. A Support user who fat-fingers an
// extra zero should get an error, not a million cards.
const maxGiftCardBatch = 1000

// GiftCardService implements gift-card issuance and redemption.
type GiftCardService struct {
	*core
	hasher *giftcard.Hasher
	policy abuse.Policy
}

// NewGiftCardService builds the service.
func NewGiftCardService(deps Deps, hasher *giftcard.Hasher, policy abuse.Policy) *GiftCardService {
	return &GiftCardService{
		core:   newCore(deps),
		hasher: hasher,
		policy: policy,
	}
}

// IssueGiftCardsCommand mints a batch of cards.
type IssueGiftCardsCommand struct {
	Value          money.Money
	Quantity       int
	Note           string
	IdempotencyKey string
}

// RedeemGiftCardCommand redeems a card into the caller's wallet.
type RedeemGiftCardCommand struct {
	Code           string
	IdempotencyKey string
}

// IssueGiftCards mints a batch of cards and returns their plaintext codes.
//
// The codes appear in this response and nowhere else, ever: only their hashes are
// stored. A replayed issuance therefore returns the card records without codes,
// which is the honest answer — they cannot be re-derived.
func (s *GiftCardService) IssueGiftCards(ctx context.Context, cmd IssueGiftCardsCommand) (IssueGiftCardsResult, error) {
	principal, err := authn.RequireStaff(ctx)
	if err != nil {
		return IssueGiftCardsResult{}, err
	}
	switch {
	case cmd.IdempotencyKey == "":
		return IssueGiftCardsResult{}, errs.InvalidArgument("an idempotency key is required to issue gift cards")
	case !cmd.Value.IsPositive():
		return IssueGiftCardsResult{}, errs.InvalidArgument("the gift card value must be greater than zero")
	case cmd.Quantity <= 0:
		return IssueGiftCardsResult{}, errs.InvalidArgument("the quantity must be at least 1")
	case cmd.Quantity > maxGiftCardBatch:
		return IssueGiftCardsResult{}, errs.InvalidArgument(
			"at most %d gift cards may be issued in one request, asked for %d", maxGiftCardBatch, cmd.Quantity)
	}
	if cmd.Value.Currency() != s.deps.Currency {
		return IssueGiftCardsResult{}, errs.InvalidArgument(
			"gift cards are issued in %s, not %s", s.deps.Currency, cmd.Value.Currency())
	}

	var (
		result IssueGiftCardsResult
		replay bool
	)

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opIssueGiftCards, cmd.IdempotencyKey, "", cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		now := s.deps.Clock.Now()
		batchID := s.deps.IDs.NewID()
		cards := make([]*giftcard.GiftCard, 0, cmd.Quantity)
		views := make([]GiftCardView, 0, cmd.Quantity)

		for i := 0; i < cmd.Quantity; i++ {
			card, code, err := giftcard.Issue(s.deps.IDs.NewID(), cmd.Value,
				principal.UserID, batchID, cmd.Note, s.hasher, now)
			if err != nil {
				return err
			}
			cards = append(cards, card)
			views = append(views, newGiftCardView(card, code))
		}

		if err := s.deps.GiftCards.InsertBatch(ctx, tx, cards); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, EventGiftCardIssued, aggregateGiftCard, batchID, now, GiftCardIssuedPayload{
			BatchID:  batchID,
			Quantity: cmd.Quantity,
			Value:    cmd.Value,
			IssuedBy: principal.UserID,
		}); err != nil {
			return err
		}

		result = IssueGiftCardsResult{BatchID: batchID, GiftCards: views}

		// The stored response has the codes stripped. Persisting them would defeat the
		// point of hashing: the idempotency table would become a plaintext code store.
		stored := IssueGiftCardsResult{BatchID: batchID, GiftCards: make([]GiftCardView, 0, len(views))}
		for _, view := range views {
			view.Code = ""
			stored.GiftCards = append(stored.GiftCards, view)
		}
		return s.saveResponse(ctx, tx, opIssueGiftCards, cmd.IdempotencyKey, stored)
	})
	if err != nil {
		return IssueGiftCardsResult{}, err
	}

	if replay {
		s.deps.Metrics.IdempotentReplay(opIssueGiftCards)
		result.IdempotentReplay = true
		logx.FromContext(ctx).Warn("replayed a gift card issuance; the codes cannot be re-revealed",
			slog.String("batch_id", result.BatchID))
		return result, nil
	}

	s.emitter.publisher.Notify()
	logx.FromContext(ctx).Info("issued gift cards",
		slog.String("batch_id", result.BatchID),
		slog.Int("quantity", cmd.Quantity),
		slog.String("issued_by", principal.UserID),
	)
	return result, nil
}

// RedeemGiftCard credits a card's value to the caller's wallet.
//
// This is the most adversarially exposed endpoint in the service: an attacker with
// no valid code can only make progress by guessing, so the flow is built around
// making guessing expensive and observable.
func (s *GiftCardService) RedeemGiftCard(ctx context.Context, cmd RedeemGiftCardCommand) (RedeemGiftCardResult, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return RedeemGiftCardResult{}, err
	}
	if cmd.IdempotencyKey == "" {
		return RedeemGiftCardResult{}, errs.InvalidArgument("an idempotency key is required to redeem a gift card")
	}
	if idgen.NormalizeCode(cmd.Code) == "" {
		return RedeemGiftCardResult{}, errs.InvalidArgument("a gift card code is required")
	}

	// Throttle before touching the database. A blocked caller must not be able to
	// use this endpoint to probe for valid codes, and must not cost us a query.
	verdict, err := s.deps.AbuseLimiter.Check(ctx, principal.UserID)
	if err != nil {
		// The limiter fails open by design — Redis being down must not stop a
		// legitimate user from redeeming a card — but it is logged loudly, because
		// while it is down the abuse rule is not being enforced.
		logx.FromContext(ctx).Error("the gift card abuse limiter is unavailable; failing open",
			slog.String("error", err.Error()))
	}
	if verdict.Blocked {
		s.deps.Metrics.RateLimitBlock("giftcard-attempt")
		return RedeemGiftCardResult{}, abuse.ErrTooManyAttempts(verdict.RetryAfter)
	}

	codeHash := s.hasher.Hash(cmd.Code)

	var (
		result RedeemGiftCardResult
		replay bool
	)

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opRedeemGiftCard, cmd.IdempotencyKey, principal.UserID, cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		// FOR UPDATE on the card row. Two requests racing with the same code must not
		// both credit a wallet; one of them will block here and then see USED.
		card, err := s.deps.GiftCards.LockByCodeHash(ctx, tx, codeHash)
		if err != nil {
			return err
		}
		cardVersion := card.Version()

		w, err := s.deps.Wallets.LockByUserID(ctx, tx, principal.UserID)
		if err != nil {
			return err
		}
		walletVersion := w.Version()
		now := s.deps.Clock.Now()

		if err := card.Redeem(principal.UserID, now); err != nil {
			return err
		}
		// A conditional UPDATE ... WHERE status = 'ACTIVE'. Belt and braces with the
		// row lock: if the row somehow changed under us, no money moves.
		applied, err := s.deps.GiftCards.MarkRedeemed(ctx, tx, card, cardVersion)
		if err != nil {
			return err
		}
		if !applied {
			return errs.Conflict("this gift card has already been redeemed").
				WithReason(giftcard.ReasonCodeAlreadyUsed)
		}

		movement, err := w.Credit(card.Value(), wallet.ReasonGiftCard, card.ID(),
			"gift card "+card.CodeHint(), now)
		if err != nil {
			return err
		}
		entry, err := s.recordMovement(ctx, tx, w, movement, walletVersion, cmd.IdempotencyKey, principal.UserID, now)
		if err != nil {
			return err
		}

		if err := s.emit(ctx, tx, EventGiftCardRedeemed, aggregateGiftCard, card.ID(), now, GiftCardRedeemedPayload{
			GiftCardID: card.ID(),
			UserID:     principal.UserID,
			WalletID:   w.ID(),
			Value:      card.Value(),
			CodeHint:   card.CodeHint(),
		}); err != nil {
			return err
		}

		result = RedeemGiftCardResult{
			Credited: card.Value(),
			Wallet:   newWalletView(w),
			Entry:    newLedgerEntryView(entry),
		}
		return s.saveResponse(ctx, tx, opRedeemGiftCard, cmd.IdempotencyKey, result)
	})
	if err != nil {
		s.recordFailedAttempt(ctx, principal.UserID, cmd.Code, err)
		return RedeemGiftCardResult{}, err
	}

	if replay {
		s.deps.Metrics.IdempotentReplay(opRedeemGiftCard)
		result.IdempotentReplay = true
		return result, nil
	}

	s.emitter.publisher.Notify()
	s.deps.Metrics.WalletOperation(opRedeemGiftCard, "success")
	s.deps.Metrics.MoneyMoved(wallet.DirectionCredit.String(), wallet.ReasonGiftCard.String(),
		result.Credited.Currency(), result.Credited.Minor())
	return result, nil
}

// recordFailedAttempt charges a failed redemption against the user's allowance and
// flags them for Support when the pattern looks like probing.
//
// Only genuine code failures count. An infrastructure error, or a rejection caused
// by the user's own wallet being frozen, is not evidence of guessing, and counting
// it would eventually lock out an innocent user.
func (s *GiftCardService) recordFailedAttempt(ctx context.Context, userID, code string, cause error) {
	if !isCodeFailure(cause) {
		return
	}
	s.deps.Metrics.WalletOperation(opRedeemGiftCard, "invalid_code")

	verdict, err := s.deps.AbuseLimiter.CheckAndRecordFailure(ctx, userID)
	if err != nil {
		logx.FromContext(ctx).Error("failed to record a gift card attempt",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}

	assessment := s.policy.Assess(verdict.Blocked, verdict.Rule, verdict.RetryAfter, verdict.FailedInWindow)
	if !assessment.Flagged {
		return
	}

	// The service never bans anybody. It reports the pattern; Auth queues the user
	// and a Support agent decides, exactly as the requirements specify.
	logx.FromContext(ctx).Warn("gift card abuse detected; flagging for support review",
		slog.String("user_id", userID),
		slog.Int64("failed_attempts", assessment.FailedAttempts),
		slog.String("code_hint", lastFour(code)),
	)

	publishErr := s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		return s.emit(ctx, tx, EventGiftCardAbuseDetected, aggregateWallet, userID,
			s.deps.Clock.Now(), GiftCardAbuseDetectedPayload{
				UserID:            userID,
				FailedAttempts:    assessment.FailedAttempts,
				WindowRule:        assessment.BlockedBy,
				DetectedAt:        s.deps.Clock.Now().Format(time.RFC3339),
				RecommendedAction: "REVIEW_FOR_BAN",
			})
	})
	if publishErr != nil {
		logx.FromContext(ctx).Error("failed to publish GiftCardAbuseDetected",
			slog.String("user_id", userID),
			slog.String("error", publishErr.Error()),
		)
		return
	}
	s.emitter.publisher.Notify()
}

// isCodeFailure reports whether an error means "that code was not usable", as
// opposed to a transient fault or a problem with the user's own wallet.
func isCodeFailure(err error) bool {
	switch errs.ReasonOf(err) {
	case giftcard.ReasonCodeNotFound, giftcard.ReasonCodeAlreadyUsed, giftcard.ReasonCodeRevoked:
		return true
	default:
		return false
	}
}

func lastFour(code string) string {
	normalised := idgen.NormalizeCode(code)
	if len(normalised) <= 4 {
		return normalised
	}
	return normalised[len(normalised)-4:]
}

// GetGiftCard reads a card by id or by code. Staff only.
func (s *GiftCardService) GetGiftCard(ctx context.Context, id, code string) (GiftCardView, error) {
	if _, err := authn.RequireStaff(ctx); err != nil {
		return GiftCardView{}, err
	}

	var (
		card *giftcard.GiftCard
		err  error
	)
	switch {
	case id != "":
		card, err = s.deps.GiftCards.FindByID(ctx, s.deps.Reader, id)
	case code != "":
		card, err = s.deps.GiftCards.FindByCodeHash(ctx, s.deps.Reader, s.hasher.Hash(code))
	default:
		return GiftCardView{}, errs.InvalidArgument("either an id or a code is required")
	}
	if err != nil {
		return GiftCardView{}, err
	}
	// Never echo the plaintext, even to staff: it is not stored, and a support
	// screen is a poor place to display a bearer instrument.
	return newGiftCardView(card, ""), nil
}

// ListGiftCards returns a filtered page of cards. Staff only.
func (s *GiftCardService) ListGiftCards(ctx context.Context, status giftcard.Status, batchID string, page, pageSize int) (GiftCardPage, error) {
	if _, err := authn.RequireStaff(ctx); err != nil {
		return GiftCardPage{}, err
	}

	limit, offset := paging(page, pageSize)
	cards, total, err := s.deps.GiftCards.List(ctx, s.deps.Reader, giftcard.Filter{
		Status:  status,
		BatchID: batchID,
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		return GiftCardPage{}, err
	}

	views := make([]GiftCardView, 0, len(cards))
	for _, card := range cards {
		views = append(views, newGiftCardView(card, ""))
	}
	pageNum, size, totalPages := pageInfo(total, limit, offset)
	return GiftCardPage{
		GiftCards:  views,
		TotalItems: total,
		Page:       pageNum,
		PageSize:   size,
		TotalPages: totalPages,
	}, nil
}

// RevokeGiftCard cancels an unredeemed card. Staff only.
func (s *GiftCardService) RevokeGiftCard(ctx context.Context, id, note string) (GiftCardView, error) {
	principal, err := authn.RequireStaff(ctx)
	if err != nil {
		return GiftCardView{}, err
	}
	if id == "" {
		return GiftCardView{}, errs.InvalidArgument("a gift card id is required")
	}
	if note == "" {
		// A revocation destroys value somebody may have been promised; an unexplained
		// one is not auditable.
		return GiftCardView{}, errs.InvalidArgument("a reason is required to revoke a gift card")
	}

	var view GiftCardView
	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		card, err := s.deps.GiftCards.FindByID(ctx, tx, id)
		if err != nil {
			return err
		}
		version := card.Version()
		if err := card.Revoke(note, s.deps.Clock.Now()); err != nil {
			return err
		}
		if err := s.deps.GiftCards.Update(ctx, tx, card, version); err != nil {
			return err
		}
		view = newGiftCardView(card, "")
		return nil
	})
	if err != nil {
		return GiftCardView{}, err
	}

	logx.FromContext(ctx).Info("gift card revoked",
		slog.String("gift_card_id", id),
		slog.String("revoked_by", principal.UserID),
	)
	return view, nil
}
