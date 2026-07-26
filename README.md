# Arcadia — Wallet Service

The wallet and internal payments service for [Arcadia](../PHASE01/README.md). It owns
every balance on the platform: what a user has, where it came from, and where it went.

Go, clean architecture, gRPC and REST, Kafka, PostgreSQL.

---

## Quick start

The service needs Postgres, Redis and Kafka. The [infra](../infra) repository brings all
three up:

```bash
cd ../infra && make up
```

Then:

```bash
curl -s localhost:8080/readyz | jq
```

To exercise the API, import [`api/postman`](api/postman) into Postman, select the
**Arcadia Local** environment, and run **Setup → Mint tokens**. The collection signs its
own JWTs, so nothing else needs to be running.

Running it from your own machine instead of the container:

```bash
cd ../infra && make up-data     # just Postgres, Redis and Kafka
cd ../wallet-service && make run
```

```bash
make test          # unit tests, race detector, no infrastructure needed
make cover         # coverage per package
make lint          # vet plus staticcheck
make docker        # build the image
```

---

## What it does

| Capability | Notes |
|---|---|
| Balances and an append-only ledger | The ledger is the source of truth; the balance column is a cached projection of it. |
| Bank top-ups | Through the payment adapter. The balance changes on confirmation, never on initiation. |
| Gift cards | Support issues them, users redeem them. Only a salted hash is stored. |
| Abuse detection | Repeated wrong codes flag a user for Support review, per the requirements. |
| Purchase saga participation | Debit, credit, refund and reversal, driven by the Store service. |
| Marketplace settlement | Atomic two-sided transfer: both wallets move or neither does. |
| Holds | Reservations for pre-orders and instalment plans. |
| Discount codes | Percentage or fixed amount, capped, with redemption limits. |
| Daily interest | The financial differentiator from the requirements, accrued daily rather than annually. |
| Reconciliation | Proves every balance still equals the sum of its ledger. |

---

## Architecture

Clean architecture, with the dependency rule enforced by the package layout: nothing in
`domain` or `app` imports a driver, a broker client or a transport.

```
cmd/wallet-service/          the binary: load config, build, run
internal/
├── domain/                  the business rules. No imports outside the platform's
│   ├── wallet/              money and error types.
│   ├── ledger/              ├─ Wallet: the balance and its invariants
│   ├── giftcard/            ├─ Ledger: immutable entries
│   ├── discount/            ├─ GiftCard, DiscountCode, Hold
│   ├── hold/                ├─ abuse: the gift-card policy
│   ├── abuse/               └─ interest: the accrual policy
│   └── interest/
├── app/                     use cases. Orchestrates aggregates, records movements,
│   ├── port/                publishes events. Depends only on interfaces in port/.
│   └── apptest/             in-memory fakes for every port
├── adapter/
│   ├── in/grpcapi/          gRPC server  ─┐
│   ├── in/restapi/          REST handlers ─┼─ three inbound adapters over one
│   ├── in/consumer/         Kafka handlers ┘  application layer
│   ├── out/repo/            PostgreSQL repositories
│   ├── out/publisher/       the transactional outbox
│   ├── out/ratelimit/       Redis sliding windows
│   └── out/paymentgw/       gRPC client to the payment adapter
├── config/                  environment loading, with validation that fails at boot
└── bootstrap/               the only place that chooses concrete infrastructure
migrations/                  versioned SQL, embedded in the binary
```

Three inbound adapters sit on the same use cases. That is what makes
`SERVER_MODE=grpc|http|both` a configuration change rather than a rewrite, and it is why
the REST and gRPC paths cannot drift apart in their validation or authorisation.

---

## The decisions worth explaining

### Money is never a float, and never a decimal

Every amount is an `int64` count of the currency's minor unit. A `float64` cannot
represent `0.1` exactly, and a ledger that drifts by a hundredth of a unit per
transaction is a ledger that fails reconciliation.

Rates are basis points (1% = 100 bps) with explicit rounding at every step. The 70/30
revenue split uses largest-remainder allocation, so the two shares always add back up to
the original price — no unit lost, none invented, whatever the price.

Over the wire, `amount_minor` is a **string**, because a JavaScript client would silently
truncate an integer above 2^53.

### The ledger really is append-only

Not by convention. `ledger_entries` has triggers that raise an exception on `UPDATE`,
`DELETE` and `TRUNCATE`, so the claim holds against anyone with a `psql` prompt, not just
against application code that chooses not to write those statements. A mistake is
corrected by appending a compensating entry, which is how the history stays auditable.

The integration suite asserts this ([`test/integration_test.go`](test/integration_test.go)) —
it is the kind of guarantee that is worth proving rather than asserting.

### Every money-moving request is idempotent

