// Package apptest provides in-memory implementations of every outbound port.
//
// These are what make the use-case tests worth reading: no Postgres, no Kafka, no
// Redis, no Docker — just the business logic under test with fast, deterministic
// collaborators. The fakes are not toys, though. They model the two behaviours the
// use cases genuinely depend on:
//
//   - transaction rollback, so that a test asserting "a rejected debit changes
//     nothing" actually proves it rather than passing by accident, and
//   - optimistic-concurrency version checks, so that a missing version bump in a
//     repository call is caught here rather than in production.
package apptest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/discount"
	"github.com/MS-Arcadia/wallet-service/internal/domain/giftcard"
	"github.com/MS-Arcadia/wallet-service/internal/domain/hold"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	"github.com/MS-Arcadia/wallet-service/internal/platform/errs"
	"github.com/MS-Arcadia/wallet-service/internal/platform/event"
	"github.com/MS-Arcadia/wallet-service/internal/platform/money"
)

// --- Records --------------------------------------------------------------
//
// Aggregates are stored as flat records and rehydrated on every read. That is what
// a real repository does, and it means a use case cannot accidentally mutate stored
// state by holding on to a pointer — a bug the fakes would otherwise hide.

type walletRecord struct {
	ID        string
	UserID    string
	Balance   money.Money
	Held      money.Money
	Status    wallet.Status
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

func toWalletRecord(w *wallet.Wallet) walletRecord {
	return walletRecord{
		ID:        w.ID(),
		UserID:    w.UserID(),
		Balance:   w.Balance(),
		Held:      w.Held(),
		Status:    w.Status(),
		Version:   w.Version(),
		CreatedAt: w.CreatedAt(),
		UpdatedAt: w.UpdatedAt(),
	}
}

func (r walletRecord) rehydrate() (*wallet.Wallet, error) {
	return wallet.Rehydrate(r.ID, r.UserID, r.Balance, r.Held, r.Status, r.Version, r.CreatedAt, r.UpdatedAt)
}

type giftCardRecord struct {
	ID         string
	CodeHash   string
	CodeHint   string
	Value      money.Money
	Status     giftcard.Status
	IssuedBy   string
	BatchID    string
	Note       string
	RedeemedBy string
	RedeemedAt *time.Time
	RevokedAt  *time.Time
	RevokeNote string
	CreatedAt  time.Time
	Version    int64
}

func toGiftCardRecord(c *giftcard.GiftCard) giftCardRecord {
	return giftCardRecord{
		ID: c.ID(), CodeHash: c.CodeHash(), CodeHint: c.CodeHint(), Value: c.Value(),
		Status: c.Status(), IssuedBy: c.IssuedBy(), BatchID: c.BatchID(), Note: c.Note(),
		RedeemedBy: c.RedeemedBy(), RedeemedAt: c.RedeemedAt(), RevokedAt: c.RevokedAt(),
		RevokeNote: c.RevokeNote(), CreatedAt: c.CreatedAt(), Version: c.Version(),
	}
}

func (r giftCardRecord) rehydrate() (*giftcard.GiftCard, error) {
	return giftcard.Rehydrate(r.ID, r.CodeHash, r.CodeHint, r.Value, r.Status,
		r.IssuedBy, r.BatchID, r.Note, r.RedeemedBy, r.RedeemedAt, r.RevokedAt,
		r.RevokeNote, r.CreatedAt, r.Version)
}

type discountRecord struct {
	ID              string
	Code            string
	PercentBps      int32
	AmountOff       money.Money
	MaxDiscount     money.Money
	MinOrderAmount  money.Money
	Status          discount.Status
	MaxRedemptions  int32
	RedemptionCount int32
	IssuedBy        string
	ExpiresAt       *time.Time
	CreatedAt       time.Time
	Version         int64
}

func toDiscountRecord(c *discount.Code) discountRecord {
	return discountRecord{
		ID: c.ID(), Code: c.Code(), PercentBps: c.PercentBps(), AmountOff: c.AmountOff(),
		MaxDiscount: c.MaxDiscount(), MinOrderAmount: c.MinOrderAmount(), Status: c.Status(),
		MaxRedemptions: c.MaxRedemptions(), RedemptionCount: c.RedemptionCount(),
		IssuedBy: c.IssuedBy(), ExpiresAt: c.ExpiresAt(), CreatedAt: c.CreatedAt(), Version: c.Version(),
	}
}

func (r discountRecord) rehydrate() (*discount.Code, error) {
	return discount.Rehydrate(r.ID, r.Code, r.PercentBps, r.AmountOff, r.MaxDiscount,
		r.MinOrderAmount, r.Status, r.MaxRedemptions, r.RedemptionCount, r.IssuedBy,
		r.ExpiresAt, r.CreatedAt, r.Version)
}

type holdRecord struct {
	ID             string
	WalletID       string
	UserID         string
	Amount         money.Money
	CapturedAmount money.Money
	Status         hold.Status
	ReferenceID    string
	Reason         string
	ExpiresAt      *time.Time
	CreatedAt      time.Time
	ResolvedAt     *time.Time
	Version        int64
}

func toHoldRecord(h *hold.Hold) holdRecord {
	return holdRecord{
		ID: h.ID(), WalletID: h.WalletID(), UserID: h.UserID(), Amount: h.Amount(),
		CapturedAmount: h.CapturedAmount(), Status: h.Status(), ReferenceID: h.ReferenceID(),
		Reason: h.Reason(), ExpiresAt: h.ExpiresAt(), CreatedAt: h.CreatedAt(),
		ResolvedAt: h.ResolvedAt(), Version: h.Version(),
	}
}

func (r holdRecord) rehydrate() (*hold.Hold, error) {
	return hold.Rehydrate(r.ID, r.WalletID, r.UserID, r.Amount, r.CapturedAmount,
		r.Status, r.ReferenceID, r.Reason, r.ExpiresAt, r.CreatedAt, r.ResolvedAt, r.Version)
}

// --- Store ----------------------------------------------------------------

// Store is the shared in-memory database behind every fake repository.
type Store struct {
	mu sync.Mutex

	Wallets   map[string]walletRecord
	GiftCards map[string]giftCardRecord
	Discounts map[string]discountRecord
	Holds     map[string]holdRecord
	Ledger    []ledger.Entry
	Idem      map[string]port.IdempotencyRecord
	Events    []event.Envelope

	// sequence assigns ledger sequence numbers, mirroring a BIGSERIAL column.
	sequence int64
	// Notified counts dispatcher wake-ups, so a test can assert that a use case
	// remembered to nudge the outbox after committing.
	Notified int
	// FailOn lets a test inject a failure at a named repository call, to exercise
	// rollback paths.
	FailOn map[string]error
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		Wallets:   make(map[string]walletRecord),
		GiftCards: make(map[string]giftCardRecord),
		Discounts: make(map[string]discountRecord),
		Holds:     make(map[string]holdRecord),
		Idem:      make(map[string]port.IdempotencyRecord),
		FailOn:    make(map[string]error),
	}
}

