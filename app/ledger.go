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
	// Backdating is a first-class operation. A settlement file that arrives late
	// describes money that moved days ago. An unbounded window, however, makes
	// every historical balance permanently provisional.
	DefaultBackdateLimit = 90 * 24 * time.Hour

	// DefaultFutureLimit is how far ahead an entry may be dated. It defaults to
	// clock skew only. Forward-dated settlement is a deliberate feature that a
	// ledger opts into with [WithFutureLimit]. A caller must not reach it by
	// sending a bad timestamp.
	DefaultFutureLimit = time.Minute
)

// Ledger is one book: a stream of events, the accounts they open, and the
// balances they produce. It is safe for concurrent use. The store serializes
// writes, and it admits one writer per ledger at a time.
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
	return func(ledger *Ledger) { ledger.now = now }
}

// WithBackdateLimit sets how far into the past an entry may be dated.
func WithBackdateLimit(limit time.Duration) Option {
	return func(ledger *Ledger) { ledger.backdateLimit = limit }
}

// WithFutureLimit sets how far ahead an entry may be dated.
func WithFutureLimit(limit time.Duration) Option {
	return func(ledger *Ledger) { ledger.futureLimit = limit }
}

// Open returns a handle on the named ledger in store.
func Open(store Store, ledgerID string, opts ...Option) (*Ledger, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is nil", domain.ErrInvalidID)
	}
	if err := domain.ValidateLedgerID(ledgerID); err != nil {
		return nil, err
	}
	ledger := &Ledger{
		store:         store,
		id:            ledgerID,
		now:           time.Now,
		backdateLimit: DefaultBackdateLimit,
		futureLimit:   DefaultFutureLimit,
	}
	for _, opt := range opts {
		opt(ledger)
	}
	return ledger, nil
}

// ID returns the ledger's identifier.
func (ledger *Ledger) ID() string { return ledger.id }

