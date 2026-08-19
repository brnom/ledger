# 6. Money as int64 minor units

Status: accepted

## Context

Floating point cannot represent 0.10 exactly, so it cannot represent money.
A bare integer of minor units can, but says nothing about which currency it is
in — and adding BRL to USD compiles fine.

## Decision

`Amount` is an `int64` count of minor units carrying its `Currency`, which
carries its own scale. Arithmetic across currencies returns
`ErrCurrencyMismatch`; arithmetic that would leave the int64 range returns
`ErrOverflow` rather than wrapping.

Amounts cross the wire as decimal strings with the currency and scale beside
them, never as JSON numbers.

Parsing rejects more decimal places than the currency has, instead of rounding.
Deciding how to round is the caller's business; silently dropping a digit is
how money goes missing.

`Allocate` splits an amount by ratios using the largest-remainder method, so a
payment split or fee breakdown sums back to the original exactly.

## Consequences

The representable range is roughly ±92 quadrillion for a two-decimal currency,
which is ample, and about ±9.2 units at 18 decimals, which is not. Assets that
need that precision are deliberately absent from the currency table; supporting
them means changing the representation, and the type exists to keep that a
contained change.
