# 4. Hash-chained events

Status: accepted

## Context

An immutable log is only immutable if something notices when it changes. A
`DELETE` from a database an operator can reach leaves no trace by itself.

## Decision

Each event stores the hash of the event before it, and its own hash over
`(domain tag, prev hash, seq, id, ledger, type, recorded time, idempotency key,
payload)`. Fields are length-prefixed before hashing, so two different events
cannot concatenate to the same bytes.

Editing, removing, or reordering any event breaks every hash after it.
`ledger.Verify` walks the log and reports where.

Two details make this hold in practice:

- **Payloads are canonically encoded**: object keys sorted, no insignificant
  whitespace, no floating-point numbers. The bytes are a function of the value,
  so an independent implementation can recompute the same hash.
- **Timestamps are truncated to microseconds at ingress**, because that is
  PostgreSQL's `timestamptz` resolution. An event hashed at nanosecond
  precision would stop verifying the moment it was read back.

## Consequences

The audit story that would otherwise be a separate feature comes with the log
itself, and it forced the canonical encoding that also makes replay
deterministic.

Changing the hashing scheme invalidates every stored chain, which is why
`hashDomain` carries a version and a golden test pins the current output. A
failing golden test is the intended alarm, not a nuisance to be updated.
