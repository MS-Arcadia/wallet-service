package app

import (
	"context"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/platform/authn"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/idgen"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// DiscountService implements promotional-code use cases.
//
// Discount codes live in the wallet service rather than in Store because they are
// money: the same rounding, capping and single-use rules that protect a balance
// have to protect a discount, and duplicating them in another service is how two
// implementations quietly diverge.
type DiscountService struct {
	*core
}

// NewDiscountService builds the service.
func NewDiscountService(deps Deps) *DiscountService {
	return &DiscountService{core: newCore(deps)}
}

// IssueDiscountCodeCommand mints a promotional code.
type IssueDiscountCodeCommand struct {
	// Code is optional; a random one is generated when empty.
	Code           string
	PercentBps     int32
	AmountOff      money.Money
	MaxDiscount    money.Money
	MinOrderAmount money.Money
	MaxRedemptions int32
	ExpiresAt      *time.Time
	IdempotencyKey string
}

// RedeemDiscountCodeCommand consumes one redemption of a code.
type RedeemDiscountCodeCommand struct {
	Code        string
	OrderAmount money.Money
	// ReferenceID is the order the discount is being applied to.
	ReferenceID    string
	IdempotencyKey string
}

// IssueDiscountCode mints a code. Staff only.
func (s *DiscountService) IssueDiscountCode(ctx context.Context, cmd IssueDiscountCodeCommand) (DiscountCodeView, error) {
	principal, err := authn.RequireStaff(ctx)
	if err != nil {
		return DiscountCodeView{}, err
	}
	if cmd.IdempotencyKey == "" {
		return DiscountCodeView{}, errs.InvalidArgument("an idempotency key is required to issue a discount code")
	}

	codeString := idgen.NormalizeCode(cmd.Code)
	if codeString == "" {
		generated, err := idgen.NewGiftCardCode()
		if err != nil {
			return DiscountCodeView{}, errs.Internal("failed to generate a discount code").WithCause(err)
		}
		codeString = idgen.NormalizeCode(generated)
	}

	var (
		view   DiscountCodeView
		replay bool
	)

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, "issue_discount", cmd.IdempotencyKey, "", cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &view)
		}

		now := s.deps.Clock.Now()
		code, err := discount.Issue(discount.Spec{
			ID:             s.deps.IDs.NewID(),
			Code:           codeString,
			PercentBps:     cmd.PercentBps,
			AmountOff:      cmd.AmountOff,
			MaxDiscount:    cmd.MaxDiscount,
			MinOrderAmount: cmd.MinOrderAmount,
			MaxRedemptions: cmd.MaxRedemptions,
			IssuedBy:       principal.UserID,
			ExpiresAt:      cmd.ExpiresAt,
		}, now)
		if err != nil {
			return err
		}
		if err := s.deps.Discounts.Insert(ctx, tx, code); err != nil {
			return err
		}

		view = newDiscountCodeView(code)
		return s.saveResponse(ctx, tx, "issue_discount", cmd.IdempotencyKey, view)
	})
	if err != nil {
		return DiscountCodeView{}, err
	}

	if replay {
		s.deps.Metrics.IdempotentReplay("issue_discount")
	}
	return view, nil
}

// PreviewDiscount computes what a code would do to an order without consuming it.
//
// The Store service calls this on every checkout render, so it must stay free of
// side effects and cheap.
func (s *DiscountService) PreviewDiscount(ctx context.Context, code string, orderAmount money.Money) (DiscountQuote, error) {
	if _, err := authn.RequirePrincipal(ctx); err != nil {
		return DiscountQuote{}, err
	}
	normalised := idgen.NormalizeCode(code)
	if normalised == "" {
		return DiscountQuote{}, errs.InvalidArgument("a discount code is required")
	}

	stored, err := s.deps.Discounts.FindByCode(ctx, s.deps.Reader, normalised)
	if err != nil {
		return DiscountQuote{}, err
	}

	quote, err := stored.Preview(orderAmount, s.deps.Clock.Now())
	if err != nil {
		if reason := errs.ReasonOf(err); reason != "" {
			s.deps.Metrics.BusinessRuleRejection(reason)
		}
		return DiscountQuote{}, err
	}

	view := newDiscountCodeView(stored)
	return DiscountQuote{Discount: quote.Discount, Payable: quote.Payable, Code: &view}, nil
}

