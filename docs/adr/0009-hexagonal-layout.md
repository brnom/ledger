# 9. Born hexagonal: the layout, done while it is still free

Status: accepted (supersedes [0008](0008-hexagonal-assessment.md))

## Context

[ADR 0008](0008-hexagonal-assessment.md) measured how hexagonal this codebase
already was, applied tier 1 — the driving port — and deferred tiers 2 and 3.
The deferral rested on one specific cost, and it is worth restating precisely,
because that cost turned out to be an artefact of one assumption rather than a
property of the layout:

> The canonical Go hexagonal layout puts the core under `internal/`, which makes
> it unimportable. This project's stated delivery is a Go library *and* an HTTP
> server, so tier 3 would force the root package into a facade of roughly eighty
> lines of type aliases.

The assumption is `internal/`. It is what the canonical layout does, so it came
along unexamined. Drop it — keep `domain` and `app` importable — and the facade
has nothing to do: a library user imports the two packages directly, and no
alias needs to exist, now or for every type added later.

What is given up by not using `internal/` is the compiler refusing to let an
outside caller reach the core. That refusal was never worth much here: the core
*is* the product. A library whose domain types cannot be named by its users is
not encapsulated, it is unusable.

The second thing that changed is timing. 0008 was written against a codebase
with an initial commit, no release and no external consumers, and it priced the
move at two to three days of import rewriting. That price only goes up. Every
week of new code is more files to move and, eventually, an API someone depends
on. The layout is free exactly once.

## Decision

Carry out tiers 2 and 3, without `internal/` and without a facade:

```
domain/                      entities, value objects, invariants, the event log
app/                         use cases + the driven ports (Store, Writer)
adapter/driven/{memstore,pgstore,storagetest}
adapter/driving/httpapi      also owns the driving port, from tier 1
cmd/ledgerd
```

The root package is gone. The module path is unchanged.

## What the split actually cost

0008 named the whole of the work: five unexported symbols crossing the
boundary — `normalizeTime`, `canonicalJSON`, `writeChunk`, `validateLedgerID`,
`postingsToWire` — and observed that four of the five were reached through
`fingerprint()`, which hashes a command to decide whether an idempotency key is
a replay or a misuse. Its recommendation:

> If tier 2 is ever done, `fingerprint` should move down rather than the
> encoders moving up.

That is what happened, and it is the reason the boundary came out small.
`fingerprint`, its three request structs and the two helpers that render an
optional timestamp and an optional id now live in `domain/fingerprint.go`,
behind three functions: `FingerprintOpenAccount`, `FingerprintCommit`,
`FingerprintRevert`. Command identity is a rule about what makes two requests
the same request, which is the ledger's business and not the service's.

The consequence is that `canonicalJSON`, `writeCanonical`, `writeChunk`,
`postingsToWire` and `hashDomain` never leave `domain` at all. Of the five
symbols, exactly two needed exporting — `NormalizeTime` and `ValidateLedgerID`
— and both were already documented domain rules with an exported constant
beside them (`TimePrecision`, `MaxLedgerIDLen`).

The public surface grew by five names. It would have grown by eighty aliases
under `internal/`.

## Where the read model lives

`Entry`, `Head`, `RecordedTransaction`, `IdempotencyRecord`, `BalanceQuery` and
`EntryQuery` went to `domain`, not `app`, though they are the vocabulary of the
driven port. Two reasons, and the first is decisive: `Projection` is built from
`Account`, `RecordedTransaction` and `Entry`, and `Project` is the domain
function that produces it — the read model is derived from the log by a domain
rule, so its shape is domain-shaped. The second is that `BalanceQuery` encodes
this project's whole thesis, including the rule that `AsOfSeq` wins over
`AsOfRecorded` because sequence numbers are exact. That is not transport.

`app` is therefore three files: the service, the commands, the two interfaces.

All sentinel errors stayed in `domain/errors.go`, including `ErrConflict` and
`ErrIdempotencyConflict`, which are arguably application concerns. They are one
vocabulary of failure that adapters match with `errors.Is`; splitting it across
two packages would make every caller import both to handle errors, and buys
nothing.

## Consequences

The dependency graph pointed inward before this change — that was 0008's
finding. What is new is that it is now checked rather than asserted: `make arch`
fails the build if `domain` ever imports anything else in the module, or if
`app` imports anything but `domain`. It runs in `make check`, so CI enforces it.

That guard is the real deliverable. Folder names are a claim about a codebase;
`go list -deps` is the fact. 0008 was right that the structure held regardless
of directory names — and a reader had to take its word for it. Now they do not.

The discoverability complaint 0008 raised against itself is also settled: the
roles are findable by looking at the tree, and this document no longer has to
serve as the map.

What was actually spent: the compile-time breakage 0008 predicted, which
surfaces immediately and is fixed by the compiler pointing at it. Behaviour is
held in place by the conformance suite running against both stores, the
property tests, and a new golden test pinning the three command fingerprints —
the one place in the move where a mistake would have been silent rather than a
build failure.
