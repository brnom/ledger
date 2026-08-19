// Package app is the application layer: the ledger's use cases and the driven
// ports they need.
//
// It orchestrates the domain -- resolving a command's identity, validating it
// against ledger state, staging the event it produces -- and it names what it
// needs from the outside world without knowing who provides it. [Store] and
// [Writer] are declared here, in the consumer, so the dependency points inward:
// an adapter imports this package in order to satisfy them, and this package
// never imports an adapter.
package app
