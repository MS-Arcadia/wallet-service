// Package abuse holds the gift-card abuse policy.
//
// The requirements call for a user who repeatedly enters wrong gift-card codes in
// a short window to be flagged, and banned at Support's discretion. Two concerns
// are separated here on purpose:
//
//   - counting attempts over a sliding window is infrastructure (Redis), and
//   - deciding what a given set of counts *means* is domain logic, which lives here
//     and is testable without a broker or a cache.
//
// The service never bans anybody itself. It publishes GiftCardAbuseDetected; the
// Auth service queues the user for Support, and a human decides. That matches the
// requirement's "با تایید و صلاحدید پشتیبان" — at Support's discretion.
package abuse

import (
	"fmt"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
)

// Window names, which double as the rate-limiter rule names.
const (
	// WindowMinute is the short burst window.
	WindowMinute = "per-minute"
	// WindowHour is the sustained-abuse window.
	WindowHour = "per-hour"
)

// Reason codes returned by this package.
const (
	ReasonCodeTooManyAttempts = "TOO_MANY_GIFT_CARD_ATTEMPTS"
)

// Threshold is one limit: at most Limit failed attempts per Window.
type Threshold struct {
	Name   string
	Limit  int64
	Window time.Duration
}

// Policy is the complete abuse policy.
type Policy struct {
	thresholds []Threshold
	// flagAt is the number of failures inside the widest window that triggers a
	// GiftCardAbuseDetected event. It is deliberately lower than the hourly block
	// limit so Support hears about a pattern before the user is fully locked out.
	flagAt int64
}

// DefaultPolicy returns the policy from the requirements: five failures a minute,
// thirty an hour, flagged for Support review at ten.
func DefaultPolicy() Policy {
	return Policy{
		thresholds: []Threshold{
			// The tightest window is listed first so that a burst produces the most
			// specific message.
			{Name: WindowMinute, Limit: 5, Window: time.Minute},
			{Name: WindowHour, Limit: 30, Window: time.Hour},
		},
		flagAt: 10,
	}
}

// NewPolicy builds a policy with custom limits, for operators who need to tighten
// or relax the defaults without a redeploy.
func NewPolicy(perMinute, perHour, flagAt int64) (Policy, error) {
	switch {
	case perMinute <= 0 || perHour <= 0:
		return Policy{}, errs.InvalidArgument("abuse thresholds must be greater than zero")
	case perHour < perMinute:
		return Policy{}, errs.InvalidArgument(
			"the hourly threshold (%d) cannot be lower than the per-minute threshold (%d)", perHour, perMinute)
	case flagAt <= 0:
		return Policy{}, errs.InvalidArgument("the flag threshold must be greater than zero")
	}
	return Policy{
		thresholds: []Threshold{
			{Name: WindowMinute, Limit: perMinute, Window: time.Minute},
			{Name: WindowHour, Limit: perHour, Window: time.Hour},
		},
		flagAt: flagAt,
	}, nil
}

// Thresholds returns the configured limits, which the adapter translates into
// rate-limiter rules.
func (p Policy) Thresholds() []Threshold { return p.thresholds }

// FlagAt returns the review threshold.
func (p Policy) FlagAt() int64 { return p.flagAt }

// Assessment is the policy's verdict on a user's recent behaviour.
type Assessment struct {
	// Blocked reports that this attempt must be refused outright.
	Blocked bool
	// BlockedBy names the violated threshold.
	BlockedBy string
	// RetryAfter is how long until the user may try again.
	RetryAfter time.Duration
	// Flagged reports that Support should review the account.
	Flagged bool
	// FailedAttempts is the count that produced this verdict.
	FailedAttempts int64
}

// Assess decides what to do about a redemption attempt.
//
// blocked and blockedBy come from the rate limiter; failedInHour is the number of
// failures observed in the wide window. The two are combined here so that all of
// the judgement lives in one testable place.
func (p Policy) Assess(blocked bool, blockedBy string, retryAfter time.Duration, failedInHour int64) Assessment {
	return Assessment{
		Blocked:        blocked,
		BlockedBy:      blockedBy,
		RetryAfter:     retryAfter,
		Flagged:        failedInHour >= p.flagAt,
		FailedAttempts: failedInHour,
	}
}

// ErrTooManyAttempts is returned when an attempt is refused.
//
// The message names no specifics beyond the retry delay. Telling somebody
// probing for codes exactly which window they tripped, and how many guesses they
// have left, would help them pace their attack.
func ErrTooManyAttempts(retryAfter time.Duration) error {
	return errs.ResourceExhausted("too many gift card attempts; try again in %s", humanise(retryAfter)).
		WithReason(ReasonCodeTooManyAttempts).
		WithDetail("retry_after_seconds", int64(retryAfter.Seconds()))
}

func humanise(d time.Duration) string {
	if d < time.Minute {
		seconds := int(d.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		return fmt.Sprintf("%d seconds", seconds)
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes())+1)
}