func (s *Store) fail(name string) error {
	if err, ok := s.FailOn[name]; ok {
		return err
	}
	return nil
}

// snapshot copies every collection so that a failed transaction can be undone.
func (s *Store) snapshot() *Store {
	copyOf := &Store{
		Wallets:   make(map[string]walletRecord, len(s.Wallets)),
		GiftCards: make(map[string]giftCardRecord, len(s.GiftCards)),
		Discounts: make(map[string]discountRecord, len(s.Discounts)),
		Holds:     make(map[string]holdRecord, len(s.Holds)),
		Idem:      make(map[string]port.IdempotencyRecord, len(s.Idem)),
		Ledger:    append([]ledger.Entry(nil), s.Ledger...),
		Events:    append([]event.Envelope(nil), s.Events...),
		sequence:  s.sequence,
	}
	for k, v := range s.Wallets {
		copyOf.Wallets[k] = v
	}
	for k, v := range s.GiftCards {
		copyOf.GiftCards[k] = v
	}
	for k, v := range s.Discounts {
		copyOf.Discounts[k] = v
	}
	for k, v := range s.Holds {
		copyOf.Holds[k] = v
	}
	for k, v := range s.Idem {
		copyOf.Idem[k] = v
	}
	return copyOf
}

func (s *Store) restore(from *Store) {
	s.Wallets = from.Wallets
	s.GiftCards = from.GiftCards
	s.Discounts = from.Discounts
	s.Holds = from.Holds
	s.Idem = from.Idem
	s.Ledger = from.Ledger
	s.Events = from.Events
	s.sequence = from.sequence
}

// EventsOfType returns every published envelope of one type.
func (s *Store) EventsOfType(eventType string) []event.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()

	matches := make([]event.Envelope, 0, 2)
	for _, envelope := range s.Events {
		if envelope.EventType == eventType {
			matches = append(matches, envelope)
		}
	}
	return matches
}

// HasEvent reports whether an event of the given type was published.
func (s *Store) HasEvent(eventType string) bool { return len(s.EventsOfType(eventType)) > 0 }

// EventTypes returns the published event types in order, for readable assertions.
func (s *Store) EventTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	types := make([]string, 0, len(s.Events))
	for _, envelope := range s.Events {
		types = append(types, envelope.EventType)
	}
	return types
}

