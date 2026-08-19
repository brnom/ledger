# 5. Event payloads stored as `json`, not `jsonb`

Status: accepted

## Context

`jsonb` is the usual choice in PostgreSQL: it indexes well and queries fast.
It also normalizes what it stores — reordering object keys, collapsing
whitespace, rewriting numbers.

The event hash covers the payload bytes exactly.

## Decision

The `payload` column is `json`, which preserves the input text verbatim. A
`payload_indexed jsonb GENERATED ALWAYS AS (payload::jsonb) STORED` column
alongside it carries a GIN index, so the payload is still queryable.

## Consequences

Storage roughly doubles for the payload. That is the price of being able to
verify a chain read back out of the database, and the alternative is a ledger
whose integrity check fails for a reason nobody can find.

The conformance suite reads every event back and calls `Verify`, so this
decision is enforced by a test rather than by memory.
