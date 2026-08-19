# 7. Standard library first

Status: accepted

## Context

The project has three goals at once: learn Go properly, be worth showing, and be
genuinely useful. A framework would work against the first two.

## Decision

The core `ledger` package imports nothing outside the standard library —
including its UUIDv7 generation, canonical JSON encoding, and hashing. Beyond
it:

- `net/http` with the 1.22 routing patterns. No router, no framework.
- `pgx/v5` for PostgreSQL, no ORM. Queries are written out.
- `rapid` for property tests, in test files only.

Errors go over the wire as RFC 9457 problem details, so clients get a standard
shape without a dependency on either side.

## Consequences

Someone can import the ledger without pulling in a dependency tree. There is no
framework behaviour to learn before reading the code, and nothing between the
domain rules and the reader.

The cost is writing things a library would have provided. For a project whose
first purpose is learning, that is the point rather than the price.
