# 1. Event sourcing with two time axes

Status: accepted

## Context

A ledger has to answer two different questions about the past:

- *What was the balance on 10 January?* — business time.
- *What did we say the balance on 10 January was, back on 15 January?* — system
  time.

Most ledgers store one timestamp and can only answer the first. That is enough
until a settlement file arrives late, a correction is discovered a week after
the fact, or an auditor asks why a report from last month no longer reproduces.

## Decision

Store the ledger as an append-only event log, and carry both axes:

- **Recorded time** is the event's position in the log. It exists for free.
- **Effective time** is a field in the payload.

`BalanceQuery` bounds either axis, or both. A correction is a new transaction
recorded now and effective in the past; nothing already recorded is touched.

## Consequences

Bitemporality costs almost nothing on top of event sourcing, which is why the
two were chosen together. Retrofitting a second time axis onto a
single-timestamp schema means rewriting every balance query and backfilling
every row, so it is done from the first commit or not at all.

The read model has to be projected from the log rather than mutated in place,
and balance queries filter on two columns instead of none. Both are paid for by
`entries_balance`, a covering index on exactly those columns.
