// Package interest implements the wallet interest policy.
//
// The requirements describe an annual return on whatever a user leaves sitting in
// their wallet, inspired by a bank deposit. Interest is accrued *daily* rather
// than once a year: a yearly lump sum would reward somebody who happened to have a
// large balance on one particular day and pay nothing to a user who kept money in
// the wallet for eleven months.
//
// Every amount is computed with integer arithmetic and rounded down. Rounding in
// the platform's favour on a per-user, per-day basis is the conservative choice —
// it can never over-pay, and the residue is a fraction of a rial.
package interest

import (
	"fmt"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
)

// DaysPerYear is the accrual denominator. A fixed 365 is used rather than the
// actual length of the calendar year so that a user's daily rate does not silently
// change on a leap year.
const DaysPerYear int64 = 365

// Policy describes how interest is calculated.
type Policy struct {
	// annualRateBps is the yearly rate in basis points; 500 bps is 5% a year.
	annualRateBps int64
	// minimumBalance is the balance a wallet must hold to earn anything. It exists
	// to stop the platform from writing a ledger entry worth a fraction of a unit
	// for millions of near-empty wallets.
	minimumBalance money.Money
	// enabled turns accrual off without removing the scheduler.
	enabled bool
}

// Config configures a Policy.
type Config struct {
	AnnualRateBps  int64
	MinimumBalance money.Money
	Enabled        bool
}

// NewPolicy builds a Policy.
func NewPolicy(cfg Config) (Policy, error) {
	switch {
	case cfg.AnnualRateBps < 0:
		return Policy{}, errs.InvalidArgument("the annual interest rate cannot be negative, got %d bps", cfg.AnnualRateBps)
	case cfg.AnnualRateBps > 10_000:
		// A rate above 100% a year is almost certainly a units mistake — someone
		// writing 20 meaning 20% when the field wants 2000 bps.
		return Policy{}, errs.InvalidArgument(
			"an annual interest rate above 100%% (10000 bps) is not accepted, got %d bps", cfg.AnnualRateBps)
	case cfg.MinimumBalance.IsNegative():
		return Policy{}, errs.InvalidArgument("the minimum balance cannot be negative")
	}
	return Policy{
		annualRateBps:  cfg.AnnualRateBps,
		minimumBalance: cfg.MinimumBalance,
		enabled:        cfg.Enabled,
	}, nil
}

// AnnualRateBps returns the configured yearly rate.
func (p Policy) AnnualRateBps() int64 { return p.annualRateBps }

// MinimumBalance returns the qualifying balance.
func (p Policy) MinimumBalance() money.Money { return p.minimumBalance }

// Enabled reports whether accrual is switched on.
func (p Policy) Enabled() bool { return p.enabled }

// WithRate returns a copy using a different rate, so that an operator can replay a
// past accrual at the rate that applied then.
func (p Policy) WithRate(annualRateBps int64) (Policy, error) {
	return NewPolicy(Config{
		AnnualRateBps:  annualRateBps,
		MinimumBalance: p.minimumBalance,
		Enabled:        p.enabled,
	})
}

// Accrual is the interest owed to one wallet for one day.
type Accrual struct {
	// Amount is the interest to credit. Zero means nothing is owed.
	Amount money.Money
	// Eligible reports whether the wallet qualified at all, which lets a report
	// distinguish "too small to qualify" from "qualified but rounded to nothing".
	Eligible bool
	// Skipped explains why nothing was credited.
	Skipped string
}

// Calculate works out one day's interest on a balance.
//
// The formula is balance * rate / 10000 / 365, evaluated as a single integer
// expression so that no intermediate rounding compounds.
func (p Policy) Calculate(balance money.Money) (Accrual, error) {
	zero := money.Zero(balance.Currency())

	if !p.enabled {
		return Accrual{Amount: zero, Skipped: "interest accrual is disabled"}, nil
	}
	if p.annualRateBps == 0 {
		return Accrual{Amount: zero, Skipped: "the configured rate is zero"}, nil
	}
	if !balance.IsPositive() {
		return Accrual{Amount: zero, Skipped: "the balance is not positive"}, nil
	}

	if p.minimumBalance.IsPositive() {
		below, err := balance.LessThan(p.minimumBalance)
		if err != nil {
			return Accrual{}, errs.Internal("failed to compare against the minimum balance").WithCause(err)
		}
		if below {
			return Accrual{
				Amount:  zero,
				Skipped: fmt.Sprintf("the balance is below the %s minimum", p.minimumBalance),
			}, nil
		}
	}

	// balance * annualRateBps can be large; guard before multiplying.
	if balance.Minor() > (1<<62)/p.annualRateBps {
		return Accrual{}, errs.Internal("the balance is too large to accrue interest on safely")
	}

	// Truncating division rounds down, which is deliberate: the platform never
	// over-pays. A wallet whose daily interest is under one minor unit earns nothing
	// that day and is reported as eligible-but-zero.
	daily := balance.Minor() * p.annualRateBps / 10_000 / DaysPerYear

	amount, err := money.New(daily, balance.Currency())
	if err != nil {
		return Accrual{}, errs.Internal("failed to build the interest amount").WithCause(err)
	}
	accrual := Accrual{Amount: amount, Eligible: true}
	if daily == 0 {
		accrual.Skipped = "one day of interest rounds to less than one minor unit"
	}
	return accrual, nil
}

// IdempotencyKey returns the key that makes an accrual run repeatable.
//
// Keying on wallet plus date means re-running a day — after a crash, or because an
// operator replayed it — credits nothing extra. Without this, a retried nightly job
// would pay every user twice.
func IdempotencyKey(walletID string, date time.Time) string {
	return fmt.Sprintf("interest:%s:%s", walletID, date.UTC().Format("2006-01-02"))
}

// AccrualDate normalises an instant to the UTC date the accrual belongs to.
func AccrualDate(at time.Time) time.Time {
	utc := at.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// ParseAccrualDate parses a YYYY-MM-DD accrual date.
func ParseAccrualDate(raw string) (time.Time, error) {
	date, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		return time.Time{}, errs.InvalidArgument("the accrual date must look like 2026-07-26, got %q", raw)
	}
	return date, nil
}
