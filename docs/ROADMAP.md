# Roadmap

Phases 0 and 1 are done — see [IMPLEMENTED.md](IMPLEMENTED.md). What follows is
what remains, in the order it should be built and with the reason each phase
comes when it does.

The ordering is not arbitrary. Each phase leans on the bitemporal core rather
than sitting beside it. Forward-dated balances are the effective-time axis
pointed the other way. Holds are a state on entries that already exist.
Reconciliation matches against entries the ledger already indexes. Nothing here
requires revisiting phase 1.

---

## Phase 2 — Forward-dated entries and settlement schedules

**The problem.** In acquiring and marketplaces, most of a merchant's money is
not available yet. A sale today releases in D+1, D+30, or in instalments across
twelve months. "How much do I have" and "how much will I have on the 15th" are
different questions, and the second one is the operation. Today the ledger
rejects any `effective_at` more than a minute in the future
(`DefaultFutureLimit`), so it cannot express this at all.

**Scope.**

- Admit future `effective_at` when the ledger opts in via `WithFutureLimit`.
- Add a `state` to entries: `scheduled` until effective time arrives, then
  `posted`. Crucially this is a *function of the query time*, not a job. A job
  may materialise a cache, but correctness must not depend on that job having
  run.
- `ProjectedBalance(account, until)` — what the balance becomes by a date.
- `Schedule(account, from, to)` — how much releases per day, which is the
  endpoint a merchant dashboard is built on.
- Split the presented balance into available and scheduled, so a caller cannot
  accidentally spend money that has not released.

**Acceptance.**

- A sale effective in 30 days does not appear in today's balance and does appear
  in `ProjectedBalance(now+30d)`.
- Passing the release date changes the balance with no job having run.
  `testing/synctest` proves it, so the test is deterministic and does not sleep.
- The property suite gains: for any date *d*, `ProjectedBalance(d)` equals the
  sum of entries effective at or before *d*. This is the same invariant the
  recorded axis already has, which is why it should be cheap to add.
- The overdraft check counts only available funds, not scheduled ones.

**Open decisions.**

- Whether `state` is a stored column or derived at query time. Deriving is
  correct and simpler. Storing is faster and can drift. Probably derive, and add
  a materialised daily rollup only if the numbers demand it.
- Whether anticipation (*antecipação de recebíveis* — selling future receivables
  at a discount) belongs here or in its own phase. It is a real product in
  Brazil and is naturally expressed as a transaction against scheduled entries.

---

## Phase 3 — Payment lifecycle

**The problem.** Every system that needs authorisation, partial capture,
release, refund and chargeback models them by hand on top of generic accounts.
It is the same five accounts and the same six transitions, reinvented and subtly
wrong each time. Held funds are the common case that a plain double-entry ledger
has no word for.

**Scope.**

- Events: `HoldPlaced`, `HoldCaptured` (full or partial), `HoldReleased`,
  `HoldExpired`.
- `available` versus `pending` as states of the same read model, not separate
  tables — the same shape phase 2 introduces for `scheduled`.
- Refunds and chargebacks as compensating transactions linked by a correlation
  id, so the whole dispute reads as a chain.
- Hold expiry driven by effective time, tested with `synctest`.

**Acceptance.**

- A hold reduces available funds without moving the posted balance.
- Partial capture splits correctly: captured amount posts, remainder releases,
  and the two sum to the hold.
- A hold cannot be captured twice, captured beyond its amount, or captured after
  release. Each case is rejected with its own error, and each is covered by the
  property suite's rejection allowlist.
- Money is conserved across every lifecycle path, which the existing
  conservation property already checks and should keep passing unchanged.

**Open decisions.**

- Whether a hold is an event type or a transaction with a distinguished state.
  The event type is more explicit. The transaction reuses everything.

---

## Phase 4 — Reconciliation

**The problem.** The daily operational pain, and always outside the ledger. It
is the work of matching what the acquirer, bank or PSP says happened against
what the ledger recorded. The breaks — the rows that do not match — are where
money actually goes missing.

**Scope.**

- Statement ingestion (CSV and JSON) as an `ExternalStatementIngested` event.
  What arrived is then part of the audit trail itself, not a side table.
- A matching engine in layers: exact by reference, then by rule with tolerance
  on amount and date, then N:N for aggregated settlements.
- Automatic suspense accounts for the unmatched, so the book still balances
  while a break is open.
- A breaks report: what did not match, by age and amount.

**Acceptance.**

- A statement matching the ledger exactly produces no breaks and no entries.
- A statement with a missing, extra or misdated row produces exactly one break
  each, of the right kind.
- Suspense accounts return to zero when a break is resolved.
- Re-ingesting the same statement is idempotent — it is a retry, not a second
  reconciliation.

**Open decisions.**

- Whether matching writes entries or only annotations. Written entries make the
  ledger reflect reality sooner. Annotations keep matching reversible. Likely
  annotations first, entries on confirmation.

---

## Phase 5 — `ledgerctl` and explainability

**The problem.** Chain verification exists but is only reachable over HTTP.
Operating a ledger means checking its integrity, rebuilding a read model, and
answering "why is this balance what it is" from a terminal.

Most of this is already built and merely unexposed — `cmd/ledgerctl/` is an
empty directory today.

**Scope.**

- `ledgerctl verify` — walks the chain, reports the first break. Wraps the
  existing `Ledger.Verify`.
- `ledgerctl replay` — rebuilds the read model from the log into a fresh schema
  and diffs it against the live one. The test suite already does this in memory.
  The command makes it an operational tool.
- Drill-down over HTTP: balance → entries → transaction → event, so any number
  can be traced to the facts that produced it.
- Audit trail export, with the hashes, so a third party can verify
  independently.

**Acceptance.**

- `verify` exits non-zero and names the sequence number on a tampered event —
  tested by tampering with a row in a real database.
- `replay` against production data produces a byte-identical read model.

---

## Phase 6 — Finishing

**Scope.**

- OpenAPI description generated from or checked against the handlers.
- Benchmarks with real numbers for the README — transactions per second against
  PostgreSQL, and how balance queries scale with account history.
- A worked end-to-end example: a marketplace sale with a split and D+30 release,
  showing available and projected balances at the same instant. This is the
  demonstration that ties phases 2 and 3 together and the one worth putting at
  the top of the README.
- Balance snapshots if the benchmarks show balance queries degrading with
  history. The `entries_balance` covering index was built with this in mind, so
  measure before adding machinery.

---

## Deliberately not planned

- **Multi-ledger transactions.** Atomicity across books would require
  coordination that undoes the single-writer property phase 1 is built on. Cross
  book movement belongs in a saga above the ledger, not inside it.
- **Built-in FX rate management.** The ledger enforces that each currency
  balances on its own, which forces an explicit FX position account. Where the
  rate comes from is a pricing concern and does not belong here.
- **Authentication.** `ledgerd` trusts its caller and belongs behind a gateway.
  Building auth into it would be scope that teaches nothing this project set out
  to demonstrate.
