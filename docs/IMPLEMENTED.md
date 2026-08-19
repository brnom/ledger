# What is implemented

An inventory of what exists today, organised by capability rather than by file,
with where each one lives and how it is held to account.

Phases 0 and 1 of the [roadmap](ROADMAP.md) are complete. 8,956 lines of Go,
two direct dependencies, 82.0% statement coverage.

---

## Money

`domain/money.go`

`Amount` is an int64 count of minor units carrying its `Currency`, which carries
its own scale. Never a float, and never a bare integer that could be added to a
different currency by accident.

- Addition, subtraction, negation and multiplication report `ErrOverflow`
  instead of wrapping, and `ErrCurrencyMismatch` instead of coercing.
- Parsing rejects more decimal places than the currency has rather than
  rounding. Deciding how to round is the caller's business; silently dropping a
  digit is how money goes missing.
- `Allocate` splits an amount across ratios by the largest-remainder method, so
  a payment split or fee breakdown sums back to the original exactly. This is
  the primitive a marketplace split needs.
- A small currency registry exists to stop the same code being used with two
  different scales in one deployment, not to be an exhaustive ISO 4217 table.

Representable range is roughly ±92 quadrillion for a two-decimal currency.

## Accounts, transactions, reversal

`domain/account.go`, `domain/transaction.go`

- Account names are colon-separated hierarchies (`liabilities:users:9f3c`),
  queryable by prefix on segment boundaries — `assets` does not match
  `assets_frozen`.
- `Normal` (debit or credit) is presentation, not storage. Balances are held
  signed and debit-positive throughout; a customer wallet is a liability
  carrying a credit balance internally, yet `Account.Presented` shows the
  customer a positive number.
- `AllowNegative` marks the accounts that may be overdrawn — the external and
  clearing accounts that represent the other side of the world — separating
  them from the ones that may not.
- Postings are signed, so a transaction balances exactly when its legs sum to
  zero: one addition rather than a case analysis. Each currency must balance on
  its own, which forces an FX position account to be named rather than letting
  an implicit rate hide inside an entry.
- `Reverse` produces the mirror transaction. A reversal is always a new
  transaction, never an edit — that is the difference between a ledger that can
  be audited and one that can only be trusted.

## The event log

`domain/event.go`, `domain/project.go`

Append-only, with each event carrying the hash of the one before it. Editing,
removing or reordering any event breaks every hash after it, and
`ledger.Verify` walks the log and reports where.

Two details make this hold against a real database:

- **Canonical encoding** — object keys sorted, no insignificant whitespace, no
  floating-point numbers. The bytes are a function of the value, so an
  independent implementation can recompute the same hash.
- **Timestamps truncated to microseconds at ingress**, matching PostgreSQL's
  `timestamptz`. An event hashed at nanosecond precision would stop verifying
  the moment it was read back.

`Project` is the single function that turns an event into read-model rows. Both
stores call it, so they cannot drift, and rebuilding the read model is a fold
over the log rather than a second implementation to keep in step.

## Bitemporality

`domain/read.go` (`BalanceQuery`), both stores

Every entry carries when the money moved (*effective*) and when the ledger
learned of it (*recorded*). `BalanceQuery` bounds either axis or both.

This is the capability the whole design exists for, so here it is measured
rather than described — real numbers from a run against PostgreSQL through the
HTTP server. A merchant sells R$500 today; three days later a settlement file
arrives describing a R$120 sale that happened three days ago:

| Query | Balance |
|---|---|
| now, everything included | 620.00 |
| effective as of yesterday | 120.00 |
| as recorded at seq 6, before the file arrived | 500.00 |
| seq 6 **and** effective as of yesterday | 0.00 |

The third row is the point: it was 500.00 before the backdated entry landed and
it is still 500.00 after. Nothing the ledger reported has changed. The fourth
row shows the axes composing — at that point in the log only today's sale
existed, and it was not yet effective yesterday.

## Idempotency

`app/ledger.go`

A key is fingerprinted against the caller-supplied fields of the command, never
the values the ledger generates, so a retry matches while a genuinely different
request does not.

- Same key, same request → the original result, nothing written, `Replayed` set.
- Same key, different request → `ErrIdempotencyConflict`. Two different payments
  must never collapse into one because a caller reused a key.
- A key used by a *failed* command is free to reuse, so a transient failure does
  not poison it forever.

## Concurrency

`app/ledger.go`, both stores

One writer per ledger: PostgreSQL takes `SELECT ... FOR UPDATE` on the ledger
row, memstore takes a mutex. A command reads the state it is about to change
with nothing able to slip in between, and the sequence is gapless by
construction — which is what the hash chain depends on.

