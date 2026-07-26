-- Gift cards and promotional discount codes.

CREATE TABLE gift_cards (
    id          UUID        PRIMARY KEY,
    -- Only the HMAC of the normalised code is stored. A gift-card code is a bearer
    -- instrument: whoever reads it can spend it, so a dump of this table must yield
    -- nothing spendable. The pepper lives in a secret, not in the database, which is
    -- what makes offline brute force of the 80-bit code space useless.
    code_hash   CHAR(64)    NOT NULL,
    -- The last four characters, so Support can identify a card in a list without
    -- being able to spend it.
    code_hint   CHAR(4)     NOT NULL,
    value_minor BIGINT      NOT NULL,
    currency    CHAR(3)     NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'ACTIVE',
    issued_by   UUID        NOT NULL,
    batch_id    UUID,
    note        TEXT        NOT NULL DEFAULT '',
    redeemed_by UUID,
    redeemed_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    revoke_note TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    version     BIGINT      NOT NULL DEFAULT 1,

    CONSTRAINT gift_cards_code_hash_key UNIQUE (code_hash),

    CONSTRAINT gift_cards_status_check
        CHECK (status IN ('ACTIVE', 'USED', 'REVOKED')),

    CONSTRAINT gift_cards_value_positive
        CHECK (value_minor > 0),

    -- A USED card must name its redeemer, and an unused one must not. Without this a
    -- bug could mark a card spent while leaving no record of who spent it.
    CONSTRAINT gift_cards_redemption_consistent
        CHECK (
            (status = 'USED'  AND redeemed_by IS NOT NULL AND redeemed_at IS NOT NULL) OR
            (status <> 'USED' AND redeemed_by IS NULL     AND redeemed_at IS NULL)
        ),

    CONSTRAINT gift_cards_revocation_consistent
        CHECK (
            (status = 'REVOKED' AND revoked_at IS NOT NULL) OR
            (status <> 'REVOKED' AND revoked_at IS NULL)
        )
);

COMMENT ON TABLE gift_cards IS
    'Prepaid cards. The plaintext code is returned once at issuance and never stored.';

CREATE INDEX gift_cards_status_idx   ON gift_cards (status, created_at DESC);
CREATE INDEX gift_cards_batch_idx    ON gift_cards (batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX gift_cards_redeemer_idx ON gift_cards (redeemed_by) WHERE redeemed_by IS NOT NULL;

-- ---------------------------------------------------------------------------

CREATE TABLE discount_codes (
    id                    UUID        PRIMARY KEY,
    -- Discount codes are advertised publicly ("use SUMMER20"), so unlike a gift card
    -- there is nothing to protect by hashing: knowing the code is the point. What
    -- limits abuse is max_redemptions, not secrecy.
    code                  TEXT        NOT NULL,
    -- Exactly one of percent_bps and amount_off_minor is set.
    percent_bps           INTEGER     NOT NULL DEFAULT 0,
    amount_off_minor      BIGINT      NOT NULL DEFAULT 0,
    max_discount_minor    BIGINT      NOT NULL DEFAULT 0,
    min_order_amount_minor BIGINT     NOT NULL DEFAULT 0,
    currency              CHAR(3)     NOT NULL,
    status                TEXT        NOT NULL DEFAULT 'ACTIVE',
    max_redemptions       INTEGER     NOT NULL DEFAULT 1,
    redemption_count      INTEGER     NOT NULL DEFAULT 0,
    issued_by             UUID        NOT NULL,
    expires_at            TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    version               BIGINT      NOT NULL DEFAULT 1,

    CONSTRAINT discount_codes_code_key UNIQUE (code),

    CONSTRAINT discount_codes_status_check
        CHECK (status IN ('ACTIVE', 'USED', 'EXPIRED', 'REVOKED')),

    CONSTRAINT discount_codes_percent_range
        CHECK (percent_bps >= 0 AND percent_bps <= 10000),

    CONSTRAINT discount_codes_amount_non_negative
        CHECK (amount_off_minor >= 0),

    -- A code is either a percentage or a fixed amount, never both and never neither.
    CONSTRAINT discount_codes_exactly_one_kind
        CHECK (
            (percent_bps > 0 AND amount_off_minor = 0) OR
            (percent_bps = 0 AND amount_off_minor > 0)
        ),

    CONSTRAINT discount_codes_redemptions_sane
        CHECK (max_redemptions > 0 AND redemption_count >= 0
               AND redemption_count <= max_redemptions)
);

COMMENT ON TABLE discount_codes IS
    'Promotional codes. They compute a reduction for the Store service; they never move money themselves.';

CREATE INDEX discount_codes_status_idx  ON discount_codes (status);
CREATE INDEX discount_codes_expiry_idx  ON discount_codes (expires_at)
    WHERE expires_at IS NOT NULL AND status = 'ACTIVE';

COMMENT ON INDEX discount_codes_expiry_idx IS
    'Supports the sweeper that flips lapsed codes to EXPIRED. Redemption checks expiry live and do not rely on it.';
