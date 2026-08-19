package app

import (
	"time"

	"github.com/brnom/ledger/domain"
)

// Result reports the outcome of a command.
type Result struct {
	// Seq is the event that recorded the command.
	Seq int64
	// EventID identifies that event.
	EventID domain.ID
	// TransactionID is set by the transaction commands.
	TransactionID domain.ID
	// RecordedAt is when the ledger recorded it.
	RecordedAt time.Time

	// Replayed is true when an idempotency key matched an earlier command and
	// this result is that command's, not a new one. Nothing was written.
	Replayed bool
}

// OpenAccountCommand opens an account.
type OpenAccountCommand struct {
	Name          domain.AccountName
	Currency      domain.Currency
	Normal        domain.Normal
	AllowNegative bool
	Metadata      map[string]string

	// EffectiveAt is when the account begins to exist in business time. No
	// entry may be dated before it. Zero means now.
	EffectiveAt time.Time

	IdempotencyKey string
}

// CommitCommand records a balanced transaction.
type CommitCommand struct {
	// ID may be supplied by the caller to make the transaction's identity part
	// of the request. Zero means the ledger generates one.
	ID domain.ID

	// EffectiveAt is when the money is considered to have moved. Zero means
	// now.
	EffectiveAt time.Time

	Postings  []domain.Posting
	Reference string
	Metadata  map[string]string

	IdempotencyKey string
}

// RevertCommand undoes a previously committed transaction by recording its
// mirror image. The original is untouched.
type RevertCommand struct {
	TransactionID domain.ID

	// EffectiveAt is when the reversal takes effect. Zero means now, which
	// leaves the original's effect standing for the period between the two --
	// the honest record of a correction discovered late. Set it to the
	// original's effective time to erase the effect entirely.
	EffectiveAt time.Time

	Reason string

	IdempotencyKey string
}