// LedgerFor returns a wallet's entries in sequence order.
func (s *Store) LedgerFor(walletID string) []ledger.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries := make([]ledger.Entry, 0, len(s.Ledger))
	for _, entry := range s.Ledger {
		if entry.WalletID == walletID {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
	return entries
}

// WalletOf returns a stored wallet by user id.
func (s *Store) WalletOf(userID string) (*wallet.Wallet, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, record := range s.Wallets {
		if record.UserID == userID {
			w, err := record.rehydrate()
			if err != nil {
				return nil, false
			}
			return w, true
		}
	}
	return nil, false
}

// --- TxManager ------------------------------------------------------------

// TxManager is a fake transaction manager with real rollback semantics.
type TxManager struct {
	store *Store
	// Depth records the deepest nesting seen, so a test can detect an accidental
	// nested transaction.
	Depth int
	depth int
}

// NewTxManager returns a TxManager over store.
func NewTxManager(store *Store) *TxManager { return &TxManager{store: store} }

// WithinTx runs fn, discarding every change when it returns an error.
func (t *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context, tx port.Tx) error) error {
	t.depth++
	if t.depth > t.Depth {
		t.Depth = t.depth
	}
	defer func() { t.depth-- }()

	before := t.store.snapshot()
	// The tx handle is nil: the fakes never touch it, and threading a real pgx.Tx
	// through an in-memory store would prove nothing.
	if err := fn(ctx, nil); err != nil {
		t.store.restore(before)
		return err
	}
	return nil
}

// WithinSerializableTx behaves identically; the fake has no concurrency to serialise.
func (t *TxManager) WithinSerializableTx(ctx context.Context, fn func(ctx context.Context, tx port.Tx) error) error {
	return t.WithinTx(ctx, fn)
}

// --- WalletRepository -----------------------------------------------------

// WalletRepo is a fake port.WalletRepository.
type WalletRepo struct{ store *Store }

// NewWalletRepo returns a WalletRepo over store.
func NewWalletRepo(store *Store) *WalletRepo { return &WalletRepo{store: store} }