// OpenAccount opens an account in the ledger.
func (ledger *Ledger) OpenAccount(ctx context.Context, cmd OpenAccountCommand) (domain.Account, Result, error) {
	fingerprint, err := domain.FingerprintOpenAccount(cmd.Name, cmd.Currency, cmd.Normal,
		cmd.AllowNegative, cmd.EffectiveAt, cmd.Metadata)
	if err != nil {
		return domain.Account{}, Result{}, err
	}

	var (
		acct domain.Account
		res  Result
	)
	err = ledger.store.Update(ctx, ledger.id, func(ctx context.Context, writer Writer) error {
		replayed, err := ledger.replayIdempotent(writer, cmd.IdempotencyKey, fingerprint, &res)
		if err != nil {
			return err
		}
		if replayed {
			// The retry gets the account the first call opened, not a fresh one. A
			// caller therefore cannot tell a replay from the original, except by
			// Result.Replayed.
			existing, _, err := writer.Account(cmd.Name)
			acct = existing
			return err
		}

		if _, exists, err := writer.Account(cmd.Name); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: %q", domain.ErrAccountExists, cmd.Name)
		}

		now := ledger.clock()
		openedAt := cmd.EffectiveAt
		if openedAt.IsZero() {
			openedAt = now
		}
		if err := ledger.checkEffective(openedAt, now); err != nil {
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
		event, err := domain.NewEvent(ledger.id, payload, now, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if err := writer.Stage(&event); err != nil {
			return err
		}
		acct.OpenedSeq = event.Seq
		res = Result{Seq: event.Seq, EventID: event.ID, RecordedAt: event.RecordedAt}
		return ledger.stageIdempotency(writer, cmd.IdempotencyKey, fingerprint, event, domain.ID{})
	})
	if err != nil {
		return domain.Account{}, Result{}, err
	}
	return acct, res, nil
}

// Commit records a transaction. It fails as a whole if any leg is invalid, any
// account is missing, or any account would be overdrawn.
func (ledger *Ledger) Commit(ctx context.Context, cmd CommitCommand) (Result, error) {
	fingerprint, err := domain.FingerprintCommit(cmd.ID, cmd.EffectiveAt, cmd.Postings,
		cmd.Reference, cmd.Metadata)
	if err != nil {
		return Result{}, err
	}

	var res Result
	err = ledger.store.Update(ctx, ledger.id, func(ctx context.Context, writer Writer) error {
		replayed, err := ledger.replayIdempotent(writer, cmd.IdempotencyKey, fingerprint, &res)
		if err != nil || replayed {
			return err
		}

		now := ledger.clock()
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
		if _, exists, err := writer.Transaction(tx.ID); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: %s", domain.ErrTransactionExists, tx.ID)
		}
		if err := ledger.checkEffective(tx.EffectiveAt, now); err != nil {
			return err
		}
		if err := ledger.admit(writer, tx); err != nil {
			return err
		}

		payload, err := domain.NewTransactionCommitted(tx)
		if err != nil {
			return err
		}
		event, err := domain.NewEvent(ledger.id, payload, now, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if err := writer.Stage(&event); err != nil {
			return err
		}
		res = Result{Seq: event.Seq, EventID: event.ID, TransactionID: tx.ID, RecordedAt: event.RecordedAt}
		return ledger.stageIdempotency(writer, cmd.IdempotencyKey, fingerprint, event, tx.ID)
	})
	return res, err
}

// Revert records the compensating transaction that undoes an earlier one.
func (ledger *Ledger) Revert(ctx context.Context, cmd RevertCommand) (Result, error) {
	fingerprint, err := domain.FingerprintRevert(cmd.TransactionID, cmd.EffectiveAt, cmd.Reason)
	if err != nil {
		return Result{}, err
	}

	var res Result
	err = ledger.store.Update(ctx, ledger.id, func(ctx context.Context, writer Writer) error {
		replayed, err := ledger.replayIdempotent(writer, cmd.IdempotencyKey, fingerprint, &res)
		if err != nil || replayed {
			return err
		}

		original, exists, err := writer.Transaction(cmd.TransactionID)
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

		now := ledger.clock()
		effectiveAt := cmd.EffectiveAt
		if effectiveAt.IsZero() {
			effectiveAt = now
		}
		effectiveAt = domain.NormalizeTime(effectiveAt)
		if err := ledger.checkEffective(effectiveAt, now); err != nil {
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
		if err := ledger.admit(writer, reversal); err != nil {
			return err
		}

		event, err := domain.NewEvent(ledger.id, payload, now, cmd.IdempotencyKey)
		if err != nil {
			return err
		}
		if err := writer.Stage(&event); err != nil {
			return err
		}
		res = Result{Seq: event.Seq, EventID: event.ID, TransactionID: payload.ID, RecordedAt: event.RecordedAt}
		return ledger.stageIdempotency(writer, cmd.IdempotencyKey, fingerprint, event, payload.ID)
	})
	return res, err
}

// admit checks a transaction against ledger state. Every account exists. The
// currencies agree. Business time is inside the accounts' lifetimes. No
// account that forbids an overdraft ends up overdrawn.
//
// The overdraft check is made against each account's balance across everything
// recorded, which is the balance a caller can spend today. It deliberately
// does not check that every intermediate historical balance stayed positive. A
// backdated entry can make a past balance negative while the present one is
// healthy. A ledger that refused such an entry could not record what actually
// happened.
func (ledger *Ledger) admit(writer Writer, tx domain.Transaction) error {
	deltas := make(map[domain.AccountName]domain.Amount, len(tx.Postings))
	for _, posting := range tx.Postings {
		acct, exists, err := writer.Account(posting.Account)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: %q", domain.ErrAccountNotFound, posting.Account)
		}
		if acct.Currency != posting.Amount.Currency() {
			return fmt.Errorf("%w: account %q is in %s, posting is in %s",
				domain.ErrCurrencyMismatch, posting.Account, acct.Currency, posting.Amount.Currency())
		}
		if tx.EffectiveAt.Before(acct.OpenedAt) {
			return fmt.Errorf("%w: transaction is effective %s but %q opened %s",
				domain.ErrEffectiveOutOfRange, tx.EffectiveAt.Format(time.RFC3339),
				posting.Account, acct.OpenedAt.Format(time.RFC3339))
		}

		running, ok := deltas[posting.Account]
		if !ok {
			running = domain.Zero(acct.Currency)
		}
		next, err := running.Add(posting.Amount)
		if err != nil {
			return err
		}
		deltas[posting.Account] = next
	}

	for _, name := range tx.Accounts() {
		acct, _, err := writer.Account(name)
		if err != nil {
			return err
		}
		if acct.AllowNegative {
			continue
		}
		current, err := writer.Balance(name)
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
// key seen with a different request is a conflict, not a replay. Two different
// payments must never collapse into one because a caller reused a key.
func (ledger *Ledger) replayIdempotent(writer Writer, key string, fingerprint [32]byte, res *Result) (bool, error) {
	if key == "" {
		return false, nil
	}
	rec, found, err := writer.Idempotency(key)
	if err != nil || !found {
		return false, err
	}
	if rec.RequestHash != fingerprint {
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

func (ledger *Ledger) stageIdempotency(writer Writer, key string, fingerprint [32]byte, event domain.Event, txID domain.ID) error {
	if key == "" {
		return nil
	}
	return writer.StageIdempotency(domain.IdempotencyRecord{
		Key:         key,
		RequestHash: fingerprint,
		Seq:         event.Seq,
		TxID:        txID,
		RecordedAt:  event.RecordedAt,
	})
}

func (ledger *Ledger) checkEffective(effectiveAt, now time.Time) error {
	if effectiveAt.Before(now.Add(-ledger.backdateLimit)) {
		return fmt.Errorf("%w: %s is more than %s in the past",
			domain.ErrEffectiveOutOfRange, effectiveAt.Format(time.RFC3339), ledger.backdateLimit)
	}
	if effectiveAt.After(now.Add(ledger.futureLimit)) {
		return fmt.Errorf("%w: %s is more than %s in the future",
			domain.ErrEffectiveOutOfRange, effectiveAt.Format(time.RFC3339), ledger.futureLimit)
	}
	return nil
}

func (ledger *Ledger) clock() time.Time { return domain.NormalizeTime(ledger.now()) }

// Balance returns an account's signed, debit-positive balance under the given
// query. Both time axes are available: see [domain.BalanceQuery].
func (ledger *Ledger) Balance(ctx context.Context, query domain.BalanceQuery) (domain.Amount, error) {
	return ledger.store.Balance(ctx, ledger.id, query)
}

// PresentedBalance returns a balance the way the account reads it. It is
// positive when the account holds what it is meant to hold, whichever side it
// is on.
func (ledger *Ledger) PresentedBalance(ctx context.Context, query domain.BalanceQuery) (domain.Amount, error) {
	acct, err := ledger.store.Account(ctx, ledger.id, query.Account)
	if err != nil {
		return domain.Amount{}, err
	}
	balance, err := ledger.store.Balance(ctx, ledger.id, query)
	if err != nil {
		return domain.Amount{}, err
	}
	return acct.Presented(balance)
}

// Entries lists entries in recorded order.
func (ledger *Ledger) Entries(ctx context.Context, query domain.EntryQuery) ([]domain.Entry, error) {
	return ledger.store.Entries(ctx, ledger.id, query)
}

// Account returns one account.
func (ledger *Ledger) Account(ctx context.Context, name domain.AccountName) (domain.Account, error) {
	return ledger.store.Account(ctx, ledger.id, name)
}

// Accounts lists accounts under a prefix.
func (ledger *Ledger) Accounts(ctx context.Context, prefix domain.AccountName) ([]domain.Account, error) {
	return ledger.store.Accounts(ctx, ledger.id, prefix)
}

// Transaction returns one transaction with its reversal linkage.
func (ledger *Ledger) Transaction(ctx context.Context, id domain.ID) (domain.RecordedTransaction, error) {
	return ledger.store.Transaction(ctx, ledger.id, id)
}

// Head returns the end of the event stream.
func (ledger *Ledger) Head(ctx context.Context) (domain.Head, error) {
	return ledger.store.Head(ctx, ledger.id)
}

// Events reads the raw event log.
func (ledger *Ledger) Events(ctx context.Context, fromSeq int64, limit int) ([]domain.Event, error) {
	return ledger.store.Events(ctx, ledger.id, fromSeq, limit)
}

// VerifyChainPageSize is how many events [Ledger.Verify] reads at a time.
const VerifyChainPageSize = 1000

// Verify walks the whole event log and checks that it is an unbroken,
// untampered chain. It is the ledger answering "has anything been changed
// behind my back" without trusting the database it is stored in.
func (ledger *Ledger) Verify(ctx context.Context) (domain.Head, error) {
	var (
		prev     = domain.GenesisHash
		seq      = int64(1)
		verified domain.Head
	)
	for {
		events, err := ledger.store.Events(ctx, ledger.id, seq, VerifyChainPageSize)
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
