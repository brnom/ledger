package domain

import "errors"

// Sentinel errors returned by the ledger. Callers should test with
// [errors.Is]; every error produced by this package wraps one of these so the
// HTTP layer can map a failure to a status code without type switching.
var (
	// ErrInvalidCurrency reports a malformed or unknown currency.
	ErrInvalidCurrency = errors.New("ledger: invalid currency")

	// ErrInvalidAmount reports a monetary value that could not be parsed.
	ErrInvalidAmount = errors.New("ledger: invalid amount")

	// ErrCurrencyMismatch reports arithmetic between two different currencies.
	ErrCurrencyMismatch = errors.New("ledger: currency mismatch")

	// ErrOverflow reports a monetary value outside the representable range.
	// See [Amount] for the limits.
	ErrOverflow = errors.New("ledger: amount overflow")
)

// Domain errors.
var (
	// ErrInvalidID reports a malformed identifier.
	ErrInvalidID = errors.New("ledger: invalid id")

	// ErrInvalidAccount reports a malformed account name or definition.
	ErrInvalidAccount = errors.New("ledger: invalid account")

	// ErrInvalidTransaction reports a transaction that violates double-entry
	// rules, such as postings that do not sum to zero.
	ErrInvalidTransaction = errors.New("ledger: invalid transaction")

	// ErrAccountNotFound reports a posting against an account that was never
	// opened.
	ErrAccountNotFound = errors.New("ledger: account not found")

	// ErrAccountExists reports opening an account name that is already in use.
	ErrAccountExists = errors.New("ledger: account already exists")

	// ErrInsufficientFunds reports a posting that would push an account past
	// zero in its normal direction when it does not allow that.
	ErrInsufficientFunds = errors.New("ledger: insufficient funds")
)

// Event and storage errors.
var (
	// ErrEncoding reports a payload that cannot be encoded canonically.
	ErrEncoding = errors.New("ledger: encoding")

	// ErrUnknownEvent reports an event whose type or payload this build does
	// not understand.
	ErrUnknownEvent = errors.New("ledger: unknown event")

	// ErrChainBroken reports that the event chain failed verification: a hash
	// does not match, a link is wrong, or a sequence number is missing.
	ErrChainBroken = errors.New("ledger: event chain broken")
)

// Command errors.
var (
	// ErrIdempotencyConflict reports an idempotency key reused with a
	// different command. The first outcome stands; the ledger will not guess
	// which of the two the caller meant.
	ErrIdempotencyConflict = errors.New("ledger: idempotency key reused with a different request")

	// ErrTransactionNotFound reports a reference to a transaction the ledger
	// has never recorded.
	ErrTransactionNotFound = errors.New("ledger: transaction not found")

	// ErrTransactionExists reports committing a transaction id already in the
	// book.
	ErrTransactionExists = errors.New("ledger: transaction already exists")

	// ErrAlreadyReverted reports a second attempt to revert a transaction.
	ErrAlreadyReverted = errors.New("ledger: transaction already reverted")

	// ErrEffectiveOutOfRange reports an effective time outside the window the
	// ledger accepts.
	ErrEffectiveOutOfRange = errors.New("ledger: effective time out of range")

	// ErrConflict reports that another writer advanced the stream first and
	// the operation should be retried.
	ErrConflict = errors.New("ledger: concurrent write conflict")
)
