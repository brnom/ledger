package domain

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Limits on a single transaction.
const (
	MaxPostings     = 1000
	MaxReferenceLen = 255
)

// Posting is one leg of a transaction: a signed movement against an account.
//
// The sign carries the accounting direction. A positive amount is a debit, a
// negative amount is a credit. One signed number, rather than a magnitude plus
// a direction flag, means a transaction balances exactly when its postings sum
// to zero. That is a single addition rather than a case analysis.
type Posting struct {
	Account AccountName
	Amount  Amount
}

// Dr returns a debit posting for the given magnitude.
func Dr(account AccountName, amount Amount) Posting {
	return Posting{Account: account, Amount: amount}
}

// Cr returns a credit posting for the given magnitude.
func Cr(account AccountName, amount Amount) Posting {
	// Safe for every amount [Posting.Validate] accepts, which excludes
	// math.MinInt64 precisely because its magnitude is not representable.
	return Posting{Account: account, Amount: FromMinor(amount.Currency(), -amount.Minor())}
}

// IsDebit reports whether the posting moves value onto the debit side.
func (p Posting) IsDebit() bool { return p.Amount.Sign() > 0 }

// Validate reports whether the posting is well formed.
func (p Posting) Validate() error {
	if err := p.Account.Validate(); err != nil {
		return err
	}
	if err := p.Amount.Currency().Validate(); err != nil {
		return err
	}
	if p.Amount.IsZero() {
		return fmt.Errorf("%w: posting to %q has a zero amount", ErrInvalidTransaction, p.Account)
	}
	if p.Amount.Minor() == math.MinInt64 {
		// Excluded so that negating a posting -- which reversal does to every leg --
		// can never overflow.
		return fmt.Errorf("%w: posting to %q is not reversible at %s",
			ErrInvalidTransaction, p.Account, p.Amount)
	}
	return nil
}

func (p Posting) String() string {
	if p.IsDebit() {
		return fmt.Sprintf("Dr %s %s", p.Account, p.Amount)
	}
	neg, _ := p.Amount.Neg()
	return fmt.Sprintf("Cr %s %s", p.Account, neg)
}

// Transaction is an atomic, balanced set of postings. It is the only way value
// moves in the ledger, and it either lands whole or not at all.
type Transaction struct {
	// ID identifies the transaction. It is time-ordered, so transactions sort
	// naturally by when they were created.
	ID ID

	// EffectiveAt is when the transaction takes effect in business time -- when
	// the money is considered to have moved. It is independent of when the ledger
	// recorded it, which is what makes backdated corrections and forward-dated
	// settlement possible without rewriting history.
	EffectiveAt time.Time

	Postings []Posting

	// Reference is a caller-supplied external identifier: an acquirer's
	// transaction id, a Pix end-to-end id, an invoice number.
	Reference string

	Metadata map[string]string
}

// NewTransfer builds the common two-legged transaction: value leaves one
// account and lands in another.
func NewTransfer(from, to AccountName, amount Amount, effectiveAt time.Time) Transaction {
	return Transaction{
		ID:          NewID(),
		EffectiveAt: effectiveAt,
		Postings:    []Posting{Cr(from, amount), Dr(to, amount)},
	}
}

// Validate enforces the double-entry rules that make a transaction admissible.
// It checks shape only. Whether the accounts exist and have room for the
// postings is the engine's business, because that needs the ledger's state.
func (tx Transaction) Validate() error {
	switch {
	case tx.ID.IsZero():
		return fmt.Errorf("%w: transaction has no id", ErrInvalidTransaction)
	case tx.EffectiveAt.IsZero():
		return fmt.Errorf("%w: transaction %s has no effective time", ErrInvalidTransaction, tx.ID)
	case len(tx.Postings) < 2:
		return fmt.Errorf("%w: transaction %s has %d postings, need at least 2",
			ErrInvalidTransaction, tx.ID, len(tx.Postings))
	case len(tx.Postings) > MaxPostings:
		return fmt.Errorf("%w: transaction %s has %d postings, max %d",
			ErrInvalidTransaction, tx.ID, len(tx.Postings), MaxPostings)
	case len(tx.Reference) > MaxReferenceLen:
		return fmt.Errorf("%w: reference is %d bytes, max %d",
			ErrInvalidTransaction, len(tx.Reference), MaxReferenceLen)
	}
	for i, posting := range tx.Postings {
		if err := posting.Validate(); err != nil {
			return fmt.Errorf("posting %d: %w", i, err)
		}
	}
	if err := validateMetadata(tx.Metadata); err != nil {
		return err
	}

	sums, err := tx.Sums()
	if err != nil {
		return err
	}
	// Each currency balances on its own. A transaction that moves BRL and USD
	// must balance both. That forces the caller to name an FX position account
	// rather than hide an implicit rate inside the entry.
	for _, cur := range sortedCurrencies(sums) {
		if sum := sums[cur]; !sum.IsZero() {
			return fmt.Errorf("%w: transaction %s does not balance in %s, off by %s",
				ErrInvalidTransaction, tx.ID, cur, sum)
		}
	}
	return nil
}

// Sums totals the postings per currency. A balanced transaction sums to zero
// in every currency it touches.
func (tx Transaction) Sums() (map[Currency]Amount, error) {
	sums := make(map[Currency]Amount)
	for i, posting := range tx.Postings {
		cur := posting.Amount.Currency()
		running, ok := sums[cur]
		if !ok {
			running = Zero(cur)
		}
		next, err := running.Add(posting.Amount)
		if err != nil {
			return nil, fmt.Errorf("%w: transaction %s overflows in %s at posting %d",
				ErrOverflow, tx.ID, cur, i)
		}
		sums[cur] = next
	}
	return sums, nil
}

// Currencies returns the currencies the transaction touches, in a stable
// order.
func (tx Transaction) Currencies() []Currency {
	seen := make(map[Currency]struct{}, len(tx.Postings))
	for _, posting := range tx.Postings {
		seen[posting.Amount.Currency()] = struct{}{}
	}
	return sortedCurrencies(seen)
}

// Accounts returns the distinct accounts the transaction touches, sorted.
func (tx Transaction) Accounts() []AccountName {
	seen := make(map[AccountName]struct{}, len(tx.Postings))
	for _, posting := range tx.Postings {
		seen[posting.Account] = struct{}{}
	}
	out := make([]AccountName, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Reverse returns the transaction that undoes tx: the same legs with their
// signs flipped, effective at the given time.
//
// A reversal is a new transaction, never an edit. The original stays in the
// book exactly as it was recorded. That is the difference between a ledger you
// can audit and one you can only trust.
func (tx Transaction) Reverse(id ID, effectiveAt time.Time) Transaction {
	postings := make([]Posting, len(tx.Postings))
	for i, posting := range tx.Postings {
		postings[i] = Posting{
			Account: posting.Account,
			Amount:  FromMinor(posting.Amount.Currency(), -posting.Amount.Minor()),
		}
	}
	return Transaction{
		ID:          id,
		EffectiveAt: effectiveAt,
		Postings:    postings,
		Reference:   tx.Reference,
	}
}

func sortedCurrencies[V any](byCurrency map[Currency]V) []Currency {
	out := make([]Currency, 0, len(byCurrency))
	for cur := range byCurrency {
		out = append(out, cur)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Scale < out[j].Scale
	})
	return out
}
