// Package clock abstracts the reading of wall-clock time.
//
// Financial use cases depend on time: a refund window, a hold expiry, an
// interest accrual date. Injecting a Clock lets those rules be tested at an
// exact instant instead of with a sleep.
package clock

import (
	"sync"
	"time"
)

// Clock reads the current time. Production code uses System; tests use Fixed.
type Clock interface {
	// Now returns the current instant in UTC.
	Now() time.Time
}

// System is the real clock. It always answers in UTC so that timestamps are
// comparable regardless of the host's timezone.
type System struct{}

// Now returns time.Now() in UTC.
func (System) Now() time.Time { return time.Now().UTC() }

// Fixed is a controllable clock for tests.
type Fixed struct {
	mu  sync.RWMutex
	now time.Time
}

// NewFixed returns a Fixed clock pinned to t.
func NewFixed(t time.Time) *Fixed { return &Fixed{now: t.UTC()} }

// Now returns the pinned instant.
func (f *Fixed) Now() time.Time {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.now
}

// Set moves the clock to t.
func (f *Fixed) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t.UTC()
}

// Advance moves the clock forward by d.
func (f *Fixed) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
