// Package memstore keeps a ledger entirely in memory.
//
// It is the reference implementation of [app.Store]: small enough to read
// in one sitting, and the yardstick the PostgreSQL store is tested against. It
// is also what makes the domain testable without a database, so the rules that
// matter can be exercised at the speed of a unit test.
//
// It is safe for concurrent use and durable for exactly as long as the process
// lives.
package memstore

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// DefaultEntryLimit caps a page of entries when a query does not say.
const DefaultEntryLimit = 1000

// Store is an in-memory [app.Store]. The zero value is not usable; call
// [New].
type Store struct {
	mu    sync.Mutex
	books map[string]*book
}

// New returns an empty store.
func New() *Store {
	return &Store{books: make(map[string]*book)}
}

// book is one ledger's state. Its mutex is the single-writer lock: the
// PostgreSQL store takes an advisory lock for the same reason, so that a
// command reads state and appends against it with nothing in between.
type book struct {
	mu sync.RWMutex

	events  []domain.Event
	entries []domain.Entry

	accounts map[domain.AccountName]domain.Account
	txs      map[domain.ID]domain.RecordedTransaction
	idem     map[string]domain.IdempotencyRecord
	balances map[domain.AccountName]domain.Amount

	head domain.Head
}

func newBook() *book {
	return &book{
		accounts: make(map[domain.AccountName]domain.Account),
		txs:      make(map[domain.ID]domain.RecordedTransaction),
		idem:     make(map[string]domain.IdempotencyRecord),
		balances: make(map[domain.AccountName]domain.Amount),
		head:     domain.Head{Hash: domain.GenesisHash},
	}
}

func (s *Store) book(ledgerID string) *book {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.books[ledgerID]
	if !ok {
		b = newBook()
		s.books[ledgerID] = b
	}
	return b
}

// Update implements [app.Store]. It holds the ledger's write lock for the
// whole callback, so validation and append are one indivisible step.
func (s *Store) Update(ctx context.Context, ledgerID string, fn func(context.Context, app.Writer) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b := s.book(ledgerID)
	b.mu.Lock()
	defer b.mu.Unlock()

	w := newWriter(ledgerID, b)
	if err := fn(ctx, w); err != nil {
		return err // everything staged is dropped with the writer
	}
	w.commit()
	return nil
}

// Head implements [app.Store].
func (s *Store) Head(ctx context.Context, ledgerID string) (domain.Head, error) {
	b := s.book(ledgerID)
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.head, nil
}

// Account implements [app.Store].
func (s *Store) Account(ctx context.Context, ledgerID string, name domain.AccountName) (domain.Account, error) {
	b := s.book(ledgerID)
	b.mu.RLock()
	defer b.mu.RUnlock()
	acct, ok := b.accounts[name]
	if !ok {
		return domain.Account{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, name)
	}
	return copyAccount(acct), nil
}

