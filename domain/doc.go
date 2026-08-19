// Package domain is the centre of the hexagon: the ledger itself, expressed in
// its own terms.
//
// It holds the entities and value objects -- money, accounts, transactions,
// events -- and the rules that make them a ledger: that postings sum to zero,
// that an amount never silently loses a minor unit, that every event hashes the
// one before it, and that a balance can be read along both time axes.
//
// It imports nothing outside the standard library and nothing else in this
// module. No context, no database, no transport. That is not a stylistic
// preference: it is what makes these rules testable in isolation and what keeps
// them from being quietly reinterpreted by whatever happens to store them.
package domain
