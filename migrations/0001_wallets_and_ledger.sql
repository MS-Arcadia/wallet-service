-- Wallets and the append-only ledger.
--
-- Money is stored as BIGINT counts of the currency's minor unit, never NUMERIC and
-- never a floating type. The application's money.Money type mirrors this exactly,
-- so no conversion happens on the way in or out and there is nothing to round.

CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    user_id       UUID        NOT NULL,
    balance_minor BIGINT      NOT NULL DEFAULT 0,
    held_minor    BIGINT      NOT NULL DEFAULT 0,
    currency      CHAR(3)     NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'ACTIVE',
    -- version drives optimistic concurrency. Every balance change increments it and
    -- every UPDATE asserts the value it read, so a lost update is impossible even if
    -- a future code path forgets to take the row lock.
    version       BIGINT      NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One wallet per user. This constraint is what makes lazy provisioning safe:
    -- two concurrent first-time reads race here and the loser reads the winner's row.
    CONSTRAINT wallets_user_id_key UNIQUE (user_id),

    CONSTRAINT wallets_status_check
        CHECK (status IN ('ACTIVE', 'FROZEN', 'CLOSED')),

    -- The domain enforces a non-negative balance in Go. This says it again in the
    -- database, because it is the one invariant that must hold no matter what writes
    -- to this table — a migration, a repair script, or a bug in a future use case.
    CONSTRAINT wallets_balance_non_negative
        CHECK (balance_minor >= 0),

    CONSTRAINT wallets_held_non_negative
        CHECK (held_minor >= 0),

    -- Reserved funds can never exceed the balance they are reserved from.
    CONSTRAINT wallets_held_within_balance
        CHECK (held_minor <= balance_minor),

    CONSTRAINT wallets_currency_check
        CHECK (currency ~ '^[A-Z]{3}$')
);

COMMENT ON TABLE wallets IS
    'One balance per user. The balance is a cached projection of ledger_entries; the reconciliation job proves the two agree.';
COMMENT ON COLUMN wallets.balance_minor IS
    'Total balance in minor currency units, including any held amount.';
COMMENT ON COLUMN wallets.held_minor IS
    'Sum of ACTIVE holds. Spendable balance is balance_minor - held_minor.';

CREATE INDEX wallets_status_id_idx ON wallets (status, id)
    WHERE status = 'ACTIVE';

COMMENT ON INDEX wallets_status_id_idx IS
    'Supports the keyset pagination used by the interest and reconciliation jobs.';

-- ---------------------------------------------------------------------------

CREATE TABLE ledger_entries (
    id              UUID        PRIMARY KEY,
    -- sequence gives auditors a total order that survives identical timestamps.
    sequence        BIGSERIAL   NOT NULL,
    wallet_id       UUID        NOT NULL REFERENCES wallets (id),
    -- user_id is denormalised so that an audit query never needs a join.
    user_id         UUID        NOT NULL,
    direction       TEXT        NOT NULL,
    -- amount is always positive; direction carries the sign. A signed amount would
    -- allow a "debit of minus one hundred" that silently credits the wallet.
    amount_minor    BIGINT      NOT NULL,
    balance_after_minor BIGINT  NOT NULL,
    currency        CHAR(3)     NOT NULL,
    reason          TEXT        NOT NULL,
    reference_id    TEXT        NOT NULL DEFAULT '',
    description     TEXT        NOT NULL DEFAULT '',
    correlation_id  TEXT        NOT NULL DEFAULT '',
    idempotency_key TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_direction_check
        CHECK (direction IN ('DEBIT', 'CREDIT')),

    CONSTRAINT ledger_amount_positive
        CHECK (amount_minor > 0),

    CONSTRAINT ledger_balance_after_non_negative
        CHECK (balance_after_minor >= 0),

    CONSTRAINT ledger_reason_check
        CHECK (reason IN (
            'PURCHASE', 'REVENUE', 'REFUND', 'REVERSAL', 'CHARGE', 'GIFTCARD',
            'TRADE', 'DISCOUNT', 'INTEREST', 'HOLD_CAPTURE', 'ADJUSTMENT'
        ))
);

COMMENT ON TABLE ledger_entries IS
    'The immutable financial record. This table is the source of truth for every balance in the platform.';
COMMENT ON COLUMN ledger_entries.balance_after_minor IS
    'Balance immediately after this entry, so an auditor can verify the chain without recomputing running totals.';

CREATE INDEX ledger_wallet_sequence_idx ON ledger_entries (wallet_id, sequence DESC);
CREATE INDEX ledger_wallet_created_idx  ON ledger_entries (wallet_id, created_at DESC);
CREATE INDEX ledger_reference_idx       ON ledger_entries (reference_id)
    WHERE reference_id <> '';
CREATE INDEX ledger_reason_idx          ON ledger_entries (wallet_id, reason);

COMMENT ON INDEX ledger_reference_idx IS
    'Finds every entry for one order or trade — the query a support agent runs on a disputed purchase.';

-- One entry per (wallet, idempotency key). Belt and braces with the idempotency
-- table: even if that bookkeeping were bypassed, a retried request could not append
-- a second entry for the same wallet.
CREATE UNIQUE INDEX ledger_wallet_idempotency_key_idx
    ON ledger_entries (wallet_id, idempotency_key)
    WHERE idempotency_key <> '';

-- ---------------------------------------------------------------------------
-- Ledger immutability.
--
-- "Append-only" is a claim that has to be enforced somewhere. Enforcing it only in
-- Go would leave it true exactly until somebody opens psql. This trigger makes an
-- UPDATE or DELETE on the ledger fail outright, whoever attempts it and whatever
-- privileges they hold short of dropping the trigger itself.

CREATE OR REPLACE FUNCTION ledger_entries_reject_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION
        'ledger_entries is append-only: % is not permitted (entry %)',
        TG_OP, coalesce(OLD.id::text, '?')
        USING ERRCODE = 'restrict_violation',
              HINT = 'Correct a mistake by appending a compensating entry (reason REVERSAL or ADJUSTMENT), never by editing history.';
END;
$$;

CREATE TRIGGER ledger_entries_no_update
    BEFORE UPDATE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_reject_mutation();

CREATE TRIGGER ledger_entries_no_delete
    BEFORE DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_reject_mutation();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_reject_mutation();
