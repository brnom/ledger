package domain

import "fmt"

// Projection is what one event contributes to the read model.
//
// Every store applies events through this function, so the in-memory store and
// the PostgreSQL store cannot drift apart in how they interpret the log -- and
// rebuilding the read model from scratch is a fold of Project over the events,
// not a second implementation that has to be kept in step.
type Projection struct {
	// Account is set by an account.opened event.
	Account *Account

	// Transaction and Entries are set by the transaction events.
	Transaction *RecordedTransaction
	Entries     []Entry
}

// Project derives the read-model changes an event produces. The event must
// already be sealed, since entries carry its sequence number.
func Project(event Event) (Projection, error) {
	if event.Seq <= 0 {
		return Projection{}, fmt.Errorf("%w: event %s has no sequence number", ErrUnknownEvent, event.ID)
	}
	payload, err := event.DecodePayload()
	if err != nil {
		return Projection{}, err
	}

	switch decoded := payload.(type) {
	case AccountOpened:
		acct, err := decoded.Account()
		if err != nil {
			return Projection{}, err
		}
		acct.OpenedSeq = event.Seq
		return Projection{Account: &acct}, nil

	case TransactionCommitted:
		tx, err := decoded.Transaction()
		if err != nil {
			return Projection{}, err
		}
		return projectTransaction(event, tx, ID{}), nil

	case TransactionReverted:
		tx, err := decoded.Transaction()
		if err != nil {
			return Projection{}, err
		}
		return projectTransaction(event, tx, decoded.RevertsID), nil

	default:
		return Projection{}, fmt.Errorf("%w: no projection for %q", ErrUnknownEvent, event.Type)
	}
}

func projectTransaction(event Event, tx Transaction, reverts ID) Projection {
	entries := make([]Entry, len(tx.Postings))
	for i, posting := range tx.Postings {
		entries[i] = Entry{
			Seq:         event.Seq,
			Index:       i,
			Account:     posting.Account,
			Amount:      posting.Amount,
			TxID:        tx.ID,
			Reference:   tx.Reference,
			EffectiveAt: tx.EffectiveAt,
			RecordedAt:  event.RecordedAt,
			Reverts:     reverts,
		}
	}
	return Projection{
		Transaction: &RecordedTransaction{
			Transaction: tx,
			Seq:         event.Seq,
			RecordedAt:  event.RecordedAt,
			Reverts:     reverts,
		},
		Entries: entries,
	}
}