// Insert stores a new wallet, rejecting a duplicate user id.
func (r *WalletRepo) Insert(_ context.Context, _ port.Tx, w *wallet.Wallet) error {
	if err := r.store.fail("wallet.Insert"); err != nil {
		return err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, record := range r.store.Wallets {
		if record.UserID == w.UserID() {
			return errs.AlreadyExists("a wallet already exists for user %s", w.UserID())
		}
	}
	r.store.Wallets[w.ID()] = toWalletRecord(w)
	return nil
}

// FindByUserID reads a wallet by owner.
func (r *WalletRepo) FindByUserID(_ context.Context, _ port.Reader, userID string) (*wallet.Wallet, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return r.findByUserLocked(userID)
}

func (r *WalletRepo) findByUserLocked(userID string) (*wallet.Wallet, error) {
	for _, record := range r.store.Wallets {
		if record.UserID == userID {
			return record.rehydrate()
		}
	}
	return nil, wallet.ErrNotFound(userID)
}

// FindByID reads a wallet by identifier.
func (r *WalletRepo) FindByID(_ context.Context, _ port.Reader, id string) (*wallet.Wallet, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.Wallets[id]
	if !ok {
		return nil, errs.NotFound("no wallet exists with id %s", id)
	}
	return record.rehydrate()
}

// LockByUserID reads a wallet, standing in for SELECT ... FOR UPDATE.
func (r *WalletRepo) LockByUserID(ctx context.Context, tx port.Tx, userID string) (*wallet.Wallet, error) {
	if err := r.store.fail("wallet.Lock"); err != nil {
		return nil, err
	}
	return r.FindByUserID(ctx, nil, userID)
}

// LockByID reads a wallet by id.
func (r *WalletRepo) LockByID(ctx context.Context, tx port.Tx, id string) (*wallet.Wallet, error) {
	if err := r.store.fail("wallet.Lock"); err != nil {
		return nil, err
	}
	return r.FindByID(ctx, nil, id)
}

// Update persists a wallet, enforcing the optimistic-concurrency version.
func (r *WalletRepo) Update(_ context.Context, _ port.Tx, w *wallet.Wallet, expectedVersion int64) error {
	if err := r.store.fail("wallet.Update"); err != nil {
		return err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.Wallets[w.ID()]
	if !ok {
		return errs.NotFound("no wallet exists with id %s", w.ID())
	}
	// The stored version must still be the one the aggregate was loaded at.
	// Enforcing this in the fake catches a use case that forgot to pass the version
	// it read, which in production would silently overwrite a concurrent update.
	if record.Version != expectedVersion {
		return errs.Aborted("wallet %s was modified concurrently", w.ID()).
			WithReason("VERSION_CONFLICT")
	}
	r.store.Wallets[w.ID()] = toWalletRecord(w)
	return nil
}

// ListActivePage returns a page of active wallets ordered by id.
func (r *WalletRepo) ListActivePage(_ context.Context, _ port.Reader, afterID string, limit int) ([]*wallet.Wallet, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	ids := make([]string, 0, len(r.store.Wallets))
	for id, record := range r.store.Wallets {
		if record.Status == wallet.StatusActive && id > afterID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	wallets := make([]*wallet.Wallet, 0, limit)
	for _, id := range ids {
		if len(wallets) >= limit {
			break
		}
		w, err := r.store.Wallets[id].rehydrate()
		if err != nil {
			return nil, err
		}
		wallets = append(wallets, w)
	}
	return wallets, nil
}

// Count returns the number of wallets.
func (r *WalletRepo) Count(_ context.Context, _ port.Reader) (int64, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return int64(len(r.store.Wallets)), nil
}

// --- LedgerRepository -----------------------------------------------------

// LedgerRepo is a fake port.LedgerRepository.
type LedgerRepo struct{ store *Store }

// NewLedgerRepo returns a LedgerRepo over store.
func NewLedgerRepo(store *Store) *LedgerRepo { return &LedgerRepo{store: store} }

// Append adds an entry, assigning it a sequence number.
func (r *LedgerRepo) Append(_ context.Context, _ port.Tx, entry ledger.Entry) error {
	if err := r.store.fail("ledger.Append"); err != nil {
		return err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	r.store.sequence++
	entry.Sequence = r.store.sequence
	r.store.Ledger = append(r.store.Ledger, entry)
	return nil
}

// List returns a filtered page of entries, newest first.
func (r *LedgerRepo) List(_ context.Context, _ port.Reader, filter ledger.Filter) (ledger.Page, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	matches := make([]ledger.Entry, 0, len(r.store.Ledger))
	for _, entry := range r.store.Ledger {
		if filter.WalletID != "" && entry.WalletID != filter.WalletID {
			continue
		}
		if filter.Direction != "" && entry.Direction != filter.Direction {
			continue
		}
		if filter.ReferenceID != "" && entry.ReferenceID != filter.ReferenceID {
			continue
		}
		if len(filter.Reasons) > 0 && !containsReason(filter.Reasons, entry.Reason) {
			continue
		}
		if filter.From != nil && entry.CreatedAt.Before(*filter.From) {
			continue
		}
		if filter.To != nil && !entry.CreatedAt.Before(*filter.To) {
			continue
		}
		matches = append(matches, entry)
	}

	sort.Slice(matches, func(i, j int) bool { return matches[i].Sequence > matches[j].Sequence })

	total := int64(len(matches))
	start := filter.Offset
	if start > len(matches) {
		start = len(matches)
	}
	end := start + filter.Limit
	if filter.Limit <= 0 || end > len(matches) {
		end = len(matches)
	}

	return ledger.Page{
		Entries:    matches[start:end],
		TotalItems: total,
		Limit:      filter.Limit,
		Offset:     filter.Offset,
	}, nil
}

func containsReason(reasons []wallet.Reason, reason wallet.Reason) bool {
	for _, candidate := range reasons {
		if candidate == reason {
			return true
		}
	}
	return false
}

// FindByIdempotencyKey returns the entry a given request produced.
func (r *LedgerRepo) FindByIdempotencyKey(_ context.Context, _ port.Reader, walletID, key string) (*ledger.Entry, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for i := range r.store.Ledger {
		entry := r.store.Ledger[i]
		if entry.WalletID == walletID && entry.IdempotencyKey == key {
			return &entry, nil
		}
	}
	return nil, errs.NotFound("no ledger entry for idempotency key %s", key)
}

// SumByWallet returns the net of a wallet's entries.
func (r *LedgerRepo) SumByWallet(_ context.Context, _ port.Reader, walletID string) (money.Money, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	entries := make([]ledger.Entry, 0, len(r.store.Ledger))
	for _, entry := range r.store.Ledger {
		if entry.WalletID == walletID {
			entries = append(entries, entry)
		}
	}
	return ledger.Balance(entries)
}

// FindMismatches compares every stored balance against its ledger sum.
func (r *LedgerRepo) FindMismatches(ctx context.Context, _ port.Reader, walletID string, limit int) ([]ledger.Mismatch, error) {
	r.store.mu.Lock()
	records := make([]walletRecord, 0, len(r.store.Wallets))
	for _, record := range r.store.Wallets {
		if walletID == "" || record.UserID == walletID || record.ID == walletID {
			records = append(records, record)
		}
	}
	r.store.mu.Unlock()

	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })

	mismatches := make([]ledger.Mismatch, 0)
	for _, record := range records {
		if len(mismatches) >= limit {
			break
		}
		ledgerBalance, err := r.SumByWallet(ctx, nil, record.ID)
		if err != nil {
			return nil, err
		}
		if record.Balance.Equal(ledgerBalance) {
			continue
		}
		delta, err := record.Balance.Sub(ledgerBalance)
		if err != nil {
			return nil, err
		}
		mismatches = append(mismatches, ledger.Mismatch{
			WalletID:      record.ID,
			UserID:        record.UserID,
			StoredBalance: record.Balance,
			LedgerBalance: ledgerBalance,
			Delta:         delta,
		})
	}
	return mismatches, nil
}

// --- GiftCardRepository ---------------------------------------------------

// GiftCardRepo is a fake port.GiftCardRepository.
type GiftCardRepo struct{ store *Store }

// NewGiftCardRepo returns a GiftCardRepo over store.
func NewGiftCardRepo(store *Store) *GiftCardRepo { return &GiftCardRepo{store: store} }

// InsertBatch stores minted cards.
func (r *GiftCardRepo) InsertBatch(_ context.Context, _ port.Tx, cards []*giftcard.GiftCard) error {
	if err := r.store.fail("giftcard.InsertBatch"); err != nil {
		return err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, card := range cards {
		for _, existing := range r.store.GiftCards {
			if existing.CodeHash == card.CodeHash() {
				return errs.AlreadyExists("a gift card with that code already exists")
			}
		}
		r.store.GiftCards[card.ID()] = toGiftCardRecord(card)
	}
	return nil
}

// FindByCodeHash looks a card up by hash.
func (r *GiftCardRepo) FindByCodeHash(_ context.Context, _ port.Reader, codeHash string) (*giftcard.GiftCard, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, record := range r.store.GiftCards {
		if record.CodeHash == codeHash {
			return record.rehydrate()
		}
	}
	return nil, giftcard.ErrNotFound()
}

// FindByID looks a card up by identifier.
func (r *GiftCardRepo) FindByID(_ context.Context, _ port.Reader, id string) (*giftcard.GiftCard, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.GiftCards[id]
	if !ok {
		return nil, giftcard.ErrNotFound()
	}
	return record.rehydrate()
}

// LockByCodeHash reads a card, standing in for SELECT ... FOR UPDATE.
func (r *GiftCardRepo) LockByCodeHash(ctx context.Context, _ port.Tx, codeHash string) (*giftcard.GiftCard, error) {
	return r.FindByCodeHash(ctx, nil, codeHash)
}

// MarkRedeemed applies a redemption, reporting false when the row already moved on.
func (r *GiftCardRepo) MarkRedeemed(_ context.Context, _ port.Tx, card *giftcard.GiftCard, expectedVersion int64) (bool, error) {
	if err := r.store.fail("giftcard.MarkRedeemed"); err != nil {
		return false, err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.GiftCards[card.ID()]
	if !ok {
		return false, giftcard.ErrNotFound()
	}
	// This mirrors UPDATE ... WHERE version = $n AND status = 'ACTIVE': zero rows
	// affected means somebody else redeemed it first.
	if record.Version != expectedVersion || record.Status != giftcard.StatusActive {
		return false, nil
	}
	r.store.GiftCards[card.ID()] = toGiftCardRecord(card)
	return true, nil
}

// Update persists a revocation.
func (r *GiftCardRepo) Update(_ context.Context, _ port.Tx, card *giftcard.GiftCard, expectedVersion int64) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.GiftCards[card.ID()]
	if !ok {
		return giftcard.ErrNotFound()
	}
	if record.Version != expectedVersion {
		return errs.Aborted("gift card %s was modified concurrently", card.ID())
	}
	r.store.GiftCards[card.ID()] = toGiftCardRecord(card)
	return nil
}

// List returns a filtered page of cards.
func (r *GiftCardRepo) List(_ context.Context, _ port.Reader, filter giftcard.Filter) ([]*giftcard.GiftCard, int64, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	ids := make([]string, 0, len(r.store.GiftCards))
	for id, record := range r.store.GiftCards {
		if filter.Status != "" && record.Status != filter.Status {
			continue
		}
		if filter.BatchID != "" && record.BatchID != filter.BatchID {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	total := int64(len(ids))
	start := filter.Offset
	if start > len(ids) {
		start = len(ids)
	}
	end := start + filter.Limit
	if filter.Limit <= 0 || end > len(ids) {
		end = len(ids)
	}

	cards := make([]*giftcard.GiftCard, 0, end-start)
	for _, id := range ids[start:end] {
		card, err := r.store.GiftCards[id].rehydrate()
		if err != nil {
			return nil, 0, err
		}
		cards = append(cards, card)
	}
	return cards, total, nil
}

// --- DiscountRepository ---------------------------------------------------

// DiscountRepo is a fake port.DiscountRepository.
type DiscountRepo struct{ store *Store }

// NewDiscountRepo returns a DiscountRepo over store.
func NewDiscountRepo(store *Store) *DiscountRepo { return &DiscountRepo{store: store} }

// Insert stores a code.
func (r *DiscountRepo) Insert(_ context.Context, _ port.Tx, code *discount.Code) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, record := range r.store.Discounts {
		if record.Code == code.Code() {
			return errs.AlreadyExists("discount code %s already exists", code.Code())
		}
	}
	r.store.Discounts[code.ID()] = toDiscountRecord(code)
	return nil
}

// FindByCode looks a code up.
func (r *DiscountRepo) FindByCode(_ context.Context, _ port.Reader, code string) (*discount.Code, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	for _, record := range r.store.Discounts {
		if record.Code == code {
			return record.rehydrate()
		}
	}
	return nil, discount.ErrNotFound()
}

// LockByCode reads a code, standing in for SELECT ... FOR UPDATE.
func (r *DiscountRepo) LockByCode(ctx context.Context, _ port.Tx, code string) (*discount.Code, error) {
	return r.FindByCode(ctx, nil, code)
}

// Update persists a redemption or revocation.
func (r *DiscountRepo) Update(_ context.Context, _ port.Tx, code *discount.Code, expectedVersion int64) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.Discounts[code.ID()]
	if !ok {
		return discount.ErrNotFound()
	}
	if record.Version != expectedVersion {
		return errs.Aborted("discount code %s was modified concurrently", code.Code())
	}
	r.store.Discounts[code.ID()] = toDiscountRecord(code)
	return nil
}

// --- HoldRepository -------------------------------------------------------

// HoldRepo is a fake port.HoldRepository.
type HoldRepo struct{ store *Store }

// NewHoldRepo returns a HoldRepo over store.
func NewHoldRepo(store *Store) *HoldRepo { return &HoldRepo{store: store} }

// Insert stores a hold.
func (r *HoldRepo) Insert(_ context.Context, _ port.Tx, h *hold.Hold) error {
	if err := r.store.fail("hold.Insert"); err != nil {
		return err
	}
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	r.store.Holds[h.ID()] = toHoldRecord(h)
	return nil
}

// FindByID looks a hold up.
func (r *HoldRepo) FindByID(_ context.Context, _ port.Reader, id string) (*hold.Hold, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.Holds[id]
	if !ok {
		return nil, hold.ErrNotFound(id)
	}
	return record.rehydrate()
}

// LockByID reads a hold, standing in for SELECT ... FOR UPDATE.
func (r *HoldRepo) LockByID(ctx context.Context, _ port.Tx, id string) (*hold.Hold, error) {
	return r.FindByID(ctx, nil, id)
}

// Update persists a hold transition.
func (r *HoldRepo) Update(_ context.Context, _ port.Tx, h *hold.Hold, expectedVersion int64) error {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	record, ok := r.store.Holds[h.ID()]
	if !ok {
		return hold.ErrNotFound(h.ID())
	}
	if record.Version != expectedVersion {
		return errs.Aborted("hold %s was modified concurrently", h.ID())
	}
	r.store.Holds[h.ID()] = toHoldRecord(h)
	return nil
}

// List returns a filtered page of holds.
func (r *HoldRepo) List(_ context.Context, _ port.Reader, filter hold.Filter) ([]*hold.Hold, int64, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	ids := make([]string, 0, len(r.store.Holds))
	for id, record := range r.store.Holds {
		if filter.WalletID != "" && record.WalletID != filter.WalletID {
			continue
		}
		if filter.UserID != "" && record.UserID != filter.UserID {
			continue
		}
		if filter.Status != "" && record.Status != filter.Status {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	total := int64(len(ids))
	start := filter.Offset
	if start > len(ids) {
		start = len(ids)
	}
	end := start + filter.Limit
	if filter.Limit <= 0 || end > len(ids) {
		end = len(ids)
	}

	holds := make([]*hold.Hold, 0, end-start)
	for _, id := range ids[start:end] {
		h, err := r.store.Holds[id].rehydrate()
		if err != nil {
			return nil, 0, err
		}
		holds = append(holds, h)
	}
	return holds, total, nil
}

// ListExpired returns active holds past their TTL.
func (r *HoldRepo) ListExpired(_ context.Context, _ port.Reader, before time.Time, limit int) ([]*hold.Hold, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()

	ids := make([]string, 0, len(r.store.Holds))
	for id, record := range r.store.Holds {
		if record.Status != hold.StatusActive || record.ExpiresAt == nil {
			continue
		}
		if before.Before(*record.ExpiresAt) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	holds := make([]*hold.Hold, 0, limit)
	for _, id := range ids {
		if len(holds) >= limit {
			break
		}
		h, err := r.store.Holds[id].rehydrate()
		if err != nil {
			return nil, err
		}
		holds = append(holds, h)
	}
	return holds, nil
}

// --- IdempotencyStore -----------------------------------------------------

// IdempotencyStore is a fake port.IdempotencyStore.
type IdempotencyStore struct{ store *Store }

// NewIdempotencyStore returns an IdempotencyStore over store.
func NewIdempotencyStore(store *Store) *IdempotencyStore { return &IdempotencyStore{store: store} }

func idemKey(key, operation string) string { return operation + ":" + key }

// Claim reserves a key, reporting whether this caller won the claim.
func (s *IdempotencyStore) Claim(_ context.Context, _ port.Tx, record port.IdempotencyRecord) (*port.IdempotencyRecord, bool, error) {
	if err := s.store.fail("idem.Claim"); err != nil {
		return nil, false, err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	composite := idemKey(record.Key, record.Operation)
	if existing, found := s.store.Idem[composite]; found {
		copyOf := existing
		return &copyOf, false, nil
	}
	s.store.Idem[composite] = record
	return nil, true, nil
}

// SaveResponse attaches a result to a claimed key.
func (s *IdempotencyStore) SaveResponse(_ context.Context, _ port.Tx, key, operation string, response []byte) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	composite := idemKey(key, operation)
	record, found := s.store.Idem[composite]
	if !found {
		return errs.Internal("idempotency key %s was never claimed", key)
	}
	record.Response = response
	s.store.Idem[composite] = record
	return nil
}

// Purge deletes old records.
func (s *IdempotencyStore) Purge(_ context.Context, _ port.Tx, before time.Time) (int64, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	var deleted int64
	for composite, record := range s.store.Idem {
		if record.CreatedAt.Before(before) {
			delete(s.store.Idem, composite)
			deleted++
		}
	}
	return deleted, nil
}

// --- EventPublisher -------------------------------------------------------

// Publisher is a fake port.EventPublisher that records envelopes.
type Publisher struct{ store *Store }

// NewPublisher returns a Publisher over store.
func NewPublisher(store *Store) *Publisher { return &Publisher{store: store} }

// Publish appends envelopes to the store.
func (p *Publisher) Publish(_ context.Context, _ port.Tx, envelopes ...event.Envelope) error {
	if err := p.store.fail("publisher.Publish"); err != nil {
		return err
	}
	p.store.mu.Lock()
	defer p.store.mu.Unlock()

	p.store.Events = append(p.store.Events, envelopes...)
	return nil
}

// Notify counts dispatcher wake-ups.
func (p *Publisher) Notify() {
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	p.store.Notified++
}

// --- PaymentGateway -------------------------------------------------------

// PaymentGateway is a fake port.PaymentGateway.
type PaymentGateway struct {
	// Requests records every call, so a test can assert the idempotency key was
	// forwarded to the bank.
	Requests []port.PaymentRequest
	// Intent is returned from InitiatePayment when Err is nil.
	Intent port.PaymentIntent
	// Err, when set, is returned instead.
	Err error
	mu  sync.Mutex
}

// NewPaymentGateway returns a gateway that answers with a canned intent.
func NewPaymentGateway() *PaymentGateway {
	return &PaymentGateway{
		Intent: port.PaymentIntent{
			ID:          "intent-1",
			RedirectURL: "https://bank.example/pay/intent-1",
			State:       "PENDING",
		},
	}
}

// InitiatePayment records the request and returns the canned intent.
func (g *PaymentGateway) InitiatePayment(_ context.Context, req port.PaymentRequest) (port.PaymentIntent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.Requests = append(g.Requests, req)
	if g.Err != nil {
		return port.PaymentIntent{}, g.Err
	}
	intent := g.Intent
	intent.Amount = req.Amount
	return intent, nil
}

// GetPaymentIntent returns the canned intent.
func (g *PaymentGateway) GetPaymentIntent(_ context.Context, id string) (port.PaymentIntent, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.Err != nil {
		return port.PaymentIntent{}, g.Err
	}
	intent := g.Intent
	intent.ID = id
	return intent, nil
}

// CallCount returns how many payments were initiated.
func (g *PaymentGateway) CallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.Requests)
}

// --- AbuseLimiter ---------------------------------------------------------

// AbuseLimiter is a fake port.AbuseLimiter with an in-memory counter.
type AbuseLimiter struct {
	mu sync.Mutex
	// Failures counts recorded failures per user.
	Failures map[string]int64
	// BlockAfter is the failure count at which Blocked becomes true.
	BlockAfter int64
	// RetryAfter is reported when blocked.
	RetryAfter time.Duration
	// Err, when set, is returned from both methods, simulating an unreachable Redis.
	Err error
}

// NewAbuseLimiter returns a limiter that blocks after five failures.
func NewAbuseLimiter() *AbuseLimiter {
	return &AbuseLimiter{
		Failures:   make(map[string]int64),
		BlockAfter: 5,
		RetryAfter: 42 * time.Second,
	}
}

// CheckAndRecordFailure registers a failure and reports the verdict.
func (l *AbuseLimiter) CheckAndRecordFailure(_ context.Context, userID string) (port.AbuseVerdict, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.Err != nil {
		return port.AbuseVerdict{}, l.Err
	}
	l.Failures[userID]++
	return l.verdictLocked(userID), nil
}

// Check reports the verdict without recording anything.
func (l *AbuseLimiter) Check(_ context.Context, userID string) (port.AbuseVerdict, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.Err != nil {
		// Fail open, exactly as the Redis-backed limiter does.
		return port.AbuseVerdict{}, l.Err
	}
	return l.verdictLocked(userID), nil
}

func (l *AbuseLimiter) verdictLocked(userID string) port.AbuseVerdict {
	count := l.Failures[userID]
	return port.AbuseVerdict{
		Blocked:        count >= l.BlockAfter,
		Rule:           "per-minute",
		RetryAfter:     l.RetryAfter,
		FailedInWindow: count,
	}
}

// Reset clears a user's counter.
func (l *AbuseLimiter) Reset(_ context.Context, userID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.Failures, userID)
	return nil
}

// --- Metrics --------------------------------------------------------------

// Metrics is a fake port.Metrics that records counters for assertions.
type Metrics struct {
	mu               sync.Mutex
	Operations       map[string]int
	Replays          map[string]int
	Rejections       map[string]int
	RateLimitBlocks  map[string]int
	MoneyMovedMinor  int64
	LedgerMismatches int64
}

// NewMetrics returns an empty Metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		Operations:      make(map[string]int),
		Replays:         make(map[string]int),
		Rejections:      make(map[string]int),
		RateLimitBlocks: make(map[string]int),
	}
}

// WalletOperation records an operation outcome.
func (m *Metrics) WalletOperation(operation, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Operations[operation+":"+outcome]++
}

// MoneyMoved accumulates moved value.
func (m *Metrics) MoneyMoved(_, _, _ string, amountMinor int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.MoneyMovedMinor += amountMinor
}

// IdempotentReplay records a replay.
func (m *Metrics) IdempotentReplay(operation string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Replays[operation]++
}

// BusinessRuleRejection records a domain rejection.
func (m *Metrics) BusinessRuleRejection(rule string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Rejections[rule]++
}

// LedgerMismatch records the reconciliation gauge.
func (m *Metrics) LedgerMismatch(count int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LedgerMismatches = count
}

// RateLimitBlock records a throttled request.
func (m *Metrics) RateLimitBlock(limiter string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RateLimitBlocks[limiter]++
}

// Count returns an operation counter.
func (m *Metrics) Count(operation, outcome string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Operations[operation+":"+outcome]
}

// Compile-time proof that every fake satisfies the port it stands in for. If a
// port gains a method, this fails here rather than in a confusing test error.
var (
	_ port.TxManager          = (*TxManager)(nil)
	_ port.WalletRepository   = (*WalletRepo)(nil)
	_ port.LedgerRepository   = (*LedgerRepo)(nil)
	_ port.GiftCardRepository = (*GiftCardRepo)(nil)
	_ port.DiscountRepository = (*DiscountRepo)(nil)
	_ port.HoldRepository     = (*HoldRepo)(nil)
	_ port.IdempotencyStore   = (*IdempotencyStore)(nil)
	_ port.EventPublisher     = (*Publisher)(nil)
	_ port.PaymentGateway     = (*PaymentGateway)(nil)
	_ port.AbuseLimiter       = (*AbuseLimiter)(nil)
	_ port.Metrics            = (*Metrics)(nil)
)
