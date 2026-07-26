//go:build integration

// Package test holds integration tests that need a real PostgreSQL.
//
// They are behind a build tag so that `go test ./...` stays fast and dependency-free.
// Run them with:
//
//	TEST_DATABASE_URL=postgres://arcadia:arcadia@localhost:5432/arcadia_wallet?sslmode=disable \
//	  go test -tags=integration ./test/...
//
// What is tested here is deliberately *only* what a fake cannot prove:
//
//   - the append-only trigger really does reject an UPDATE on the ledger,
//   - the CHECK constraints really do refuse a negative balance,
//   - SELECT ... FOR UPDATE really does serialise two concurrent debits,
//   - the migrations really do apply to an empty database, in order.
//
// Business rules are covered by the unit tests in internal/domain and internal/app,
// which run in milliseconds and need nothing installed. Re-testing them here would
// only make the suite slower and no more trustworthy.
package test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/clock"
	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/arcadia-platform/pkg/idgen"
	"github.com/MS-Arcadia/arcadia-platform/pkg/logx"
	"github.com/MS-Arcadia/arcadia-platform/pkg/migrate"
	"github.com/MS-Arcadia/arcadia-platform/pkg/money"
	"github.com/MS-Arcadia/arcadia-platform/pkg/postgres"
	"github.com/MS-Arcadia/wallet-service/internal/adapter/out/repo"
	"github.com/MS-Arcadia/wallet-service/internal/app/port"
	"github.com/MS-Arcadia/wallet-service/internal/domain/ledger"
	"github.com/MS-Arcadia/wallet-service/internal/domain/wallet"
	walletmigrations "github.com/MS-Arcadia/wallet-service/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const currency = "IRR"

func irr(minor int64) money.Money { return money.MustNew(minor, currency) }

// fixture holds a migrated database and the repositories under test.
type fixture struct {
	pool    *pgxpool.Pool
	txm     *postgres.PoolTxManager
	wallets *repo.WalletRepo
	ledger  *repo.LedgerRepo
	ids     idgen.UUIDv7
	clock   clock.Clock
}

func setup(t *testing.T) *fixture {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping the integration suite")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, postgres.Config{
		DSN:             dsn,
		MaxConns:        10,
		ApplicationName: "wallet-service-integration",
	})
	require.NoError(t, err, "could not reach the test database")
	t.Cleanup(pool.Close)

	// Migrations run against the real database, which is itself the first assertion:
	// a broken migration fails here rather than in a deployment.
	migrations, err := migrate.Load(walletmigrations.FS, walletmigrations.Dir)
	require.NoError(t, err)

	runner, closeRunner, err := migrate.Connect(ctx, dsn, logx.NewNop())
	require.NoError(t, err)
	require.NoError(t, runner.Up(ctx, migrations))
	require.NoError(t, closeRunner(ctx))

	f := &fixture{
		pool:    pool,
		txm:     postgres.NewTxManager(pool),
		wallets: repo.NewWalletRepo(),
		ledger:  repo.NewLedgerRepo(),
		clock:   clock.System{},
	}
	f.truncate(t)
	return f
}

// truncate clears the tables between tests.
//
// The ledger cannot be truncated — a trigger forbids it, which is exactly the property
// under test — so it is dropped and recreated by re-running the migration instead.
func (f *fixture) truncate(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	// ledger_entries is protected by the append-only trigger, so it has to be emptied
	// with the trigger disabled. This is the only place in the codebase that does so,
	// and it exists purely to give each test a clean slate.
	_, err := f.pool.Exec(ctx, `
		ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_no_delete;
		DELETE FROM ledger_entries;
		ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_no_delete;
		DELETE FROM holds;
		DELETE FROM gift_cards;
		DELETE FROM discount_codes;
		DELETE FROM idempotency_keys;
		DELETE FROM outbox_messages;
		DELETE FROM wallets;`)
	require.NoError(t, err)
}

// newWallet inserts a wallet with the given balance.
func (f *fixture) newWallet(t *testing.T, userID string, balanceMinor int64) *wallet.Wallet {
	t.Helper()
	ctx := context.Background()
	now := f.clock.Now()

	w, err := wallet.New(f.ids.NewID(), userID, currency, now)
	require.NoError(t, err)

	err = f.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := f.wallets.Insert(ctx, tx, w); err != nil {
			return err
		}
		if balanceMinor > 0 {
			version := w.Version()
			movement, err := w.Credit(irr(balanceMinor), wallet.ReasonCharge, "fixture", "", now)
			if err != nil {
				return err
			}
			if err := f.wallets.Update(ctx, tx, w, version); err != nil {
				return err
			}
			entry, err := ledger.NewEntry(f.ids.NewID(), w, movement, "", "fixture-"+userID, now)
			if err != nil {
				return err
			}
			return f.ledger.Append(ctx, tx, entry)
		}
		return nil
	})
	require.NoError(t, err)
	return w
}

