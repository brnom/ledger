# 2. One writer per ledger

Status: accepted

## Context

Committing a transaction means reading state, then appending to the log based on
what was read. The reads ask whether these accounts exist and whether there is
enough money. If another writer slips in between, two commands can each observe
a balance of 100 and each spend it.

The event sequence also has to be gapless. The hash chain links event *n* to
event *n-1*, so a hole makes the chain unverifiable. A `bigserial` leaves holes
whenever a transaction rolls back.

## Decision

Every write takes `SELECT ... FOR UPDATE` on the ledger's row before doing
anything else, and holds it until commit. Reads inside the command see a state
nobody else can change. The sequence number is assigned in Go from the observed
head, so it is contiguous by construction.

The in-memory store does the same thing with a mutex, which is why both stores
pass the same conformance tests.

## Consequences

Writes to one ledger are serial. That is the cost, and it is the right cost: a
ledger's whole purpose is to be a single agreed order of events. Throughput
scales by splitting books, not by interleaving writers within one.

An advisory lock was the alternative. A row lock was chosen because it is exact.
Advisory locks are keyed by a hashed integer, so two unrelated ledgers can
collide and serialize against each other for no reason.

`ErrConflict` exists for the case where the lock is lost anyway. The unique
constraint on `(ledger_id, seq)` is the backstop. It turns a bug in this
reasoning into a failed write rather than a corrupted chain.
