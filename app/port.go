package app

import (
	"context"

	"github.com/brnom/ledger/domain"
)

// Store persists a ledger's events and the read model projected from them.
//
// Writes go through [Store.Update], which gives the caller a [Writer] holding
// the ledger's single-writer lock. Everything the engine needs in order to
// validate a command is readable through that Writer. Validation and append
// therefore happen in one transaction, and no other writer can come between
// them.
type Store interface {
	// Update runs fn as the sole writer of the given ledger. Events staged
	// through the Writer are committed atomically with their projections when fn
	// returns nil, and discarded entirely when it returns an error.
	Update(ctx context.Context, ledgerID string, fn func(context.Context, Writer) error) error

	// Head returns the end of the stream.
	Head(ctx context.Context, ledgerID string) (domain.Head, error)

	// Account returns one account, or [domain.ErrAccountNotFound].
	Account(ctx context.Context, ledgerID string, name domain.AccountName) (domain.Account, error)

	// Accounts lists accounts under a prefix. An empty prefix lists all of them.
	Accounts(ctx context.Context, ledgerID string, prefix domain.AccountName) ([]domain.Account, error)

	// Balance sums the entries a query selects. The result is signed and
	// debit-positive. Use [domain.Account.Presented] to show it the account's
	// way.
	Balance(ctx context.Context, ledgerID string, query domain.BalanceQuery) (domain.Amount, error)

	// Entries lists entries in (Seq, Index) order.
	Entries(ctx context.Context, ledgerID string, query domain.EntryQuery) ([]domain.Entry, error)

	// Transaction returns one transaction, or [domain.ErrTransactionNotFound].
	Transaction(ctx context.Context, ledgerID string, id domain.ID) (domain.RecordedTransaction, error)

	// Events reads the raw log from fromSeq, for chain verification and replay.
	Events(ctx context.Context, ledgerID string, fromSeq int64, limit int) ([]domain.Event, error)

	// Close releases resources held by the store.
	Close() error
}

// Writer is the transactional handle a command uses to inspect the ledger and
// append to it. Its reads see the ledger as of the moment the writer took the
// lock, plus anything staged through it.
type Writer interface {
	// LedgerID names the stream being written.
	LedgerID() string

	// Head returns the end of the stream, advancing as events are staged.
	Head() domain.Head

	// Account returns an account and whether it exists.
	Account(name domain.AccountName) (domain.Account, bool, error)

	// Balance returns an account's current signed balance across everything
	// recorded, which is what an availability check is measured against.
	Balance(name domain.AccountName) (domain.Amount, error)

	// Transaction returns a previously committed transaction and whether it
	// exists, including whether it has already been reverted.
	Transaction(id domain.ID) (domain.RecordedTransaction, bool, error)

	// Idempotency returns what a key produced before, if anything.
	Idempotency(key string) (domain.IdempotencyRecord, bool, error)

	// Stage seals an event onto the end of the stream and records the entries it
	// projects. Nothing is durable until Update's callback returns nil.
	Stage(event *domain.Event) error

	// StageIdempotency records the outcome of a keyed command.
	StageIdempotency(rec domain.IdempotencyRecord) error
}