## Storage

`adapter/driven/memstore`, `adapter/driven/pgstore`, `adapter/driven/storagetest`

Two adapters behind `ledger.Store`. The in-memory one is small enough to read in
one sitting and is the reference implementation; PostgreSQL is the real one,
with embedded versioned migrations.

Both pass the same conformance suite, so "memstore is the reference" is a
checked claim. Events and the entries projected from them are written in the
same database transaction — no follower, no lag, no "your transfer succeeded"
followed by an unchanged balance.

## HTTP

`adapter/driving/httpapi`, `cmd/ledgerd`

`net/http` with 1.22 routing patterns, no framework. Errors are RFC 9457 problem
details, so a client can handle failures by a stable type URI rather than by
parsing prose. Detail is carried on 4xx, where it tells the caller what they did
wrong, and withheld on 5xx, where the cause is ours.

`httpapi.Ledger` is the driving port — the twelve methods the transport needs —
so the HTTP layer can be exercised without a store. See
[ADR 8](adr/0008-hexagonal-assessment.md).

`ledgerd` runs in memory with no configuration, or against PostgreSQL with
`-dsn`.

---

## How this is held to account

The tests are the argument that the above works, so they are the part worth
reading.

**Property tests** (`app/property_test.go`) drive random command sequences and
assert what must hold regardless of what was asked: every currency sums to zero
across the book, no account goes overdrawn without permission, the chain
verifies, replay reproduces storage, and — the one that matters most — for
*every* prefix of the log, the balance as of that point equals the sum of
exactly the entries recorded up to it.

They were checked for teeth rather than assumed to have any. Two deliberate
mutations were injected into the bitemporal query — an off-by-one on the
recorded bound, and dropping the effective bound entirely — and the suite caught
them in 1 and 3 runs respectively.

**The conformance suite** (`adapter/driven/storagetest`) runs the same tests against
memstore and PostgreSQL. It has already caught a real divergence: SQL
`ORDER BY` used the database's collation, which orders punctuation differently
from Go, so account listings came back in two different orders. Fixed with
`COLLATE "C"`.

**Replay determinism** — folding `Project` over the log reproduces the
materialized read model entry for entry, including after a round trip through
PostgreSQL, where each event's hash is recomputed from what came back out.

**Concurrency**, under `-race`: N writers against an account with funds for only
10 of them must let exactly 10 through, and N callers sharing one idempotency
key must produce exactly one write.

**Fuzzing** on amount parsing, canonical formatting, and `Allocate`'s guarantee
that a split never creates or loses a minor unit.

**A golden hash test** pins the hashing scheme. Its failing is the intended
alarm, not a nuisance to update: a changed hash invalidates every chain in
storage.

**A driving-port stub** reaches failure paths a real ledger will not produce on
demand — a lost write race, a broken chain — and found a real bug doing so: 5xx
responses were leaking wrapped error detail to the client.

```bash
make check              # gofmt, vet, staticcheck, tests under -race
make test-integration   # the above plus PostgreSQL via docker compose
make prop               # property tests, 10,000 cases
make fuzz               # every fuzz target
```

---

## Known limits

Stated plainly, because a portfolio that only lists strengths is not worth much.

- **No forward-dated balances yet.** `effective_at` in the future is rejected
  beyond a minute of clock skew. Scheduled entries and D+30 settlement
  projections are phase 2 — the query shape supports them, the write path does
  not admit them.
- **No payment lifecycle.** Holds, partial capture and chargebacks are modelled
  by hand today. Phase 3.
- **Overdraft is checked against the current balance, not every historical
  point.** A backdated entry can leave a past balance negative while the present
  one is healthy. Refusing such an entry would make the ledger unable to record
  what actually happened, so it is admitted deliberately — but it means "this
  account was never overdrawn" is not a guarantee the ledger makes.
- **int64 minor units cap high-precision assets.** At 18 decimals the ceiling is
  about 9.2 units, which is useless for wei-denominated ETH. Such assets are
  deliberately absent from the currency registry rather than silently broken.
- **Writes are serial per ledger.** That is the intended trade — a ledger's
  purpose is a single agreed order — but throughput scales by splitting books,
  not by adding writers to one.
- **No authentication or multi-tenancy in the HTTP layer.** `ledgerd` serves one
  ledger and trusts its caller. Fine behind a gateway, not fine on the open
  internet.
- **`cmd/ledgerctl` is an empty directory.** Chain verification is reachable over
  HTTP; the CLI is phase 5.
