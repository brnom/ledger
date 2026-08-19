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
// negative amount is a credit. Keeping one signed number rather than a
// magnitude plus a direction flag means a transaction balances exactly when
// its postings sum to zero, which is a single addition rather than a case
// analysis.
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
		// Excluded so that negating a posting -- which reversal does to every
		// leg -- can never overflow.
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

	// EffectiveAt is when the transaction takes effect in business time --
	// when the money is considered to have moved. It is independent of when
	// the ledger recorded it, which is what makes backdated corrections and
	// forward-dated settlement possible without rewriting history.
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
// It checks shape only; whether the accounts exist and have room for the
// postings is the engine's business, since that needs the ledger's state.
func (t Transaction) Validate() error {
	switch {
	case t.ID.IsZero():
		return fmt.Errorf("%w: transaction has no id", ErrInvalidTransaction)
	case t.EffectiveAt.IsZero():
		return fmt.Errorf("%w: transaction %s has no effective time", ErrInvalidTransaction, t.ID)
	case len(t.Postings) < 2:
		return fmt.Errorf("%w: transaction %s has %d postings, need at least 2",
			ErrInvalidTransaction, t.ID, len(t.Postings))
	case len(t.Postings) > MaxPostings:
		return fmt.Errorf("%w: transaction %s has %d postings, max %d",
			ErrInvalidTransaction, t.ID, len(t.Postings), MaxPostings)
	case len(t.Reference) > MaxReferenceLen:
		return fmt.Errorf("%w: reference is %d bytes, max %d",
			ErrInvalidTransaction, len(t.Reference), MaxReferenceLen)
	}
	for i, p := range t.Postings {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("posting %d: %w", i, err)
		}
	}
	if err := validateMetadata(t.Metadata); err != nil {
		return err
	}

	sums, err := t.Sums()
	if err != nil {
		return err
	}
	// Each currency balances on its own. A transaction that moves BRL and USD
	// must balance both, which forces an FX position account to be named
	// rather than letting an implicit rate hide inside the entry.
	for _, cur := range sortedCurrencies(sums) {
		if sum := sums[cur]; !sum.IsZero() {
			return fmt.Errorf("%w: transaction %s does not balance in %s, off by %s",
				ErrInvalidTransaction, t.ID, cur, sum)
		}
	}
	return nil
}

// Sums totals the postings per currency. A balanced transaction sums to zero
// in every currency it touches.
func (t Transaction) Sums() (map[Currency]Amount, error) {
	sums := make(map[Currency]Amount)
	for i, p := range t.Postings {
		cur := p.Amount.Currency()
		running, ok := sums[cur]
		if !ok {
			running = Zero(cur)
		}
		next, err := running.Add(p.Amount)
		if err != nil {
			return nil, fmt.Errorf("%w: transaction %s overflows in %s at posting %d",
				ErrOverflow, t.ID, cur, i)
		}
		sums[cur] = next
	}
	return sums, nil
}

// Currencies returns the currencies the transaction touches, in a stable
// order.
func (t Transaction) Currencies() []Currency {
	seen := make(map[Currency]struct{}, len(t.Postings))
	for _, p := range t.Postings {
		seen[p.Amount.Currency()] = struct{}{}
	}
	return sortedCurrencies(seen)
}

// Accounts returns the distinct accounts the transaction touches, sorted.
func (t Transaction) Accounts() []AccountName {
	seen := make(map[AccountName]struct{}, len(t.Postings))
	for _, p := range t.Postings {
		seen[p.Account] = struct{}{}
	}
	out := make([]AccountName, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Reverse returns the transaction that undoes t: the same legs with their
// signs flipped, effective at the given time.
//
// A reversal is a new transaction, never an edit. The original stays in the
// book exactly as it was recorded, which is the difference between a ledger
// that can be audited and one that can only be trusted.
func (t Transaction) Reverse(id ID, effectiveAt time.Time) Transaction {
	postings := make([]Posting, len(t.Postings))
	for i, p := range t.Postings {
		postings[i] = Posting{
			Account: p.Account,
			Amount:  FromMinor(p.Amount.Currency(), -p.Amount.Minor()),
		}
	}
	return Transaction{
		ID:          id,
		EffectiveAt: effectiveAt,
		Postings:    postings,
		Reference:   t.Reference,
	}
}

func sortedCurrencies[V any](m map[Currency]V) []Currency {
	out := make([]Currency, 0, len(m))
	for cur := range m {
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
