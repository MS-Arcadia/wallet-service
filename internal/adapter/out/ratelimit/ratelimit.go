// Package ratelimit implements the gift-card abuse limiter on Redis.
//
// The domain owns the thresholds (see internal/domain/abuse); this adapter owns the
// counting. Splitting them that way means the policy is unit-testable without Redis,
// and the counting mechanism can change without touching a business rule.
package ratelimit

import (
	"context"
	"log/slog"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/abuse"
	"github.com/MS-Arcadia/wallet-service/internal/platform/redisx"
)

// GiftCardLimiter counts failed gift-card redemption attempts per user.
type GiftCardLimiter struct {
	limiter *redisx.Limiter
	policy  abuse.Policy
	logger  *slog.Logger
	// widestRule is the rule whose window is used to report the failure count that
	// the flagging decision is based on.
	widestRule string
}

// New builds a GiftCardLimiter from an abuse policy.
//
// The limiter fails open: if Redis is unreachable the abuse rule stops being
// enforced, but a legitimate user can still spend their gift card. Failing closed
// would mean a cache outage locks every customer out of their own money, which is a
// worse outcome than temporarily not catching a guesser.
func New(client *redisx.Client, policy abuse.Policy, logger *slog.Logger) *GiftCardLimiter {
	if logger == nil {
		logger = slog.Default()
	}

	thresholds := policy.Thresholds()
	rules := make([]redisx.Rule, 0, len(thresholds))
	widest := ""
	for _, threshold := range thresholds {
		rules = append(rules, redisx.Rule{
			Name:   threshold.Name,
			Limit:  threshold.Limit,
			Window: threshold.Window,
		})
		widest = threshold.Name
	}

	return &GiftCardLimiter{
		limiter:    redisx.NewLimiter(client, "giftcard-attempt", true, rules...),
		policy:     policy,
		logger:     logger.With(slog.String("component", "giftcard-limiter")),
		widestRule: widest,
	}
}

// CheckAndRecordFailure registers a failed attempt and reports the verdict.
func (l *GiftCardLimiter) CheckAndRecordFailure(ctx context.Context, userID string) (port.AbuseVerdict, error) {
	decision, err := l.limiter.Allow(ctx, userID)
	if err != nil {
		// The limiter already decided how to fail; Decision.Allowed carries that choice.
		return port.AbuseVerdict{Blocked: !decision.Allowed}, err
	}
	return l.verdict(ctx, userID, decision), nil
}

// Check reports the verdict without recording anything.
//
// A successful redemption must not cost a user any of their allowance, so the happy
// path only ever inspects.
func (l *GiftCardLimiter) Check(ctx context.Context, userID string) (port.AbuseVerdict, error) {
	decision, err := l.limiter.Peek(ctx, userID)
	if err != nil {
		return port.AbuseVerdict{Blocked: !decision.Allowed}, err
	}
	return l.verdict(ctx, userID, decision), nil
}

// Reset clears a user's counters, used when Support clears their flag.
func (l *GiftCardLimiter) Reset(ctx context.Context, userID string) error {
	return l.limiter.Reset(ctx, userID)
}

func (l *GiftCardLimiter) verdict(ctx context.Context, userID string, decision redisx.Decision) port.AbuseVerdict {
	verdict := port.AbuseVerdict{
		Blocked:        !decision.Allowed,
		Rule:           decision.Rule,
		RetryAfter:     decision.RetryAfter,
		FailedInWindow: decision.Count,
	}

	// The flagging decision is based on the widest window, not on whichever rule
	// happened to trip. A burst of five in a minute is a fumbling user; thirty over an
	// hour is somebody working through the code space.
	if count, err := l.limiter.Count(ctx, userID, l.widestRule); err == nil {
		verdict.FailedInWindow = count
	} else {
		l.logger.Debug("could not read the wide-window attempt count",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
	}
	return verdict
}

var _ port.AbuseLimiter = (*GiftCardLimiter)(nil)
