# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

`make help` lists every target. The ones that matter:

```bash
make check              # what CI runs: fmtcheck, vet, staticcheck, arch, tests under -race
make test               # unit tests; the PostgreSQL ones skip themselves without a DSN
make test-integration   # starts the compose PostgreSQL and runs everything against it
make arch               # fails if the dependency graph stops pointing inward
make prop CHECKS=10000  # property tests, hard
make fuzz FUZZTIME=30s  # every fuzz target, discovered by grep, one package at a time
make run                # ledgerd in memory on :8080; make run-pg for PostgreSQL
```

Running one test: `go test ./domain -run TestFingerprintGolden`, `go test ./app -run TestRevert -v`.

Two traps when composing `go test` by hand:

- **Package arguments must come before any flag `go test` does not know itself.**
  `go test -run TestProperty -rapid.checks=100 ./app` silently drops `./app` and
  runs the default package; `go test ./app -run TestProperty -rapid.checks=100`
  is correct. This is why `make prop` scopes to `PROP_PKGS` rather than `./...`.
- **`-rapid.checks` only exists in binaries that import rapid** (`./app`), so it
  cannot be passed to `./...`.

PostgreSQL tests read `LEDGER_TEST_POSTGRES_DSN` and skip without it. The compose
database listens on **55432** locally (5432 in CI).

## Architecture

Ports and adapters, with the dependency graph enforced by `make arch` rather than
by convention. [ADR 0009](docs/adr/0009-hexagonal-layout.md) explains why the core
is importable instead of under `internal/`; [ADR 0008](docs/adr/0008-hexagonal-assessment.md)
is the measurement that preceded it and is still the map of which role lives where.

```
domain/   entities, value objects, the event log, command identity, projection
          imports nothing outside the stdlib and nothing else in this module
app/      the use cases + the driven ports (Store, Writer), declared in the consumer
adapter/driven/{memstore,pgstore,storagetest}
adapter/driving/httpapi   also declares its own driving port (httpapi.Ledger)
cmd/ledgerd
```

The design decisions are in [`docs/adr`](docs/adr) and are the intended reading
order for anything non-obvious. Four load-bearing ones:

**The log is the recorded-time axis.** Every entry carries `EffectiveAt` (when the
money moved) and `RecordedAt`/`Seq` (when the ledger learned of it). `BalanceQuery`
selects along both, and `AsOfSeq` beats `AsOfRecorded` because sequence numbers are
exact. This is the reason the project exists; nothing may make a historical balance
irreproducible.

**One writer per ledger.** `Store.Update` hands the caller a `Writer` holding the
ledger's lock (`SELECT ... FOR UPDATE` in pgstore). Everything a command needs in
order to validate is readable through that `Writer`, so validation and append are
one transaction. Never validate against `Store` reads and then append.

**`domain.Project` is the only place an event becomes read-model rows.** Both
stores call it, which is what keeps them from drifting and makes a rebuild a fold
rather than a second implementation.

**The event chain hashes canonical bytes.** `NewEvent` encodes payloads with the
package's canonical JSON (sorted keys, no floats) and each event hashes the one
before it.

## Invariants that fail quietly if broken

- **Timestamps are truncated to `TimePrecision` (microseconds) before hashing.**
  A nanosecond that survives into an event makes the recomputed hash differ after
  a PostgreSQL round trip, and the chain looks broken. Use `domain.NormalizeTime`.
- **Payloads are stored as `json`, not `jsonb`** — `jsonb` reorders keys and the
  hash covers the exact bytes ([ADR 0005](docs/adr/0005-payloads-as-json-not-jsonb.md)).
- **Command fingerprints cover only what the caller supplied.** `domain.Fingerprint*`
  deliberately excludes a generated transaction id or effective time; including one
  would give every retry a different fingerprint and defeat idempotency.
- **Golden hash tests exist for both schemes** (`TestEventHashGolden` and
  `TestCanonicalJSONGolden` in `domain/event_test.go`, `TestFingerprintGolden` in
  `domain/fingerprint_test.go`). Failing them is the intended alarm, not a value to
  update — changing either scheme invalidates every stored hash and idempotency key.
- **Balances are held signed and debit-positive everywhere.** `Normal` is
  presentation only, applied by `Account.Presented`, and that is the single place
  the sign flips.
- **Money is `int64` minor units** and arithmetic reports `ErrOverflow` /
  `ErrCurrencyMismatch` rather than wrapping or coercing. Never introduce a float.

## Working in this repo

- **Adding a `Store` or `Writer` method** means implementing it in memstore *and*
  pgstore *and* extending `adapter/driven/storagetest`, which is what makes "the
  in-memory store is the reference implementation" a checked claim. That suite has
  already caught a real divergence (SQL `ORDER BY` collation vs Go byte order).
- **`domain` may not import anything else in the module, and `app` may import only
  `domain`.** If a symbol is needed across the boundary, prefer moving the rule
  down into `domain` over exporting an encoder upward — that is the reasoning
  recorded in ADR 0009 for why `fingerprint` lives in the domain.
- **Stdlib first** ([ADR 0007](docs/adr/0007-stdlib-first.md)): two direct
  dependencies, pgx and rapid. UUIDv7, canonical JSON and hashing are hand-written
  on purpose.
- **Never commit to `main`.** Every change starts with a feature branch off
  `main`. Check the current branch before the first commit of any task.
- **Commits follow Conventional Commits** (`feat(domain):`, `fix(build):`, `test(app):`,
  `docs:`, `ci:`, `chore:`), with a body explaining the reasoning rather than the diff.
  Keep each commit to one coherent concern, because the history that lands is the
  history you wrote.
- **Work lands through a PR, rebase merged.** Open it with `gh pr create` and let
  the author merge it. Rebase merge, never squash and never a merge commit: `main`
  stays a linear sequence of individually meaningful commits, so `git log` and
  `git bisect` keep working. Squashing would collapse a multi-part change into one
  unreviewable blob.
- **Docs carry claims that must stay true.** `README.md` states a coverage figure
  and a line count, and `docs/IMPLEMENTED.md` names the file each capability lives
  in. Moving code means updating those.
- **[docs/ROADMAP.md](docs/ROADMAP.md)** has phases 2–5 (forward-dated settlement,
  payment holds, reconciliation, tooling) with scope and acceptance criteria. Each
  builds on the bitemporal core rather than beside it.
