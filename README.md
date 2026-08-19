# ledger

A double-entry ledger in Go, built around the question most ledgers cannot
answer: **what did we believe the balance was, back then?**

```
Balance(account, asOfEffective, asOfRecorded)
  ├── effective = now,   recorded = now    → the balance today
  ├── effective = D+30,  recorded = now    → what settles by the 30th
  └── effective = Jan 10, recorded = Jan 12 → what we reported on the 12th
```

Every entry carries two timestamps: when the money moved (*effective*) and when
the ledger learned of it (*recorded*). A settlement file that arrives three days
late is recorded today and takes effect three days ago. The balance the ledger
reported three days ago does not change. That is the whole idea, and everything
else in the design follows from making it true.

> Status: the core is complete and tested — accounts, transactions, reversals,
> idempotency, bitemporal balances, a hash-chained event log, and stores for
> memory and PostgreSQL. Forward-dated settlement, payment holds, and
> reconciliation are the next phases.

## Why

Formance, Blnk, Midaz, TigerBeetle and the rest have double-entry, throughput
and idempotency well covered. What stays unsolved is the shape of the problem in
payments. Money that has moved but is not yet available. Corrections that arrive
after the report was filed. An auditor who wants last month's number to
reproduce exactly.

Four things this is built to do that a single-timestamp ledger cannot:

1. **Two time axes** — backdated corrections that do not rewrite history, and
   the foundation for forward-dated settlement schedules.
2. **Payment lifecycle** — authorization holds, partial capture, chargebacks as
   ledger primitives rather than something modelled by hand on top.
3. **Reconciliation** — matching external statements as a first-class operation.
4. **Provable integrity** — every balance traceable to the entries that made it,
   over a log that can be shown to be unaltered.

The first is built. The rest are phased on top of it, not bolted beside it.

## Try it

```bash
make run                                   # in memory, no setup
curl -X POST localhost:8080/v1/accounts \
  -d '{"name":"assets:cash","currency":"BRL","normal":"debit","allow_negative":true}'
curl -X POST localhost:8080/v1/accounts \
  -d '{"name":"liabilities:users:1","currency":"BRL","normal":"credit"}'

curl -X POST localhost:8080/v1/transactions \
  -H 'Idempotency-Key: pay-1' \
  -d '{"reference":"pix-e2e-1","postings":[
        {"account":"assets:cash","amount":"100.00","currency":"BRL"},
        {"account":"liabilities:users:1","amount":"-100.00","currency":"BRL"}]}'

curl 'localhost:8080/v1/accounts/liabilities:users:1/balance'
curl 'localhost:8080/v1/accounts/liabilities:users:1/balance?as_of_seq=2'
curl localhost:8080/v1/verify
```

`make run-pg` does the same against PostgreSQL via `docker compose`.

## As a library

The HTTP server is one adapter over the same core. `domain` holds the ledger's
own vocabulary and `app` holds the use cases. The rules live below both, so
either path gets the same guarantees.

```go
store := memstore.New()                  // or pgstore.Open(ctx, dsn)
l, _ := app.Open(store, "main")

l.OpenAccount(ctx, app.OpenAccountCommand{
    Name: "liabilities:users:1", Currency: domain.MustCurrency("BRL"),
    Normal: domain.Credit,
})

l.Commit(ctx, app.CommitCommand{
    IdempotencyKey: "pay-1",
    Postings: []domain.Posting{
        domain.Dr("assets:cash", domain.FromMinor(brl, 10000)),
        domain.Cr("liabilities:users:1", domain.FromMinor(brl, 10000)),
    },
})

// What we believed, as of event 12.
l.Balance(ctx, domain.BalanceQuery{Account: "liabilities:users:1", AsOfSeq: 12})
```

## Design

Nine decisions, each written up in [`docs/adr`](docs/adr):

