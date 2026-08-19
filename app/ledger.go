package app

import (
	"context"
	"fmt"
	"time"

	"github.com/brnom/ledger/domain"
)

// Defaults for the window of business time the ledger accepts.
const (
	// DefaultBackdateLimit is how far into the past an entry may be dated.
	// Backdating is a first-class operation -- a settlement file that arrives
	// late describes money that moved days ago -- but an unbounded window
	// makes every historical balance permanently provisional.
	DefaultBackdateLimit = 90 * 24 * time.Hour

	// DefaultFutureLimit is how far ahead an entry may be dated. It defaults
	// to clock skew only: forward-dated settlement is a deliberate feature
	// that a ledger opts into with [WithFutureLimit], not something a caller
	// should stumble into by sending a bad timestamp.
	DefaultFutureLimit = time.Minute
)

// Ledger is one book: a stream of events, the accounts they open, and the
// balances they produce. It is safe for concurrent use; serialization happens
// in the store, which admits one writer per ledger at a time.
type Ledger struct {
	store Store
	id    string

	now           func() time.Time
	backdateLimit time.Duration
	futureLimit   time.Duration
}

// Option configures a [Ledger].
type Option func(*Ledger)

// WithClock replaces the ledger's source of time. Tests use it to make
// recorded timestamps deterministic.
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) { l.now = now }
}

// WithBackdateLimit sets how far into the past an entry may be dated.
func WithBackdateLimit(d time.Duration) Option {
	return func(l *Ledger) { l.backdateLimit = d }
}

// WithFutureLimit sets how far ahead an entry may be dated.
func WithFutureLimit(d time.Duration) Option {
	return func(l *Ledger) { l.futureLimit = d }
}

