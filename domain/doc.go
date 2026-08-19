// Package domain is the centre of the hexagon: the ledger itself, expressed in
// its own terms.
//
// It holds the entities and value objects: money, accounts, transactions,
// events. It also holds the rules that make them a ledger. Postings sum to
// zero. An amount never silently loses a minor unit. Every event hashes the
// one before it. A balance can be read along both time axes.
//
// It imports nothing outside the standard library and nothing else in this
// module. No context, no database, no transport. That is not a stylistic
// preference. It is what makes these rules testable in isolation. It also
// keeps whatever stores them from reinterpreting them quietly.
package domain
