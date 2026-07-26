-- Holds: reservations against a balance, used by pre-orders and instalment plans.
--
-- A hold moves no money, so it appends nothing to the ledger. It only changes how
-- much of an existing balance is spendable. Keeping it out of the ledger is what
-- lets reconciliation stay a simple sum.

CREATE TABLE holds (
    id                    UUID        PRIMARY KEY,
    wallet_id             UUID        NOT NULL REFERENCES wallets (id),
    user_id               UUID        NOT NULL,
    amount_minor          BIGINT      NOT NULL,
    -- captured_amount_minor accumulates partial captures, which is how a
    -- three-instalment plan draws down a single reservation.
    captured_amount_minor BIGINT      NOT NULL DEFAULT 0,
    currency              CHAR(3)     NOT NULL,
    status                TEXT        NOT NULL DEFAULT 'ACTIVE',
    -- reference_id points at the pre-order or plan. It is mandatory: a hold nobody
    -- can trace back to its purpose could never be reconciled.
    reference_id          TEXT        NOT NULL,
    reason                TEXT        NOT NULL DEFAULT '',
    expires_at            TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at           TIMESTAMPTZ,
    version               BIGINT      NOT NULL DEFAULT 1,

    CONSTRAINT holds_status_check
        CHECK (status IN ('ACTIVE', 'CAPTURED', 'RELEASED', 'EXPIRED')),

    CONSTRAINT holds_amount_positive
        CHECK (amount_minor > 0),

    -- Never capture more than was reserved.
    CONSTRAINT holds_captured_within_amount
        CHECK (captured_amount_minor >= 0 AND captured_amount_minor <= amount_minor),

    CONSTRAINT holds_reference_not_blank
        CHECK (reference_id <> ''),

    -- A terminal hold records when it was resolved; an active one has not been.
    CONSTRAINT holds_resolution_consistent
        CHECK (
            (status = 'ACTIVE'  AND resolved_at IS NULL) OR
            (status <> 'ACTIVE' AND resolved_at IS NOT NULL)
        )
);

COMMENT ON TABLE holds IS
    'Reservations against a wallet balance. The sum of ACTIVE holds equals wallets.held_minor.';

CREATE INDEX holds_wallet_idx ON holds (wallet_id, created_at DESC);
CREATE INDEX holds_user_idx   ON holds (user_id, status);

-- The sweeper's query: active holds whose TTL has elapsed. A partial index keeps it
-- proportional to the number of live holds rather than to the whole table.
CREATE INDEX holds_expiry_sweep_idx ON holds (expires_at)
    WHERE status = 'ACTIVE' AND expires_at IS NOT NULL;

CREATE INDEX holds_reference_idx ON holds (reference_id);
