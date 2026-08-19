# 3. Projections in the write transaction

Status: accepted

## Context

Event-sourced systems usually project their read models asynchronously. That
buys throughput and costs consistency: for some window, the log and the read
model disagree, and a caller who writes then reads gets an answer from before
their own write.

For a ledger that window is not a technical detail. "Your transfer succeeded"
followed by a balance that has not moved is a support ticket at best.

## Decision

Events and the entries they project are written in the same database
transaction. There is no follower and no lag.

The log stays authoritative: `ledger.Project` is the single function that turns
an event into read-model rows, both stores call it, and the tests replay the
whole log through it and require the result to match the materialized rows
entry for entry.

## Consequences

A write does more work, since it touches the log, the entries, the running
balances and the transaction index at once. In exchange there is no eventual
consistency to reason about anywhere in the system.

Because the projection is one pure function rather than a second
implementation, rebuilding the read model is a fold over the events, and
"replay reproduces storage" is a property the test suite checks on every run
rather than a claim in a document.
