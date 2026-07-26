// Package money provides an exact monetary value type.
//
// Every amount in Arcadia is an integer count of the currency's minor unit.
// Floating point is never used: a float64 cannot represent 0.1 exactly, and a
// ledger that drifts by a hundredth of a unit per transaction is a ledger that
// fails reconciliation. Percentages are expressed in basis points (1% == 100
// bps) and every rounding step is explicit.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Errors returned by this package. Callers map these onto domain errors.
var (
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrInvalidCurrency  = errors.New("money: currency must be a 3-letter ISO-4217 code")
	ErrOverflow         = errors.New("money: arithmetic overflow")
	ErrNegativeAmount   = errors.New("money: amount must not be negative")
	ErrInvalidFormat    = errors.New("money: invalid format")
)

// Money is an immutable amount in the minor unit of a currency.
//
// The zero value is a valid "zero, currency unknown" and compares equal to any
// zero amount, which keeps aggregate initialisation simple.
type Money struct {
	minor    int64
	currency string
}

// New builds a Money from minor units. The currency is normalised to upper case.
func New(minor int64, currency string) (Money, error) {
	cur, err := normaliseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	return Money{minor: minor, currency: cur}, nil
}

// MustNew is New for statically known values; it panics on a bad currency and is
// intended for tests and package-level constants only.
func MustNew(minor int64, currency string) Money {
	m, err := New(minor, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero returns a zero amount in the given currency.
func Zero(currency string) Money {
	cur, err := normaliseCurrency(currency)
	if err != nil {
		return Money{}
	}
	return Money{currency: cur}
}

func normaliseCurrency(currency string) (string, error) {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if len(cur) != 3 {
		return "", fmt.Errorf("%w: got %q", ErrInvalidCurrency, currency)
	}
	for i := 0; i < len(cur); i++ {
		if cur[i] < 'A' || cur[i] > 'Z' {
			return "", fmt.Errorf("%w: got %q", ErrInvalidCurrency, currency)
		}
	}
	return cur, nil
}

// Minor returns the raw amount in minor units.
func (m Money) Minor() int64 { return m.minor }

// Currency returns the ISO-4217 code, or "" for the zero value.
func (m Money) Currency() string { return m.currency }

// IsZero reports whether the amount is exactly zero, regardless of currency.
func (m Money) IsZero() bool { return m.minor == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (m Money) IsPositive() bool { return m.minor > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (m Money) IsNegative() bool { return m.minor < 0 }

// Add returns m+other. Both operands must share a currency.
func (m Money) Add(other Money) (Money, error) {
	cur, err := m.unify(other)
	if err != nil {
		return Money{}, err
	}
	sum := m.minor + other.minor
	// Overflow detection: the sum's sign flipped away from both operands.
	if (m.minor > 0 && other.minor > 0 && sum < 0) || (m.minor < 0 && other.minor < 0 && sum >= 0) {
		return Money{}, ErrOverflow
	}
	return Money{minor: sum, currency: cur}, nil
}

// Sub returns m-other. Both operands must share a currency.
func (m Money) Sub(other Money) (Money, error) {
	cur, err := m.unify(other)
	if err != nil {
		return Money{}, err
	}
	diff := m.minor - other.minor
	if (m.minor >= 0 && other.minor < 0 && diff < 0) || (m.minor < 0 && other.minor > 0 && diff > 0) {
		return Money{}, ErrOverflow
	}
	return Money{minor: diff, currency: cur}, nil
}

// Neg returns -m.
func (m Money) Neg() Money {
	if m.minor == math.MinInt64 {
		// Unreachable for real balances; guarding keeps Neg total.
		return Money{minor: math.MaxInt64, currency: m.currency}
	}
	return Money{minor: -m.minor, currency: m.currency}
}

// Abs returns |m|.
func (m Money) Abs() Money {
	if m.minor < 0 {
		return m.Neg()
	}
	return m
}

// MulInt scales the amount by an integer factor.
func (m Money) MulInt(factor int64) (Money, error) {
	if factor == 0 || m.minor == 0 {
		return Money{currency: m.currency}, nil
	}
	product := m.minor * factor
	if product/factor != m.minor {
		return Money{}, ErrOverflow
	}
	return Money{minor: product, currency: m.currency}, nil
}

// Percent applies a basis-point rate and rounds half-up on the absolute value,
// so that +0.5 rounds to +1 and -0.5 rounds to -1 (symmetric rounding).
//
// 100 bps == 1%, 10_000 bps == 100%.
func (m Money) Percent(bps int64) (Money, error) {
	if bps == 0 || m.minor == 0 {
		return Money{currency: m.currency}, nil
	}
	neg := m.minor < 0
	abs := m.minor
	if neg {
		abs = -abs
	}
	// abs*bps can overflow for very large balances; check before multiplying.
	if bps != 0 && abs > math.MaxInt64/bps {
		return Money{}, ErrOverflow
	}
	scaled := abs*bps + 5_000 // +half of 10_000 for round-half-up
	result := scaled / 10_000
	if neg {
		result = -result
	}
	return Money{minor: result, currency: m.currency}, nil
}

// Allocate splits the amount across weights without losing a single minor unit.
//
// The largest-remainder method is used: every recipient gets the floor of its
// share, then the units lost to truncation are handed out one at a time in
// descending remainder order. This is the standard way to make a 70/30 revenue
// split of an odd amount add back up to the original.
func (m Money) Allocate(weights ...int64) ([]Money, error) {
	if len(weights) == 0 {
		return nil, errors.New("money: Allocate requires at least one weight")
	}
	var total int64
	for _, w := range weights {
		if w < 0 {
			return nil, errors.New("money: Allocate weights must not be negative")
		}
		total += w
	}
	if total == 0 {
		return nil, errors.New("money: Allocate weights must not all be zero")
	}

	neg := m.minor < 0
	abs := m.minor
	if neg {
		abs = -abs
	}

	shares := make([]int64, len(weights))
	remainders := make([]int64, len(weights))
	var distributed int64
	for i, w := range weights {
		if w != 0 && abs > math.MaxInt64/w {
			return nil, ErrOverflow
		}
		numerator := abs * w
		shares[i] = numerator / total
		remainders[i] = numerator % total
		distributed += shares[i]
	}

	// Hand out the leftover units to the largest remainders first, breaking ties
	// by index so the result is deterministic.
	leftover := abs - distributed
	for leftover > 0 {
		best := -1
		for i := range shares {
			if remainders[i] == 0 {
				continue
			}
			if best == -1 || remainders[i] > remainders[best] {
				best = i
			}
		}
		if best == -1 {
			// All remainders consumed but units remain (possible when some
			// weights are zero); fall back to the first non-zero weight.
			for i, w := range weights {
				if w > 0 {
					best = i
					break
				}
			}
		}
		shares[best]++
		remainders[best] = 0
		leftover--
	}

	out := make([]Money, len(shares))
	for i, s := range shares {
		if neg {
			s = -s
		}
		out[i] = Money{minor: s, currency: m.currency}
	}
	return out, nil
}

// Cmp returns -1, 0 or +1 comparing m with other. It reports an error rather
// than silently comparing across currencies.
func (m Money) Cmp(other Money) (int, error) {
	if _, err := m.unify(other); err != nil {
		return 0, err
	}
	switch {
	case m.minor < other.minor:
		return -1, nil
	case m.minor > other.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// GreaterThan is a convenience wrapper over Cmp.
func (m Money) GreaterThan(other Money) (bool, error) {
	c, err := m.Cmp(other)
	return c > 0, err
}

// LessThan is a convenience wrapper over Cmp.
func (m Money) LessThan(other Money) (bool, error) {
	c, err := m.Cmp(other)
	return c < 0, err
}

// Equal reports whether both the amount and the currency match.
func (m Money) Equal(other Money) bool {
	return m.minor == other.minor && m.sameCurrency(other)
}

// SameCurrency reports whether two amounts can be combined arithmetically.
func (m Money) SameCurrency(other Money) bool { return m.sameCurrency(other) }

func (m Money) sameCurrency(other Money) bool {
	// A zero value ("", 0) is compatible with everything, which lets callers
	// start from an empty accumulator.
	if m.currency == "" || other.currency == "" {
		return true
	}
	return m.currency == other.currency
}

func (m Money) unify(other Money) (string, error) {
	if !m.sameCurrency(other) {
		return "", fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	if m.currency != "" {
		return m.currency, nil
	}
	return other.currency, nil
}

// String renders the amount with two decimal places for readability, e.g.
// "1234.50 IRR". It is for logs and errors, never for parsing.
func (m Money) String() string {
	cur := m.currency
	if cur == "" {
		cur = "???"
	}
	neg := m.minor < 0
	abs := m.minor
	if neg {
		abs = -abs
	}
	s := fmt.Sprintf("%d.%02d %s", abs/100, abs%100, cur)
	if neg {
		return "-" + s
	}
	return s
}

// wireMoney is the JSON shape used by the REST adapters. Amounts travel as
// strings so that a JavaScript client cannot silently truncate an int64 that
// exceeds 2^53.
type wireMoney struct {
	AmountMinor string `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// MarshalJSON implements json.Marshaler.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(wireMoney{
		AmountMinor: strconv.FormatInt(m.minor, 10),
		Currency:    m.currency,
	})
}

// UnmarshalJSON implements json.Unmarshaler. It accepts either a JSON string or
// a JSON number for amount_minor so that hand-written requests stay ergonomic.
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw struct {
		AmountMinor json.Number `json:"amount_minor"`
		Currency    string      `json:"currency"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidFormat, err)
	}
	minor := int64(0)
	if raw.AmountMinor.String() != "" {
		v, err := strconv.ParseInt(raw.AmountMinor.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("%w: amount_minor %q is not an integer", ErrInvalidFormat, raw.AmountMinor)
		}
		minor = v
	}
	cur, err := normaliseCurrency(raw.Currency)
	if err != nil {
		return err
	}
	m.minor = minor
	m.currency = cur
	return nil
}

// Sum adds a list of amounts, returning a zero value for an empty list.
func Sum(amounts ...Money) (Money, error) {
	var acc Money
	for _, a := range amounts {
		next, err := acc.Add(a)
		if err != nil {
			return Money{}, err
		}
		acc = next
	}
	return acc, nil
}
