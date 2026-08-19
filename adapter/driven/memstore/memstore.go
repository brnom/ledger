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
	bk, ok := s.books[ledgerID]
	if !ok {
		bk = newBook()
		s.books[ledgerID] = bk
	}
	return bk
}

// Update implements [app.Store]. It holds the ledger's write lock for the
// whole callback, so validation and append are one indivisible step.
func (s *Store) Update(ctx context.Context, ledgerID string, fn func(context.Context, app.Writer) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bk := s.book(ledgerID)
	bk.mu.Lock()
	defer bk.mu.Unlock()

	writer := newWriter(ledgerID, bk)
	if err := fn(ctx, writer); err != nil {
		return err // everything staged is dropped with the writer
	}
	writer.commit()
	return nil
}

// Head implements [app.Store].
func (s *Store) Head(ctx context.Context, ledgerID string) (domain.Head, error) {
	bk := s.book(ledgerID)
	bk.mu.RLock()
	defer bk.mu.RUnlock()
	return bk.head, nil
}

// Account implements [app.Store].
func (s *Store) Account(ctx context.Context, ledgerID string, name domain.AccountName) (domain.Account, error) {
	bk := s.book(ledgerID)
	bk.mu.RLock()
	defer bk.mu.RUnlock()
	acct, ok := bk.accounts[name]
	if !ok {
		return domain.Account{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, name)
	}
	return copyAccount(acct), nil
}

