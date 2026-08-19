package domain

import "time"

// Entry is one posting as recorded in the read model: the flattened, queryable
// form of a leg of a committed transaction.
//
// Entries are what balance queries scan. They are derived from events and can
// always be rebuilt from them. That is what makes a replay a safe operation
// rather than a leap of faith.
type Entry struct {
	// Seq is the event that recorded this entry. It is the entry's position on
	// the recorded-time axis.
	Seq int64
	// Index is the posting's position within its transaction, so the original
	// order of legs survives into the read model.
	Index int

	Account AccountName
	Amount  Amount

	TxID      ID
	Reference string

	// EffectiveAt is business time: when the movement counts.
	EffectiveAt time.Time
	// RecordedAt is system time: when the ledger learned of it.
	RecordedAt time.Time

	// Reverts is set when this entry belongs to a reversal, naming the
	// transaction being undone.
	Reverts ID
}

// BalanceQuery selects a balance along both time axes. The zero value of each
// bound means "unbounded", so an empty query returns the balance of everything
// recorded so far.
type BalanceQuery struct {
	Account AccountName

	// AsOfEffective counts only entries effective at or before this instant. It
	// answers "what is the balance on this date".
	AsOfEffective time.Time

	// AsOfRecorded counts only entries recorded at or before this instant, and
	// AsOfSeq only those at or before that event. They answer "what did we
	// believe at that moment", which is the question an auditor asks and a
	// single-axis ledger cannot answer.
	//
	// AsOfSeq takes precedence when both are set: sequence numbers are exact,
	// while several events can share a recorded timestamp.
	AsOfRecorded time.Time
	AsOfSeq      int64
}

// EntryQuery selects entries for listing. Bounds follow the same convention as
// [BalanceQuery]: the zero value of a field means unbounded.
type EntryQuery struct {
	// Account matches one account exactly. AccountPrefix matches a whole subtree
	// of the account hierarchy. Setting both is an error.
	Account       AccountName
	AccountPrefix AccountName

	TxID ID

	EffectiveFrom, EffectiveTo time.Time
	RecordedFrom, RecordedTo   time.Time
	FromSeq, ToSeq             int64

	// Limit caps the number of entries returned. Zero means the store's default.
	// Entries come back ordered by (Seq, Index).
	Limit int
	// AfterSeq and AfterIndex continue a previous page.
	AfterSeq   int64
	AfterIndex int
}

// IdempotencyRecord remembers what a caller's idempotency key produced, so a
// retry can be answered with the original outcome instead of writing twice.
type IdempotencyRecord struct {
	Key string

	// RequestHash fingerprints the command the key was first used with. A replay
	// of the key with a different command is a caller bug, such as two different
	// payments that share a key. The ledger reports it. It does not answer with
	// the first result.
	RequestHash [32]byte

	Seq        int64
	TxID       ID
	RecordedAt time.Time
}

// Head is the current end of a ledger's event stream.
type Head struct {
	Seq  int64
	Hash [32]byte
}

// RecordedTransaction is a transaction as the ledger holds it, together with
// where it sits in the stream and how it relates to any reversal.
type RecordedTransaction struct {
	Transaction

	// Seq is the event that committed it.
	Seq        int64
	RecordedAt time.Time

	// Reverts names the transaction this one undoes, if it is a reversal.
	Reverts ID
	// RevertedBy names the transaction that undid this one, if any. A transaction
	// can be reverted at most once. A second correction has to be a new
	// transaction, so the audit trail stays a chain rather than a set.
	RevertedBy ID
}