// RedeemDiscountCode consumes one redemption and returns the resulting quote.
//
// It moves no money by itself: the Store service applies the returned discount to
// the order it is about to charge. The redemption is nonetheless idempotent and
// row-locked, because two concurrent checkouts must not both consume the last
// allowance of a single-use code.
func (s *DiscountService) RedeemDiscountCode(ctx context.Context, cmd RedeemDiscountCodeCommand) (DiscountQuote, error) {
	principal, err := authn.RequirePrincipal(ctx)
	if err != nil {
		return DiscountQuote{}, err
	}
	switch {
	case cmd.IdempotencyKey == "":
		return DiscountQuote{}, errs.InvalidArgument("an idempotency key is required to redeem a discount code")
	case cmd.ReferenceID == "":
		return DiscountQuote{}, errs.InvalidArgument("a reference id is required so the redemption can be reconciled")
	}
	normalised := idgen.NormalizeCode(cmd.Code)
	if normalised == "" {
		return DiscountQuote{}, errs.InvalidArgument("a discount code is required")
	}

	var (
		result DiscountQuote
		replay bool
	)

	err = s.deps.TxManager.WithinTx(ctx, func(ctx context.Context, tx port.Tx) error {
		claimed, existing, err := s.claim(ctx, tx, opRedeemDiscount, cmd.IdempotencyKey, principal.UserID, cmd)
		if err != nil {
			return err
		}
		if !claimed {
			replay = true
			return s.core.replay(existing, &result)
		}

		stored, err := s.deps.Discounts.LockByCode(ctx, tx, normalised)
		if err != nil {
			return err
		}
		version := stored.Version()
		now := s.deps.Clock.Now()

		quote, err := stored.Redeem(cmd.OrderAmount, now)
		if err != nil {
			return err
		}
		if err := s.deps.Discounts.Update(ctx, tx, stored, version); err != nil {
			return err
		}

		if err := s.emit(ctx, tx, EventDiscountCodeRedeemed, aggregateDiscount, stored.ID(), now,
			DiscountRedeemedPayload{
				CodeID:      stored.ID(),
				Code:        stored.Code(),
				UserID:      principal.UserID,
				OrderAmount: cmd.OrderAmount,
				Discount:    quote.Discount,
				ReferenceID: cmd.ReferenceID,
			}); err != nil {
			return err
		}

		result = DiscountQuote{Discount: quote.Discount, Payable: quote.Payable}
		return s.saveResponse(ctx, tx, opRedeemDiscount, cmd.IdempotencyKey, result)
	})
	if err != nil {
		if reason := errs.ReasonOf(err); reason != "" {
			s.deps.Metrics.BusinessRuleRejection(reason)
		}
		return DiscountQuote{}, err
	}

	if replay {
		s.deps.Metrics.IdempotentReplay(opRedeemDiscount)
		result.IdempotentReplay = true
		return result, nil
	}

	s.publisher.Notify()
	return result, nil
}

// GetDiscountCode reads a code.
func (s *DiscountService) GetDiscountCode(ctx context.Context, code string) (DiscountCodeView, error) {
	if _, err := authn.RequirePrincipal(ctx); err != nil {
		return DiscountCodeView{}, err
	}
	normalised := idgen.NormalizeCode(code)
	if normalised == "" {
		return DiscountCodeView{}, errs.InvalidArgument("a discount code is required")
	}

	stored, err := s.deps.Discounts.FindByCode(ctx, s.deps.Reader, normalised)
	if err != nil {
		return DiscountCodeView{}, err
	}
	return newDiscountCodeView(stored), nil
}