// --- Migrations -----------------------------------------------------------

func TestMigrationsApplyAndAreIdempotent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Every table the service depends on exists.
	for _, table := range []string{
		"wallets", "ledger_entries", "gift_cards", "discount_codes",
		"holds", "idempotency_keys", "outbox_messages", "processed_events",
		"schema_migrations",
	} {
		var exists bool
		err := f.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			                WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "table %s should exist after migrating", table)
	}

	// Re-running is a no-op: setup already applied them once, and a second Up must not
	// fail or duplicate anything.
	migrations, err := migrate.Load(walletmigrations.FS, walletmigrations.Dir)
	require.NoError(t, err)

	runner, closeRunner, err := migrate.Connect(ctx, os.Getenv("TEST_DATABASE_URL"), logx.NewNop())
	require.NoError(t, err)
	defer func() { _ = closeRunner(ctx) }()

	require.NoError(t, runner.Up(ctx, migrations))

	version, err := runner.Version(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, migrations[len(migrations)-1].Version, version)
}

// --- Ledger immutability --------------------------------------------------

func TestTheLedgerRejectsAnUpdate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	w := f.newWallet(t, "u-immutable", 100_000)

	entries := f.entriesFor(t, w.ID())
	require.NotEmpty(t, entries)

	// The claim "the ledger is append-only" has to be true against somebody with a psql
	// prompt, not just against application code that chooses not to write an UPDATE.
	_, err := f.pool.Exec(ctx,
		`UPDATE ledger_entries SET amount_minor = 999999 WHERE id = $1`, entries[0].ID)
	require.Error(t, err, "the append-only trigger must reject an UPDATE")
	assert.Contains(t, err.Error(), "append-only")
}

func TestTheLedgerRejectsADelete(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	w := f.newWallet(t, "u-immutable-2", 100_000)

	entries := f.entriesFor(t, w.ID())
	require.NotEmpty(t, entries)

	_, err := f.pool.Exec(ctx, `DELETE FROM ledger_entries WHERE id = $1`, entries[0].ID)
	require.Error(t, err, "the append-only trigger must reject a DELETE")
	assert.Contains(t, err.Error(), "append-only")
}

func (f *fixture) entriesFor(t *testing.T, walletID string) []ledger.Entry {
	t.Helper()
	page, err := f.ledger.List(context.Background(), f.pool, ledger.Filter{WalletID: walletID, Limit: 50})
	require.NoError(t, err)
	return page.Entries
}

// --- Database-level invariants --------------------------------------------

func TestTheDatabaseRefusesANegativeBalance(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	w := f.newWallet(t, "u-negative", 10_000)

	// Bypass the domain entirely, the way a repair script or a future bug would.
	_, err := f.pool.Exec(ctx,
		`UPDATE wallets SET balance_minor = -1 WHERE id = $1`, w.ID())
	require.Error(t, err, "the CHECK constraint must refuse a negative balance")
}

func TestTheDatabaseRefusesHoldingMoreThanTheBalance(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	w := f.newWallet(t, "u-overheld", 10_000)

	_, err := f.pool.Exec(ctx,
		`UPDATE wallets SET held_minor = 20000 WHERE id = $1`, w.ID())
	require.Error(t, err, "reserved funds cannot exceed the balance they are reserved from")
}

func TestOneWalletPerUser(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.newWallet(t, "u-unique", 0)

	second, err := wallet.New(f.ids.NewID(), "u-unique", currency, f.clock.Now())
	require.NoError(t, err)

	err = f.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return f.wallets.Insert(ctx, tx, second)
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeAlreadyExists, errs.CodeOf(err),
		"lazy provisioning relies on this constraint to settle a race between two first-time reads")
}

// --- Concurrency ----------------------------------------------------------

func TestConcurrentDebitsCannotOverdraw(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// Ten workers each try to take 200 from a balance of 1000. Exactly five must
	// succeed. This is the property the whole locking design exists for, and it cannot
	// be demonstrated with an in-memory fake.
	const (
		startingBalance = 1_000
		debitAmount     = 200
		workers         = 10
	)
	f.newWallet(t, "u-concurrent", startingBalance)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		rejected  int
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(attempt int) {
			defer wg.Done()

			err := f.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				// FOR UPDATE. Without it, every worker reads 1000, every worker decides it has
				// enough, and the balance ends up at -1000.
				locked, err := f.wallets.LockByUserID(ctx, tx, "u-concurrent")
				if err != nil {
					return err
				}
				version := locked.Version()

				movement, err := locked.Debit(irr(debitAmount), wallet.ReasonPurchase,
					"concurrent", "", f.clock.Now())
				if err != nil {
					return err
				}
				if err := f.wallets.Update(ctx, tx, locked, version); err != nil {
					return err
				}
				entry, err := ledger.NewEntry(f.ids.NewID(), locked, movement, "",
					"concurrent-debit", f.clock.Now())
				if err != nil {
					return err
				}
				return f.ledger.Append(ctx, tx, entry)
			})

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
				return
			}
			// A rejection must be for insufficient funds, not a deadlock or a lost update.
			if errs.ReasonOf(err) == wallet.ReasonCodeInsufficientFunds {
				rejected++
				return
			}
			t.Errorf("attempt %d failed for an unexpected reason: %v", attempt, err)
		}(i)
	}
	wg.Wait()

	assert.Equal(t, startingBalance/debitAmount, succeeded, "exactly five debits should fit in the balance")
	assert.Equal(t, workers-startingBalance/debitAmount, rejected)

	final, err := f.wallets.FindByUserID(ctx, f.pool, "u-concurrent")
	require.NoError(t, err)
	assert.True(t, final.Balance().IsZero(), "the balance should be exactly drained, never negative")
	assert.False(t, final.Balance().IsNegative())
}