// Accounts implements [app.Store].
func (s *Store) Accounts(ctx context.Context, ledgerID string, prefix domain.AccountName) ([]domain.Account, error) {
	bk := s.book(ledgerID)
	bk.mu.RLock()
	defer bk.mu.RUnlock()

	out := make([]domain.Account, 0, len(bk.accounts))
	for name, acct := range bk.accounts {
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
	bk := s.book(ledgerID)
	bk.mu.RLock()
	defer bk.mu.RUnlock()
	tx, ok := bk.txs[id]
	if !ok {
		return domain.RecordedTransaction{}, fmt.Errorf("%w: %s", domain.ErrTransactionNotFound, id)
	}
	return tx, nil
}

// Balance implements [app.Store]. It sums the entries the query selects
// along both time axes.
func (s *Store) Balance(ctx context.Context, ledgerID string, query domain.BalanceQuery) (domain.Amount, error) {
	bk := s.book(ledgerID)
	bk.mu.RLock()
	defer bk.mu.RUnlock()

	acct, ok := bk.accounts[query.Account]
	if !ok {
		return domain.Amount{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, query.Account)
	}

	sum := domain.Zero(acct.Currency)
	for _, entry := range bk.entries {
		if entry.Account != query.Account || !matchesBalanceQuery(entry, query) {
			continue
		}
		next, err := sum.Add(entry.Amount)
		if err != nil {
			return domain.Amount{}, err
		}
		sum = next
	}
	return sum, nil
}

func matchesBalanceQuery(entry domain.Entry, query domain.BalanceQuery) bool {
	if !query.AsOfEffective.IsZero() && entry.EffectiveAt.After(query.AsOfEffective) {
		return false
	}
	// A sequence bound is exact, so it wins over a timestamp bound: several
	// events can share a recorded time, but only one can have a given Seq.
	if query.AsOfSeq > 0 {
		return entry.Seq <= query.AsOfSeq
	}
	if !query.AsOfRecorded.IsZero() && entry.RecordedAt.After(query.AsOfRecorded) {
		return false
	}
	return true
}

// Entries implements [app.Store].
func (s *Store) Entries(ctx context.Context, ledgerID string, query domain.EntryQuery) ([]domain.Entry, error) {
	if query.Account != "" && query.AccountPrefix != "" {
		return nil, fmt.Errorf("%w: set Account or AccountPrefix, not both", domain.ErrInvalidAccount)
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultEntryLimit
	}

	bk := s.book(ledgerID)
	bk.mu.RLock()
	defer bk.mu.RUnlock()

	out := make([]domain.Entry, 0, min(limit, len(bk.entries)))
	for _, entry := range bk.entries {
		if !matchesEntryQuery(entry, query) {
			continue
		}
		out = append(out, entry)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func matchesEntryQuery(entry domain.Entry, query domain.EntryQuery) bool {
	switch {
	case query.Account != "" && entry.Account != query.Account:
		return false
	case query.AccountPrefix != "" && !entry.Account.HasPrefix(query.AccountPrefix):
		return false
	case !query.TxID.IsZero() && entry.TxID != query.TxID:
		return false
	case !query.EffectiveFrom.IsZero() && entry.EffectiveAt.Before(query.EffectiveFrom):
		return false
	case !query.EffectiveTo.IsZero() && entry.EffectiveAt.After(query.EffectiveTo):
		return false
	case !query.RecordedFrom.IsZero() && entry.RecordedAt.Before(query.RecordedFrom):
		return false
	case !query.RecordedTo.IsZero() && entry.RecordedAt.After(query.RecordedTo):
		return false
	case query.FromSeq > 0 && entry.Seq < query.FromSeq:
		return false
	case query.ToSeq > 0 && entry.Seq > query.ToSeq:
		return false
	case query.AfterSeq > 0 && (entry.Seq < query.AfterSeq || (entry.Seq == query.AfterSeq && entry.Index <= query.AfterIndex)):
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

	bk := s.book(ledgerID)
	bk.mu.RLock()
	defer bk.mu.RUnlock()

	if fromSeq > int64(len(bk.events)) {
		return nil, nil
	}
	end := min(int64(len(bk.events)), fromSeq-1+int64(limit))
	// Events are immutable, so handing out a copy of the slice header over
	// shared backing memory is safe as long as callers do not write to it.
	out := make([]domain.Event, end-fromSeq+1)
	copy(out, bk.events[fromSeq-1:end])
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

func newWriter(ledgerID string, bk *book) *writer {
	return &writer{
		ledgerID:   ledgerID,
		book:       bk,
		head:       bk.head,
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
		if reversal, reverted := w.revertedBy[id]; reverted {
			tx.RevertedBy = reversal
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
func (w *writer) Stage(event *domain.Event) error {
	if event.LedgerID != w.ledgerID {
		return fmt.Errorf("%w: event is for ledger %q, writer holds %q",
			domain.ErrInvalidID, event.LedgerID, w.ledgerID)
	}
	event.Seal(w.head.Seq+1, w.head.Hash)

	proj, err := domain.Project(*event)
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

	w.events = append(w.events, *event)
	w.entries = append(w.entries, proj.Entries...)
	w.head = domain.Head{Seq: event.Seq, Hash: event.Hash}
	return nil
}

func (w *writer) StageIdempotency(rec domain.IdempotencyRecord) error {
	w.idem = append(w.idem, rec)
	return nil
}

// commit folds the staged overlay into the book. It runs under the book's
// write lock and cannot fail, which is what makes the command atomic.
func (w *writer) commit() {
	bk := w.book
	bk.events = append(bk.events, w.events...)
	bk.entries = append(bk.entries, w.entries...)
	for name, acct := range w.accounts {
		bk.accounts[name] = acct
	}
	for id, tx := range w.txs {
		bk.txs[id] = tx
	}
	for original, reversal := range w.revertedBy {
		if tx, ok := bk.txs[original]; ok {
			tx.RevertedBy = reversal
			bk.txs[original] = tx
		}
	}
	for _, rec := range w.idem {
		bk.idem[rec.Key] = rec
	}
	for name, delta := range w.deltas {
		current, ok := bk.balances[name]
		if !ok {
			current = domain.Zero(delta.Currency())
		}
		// Overflow was already ruled out when the delta was staged against
		// this same balance.
		next, _ := current.Add(delta)
		bk.balances[name] = next
	}
	bk.head = w.head
}

func copyAccount(acct domain.Account) domain.Account {
	if acct.Metadata == nil {
		return acct
	}
	copied := make(map[string]string, len(acct.Metadata))
	for key, value := range acct.Metadata {
		copied[key] = value
	}
	acct.Metadata = copied
	return acct
}

var _ app.Store = (*Store)(nil)
var _ app.Writer = (*writer)(nil)