// Accounts implements [app.Store].
func (s *Store) Accounts(ctx context.Context, ledgerID string, prefix domain.AccountName) ([]domain.Account, error) {
	b := s.book(ledgerID)
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]domain.Account, 0, len(b.accounts))
	for name, acct := range b.accounts {
		if prefix != "" && !name.HasPrefix(prefix) {
			continue
		}
		out = append(out, copyAccount(acct))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Transaction implements [app.Store].
func (s *Store) Transaction(ctx context.Context, ledgerID string, id domain.ID) (domain.RecordedTransaction, error) {
	b := s.book(ledgerID)
	b.mu.RLock()
	defer b.mu.RUnlock()
	tx, ok := b.txs[id]
	if !ok {
		return domain.RecordedTransaction{}, fmt.Errorf("%w: %s", domain.ErrTransactionNotFound, id)
	}
	return tx, nil
}

// Balance implements [app.Store]. It sums the entries the query selects
// along both time axes.
func (s *Store) Balance(ctx context.Context, ledgerID string, q domain.BalanceQuery) (domain.Amount, error) {
	b := s.book(ledgerID)
	b.mu.RLock()
	defer b.mu.RUnlock()

	acct, ok := b.accounts[q.Account]
	if !ok {
		return domain.Amount{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, q.Account)
	}

	sum := domain.Zero(acct.Currency)
	for _, e := range b.entries {
		if e.Account != q.Account || !matchesBalanceQuery(e, q) {
			continue
		}
		next, err := sum.Add(e.Amount)
		if err != nil {
			return domain.Amount{}, err
		}
		sum = next
	}
	return sum, nil
}

func matchesBalanceQuery(e domain.Entry, q domain.BalanceQuery) bool {
	if !q.AsOfEffective.IsZero() && e.EffectiveAt.After(q.AsOfEffective) {
		return false
	}
	// A sequence bound is exact, so it wins over a timestamp bound: several
	// events can share a recorded time, but only one can have a given Seq.
	if q.AsOfSeq > 0 {
		return e.Seq <= q.AsOfSeq
	}
	if !q.AsOfRecorded.IsZero() && e.RecordedAt.After(q.AsOfRecorded) {
		return false
	}
	return true
}

// Entries implements [app.Store].
func (s *Store) Entries(ctx context.Context, ledgerID string, q domain.EntryQuery) ([]domain.Entry, error) {
	if q.Account != "" && q.AccountPrefix != "" {
		return nil, fmt.Errorf("%w: set Account or AccountPrefix, not both", domain.ErrInvalidAccount)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultEntryLimit
	}

	b := s.book(ledgerID)
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]domain.Entry, 0, min(limit, len(b.entries)))
	for _, e := range b.entries {
		if !matchesEntryQuery(e, q) {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func matchesEntryQuery(e domain.Entry, q domain.EntryQuery) bool {
	switch {
	case q.Account != "" && e.Account != q.Account:
		return false
	case q.AccountPrefix != "" && !e.Account.HasPrefix(q.AccountPrefix):
		return false
	case !q.TxID.IsZero() && e.TxID != q.TxID:
		return false
	case !q.EffectiveFrom.IsZero() && e.EffectiveAt.Before(q.EffectiveFrom):
		return false
	case !q.EffectiveTo.IsZero() && e.EffectiveAt.After(q.EffectiveTo):
		return false
	case !q.RecordedFrom.IsZero() && e.RecordedAt.Before(q.RecordedFrom):
		return false
	case !q.RecordedTo.IsZero() && e.RecordedAt.After(q.RecordedTo):
		return false
	case q.FromSeq > 0 && e.Seq < q.FromSeq:
		return false
	case q.ToSeq > 0 && e.Seq > q.ToSeq:
		return false
	case q.AfterSeq > 0 && (e.Seq < q.AfterSeq || (e.Seq == q.AfterSeq && e.Index <= q.AfterIndex)):
		return false
	}
	return true
}

// Events implements [app.Store].
func (s *Store) Events(ctx context.Context, ledgerID string, fromSeq int64, limit int) ([]domain.Event, error) {
	if fromSeq < 1 {
		fromSeq = 1
	}
	if limit <= 0 {
		limit = DefaultEntryLimit
	}

	b := s.book(ledgerID)
	b.mu.RLock()
	defer b.mu.RUnlock()

	if fromSeq > int64(len(b.events)) {
		return nil, nil
	}
	end := min(int64(len(b.events)), fromSeq-1+int64(limit))
	// Events are immutable, so handing out a copy of the slice header over
	// shared backing memory is safe as long as callers do not write to it.
	out := make([]domain.Event, end-fromSeq+1)
	copy(out, b.events[fromSeq-1:end])
	return out, nil
}

// Close implements [app.Store].
func (s *Store) Close() error { return nil }

// writer stages a command's effects and applies them only if the command
// succeeds. Reads fall through to the book, so a command sees committed state
// plus whatever it has staged so far.
type writer struct {
	ledgerID string
	book     *book

	head       domain.Head
	events     []domain.Event
	entries    []domain.Entry
	accounts   map[domain.AccountName]domain.Account
	txs        map[domain.ID]domain.RecordedTransaction
	idem       []domain.IdempotencyRecord
	deltas     map[domain.AccountName]domain.Amount
	revertedBy map[domain.ID]domain.ID
}

func newWriter(ledgerID string, b *book) *writer {
	return &writer{
		ledgerID:   ledgerID,
		book:       b,
		head:       b.head,
		accounts:   make(map[domain.AccountName]domain.Account),
		txs:        make(map[domain.ID]domain.RecordedTransaction),
		deltas:     make(map[domain.AccountName]domain.Amount),
		revertedBy: make(map[domain.ID]domain.ID),
	}
}

func (w *writer) LedgerID() string { return w.ledgerID }

func (w *writer) Head() domain.Head { return w.head }

func (w *writer) Account(name domain.AccountName) (domain.Account, bool, error) {
	if acct, ok := w.accounts[name]; ok {
		return copyAccount(acct), true, nil
	}
	acct, ok := w.book.accounts[name]
	return copyAccount(acct), ok, nil
}

func (w *writer) Balance(name domain.AccountName) (domain.Amount, error) {
	acct, ok, err := w.Account(name)
	if err != nil {
		return domain.Amount{}, err
	}
	if !ok {
		return domain.Amount{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, name)
	}

	sum, ok := w.book.balances[name]
	if !ok {
		sum = domain.Zero(acct.Currency)
	}
	if delta, ok := w.deltas[name]; ok {
		return sum.Add(delta)
	}
	return sum, nil
}

func (w *writer) Transaction(id domain.ID) (domain.RecordedTransaction, bool, error) {
	tx, ok := w.txs[id]
	if !ok {
		tx, ok = w.book.txs[id]
	}
	if ok {
		if by, reverted := w.revertedBy[id]; reverted {
			tx.RevertedBy = by
		}
	}
	return tx, ok, nil
}

func (w *writer) Idempotency(key string) (domain.IdempotencyRecord, bool, error) {
	for _, rec := range w.idem {
		if rec.Key == key {
			return rec, true, nil
		}
	}
	rec, ok := w.book.idem[key]
	return rec, ok, nil
}

// Stage seals the event at the end of the stream and folds its projection into
// the writer's overlay.
func (w *writer) Stage(e *domain.Event) error {
	if e.LedgerID != w.ledgerID {
		return fmt.Errorf("%w: event is for ledger %q, writer holds %q",
			domain.ErrInvalidID, e.LedgerID, w.ledgerID)
	}
	e.Seal(w.head.Seq+1, w.head.Hash)

	proj, err := domain.Project(*e)
	if err != nil {
		return err
	}
	if proj.Account != nil {
		w.accounts[proj.Account.Name] = *proj.Account
	}
	if proj.Transaction != nil {
		w.txs[proj.Transaction.ID] = *proj.Transaction
		if !proj.Transaction.Reverts.IsZero() {
			w.revertedBy[proj.Transaction.Reverts] = proj.Transaction.ID
		}
	}
	for _, entry := range proj.Entries {
		running, ok := w.deltas[entry.Account]
		if !ok {
			running = domain.Zero(entry.Amount.Currency())
		}
		next, err := running.Add(entry.Amount)
		if err != nil {
			return err
		}
		w.deltas[entry.Account] = next
	}

	w.events = append(w.events, *e)
	w.entries = append(w.entries, proj.Entries...)
	w.head = domain.Head{Seq: e.Seq, Hash: e.Hash}
	return nil
}

func (w *writer) StageIdempotency(rec domain.IdempotencyRecord) error {
	w.idem = append(w.idem, rec)
	return nil
}

// commit folds the staged overlay into the book. It runs under the book's
// write lock and cannot fail, which is what makes the command atomic.
func (w *writer) commit() {
	b := w.book
	b.events = append(b.events, w.events...)
	b.entries = append(b.entries, w.entries...)
	for name, acct := range w.accounts {
		b.accounts[name] = acct
	}
	for id, tx := range w.txs {
		b.txs[id] = tx
	}
	for original, by := range w.revertedBy {
		if tx, ok := b.txs[original]; ok {
			tx.RevertedBy = by
			b.txs[original] = tx
		}
	}
	for _, rec := range w.idem {
		b.idem[rec.Key] = rec
	}
	for name, delta := range w.deltas {
		current, ok := b.balances[name]
		if !ok {
			current = domain.Zero(delta.Currency())
		}
		// Overflow was already ruled out when the delta was staged against
		// this same balance.
		next, _ := current.Add(delta)
		b.balances[name] = next
	}
	b.head = w.head
}

func copyAccount(a domain.Account) domain.Account {
	if a.Metadata == nil {
		return a
	}
	m := make(map[string]string, len(a.Metadata))
	for k, v := range a.Metadata {
		m[k] = v
	}
	a.Metadata = m
	return a
}

var _ app.Store = (*Store)(nil)
var _ app.Writer = (*writer)(nil)
