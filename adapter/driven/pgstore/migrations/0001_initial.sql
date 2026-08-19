-- One row per ledger. Its purpose is not bookkeeping but locking: a writer
-- takes SELECT ... FOR UPDATE on this row for the duration of its transaction,
-- which is how the ledger admits exactly one writer at a time and gets a
-- sequence with no gaps. A gapless sequence is what the hash chain needs.
CREATE TABLE ledgers (
    id         text        PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The event log: the ledger's source of truth. Nothing here is ever updated or
-- deleted; the tables below are projections that can be rebuilt from it.
CREATE TABLE events (
    ledger_id       text        NOT NULL REFERENCES ledgers (id),
    seq             bigint      NOT NULL CHECK (seq > 0),

    -- A stream-wide ordering across every ledger, for change data capture and
    -- replication. It is not the domain sequence and may have gaps.
    global_seq      bigserial   NOT NULL UNIQUE,

    event_id        uuid        NOT NULL,
    type            text        NOT NULL,

    -- json, deliberately not jsonb. jsonb normalizes: it reorders object keys,
    -- collapses whitespace and rewrites numbers. The event hash covers these
    -- exact bytes, so jsonb would silently break every chain on read-back.
    -- The generated column below buys back the ability to query the payload
    -- without putting the authoritative bytes at risk.
    payload         json        NOT NULL,
    payload_indexed jsonb       GENERATED ALWAYS AS (payload::jsonb) STORED,

    recorded_at     timestamptz NOT NULL,
    idempotency_key text,

    prev_hash       bytea       NOT NULL CHECK (octet_length(prev_hash) = 32),
    hash            bytea       NOT NULL CHECK (octet_length(hash) = 32),

    PRIMARY KEY (ledger_id, seq)
);

CREATE INDEX events_recorded_at ON events (ledger_id, recorded_at, seq);
CREATE INDEX events_payload ON events USING gin (payload_indexed);

CREATE TABLE accounts (
    ledger_id      text        NOT NULL REFERENCES ledgers (id),
    name           text        NOT NULL,
    currency_code  text        NOT NULL,
    currency_scale smallint    NOT NULL CHECK (currency_scale BETWEEN 0 AND 18),
    normal         text        NOT NULL CHECK (normal IN ('debit', 'credit')),
    allow_negative boolean     NOT NULL,
    metadata       jsonb       NOT NULL DEFAULT '{}',
    opened_at      timestamptz NOT NULL,
    opened_seq     bigint      NOT NULL,

    PRIMARY KEY (ledger_id, name)
);

CREATE TABLE transactions (
    ledger_id    text        NOT NULL REFERENCES ledgers (id),
    tx_id        uuid        NOT NULL,
    seq          bigint      NOT NULL,
    effective_at timestamptz NOT NULL,
    recorded_at  timestamptz NOT NULL,
    reference    text        NOT NULL DEFAULT '',
    metadata     jsonb       NOT NULL DEFAULT '{}',

    -- reverts points at the transaction this one undoes; reverted_by points
    -- back. reverted_by is the one column in the schema that is updated after
    -- insert, and only ever from NULL, which the partial unique index below
    -- turns into a hard guarantee that a transaction is reverted at most once.
    reverts      uuid,
    reverted_by  uuid,

    PRIMARY KEY (ledger_id, tx_id)
);

CREATE UNIQUE INDEX transactions_reverts_once
    ON transactions (ledger_id, reverts) WHERE reverts IS NOT NULL;

CREATE INDEX transactions_reference
    ON transactions (ledger_id, reference) WHERE reference <> '';

-- The read model. One row per posting, which is what balance queries scan.
CREATE TABLE entries (
    ledger_id      text        NOT NULL REFERENCES ledgers (id),
    seq            bigint      NOT NULL,
    idx            integer     NOT NULL CHECK (idx >= 0),

    account        text        NOT NULL,
    amount_minor   bigint      NOT NULL,
    currency_code  text        NOT NULL,
    currency_scale smallint    NOT NULL,

    tx_id          uuid        NOT NULL,
    reference      text        NOT NULL DEFAULT '',

    -- The two time axes. effective_at is business time, recorded_at is system
    -- time, and every balance query bounds one, the other, or both.
    effective_at   timestamptz NOT NULL,
    recorded_at    timestamptz NOT NULL,

    reverts        uuid,

    PRIMARY KEY (ledger_id, seq, idx),
    FOREIGN KEY (ledger_id, seq) REFERENCES events (ledger_id, seq)
);

-- The bitemporal balance index: everything a balance query filters on, with
-- the amount carried in the index so the sum never touches the heap.
CREATE INDEX entries_balance
    ON entries (ledger_id, account, effective_at, seq) INCLUDE (amount_minor);

CREATE INDEX entries_tx ON entries (ledger_id, tx_id);
CREATE INDEX entries_recorded ON entries (ledger_id, account, recorded_at);

-- Running balances, maintained alongside the entries in the same transaction.
-- They exist so an overdraft check is a primary-key lookup rather than a scan
-- of every entry the account ever had; the entries remain authoritative and
-- the two are cross-checked by the integration tests.
CREATE TABLE balances (
    ledger_id      text     NOT NULL REFERENCES ledgers (id),
    account        text     NOT NULL,
    amount_minor   bigint   NOT NULL,
    currency_code  text     NOT NULL,
    currency_scale smallint NOT NULL,

    PRIMARY KEY (ledger_id, account)
);

CREATE TABLE idempotency (
    ledger_id    text        NOT NULL REFERENCES ledgers (id),
    key          text        NOT NULL,
    request_hash bytea       NOT NULL CHECK (octet_length(request_hash) = 32),
    seq          bigint      NOT NULL,
    tx_id        uuid,
    recorded_at  timestamptz NOT NULL,

    PRIMARY KEY (ledger_id, key)
);
