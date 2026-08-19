// Package app is the application layer: the ledger's use cases and the driven
// ports they need.
//
// It orchestrates the domain. It resolves a command's identity, validates the
// command against ledger state, and stages the event the command produces. It
// also names what it needs from the outside world without knowing who provides
// it. [Store] and [Writer] are declared here, in the consumer, so the
// dependency points inward. An adapter imports this package in order to
// satisfy them. This package never imports an adapter.
package app