An `Idempotency-Key` is **required**, not defaulted. A generated key would make every
retry look like a new request, which is the exact failure the mechanism prevents.

The first request claims the key; a retry replays the stored response and reports
`idempotent_replay: true`. A retry with a *different* payload under the same key is
rejected as `IDEMPOTENCY_KEY_REUSED`, because that is a client bug and quietly returning
the old answer would hide it.

For Kafka consumers, the event id is the key. Kafka delivers at least once, so a
redelivered `BankPaymentConfirmed` genuinely does arrive — and credits nothing extra.

### Concurrent debits cannot overdraw

Every balance change locks its wallet row `FOR UPDATE`. Without it, two concurrent debits
both read the same balance, both decide there are sufficient funds, and both commit.
`CHECK (balance_minor >= 0)` in the schema is the backstop.

A two-sided transfer locks both rows **in a canonical order**, which is what stops two
opposite trades between the same pair of users from deadlocking each other.

### A state change and its announcement commit together

The Transactional Outbox. A use case writes its aggregate *and* an `outbox_messages` row
through the same transaction, so both commit or neither does. There is no window in which
a wallet has been debited and nothing knows about it.

A background dispatcher drains the table with `FOR UPDATE SKIP LOCKED`, so several
replicas share the work without publishing the same message twice. Delivery is
at-least-once; the receiving side's `processed_events` table turns that into
effectively-exactly-once.

### An insufficient balance is an event, not just an error

When a saga debit is declined, the service returns `422 INSUFFICIENT_FUNDS` *and*
publishes `PaymentFailed`. Both matter: the RPC answers this caller, and the event reaches
the Store orchestrator, which listens on the broker rather than holding the call open.
Without the event, the saga would stall forever.

### A gift-card code is treated as a password

Only an HMAC of the normalised code is stored, with a server-side pepper that lives in a
secret rather than the database. A dump of `gift_cards` yields nothing spendable, and 80
bits of entropy makes offline brute force pointless even before the rate limiter.

The plaintext is returned exactly once, in the response that mints the card. A replayed
issuance returns the records with empty `code` fields — the honest answer, since they were
never stored.

An unknown code and a malformed one produce the *identical* error, so the endpoint cannot
be used to enumerate live codes.

### The service never bans anybody

Repeated failures publish `GiftCardAbuseDetected`. Auth queues the user, and a Support
agent decides — which is what the requirements ask for. The counting lives in Redis, the
thresholds and the decision live in `domain/abuse`, so the policy is testable without a
cache.

The limiter **fails open**: if Redis is down the rule stops being enforced, but a
legitimate user can still spend their gift card. Failing closed would mean a cache outage
locks every customer out of their own money.

### Interest is accrued daily

Not annually. A yearly lump sum would reward whoever happened to hold a large balance on
one particular day and pay nothing to a user who kept money in the wallet for eleven
months.

The idempotency key is `interest:<wallet>:<date>`, so re-running a day — after a crash, or
because an operator replayed it — pays nothing extra. Amounts round down, so the platform
can never over-pay.

### Liveness and readiness are different things

`/livez` deliberately probes nothing: if it can answer, the process is alive. `/readyz`
probes dependencies. A database blip must fail readiness and *not* liveness — restarting
the pod would not fix Postgres and would discard in-flight work.

Redis is registered as **non-critical**, so losing it reports `DEGRADED` and keeps serving.
That is the bulkhead tactic from the architecture document made concrete.

---

## API

Both transports expose the same operations. gRPC is defined in
[`infra/contracts/proto/arcadia/wallet/v1/wallet.proto`](../infra/contracts/proto/arcadia/wallet/v1/wallet.proto).

### REST

| | |
|---|---|
| `GET /v1/wallets/me` | Your wallet, provisioned on first access |
| `GET /v1/wallets/me/ledger` | Transaction history, filterable |
| `GET /v1/wallets/me/holds` | Your reservations |
| `POST /v1/wallets/me/charges` | Start a bank top-up |
| `POST /v1/wallets/me/gift-cards/redeem` | Redeem a gift card |
| `GET /v1/wallets/{userID}` | Any wallet (Support/Admin) |
| `POST /v1/wallets/{userID}/debit` | Saga: take money |
| `POST /v1/wallets/{userID}/credit` | Saga: give money |
| `POST /v1/transfers` | Settle a trade, atomically |
| `POST /v1/wallets/{userID}/holds` | Reserve funds |
| `POST /v1/holds/{holdID}/capture` | Turn a reservation into a debit |
| `POST /v1/holds/{holdID}/release` | Give a reservation back |
| `POST /v1/gift-cards` | Mint cards (Support) |
| `POST /v1/discount-codes` | Mint a code (Support/Admin) |
| `POST /v1/discount-codes/{code}/preview` | Compute a discount, no side effects |
| `POST /v1/discount-codes/{code}/redeem` | Consume a redemption |
| `POST /v1/admin/reconcile` | Prove balances match the ledger (Admin) |
| `POST /v1/admin/interest/accrue` | Run an accrual cycle (Admin) |
| `POST /v1/admin/wallets/{userID}/freeze` | Suspend a wallet (Support) |
| `POST /v1/admin/wallets/{userID}/adjust` | Manual correction (Admin) |

