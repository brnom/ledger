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
func Project(e Event) (Projection, error) {
	if e.Seq <= 0 {
		return Projection{}, fmt.Errorf("%w: event %s has no sequence number", ErrUnknownEvent, e.ID)
	}
	payload, err := e.DecodePayload()
	if err != nil {
		return Projection{}, err
	}

	switch p := payload.(type) {
	case AccountOpened:
		acct, err := p.Account()
		if err != nil {
			return Projection{}, err
		}
		acct.OpenedSeq = e.Seq
		return Projection{Account: &acct}, nil

	case TransactionCommitted:
		tx, err := p.Transaction()
		if err != nil {
			return Projection{}, err
		}
		return projectTransaction(e, tx, ID{}), nil

	case TransactionReverted:
		tx, err := p.Transaction()
		if err != nil {
			return Projection{}, err
		}
		return projectTransaction(e, tx, p.RevertsID), nil

	default:
		return Projection{}, fmt.Errorf("%w: no projection for %q", ErrUnknownEvent, e.Type)
	}
}

func projectTransaction(e Event, tx Transaction, reverts ID) Projection {
	entries := make([]Entry, len(tx.Postings))
	for i, p := range tx.Postings {
		entries[i] = Entry{
			Seq:         e.Seq,
			Index:       i,
			Account:     p.Account,
			Amount:      p.Amount,
			TxID:        tx.ID,
			Reference:   tx.Reference,
			EffectiveAt: tx.EffectiveAt,
			RecordedAt:  e.RecordedAt,
			Reverts:     reverts,
		}
	}
	return Projection{
		Transaction: &RecordedTransaction{
			Transaction: tx,
			Seq:         e.Seq,
			RecordedAt:  e.RecordedAt,
			Reverts:     reverts,
		},
		Entries: entries,
	}
}
