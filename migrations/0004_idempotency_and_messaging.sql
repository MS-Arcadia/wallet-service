-- Idempotency bookkeeping, the transactional outbox, and consumer deduplication.
--
-- These three tables are the machinery behind the platform's exactly-once
-- guarantee for financial operations:
--
--   idempotency_keys  a client request takes effect at most once
--   outbox_messages   a state change and its announcement commit together
--   processed_events  an at-least-once delivery is handled at most once

CREATE TABLE idempotency_keys (
    key          TEXT        NOT NULL,
    -- operation namespaces the key, so the same key value used for a debit and for a
    -- credit does not collide.
    operation    TEXT        NOT NULL,
    -- request_hash fingerprints the payload. A second request under the same key with
    -- a different payload is a client bug, not a retry, and is rejected rather than
    -- silently answered with the first result.
    request_hash CHAR(64)    NOT NULL,
    -- response is the marshalled result of the original call, replayed verbatim to a
    -- retry. NULL means the original attempt claimed the key and has not finished:
    -- a concurrent retry is told to come back rather than being handed a stale answer.
    response     JSONB,
    wallet_id    TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,

    PRIMARY KEY (key, operation)
);

COMMENT ON TABLE idempotency_keys IS
    'Makes every money-moving request replay-safe. The primary key is the concurrency control: the second INSERT loses.';
COMMENT ON COLUMN idempotency_keys.response IS
    'NULL while the original request is still in flight; set once it commits.';

CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

COMMENT ON INDEX idempotency_keys_created_idx IS
    'Supports the retention sweeper. Keys are kept well beyond any realistic client retry window.';

-- ---------------------------------------------------------------------------

CREATE TABLE outbox_messages (
    -- The event id doubles as the row id: exactly one row per event.
    id             UUID        PRIMARY KEY,
    topic          TEXT        NOT NULL,
    -- partition_key is the aggregate id, so two events about one wallet always land
    -- on the same Kafka partition and are therefore never reordered.
    partition_key  TEXT        NOT NULL,
    event_id       UUID        NOT NULL,
    event_type     TEXT        NOT NULL,
    payload        JSONB       NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'PENDING',
    attempt_count  INTEGER     NOT NULL DEFAULT 0,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- available_at implements the retry backoff: a failed message is invisible to the
    -- dispatcher until this time passes.
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    correlation_id TEXT,
    trace_id       TEXT,

    CONSTRAINT outbox_event_id_key UNIQUE (event_id),

    CONSTRAINT outbox_status_check
        CHECK (status IN ('PENDING', 'PUBLISHED', 'FAILED'))
);

COMMENT ON TABLE outbox_messages IS
    'Transactional outbox. Rows are written in the same transaction as the state change they describe, then drained to Kafka by the dispatcher.';

-- The dispatcher's hot path: due pending rows in age order. A partial index keeps it
-- proportional to the backlog rather than to the whole table's history.
CREATE INDEX outbox_pending_idx ON outbox_messages (available_at, created_at)
    WHERE status = 'PENDING';

-- Supports the retention sweep that removes delivered rows.
CREATE INDEX outbox_published_idx ON outbox_messages (published_at)
    WHERE status = 'PUBLISHED';

-- FAILED rows are what the "DLQ depth is zero" alert watches; there are few of them,
-- and each one needs an operator.
CREATE INDEX outbox_failed_idx ON outbox_messages (created_at)
    WHERE status = 'FAILED';

-- ---------------------------------------------------------------------------

CREATE TABLE processed_events (
    event_id     UUID        NOT NULL,
    -- consumer names the logical handler, so that two independent consumers of the
    -- same event do not shadow each other's bookkeeping.
    consumer     TEXT        NOT NULL,
    event_type   TEXT        NOT NULL DEFAULT '',
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (event_id, consumer)
);

COMMENT ON TABLE processed_events IS
    'Consumer inbox. Recording an event id in the same transaction as its effect turns at-least-once delivery into exactly-once processing.';

CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);