| | |
|---|---|
| [Event sourcing with two time axes](docs/adr/0001-event-sourcing-with-bitemporal-time.md) | The log *is* the recorded axis, so bitemporality is nearly free — and impossible to retrofit later. |
| [One writer per ledger](docs/adr/0002-one-writer-per-ledger.md) | `SELECT ... FOR UPDATE` on the ledger row. Reads and the append that follows them cannot be interleaved, and the sequence has no gaps. |
| [Projections in the write transaction](docs/adr/0003-projections-in-the-write-transaction.md) | No follower, no lag, no "your transfer succeeded" followed by an unchanged balance. |
| [Hash-chained events](docs/adr/0004-hash-chained-events.md) | Each event hashes the one before it. Editing any event breaks everything after it. |
| [Payloads as `json`, not `jsonb`](docs/adr/0005-payloads-as-json-not-jsonb.md) | `jsonb` reorders keys; the hash covers the exact bytes. |
| [Money as int64 minor units](docs/adr/0006-integer-minor-units.md) | Never a float. Cross-currency arithmetic is a compile-time-shaped error, overflow is reported, not wrapped. |
| [Standard library first](docs/adr/0007-stdlib-first.md) | The core imports nothing outside the stdlib — including its own UUIDv7, canonical JSON and hashing. |
| [How hexagonal this already is](docs/adr/0008-hexagonal-assessment.md) | Ports, adapters and a contract suite were already there; what was missing was a driving port, which found a bug the day it was added. |
| [Born hexagonal](docs/adr/0009-hexagonal-layout.md) | The layout the assessment priced and deferred, done while it is still free — and without the alias facade that made it look expensive. |

### Layout

Ports and adapters, with the domain at the centre. Every arrow points inward,
and `make arch` fails the build if one ever stops doing so.

```
domain/                       the centre: no context, no I/O, no module imports
  money.go                    Amount, Currency, checked arithmetic, Allocate
  account.go                  account names, debit/credit normals, overdraft policy
  transaction.go              postings, the sum-to-zero rule, reversal
  event.go                    event envelope, canonical encoding, the hash chain
  fingerprint.go              command identity, which is what idempotency turns on
  project.go                  the one function that turns an event into read-model rows
  read.go                     Entry, Head, and the bitemporal queries

app/                          the use cases
  ledger.go                   OpenAccount, Commit, Revert, Balance, Verify
  command.go                  what a caller may ask for, and what comes back
  port.go                     the driven ports: Store and Writer

adapter/driven/memstore       reference implementation, in memory
adapter/driven/pgstore        PostgreSQL, with embedded migrations
adapter/driven/storagetest    the conformance suite both stores must pass
adapter/driving/httpapi       HTTP with RFC 9457 problem details, and the driving port
cmd/ledgerd                   the server
```

## Testing

The tests are the argument that this works, so they are the part worth reading.

- **Property tests** (`rapid`) drive random command sequences and assert what
  must hold regardless. Every currency sums to zero across the book. No account
  goes overdrawn without permission. The chain verifies. And the one that
  matters most: for *every* prefix of the log, the balance as of that point
  equals the sum of exactly the entries recorded up to it.
- **A conformance suite** runs the same tests against memstore and PostgreSQL,
  so "the in-memory store is the reference implementation" is checked rather
  than asserted. It has already caught a real difference: SQL `ORDER BY` used
  the database's collation, which orders punctuation differently from Go.
- **Replay determinism** — folding `Project` over the log reproduces the
  materialized read model entry for entry. If the two ever disagree, the log has
  stopped being the source of truth.
- **Concurrency** — under `-race`. N writers against one account with funds for
  only 10 of them must let exactly 10 through. N callers sharing one idempotency
  key must produce exactly one write.
- **Fuzzing** on amount parsing, canonical formatting, and `Allocate`'s
  guarantee that a split never creates or loses a minor unit.
- **Golden hash tests** pin both hashing schemes: the event chain, and the
  command fingerprint that idempotency is built on. Their failing is the
  intended alarm, not a nuisance to update.
- **An architecture test** — `make arch` reads the real import graph. It fails
  the build if `domain` ever depends on anything else in the module, or if `app`
  depends on anything but `domain`.

```bash
make check              # gofmt, vet, staticcheck, the arch guard, tests under -race
make test-integration   # the above plus PostgreSQL, via docker compose
make prop               # property tests, 10,000 cases
make fuzz               # every fuzz target
```

Statement coverage is > 80%. The uncovered remainder is mostly `cmd/ledgerd`
wiring and error branches that need a database failure to reach.

## Roadmap

| Phase | | |
|---|---|---|
| 0–1 | Core: double-entry, event log, hash chain, idempotency, bitemporal balances, PostgreSQL, HTTP | **done** |
| 2 | Forward-dated entries; `ProjectedBalance` and per-day settlement schedules (D+1, D+30) | next |
| 3 | Payment lifecycle: holds, partial capture, release, chargeback; available vs pending | |
| 4 | Reconciliation: statement ingestion, matching engine, suspense accounts | |
| 5 | `ledgerctl verify` / `replay`, balance drill-down, audit export | |

Each phase with its scope, acceptance criteria and open decisions — and what is
deliberately *not* planned — is in [docs/ROADMAP.md](docs/ROADMAP.md).

## License

[Apache License 2.0](LICENSE). Permissive, with an explicit patent grant.
