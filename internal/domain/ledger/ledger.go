// Package ledger models the append-only financial record.
//
// The ledger — not the wallet's balance column — is the source of truth. The
// balance is a cached projection of the ledger that exists so that a read does not
// have to sum a user's entire history; the reconciliation job periodically proves
// the two agree, and any disagreement is a page-on-call incident.
//
// Entries are immutable by construction: this package offers no setter, and the
// database rejects UPDATE and DELETE on the table outright.
package ledger

import (
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
)

// Entry is one immutable line of the ledger.
type Entry struct {
	// ID uniquely identifies the entry.
	ID string
	// Sequence is a per-database monotonically increasing number assigned on insert.
	// It gives auditors a total order that survives identical timestamps.
	Sequence int64
	// WalletID is the affected wallet.
	WalletID string
	// UserID is denormalised so that an audit query never needs a join.
	UserID string
	// Direction is DEBIT or CREDIT.
	Direction wallet.Direction
	// Amount is always positive; Direction carries the sign.
	Amount money.Money
	// BalanceAfter is the wallet balance immediately after this entry, which lets
	// an auditor verify the whole chain without recomputing running totals.
	BalanceAfter money.Money
	// Reason explains the movement.
	Reason wallet.Reason
	// ReferenceID points at the originating entity: an order, a trade, a payment
	// intent, a gift card.
	ReferenceID string
	// Description is human-readable context for support staff.
	Description string
	// CorrelationID ties this entry to the distributed transaction that caused it.
	CorrelationID string
	// IdempotencyKey records which client request produced the entry.
	IdempotencyKey string
	// CreatedAt is when the entry was appended.
	CreatedAt time.Time
}

// NewEntry builds an Entry from a movement the Wallet aggregate just applied.
func NewEntry(
	id string,
	w *wallet.Wallet,
	movement wallet.Movement,
	correlationID, idempotencyKey string,
	at time.Time,
) (Entry, error) {
	if id == "" {
		return Entry{}, errs.Internal("a ledger entry requires an id")
	}
	if w == nil {
		return Entry{}, errs.Internal("a ledger entry requires a wallet")
	}
	if !movement.Amount.IsPositive() {
		return Entry{}, errs.Internal("a ledger entry amount must be positive; direction carries the sign")
	}

	return Entry{
		ID:             id,
		WalletID:       w.ID(),
		UserID:         w.UserID(),
		Direction:      movement.Direction,
		Amount:         movement.Amount,
		BalanceAfter:   movement.BalanceAfter,
		Reason:         movement.Reason,
		ReferenceID:    movement.ReferenceID,
		Description:    movement.Description,
		CorrelationID:  correlationID,
		IdempotencyKey: idempotencyKey,
		CreatedAt:      at.UTC(),
	}, nil
}

// SignedAmount returns the amount with a sign: negative for a debit. Summing the
// signed amounts of every entry must reproduce the wallet balance exactly.
func (e Entry) SignedAmount() money.Money {
	if e.Direction == wallet.DirectionDebit {
		return e.Amount.Neg()
	}
	return e.Amount
}

// Filter narrows a ledger query.
type Filter struct {
	// WalletID restricts the query to one wallet. Required for user-facing reads.
	WalletID string
	// Reasons, when non-empty, keeps only these reasons.
	Reasons []wallet.Reason
	// Direction, when non-empty, keeps only this side.
	Direction wallet.Direction
	// ReferenceID, when set, finds every entry for one order or trade — the query a
	// support agent runs when a user disputes a purchase.
	ReferenceID string
	// From and To bound CreatedAt as [From, To).
	From *time.Time
	To   *time.Time
	// Limit and Offset paginate.
	Limit  int
	Offset int
}

// Page is a paginated slice of the ledger.
type Page struct {
	Entries    []Entry
	TotalItems int64
	Limit      int
	Offset     int
}

// Balance sums a set of entries into a net amount. Used by reconciliation.
func Balance(entries []Entry) (money.Money, error) {
	var total money.Money
	for _, entry := range entries {
		next, err := total.Add(entry.SignedAmount())
		if err != nil {
			return money.Money{}, errs.Internal("failed to sum ledger entries").WithCause(err)
		}
		total = next
	}
	return total, nil
}

// Mismatch records a wallet whose stored balance disagrees with its ledger.
type Mismatch struct {
	WalletID      string
	UserID        string
	StoredBalance money.Money
	LedgerBalance money.Money
	Delta         money.Money
}