func TestConcurrentDebitsKeepTheLedgerConsistent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	w := f.newWallet(t, "u-ledger-race", 1_000)

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				locked, err := f.wallets.LockByUserID(ctx, tx, "u-ledger-race")
				if err != nil {
					return err
				}
				version := locked.Version()
				movement, err := locked.Debit(irr(150), wallet.ReasonPurchase, "race", "", f.clock.Now())
				if err != nil {
					return err
				}
				if err := f.wallets.Update(ctx, tx, locked, version); err != nil {
					return err
				}
				entry, err := ledger.NewEntry(f.ids.NewID(), locked, movement, "", "", f.clock.Now())
				if err != nil {
					return err
				}
				return f.ledger.Append(ctx, tx, entry)
			})
		}()
	}
	wg.Wait()

	// Reconciliation must be clean whatever order the workers interleaved in: the sum of
	// the ledger has to equal the stored balance exactly.
	stored, err := f.wallets.FindByUserID(ctx, f.pool, "u-ledger-race")
	require.NoError(t, err)

	ledgerBalance, err := f.ledger.SumByWallet(ctx, f.pool, w.ID())
	require.NoError(t, err)

	assert.True(t, stored.Balance().Equal(ledgerBalance),
		"stored balance %s must equal the ledger sum %s", stored.Balance(), ledgerBalance)

	mismatches, err := f.ledger.FindMismatches(ctx, f.pool, "", 10)
	require.NoError(t, err)
	assert.Empty(t, mismatches, "reconciliation must find nothing after concurrent debits")
}

// --- Idempotency ----------------------------------------------------------

func TestTheIdempotencyKeyIsClaimedExactlyOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	store := repo.NewIdempotencyStore()

	// Twenty workers race for the same key. Exactly one must win the claim, because that
	// is what stops twenty retries of one request from moving money twenty times.
	const workers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claims  int
		replays int
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				_, claimed, err := store.Claim(ctx, tx, port.IdempotencyRecord{
					Key:         "shared-key",
					Operation:   "debit",
					RequestHash: "same-hash-for-every-worker",
					CreatedAt:   f.clock.Now(),
				})
				if err != nil {
					return err
				}

				mu.Lock()
				defer mu.Unlock()
				if claimed {
					claims++
				} else {
					replays++
				}
				return nil
			})
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, claims, "exactly one worker may own the key")
	assert.Equal(t, workers-1, replays)
}

// --- Ledger sequencing ----------------------------------------------------

func TestLedgerSequenceIsMonotonic(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	w := f.newWallet(t, "u-sequence", 100_000)

	for i := 0; i < 5; i++ {
		err := f.txm.WithinTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			locked, err := f.wallets.LockByUserID(ctx, tx, "u-sequence")
			if err != nil {
				return err
			}
			version := locked.Version()
			movement, err := locked.Debit(irr(1_000), wallet.ReasonPurchase, "seq", "", f.clock.Now())
			if err != nil {
				return err
			}
			if err := f.wallets.Update(ctx, tx, locked, version); err != nil {
				return err
			}
			entry, err := ledger.NewEntry(f.ids.NewID(), locked, movement, "", "", f.clock.Now())
			if err != nil {
				return err
			}
			return f.ledger.Append(ctx, tx, entry)
		})
		require.NoError(t, err)
	}

	page, err := f.ledger.List(ctx, f.pool, ledger.Filter{WalletID: w.ID(), Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Entries, 6, "the fixture credit plus five debits")

	// Newest first, and strictly decreasing. Two entries written in the same transaction
	// share a timestamp, so an ordering that relied on created_at would be unstable and
	// paging would skip or repeat rows.
	for i := 1; i < len(page.Entries); i++ {
		assert.Greater(t, page.Entries[i-1].Sequence, page.Entries[i].Sequence)
	}
	assert.NotZero(t, page.Entries[0].Sequence, "the database assigns the sequence")
}