// Open returns a handle on the named ledger in store.
func Open(store Store, ledgerID string, opts ...Option) (*Ledger, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is nil", domain.ErrInvalidID)
	}
	if err := domain.ValidateLedgerID(ledgerID); err != nil {
		return nil, err
	}
	l := &Ledger{
		store:         store,
		id:            ledgerID,
		now:           time.Now,
		backdateLimit: DefaultBackdateLimit,
		futureLimit:   DefaultFutureLimit,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// ID returns the ledger's identifier.
func (l *Ledger) ID() string { return l.id }

// OpenAccount opens an account in the ledger.
func (l *Ledger) OpenAccount(ctx context.Context, cmd OpenAccountCommand) (domain.Account, Result, error) {
	fp, err := domain.FingerprintOpenAccount(cmd.Name, cmd.Currency, cmd.Normal,
		cmd.AllowNegative, cmd.EffectiveAt, cmd.Metadata)
	if err != nil {
		return domain.Account{}, Result{}, err
	}

	var (
		acct domain.Account
		res  Result
	)
	err = l.store.Update(ctx, l.id, func(ctx context.Context, w Writer) error {
		replayed, err := l.replayIdempotent(w, cmd.IdempotencyKey, fp, &res)
		if err != nil {
			return err
		}
		if replayed {
			// The retry gets the account the first call opened, not a fresh
			// one, so a caller cannot tell a replay from the original except
			// by Result.Replayed.
			existing, _, err := w.Account(cmd.Name)
			acct = existing
			return err
		}

		if _, exists, err := w.Account(cmd.Name); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: %q", domain.ErrAccountExists, cmd.Name)
		}

		now := l.clock()
		openedAt := cmd.EffectiveAt
		if openedAt.IsZero() {
			openedAt = now
		}
		if err := l.checkEffective(openedAt, now); err != nil {
			return err
		}

		acct = domain.Account{
			Name:          cmd.Name,
			Currency:      cmd.Currency,
			Normal:        cmd.Normal,
			AllowNegative: cmd.AllowNegative,
			Metadata:      cmd.Metadata,
			OpenedAt:      domain.NormalizeTime(openedAt),
		}
		payload, err := domain.NewAccountOpened(acct)
		if err != nil {
			return err
		}
		e, err := domain.NewEvent(l.id, payload, now, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if err := w.Stage(&e); err != nil {
			return err
		}
		acct.OpenedSeq = e.Seq
		res = Result{Seq: e.Seq, EventID: e.ID, RecordedAt: e.RecordedAt}
		return l.stageIdempotency(w, cmd.IdempotencyKey, fp, e, domain.ID{})
	})
	if err != nil {
		return domain.Account{}, Result{}, err
	}
	return acct, res, nil
}

// Commit records a transaction. It fails as a whole if any leg is invalid, any
// account is missing, or any account would be overdrawn.
func (l *Ledger) Commit(ctx context.Context, cmd CommitCommand) (Result, error) {
	fp, err := domain.FingerprintCommit(cmd.ID, cmd.EffectiveAt, cmd.Postings,
		cmd.Reference, cmd.Metadata)
	if err != nil {
		return Result{}, err
	}

	var res Result
	err = l.store.Update(ctx, l.id, func(ctx context.Context, w Writer) error {
		replayed, err := l.replayIdempotent(w, cmd.IdempotencyKey, fp, &res)
		if err != nil || replayed {
			return err
		}

		now := l.clock()
		tx := domain.Transaction{
			ID:          cmd.ID,
			EffectiveAt: cmd.EffectiveAt,
			Postings:    cmd.Postings,
			Reference:   cmd.Reference,
			Metadata:    cmd.Metadata,
		}
		if tx.ID.IsZero() {
			tx.ID = domain.NewID()
		}
		if tx.EffectiveAt.IsZero() {
			tx.EffectiveAt = now
		}
		tx.EffectiveAt = domain.NormalizeTime(tx.EffectiveAt)
		if err := tx.Validate(); err != nil {
			return err
		}
		if _, exists, err := w.Transaction(tx.ID); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: %s", domain.ErrTransactionExists, tx.ID)
		}
		if err := l.checkEffective(tx.EffectiveAt, now); err != nil {
			return err
		}
		if err := l.admit(w, tx); err != nil {
			return err
		}

		payload, err := domain.NewTransactionCommitted(tx)
		if err != nil {
			return err
		}
		e, err := domain.NewEvent(l.id, payload, now, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if err := w.Stage(&e); err != nil {
			return err
		}
		res = Result{Seq: e.Seq, EventID: e.ID, TransactionID: tx.ID, RecordedAt: e.RecordedAt}
		return l.stageIdempotency(w, cmd.IdempotencyKey, fp, e, tx.ID)
	})
	return res, err
}

// Revert records the compensating transaction that undoes an earlier one.
func (l *Ledger) Revert(ctx context.Context, cmd RevertCommand) (Result, error) {
	fp, err := domain.FingerprintRevert(cmd.TransactionID, cmd.EffectiveAt, cmd.Reason)
	if err != nil {
		return Result{}, err
	}

	var res Result
	err = l.store.Update(ctx, l.id, func(ctx context.Context, w Writer) error {
		replayed, err := l.replayIdempotent(w, cmd.IdempotencyKey, fp, &res)
		if err != nil || replayed {
			return err
		}

		original, exists, err := w.Transaction(cmd.TransactionID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %s", domain.ErrTransactionNotFound, cmd.TransactionID)
		}
		if !original.RevertedBy.IsZero() {
			return fmt.Errorf("%w: %s was reverted by %s",
				domain.ErrAlreadyReverted, cmd.TransactionID, original.RevertedBy)
		}

		now := l.clock()
		effectiveAt := cmd.EffectiveAt
		if effectiveAt.IsZero() {
			effectiveAt = now
		}
		effectiveAt = domain.NormalizeTime(effectiveAt)
		if err := l.checkEffective(effectiveAt, now); err != nil {
			return err
		}

		payload, err := domain.NewTransactionReverted(original.Transaction, domain.NewID(), effectiveAt, cmd.Reason)
		if err != nil {
			return err
		}
		reversal, err := payload.Transaction()
		if err != nil {
			return err
		}
		if err := l.admit(w, reversal); err != nil {
			return err
		}

		e, err := domain.NewEvent(l.id, payload, now, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if err := w.Stage(&e); err != nil {
			return err
		}
		res = Result{Seq: e.Seq, EventID: e.ID, TransactionID: payload.ID, RecordedAt: e.RecordedAt}
		return l.stageIdempotency(w, cmd.IdempotencyKey, fp, e, payload.ID)
	})
	return res, err
}

// admit checks a transaction against ledger state: every account exists, the
// currencies line up, business time is within the accounts' lifetimes, and no
// account that forbids it ends up overdrawn.
//
// The overdraft check is made against each account's balance across everything
// recorded, which is the balance a caller can spend today. It deliberately does
// not verify that no intermediate historical balance went negative: a backdated
// entry can make a past balance negative while the present one is healthy, and
// refusing such an entry would make the ledger unable to record what actually
// happened.
func (l *Ledger) admit(w Writer, tx domain.Transaction) error {
	deltas := make(map[domain.AccountName]domain.Amount, len(tx.Postings))
	for _, p := range tx.Postings {
		acct, exists, err := w.Account(p.Account)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %q", domain.ErrAccountNotFound, p.Account)
		}
		if acct.Currency != p.Amount.Currency() {
			return fmt.Errorf("%w: account %q is in %s, posting is in %s",
				domain.ErrCurrencyMismatch, p.Account, acct.Currency, p.Amount.Currency())
		}
		if tx.EffectiveAt.Before(acct.OpenedAt) {
			return fmt.Errorf("%w: transaction is effective %s but %q opened %s",
				domain.ErrEffectiveOutOfRange, tx.EffectiveAt.Format(time.RFC3339),
				p.Account, acct.OpenedAt.Format(time.RFC3339))
		}

		running, ok := deltas[p.Account]
		if !ok {
			running = domain.Zero(acct.Currency)
		}
		next, err := running.Add(p.Amount)
		if err != nil {
			return err
		}
		deltas[p.Account] = next
	}

	for _, name := range tx.Accounts() {
		acct, _, err := w.Account(name)
		if err != nil {
			return err
		}
		if acct.AllowNegative {
			continue
		}
		current, err := w.Balance(name)
		if err != nil {
			return err
		}
		after, err := current.Add(deltas[name])
		if err != nil {
			return err
		}
		presented, err := acct.Presented(after)
		if err != nil {
			return err
		}
		if presented.Sign() < 0 {
			available, _ := acct.Presented(current)
			return fmt.Errorf("%w: %q holds %s, transaction would leave %s",
				domain.ErrInsufficientFunds, name, available, presented)
		}
	}
	return nil
}

// replayIdempotent reports whether the key has already produced a result. A
// key seen with a different request is a conflict rather than a replay: two
// different payments must never collapse into one because a caller reused a
// key.
func (l *Ledger) replayIdempotent(w Writer, key string, fp [32]byte, res *Result) (bool, error) {
	if key == "" {
		return false, nil
	}
	rec, found, err := w.Idempotency(key)
	if err != nil || !found {
		return false, err
	}
	if rec.RequestHash != fp {
		return false, fmt.Errorf("%w: %q", domain.ErrIdempotencyConflict, key)
	}
	*res = Result{
		Seq:           rec.Seq,
		TransactionID: rec.TxID,
		RecordedAt:    rec.RecordedAt,
		Replayed:      true,
	}
	return true, nil
}

func (l *Ledger) stageIdempotency(w Writer, key string, fp [32]byte, e domain.Event, txID domain.ID) error {
	if key == "" {
		return nil
	}
	return w.StageIdempotency(domain.IdempotencyRecord{
		Key:         key,
		RequestHash: fp,
		Seq:         e.Seq,
		TxID:        txID,
		RecordedAt:  e.RecordedAt,
	})
}

func (l *Ledger) checkEffective(effectiveAt, now time.Time) error {
	if effectiveAt.Before(now.Add(-l.backdateLimit)) {
		return fmt.Errorf("%w: %s is more than %s in the past",
			domain.ErrEffectiveOutOfRange, effectiveAt.Format(time.RFC3339), l.backdateLimit)
	}
	if effectiveAt.After(now.Add(l.futureLimit)) {
		return fmt.Errorf("%w: %s is more than %s in the future",
			domain.ErrEffectiveOutOfRange, effectiveAt.Format(time.RFC3339), l.futureLimit)
	}
	return nil
}

func (l *Ledger) clock() time.Time { return domain.NormalizeTime(l.now()) }

// Balance returns an account's signed, debit-positive balance under the given
// query. Both time axes are available: see [domain.BalanceQuery].
func (l *Ledger) Balance(ctx context.Context, q domain.BalanceQuery) (domain.Amount, error) {
	return l.store.Balance(ctx, l.id, q)
}

// PresentedBalance returns a balance the way the account reads it: positive
// when the account holds what it is meant to hold, whichever side it is on.
func (l *Ledger) PresentedBalance(ctx context.Context, q domain.BalanceQuery) (domain.Amount, error) {
	acct, err := l.store.Account(ctx, l.id, q.Account)
	if err != nil {
		return domain.Amount{}, err
	}
	balance, err := l.store.Balance(ctx, l.id, q)
	if err != nil {
		return domain.Amount{}, err
	}
	return acct.Presented(balance)
}

// Entries lists entries in recorded order.
func (l *Ledger) Entries(ctx context.Context, q domain.EntryQuery) ([]domain.Entry, error) {
	return l.store.Entries(ctx, l.id, q)
}

// Account returns one account.
func (l *Ledger) Account(ctx context.Context, name domain.AccountName) (domain.Account, error) {
	return l.store.Account(ctx, l.id, name)
}

// Accounts lists accounts under a prefix.
func (l *Ledger) Accounts(ctx context.Context, prefix domain.AccountName) ([]domain.Account, error) {
	return l.store.Accounts(ctx, l.id, prefix)
}

// Transaction returns one transaction with its reversal linkage.
func (l *Ledger) Transaction(ctx context.Context, id domain.ID) (domain.RecordedTransaction, error) {
	return l.store.Transaction(ctx, l.id, id)
}

// Head returns the end of the event stream.
func (l *Ledger) Head(ctx context.Context) (domain.Head, error) {
	return l.store.Head(ctx, l.id)
}

// Events reads the raw event log.
func (l *Ledger) Events(ctx context.Context, fromSeq int64, limit int) ([]domain.Event, error) {
	return l.store.Events(ctx, l.id, fromSeq, limit)
}

// VerifyChainPageSize is how many events [Ledger.Verify] reads at a time.
const VerifyChainPageSize = 1000

// Verify walks the whole event log and checks that it is an unbroken,
// untampered chain. It is the ledger answering "has anything been changed
// behind my back" without trusting the database it is stored in.
func (l *Ledger) Verify(ctx context.Context) (domain.Head, error) {
	var (
		prev     = domain.GenesisHash
		seq      = int64(1)
		verified domain.Head
	)
	for {
		events, err := l.store.Events(ctx, l.id, seq, VerifyChainPageSize)
		if err != nil {
			return verified, err
		}
		if len(events) == 0 {
			return verified, nil
		}
		prev, err = domain.VerifyChain(events, seq, prev)
		if err != nil {
			return verified, err
		}
		last := events[len(events)-1]
		verified = domain.Head{Seq: last.Seq, Hash: last.Hash}
		seq = last.Seq + 1
	}
}
