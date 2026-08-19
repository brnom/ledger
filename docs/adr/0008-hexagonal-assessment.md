# 8. How hexagonal this already is, and what finishing the job would cost

Status: superseded by [0009](0009-hexagonal-layout.md)

Tier 1 was applied here and still stands. Tiers 2 and 3 were deferred on the
costing below; 0009 revisits that costing and carries them out. The measurement
in this document is unchanged and is the reason 0009 was cheap.

## Context

The question came up whether to migrate this codebase to hexagonal
architecture — ports and adapters, with the domain at the centre and
infrastructure at the edges.

The question deserves a measurement rather than an opinion, because the answer
turned out to be mostly "it already is", and the remaining gap is smaller and
differently shaped than it looks from the outside.

## What was already there

| Hexagonal concept | Where it lives |
|---|---|
| Driven ports | `ledger.Store`, `ledger.Writer` in `store.go` — declared by the consumer, not exported by the adapter |
| Driven adapters | `storage/memstore`, `storage/pgstore` |
| Driving adapters | `httpapi`, `cmd/ledgerd` |
| Domain free of infrastructure | the root package imports no `net`, no `database`, no driver |
| Port contract tests | `storage/storagetest` — the same suite runs against both adapters |

That last row matters more than the folder names. The main thing hexagonal
promises is that you can swap an adapter and know the system still behaves;
`storagetest` is that promise, discharged. It has already earned its keep by
catching a real divergence between the two adapters: SQL `ORDER BY` used the
database's collation, which orders punctuation differently from Go's byte
comparison, so the same account listing came back in two different orders. That
is exactly the class of bug a shared contract suite exists to find, and no
amount of folder restructuring would have found it.

## What was actually missing

Two things, and only one of them was coupling.

**1. The driving edge had no port.** `httpapi.New` took a concrete
`*ledger.Ledger`. Measured against the code rather than guessed: the transport
uses exactly twelve methods — `ID`, `OpenAccount`, `Commit`, `Revert`,
`Balance`, `Entries`, `Account`, `Accounts`, `Transaction`, `Head`, `Events`,
`Verify`. Notably *not* `PresentedBalance`, because the handler composes
`Account` + `Balance` + `Account.Presented` itself.

**2. Layer names.** Entities, value objects, ports and the application service
all live in one package. In Go this is largely signalling: the package system
already provides the isolation that a `domain/` folder announces.

## Decision

Apply tier 1 only: declare the driving port in `httpapi`, next to the code that
consumes it, and record this assessment. Leave the package layout alone.

The port was worth it on its own merits, and it paid immediately. Writing a stub
that implements the twelve methods exposed a bug in the transport: `problem.go`
documented that server-side failures carry no detail to the client, but
`problemFor` attached `err.Error()` to every mapped error including 5xx, so a
broken hash chain would have replied with the failing sequence number. A real
ledger will not produce `ErrChainBroken` or `ErrConflict` on demand — the
single-writer lock exists precisely to prevent the latter — so no test built on
the real thing would have reached those paths. That is the concrete argument for
a driving port, and it is a better one than symmetry.

## What tiers 2 and 3 would cost

**Tier 2 — separate application from domain. About a day, low risk.**

Five unexported symbols cross the boundary, all of them consumed by `ledger.go`:
`normalizeTime` (time.go), and `canonicalJSON`, `writeChunk`,
`validateLedgerID`, `postingsToWire` (event.go). Splitting the packages means
exporting them, relocating them, or introducing a shared internal package.

The interesting part is not the mechanics. Four of the five are reached by
`fingerprint()`, which hashes a command to decide whether an idempotency key is
being replayed or misused. An application service reaching into the domain's
canonical encoding is a smell in one direction and a finding in the other:
command identity is arguably a domain rule, not application plumbing. If tier 2
is ever done, `fingerprint` should move down rather than the encoders moving up.

**Tier 3 — the canonical layout. Two to three days, moderate risk.**

`internal/domain`, `internal/app`, `internal/adapters/{driven,driving}`, with
imports rewritten across roughly twenty-five files and about 1,190 lines of
white-box tests moving with their subjects (`money_test.go`, `event_test.go`,
`account_test.go`, `transaction_test.go` are in `package ledger` because they
test unexported behaviour).

The risk is almost entirely compile-time and surfaces immediately. Behaviour is
protected by the property tests, the conformance suite and the end-to-end run,
none of which care where a type is declared.

## The tension that settles it

The canonical Go hexagonal layout puts the core under `internal/`, which makes
it unimportable. This project's stated delivery is a Go library *and* an HTTP
server, so tier 3 would force the root package into a facade of roughly eighty
lines of type aliases (`type Amount = domain.Amount`, and so on) to keep library
users working.

That is a permanent maintenance surface — every new exported type needs an alias
— bought in exchange for legibility to a reader who is specifically looking for
hexagonal folder names. For a codebase whose argument is that it can be read,
adding an indirection layer to signal that it is well structured is a poor
trade.

## Consequences

The dependency graph now points inward at both edges: adapters depend on the
core, the core depends on nothing. That is the property hexagonal is *for*, and
it holds regardless of the directory names.

What is given up is discoverability for a reader who scans for `domain/` and
`adapters/` folders. This document is the mitigation: it names where each
hexagonal role lives, so the structure is findable even though it is flat.

If the decision is ever revisited — a second driving adapter such as gRPC, or a
second application service sharing the domain — tier 2 is the point at which the
split starts paying for itself, and the five symbols above are the whole of the
work.