Plus `/livez`, `/readyz` and `/metrics`, which are served regardless of `SERVER_MODE`
because Kubernetes and Prometheus both need them.

### Events

Published on `wallet-events`: `WalletCreated`, `WalletDebited`, `WalletCredited`,
`PaymentFailed`, `FundsTransferred`, `GiftCardIssued`, `GiftCardRedeemed`,
`GiftCardAbuseDetected`, `HoldPlaced`, `HoldCaptured`, `HoldReleased`, `InterestAccrued`,
`WalletFrozen`, `WalletUnfrozen`, `DiscountCodeRedeemed`, `ChargeInitiated`,
`LedgerMismatchDetected`. Every money movement is mirrored to `audit-events`.

Consumed: `payment-events` (bank settlements), `user-events` (provision a wallet),
`wallet-commands` (the Store saga), `trade-events` (marketplace settlement).

---

## Configuration

Everything comes from the environment; see [`internal/config`](internal/config/config.go).
Required, with no usable default:

| | |
|---|---|
| `DATABASE_URL` | PostgreSQL DSN |
| `GIFT_CARD_PEPPER` | HMAC key for code hashing, ≥32 bytes |
| `JWT_SECRET` or `JWT_PUBLIC_KEY` | Token verification material |

The most commonly changed of the rest:

| | Default | |
|---|---|---|
| `SERVER_MODE` | `grpc` | `grpc`, `http` or `both` |
| `WALLET_CURRENCY` | `IRR` | ISO-4217 |
| `INTEREST_ANNUAL_RATE_BPS` | `500` | 5% a year |
| `GIFTCARD_ABUSE_PER_MINUTE` | `5` | From the requirements |
| `GIFTCARD_ABUSE_PER_HOUR` | `30` | |
| `GIFTCARD_ABUSE_FLAG_AT` | `10` | Support review threshold |
| `JOB_RECONCILE_INTERVAL` | `15m` | |

A misconfigured service **refuses to boot** and reports every problem at once. A short
`GIFT_CARD_PEPPER` or a missing `JWT_SECRET` stops the process rather than starting one
that accepts forged tokens.

---

## Testing

```bash
make test              # unit: fast, no infrastructure
make test-integration  # needs a real Postgres
```

Unit tests cover the domain and the application layer against the in-memory fakes in
[`internal/app/apptest`](internal/app/apptest/fakes.go). Those fakes are not toys — they
model transaction rollback and optimistic-concurrency version checks, so a test asserting
"a rejected debit changes nothing" actually proves it.

The integration suite deliberately covers **only what a fake cannot prove**: that the
append-only trigger rejects an `UPDATE`, that the `CHECK` constraints refuse a negative
balance, that `FOR UPDATE` serialises ten concurrent debits into exactly five successes,
and that the migrations apply to an empty database.

```bash
TEST_DATABASE_URL=postgres://arcadia:arcadia@localhost:5432/arcadia_wallet?sslmode=disable \
  go test -tags=integration ./test/...
```

---

## Operational notes

**`arcadia_ledger_mismatch_count` must be zero.** Anything else means a balance no longer
equals the sum of its ledger, which is a P1. Do not correct a balance by hand: find the
movement recorded without an entry (or the reverse) and fix it with an `ADJUSTMENT`, so
the history stays auditable.

**A dead-lettered message is a business operation that did not happen.** The SLO target
for DLQ depth is zero.

**A `FAILED` outbox row** means a state change committed but nothing was told about it.
Inspect `outbox_messages WHERE status = 'FAILED'` and its `last_error`.

**Rotating `GIFT_CARD_PEPPER` makes every unredeemed gift card unredeemable**, because the
stored hashes can no longer be reproduced. Treat it as permanent state, not a rotatable
credential.

---

## Not implemented

Deliberately out of scope, and where each belongs:

* **The 12-hour refund window and the "gifts are not refundable" rule** — the Store
  service owns the order and its timestamp. This service moves the money when told to.
* **The 70/30 split calculation** — also Store's, though `money.Allocate` provides the
  exact arithmetic.
* **Multi-currency wallets** — the `money` type is ready; the ledger schema is not.
* **Withdrawals to a bank** — the requirements only describe money entering the wallet.
