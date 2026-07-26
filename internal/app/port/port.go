// Package port declares the interfaces the application layer depends on.
//
// This is where the dependency rule is enforced. Use cases talk to these
// interfaces; the concrete Postgres, Kafka, Redis and gRPC implementations live in
// internal/adapter and are injected at boot. Nothing in internal/app or
// internal/domain imports a driver, so the entire business logic of the service is
// testable with the in-memory fakes in internal/app/apptest.
package port

import (
	"context"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/event"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/jackc/pgx/v5"
)

// Tx is the transaction handle threaded through a unit of work.
//
// It is pgx.Tx rather than a hand-rolled abstraction on purpose. Inventing a
// database-agnostic transaction interface here would buy nothing — the service is
// committed to Postgres for its ledger, because it relies on ACID guarantees and
// on FOR UPDATE row locking — and it would cost every repository an extra layer of
// indirection. The ports stay narrow where abstraction pays and concrete where it
// does not.
type Tx = pgx.Tx

// TxManager runs a unit of work transactionally.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
	WithinSerializableTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Reader is the read-only query surface, satisfied by both the pool and a Tx, so
// that a repository method works inside or outside a transaction.
type Reader interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WalletRepository persists wallets.
type WalletRepository interface {
	// Insert stores a new wallet.
	Insert(ctx context.Context, tx Tx, w *wallet.Wallet) error
	// FindByUserID reads a wallet without locking it. For display only.
	FindByUserID(ctx context.Context, r Reader, userID string) (*wallet.Wallet, error)
	// FindByID reads a wallet by its own identifier.
	FindByID(ctx context.Context, r Reader, id string) (*wallet.Wallet, error)
	// LockByUserID reads a wallet FOR UPDATE, serialising concurrent money movement
	// on that row. Every use case that changes a balance must go through this: two
	// simultaneous debits that both read the same balance would otherwise both pass
	// the sufficiency check and overdraw the account.
	LockByUserID(ctx context.Context, tx Tx, userID string) (*wallet.Wallet, error)
	// LockByID is LockByUserID addressed by wallet id.
	LockByID(ctx context.Context, tx Tx, id string) (*wallet.Wallet, error)
	// Update persists a mutated wallet, checking the version it was loaded at.
	Update(ctx context.Context, tx Tx, w *wallet.Wallet, expectedVersion int64) error
	// ListActivePage returns a page of active wallets ordered by id, for the
	// interest and reconciliation jobs. Paging by id keeps memory flat regardless of
	// how many wallets exist.
	ListActivePage(ctx context.Context, r Reader, afterID string, limit int) ([]*wallet.Wallet, error)
	// Count returns the number of wallets.
	Count(ctx context.Context, r Reader) (int64, error)
}

// LedgerRepository appends to and queries the immutable ledger.
type LedgerRepository interface {
	// Append writes one entry. There is deliberately no Update or Delete.
	Append(ctx context.Context, tx Tx, entry ledger.Entry) error
	// List returns a filtered, paginated slice of entries.
	List(ctx context.Context, r Reader, filter ledger.Filter) (ledger.Page, error)
	// FindByIdempotencyKey returns the entry a previous request produced, which is
	// how a replayed money movement reconstructs its original response from the
	// source of truth instead of a cached copy.
	FindByIdempotencyKey(ctx context.Context, r Reader, walletID, key string) (*ledger.Entry, error)
	// SumByWallet returns the net of every entry for a wallet, used to verify the
	// cached balance during reconciliation.
	SumByWallet(ctx context.Context, r Reader, walletID string) (money.Money, error)
	// FindMismatches returns wallets whose stored balance disagrees with their
	// ledger. The comparison runs in the database because summing millions of
	// entries in Go would be pointless network traffic.
	FindMismatches(ctx context.Context, r Reader, walletID string, limit int) ([]ledger.Mismatch, error)
}

// GiftCardRepository persists gift cards.
type GiftCardRepository interface {
	// InsertBatch stores freshly minted cards.
	InsertBatch(ctx context.Context, tx Tx, cards []*giftcard.GiftCard) error
	// FindByCodeHash looks a card up by the hash of its code.
	FindByCodeHash(ctx context.Context, r Reader, codeHash string) (*giftcard.GiftCard, error)
	// FindByID looks a card up by identifier.
	FindByID(ctx context.Context, r Reader, id string) (*giftcard.GiftCard, error)
	// LockByCodeHash reads a card FOR UPDATE so that two concurrent redemptions of
	// one code cannot both succeed.
	LockByCodeHash(ctx context.Context, tx Tx, codeHash string) (*giftcard.GiftCard, error)
	// MarkRedeemed applies a redemption with a conditional update that only matches
	// a row still in ACTIVE. It reports false when the row had already changed
	// hands, which is the last line of defence against double redemption.
	MarkRedeemed(ctx context.Context, tx Tx, card *giftcard.GiftCard, expectedVersion int64) (bool, error)
	// Update persists a revocation.
	Update(ctx context.Context, tx Tx, card *giftcard.GiftCard, expectedVersion int64) error
	// List returns a filtered page of cards.
	List(ctx context.Context, r Reader, filter giftcard.Filter) ([]*giftcard.GiftCard, int64, error)
}

// DiscountRepository persists discount codes.
type DiscountRepository interface {
	Insert(ctx context.Context, tx Tx, code *discount.Code) error
	FindByCode(ctx context.Context, r Reader, code string) (*discount.Code, error)
	LockByCode(ctx context.Context, tx Tx, code string) (*discount.Code, error)
	Update(ctx context.Context, tx Tx, code *discount.Code, expectedVersion int64) error
}

// HoldRepository persists holds.
type HoldRepository interface {
	Insert(ctx context.Context, tx Tx, h *hold.Hold) error
	FindByID(ctx context.Context, r Reader, id string) (*hold.Hold, error)
	LockByID(ctx context.Context, tx Tx, id string) (*hold.Hold, error)
	Update(ctx context.Context, tx Tx, h *hold.Hold, expectedVersion int64) error
	List(ctx context.Context, r Reader, filter hold.Filter) ([]*hold.Hold, int64, error)
	// ListExpired returns active holds past their TTL, for the sweeper.
	ListExpired(ctx context.Context, r Reader, before time.Time, limit int) ([]*hold.Hold, error)
}

// IdempotencyRecord is the bookkeeping row that makes a request replay-safe.
type IdempotencyRecord struct {
	// Key is the client-supplied idempotency key.
	Key string
	// Operation namespaces the key, so that the same key used for a debit and for a
	// credit does not collide.
	Operation string
	// RequestHash fingerprints the request payload. A second request under the same
	// key with a different payload is a client bug, not a retry, and is rejected.
	RequestHash string
	// Response is the marshalled result of the original call, replayed verbatim.
	Response []byte
	// WalletID scopes the record for auditing.
	WalletID  string
	CreatedAt time.Time
}

// IdempotencyStore guarantees that a money-moving request takes effect once.
type IdempotencyStore interface {
	// Claim attempts to reserve the key inside tx.
	//
	// It returns claimed=true when this is the first time the key has been seen, in
	// which case the caller proceeds with the operation. It returns the stored
	// record and claimed=false when the key already exists, in which case the caller
	// must replay that record's response rather than moving money again.
	Claim(ctx context.Context, tx Tx, record IdempotencyRecord) (existing *IdempotencyRecord, claimed bool, err error)
	// SaveResponse attaches the result to a claimed key.
	SaveResponse(ctx context.Context, tx Tx, key, operation string, response []byte) error
	// Purge deletes records older than the retention window.
	Purge(ctx context.Context, tx Tx, before time.Time) (int64, error)
}

// EventPublisher records domain events for asynchronous delivery.
//
// It writes to the outbox inside the caller's transaction — never straight to the
// broker — which is what makes a state change and its announcement atomic.
type EventPublisher interface {
	// Publish appends envelopes to the outbox within tx.
	Publish(ctx context.Context, tx Tx, envelopes ...event.Envelope) error
	// Notify wakes the dispatcher after the transaction commits, so that the happy
	// path does not wait for the next poll tick. It must never be called before the
	// commit: the dispatcher would find nothing and go back to sleep.
	Notify()
}

// PaymentRequest asks the Payment Adapter to start a bank top-up.
type PaymentRequest struct {
	UserID    string
	Amount    money.Money
	ReturnURL string
	// IdempotencyKey is forwarded so that a retried charge does not create a second
	// payment intent at the bank.
	IdempotencyKey string
	Metadata       map[string]string
}

// PaymentIntent is the Payment Adapter's answer.
type PaymentIntent struct {
	ID          string
	RedirectURL string
	Amount      money.Money
	State       string
	ExpiresAt   *time.Time
}

// PaymentGateway is the outbound port to the Payment Adapter service.
//
// The wallet knows nothing about any specific bank: that translation is the
// adapter's job — the Anti-Corruption Layer from the architecture document — and
// this interface is the boundary it defends.
type PaymentGateway interface {
	// InitiatePayment creates an intent and returns where to send the user.
	InitiatePayment(ctx context.Context, req PaymentRequest) (PaymentIntent, error)
	// GetPaymentIntent reads an intent's current state, for reconciliation.
	GetPaymentIntent(ctx context.Context, id string) (PaymentIntent, error)
}

// AbuseVerdict is the rate limiter's report on a user's recent attempts.
type AbuseVerdict struct {
	// Blocked reports that the attempt must be refused.
	Blocked bool
	// Rule names the violated threshold.
	Rule string
	// RetryAfter is how long until the user may try again.
	RetryAfter time.Duration
	// FailedInWindow is the number of failures in the widest window.
	FailedInWindow int64
}

// AbuseLimiter counts failed gift-card attempts over sliding windows.
type AbuseLimiter interface {
	// CheckAndRecordFailure registers a failed attempt and reports the verdict.
	CheckAndRecordFailure(ctx context.Context, userID string) (AbuseVerdict, error)
	// Check reports the verdict without recording anything, so a legitimate
	// redemption is not charged against the user's allowance.
	Check(ctx context.Context, userID string) (AbuseVerdict, error)
	// Reset clears a user's counters, used when Support clears their flag.
	Reset(ctx context.Context, userID string) error
}

// Metrics is the instrumentation the use cases report through.
type Metrics interface {
	WalletOperation(operation, outcome string)
	MoneyMoved(direction, reason, currency string, amountMinor int64)
	IdempotentReplay(operation string)
	BusinessRuleRejection(rule string)
	LedgerMismatch(count int64)
	RateLimitBlock(limiter string)
}
