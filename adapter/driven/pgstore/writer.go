package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// writer is the [app.Writer] a command runs against. It holds the ledger's
// single-writer lock through the enclosing transaction, so its reads and the
// append that follows them cannot be interleaved with another writer.
//
// Effects are staged rather than written as they happen. Reads consult the
// staged overlay first and the database second. flush writes the whole batch
// at the end. That keeps a command to one round of writes and makes "nothing
// happened" the natural outcome of an error.
type writer struct {
	ledgerID string
	tx       pgx.Tx
	head     domain.Head

	// ctx is the Update call's context. A context held in a struct is normally a
	// smell. This writer exists only for the duration of that single call, and
	// [app.Writer]'s methods take no context of their own.
	ctx context.Context

	events  []domain.Event
	entries []domain.Entry
	idem    []domain.IdempotencyRecord

	accounts   map[domain.AccountName]domain.Account
	txs        map[domain.ID]domain.RecordedTransaction
	deltas     map[domain.AccountName]domain.Amount
	revertedBy map[domain.ID]domain.ID
}

func newWriter(ctx context.Context, ledgerID string, tx pgx.Tx) (*writer, error) {
	head, err := readHead(ctx, tx, ledgerID)
	if err != nil {
		return nil, err
	}
	return &writer{
		ledgerID:   ledgerID,
		tx:         tx,
		ctx:        ctx,
		head:       head,
		accounts:   make(map[domain.AccountName]domain.Account),
		txs:        make(map[domain.ID]domain.RecordedTransaction),
		deltas:     make(map[domain.AccountName]domain.Amount),
		revertedBy: make(map[domain.ID]domain.ID),
	}, nil
}

func (w *writer) LedgerID() string { return w.ledgerID }

func (w *writer) Head() domain.Head { return w.head }

func (w *writer) Account(name domain.AccountName) (domain.Account, bool, error) {
	if acct, ok := w.accounts[name]; ok {
		return acct, true, nil
	}
	acct, err := readAccount(w.ctx, w.tx, w.ledgerID, name)
	if errors.Is(err, domain.ErrAccountNotFound) {
		return domain.Account{}, false, nil
	}
	if err != nil {
		return domain.Account{}, false, err
	}
	return acct, true, nil
}

func (w *writer) Balance(name domain.AccountName) (domain.Amount, error) {
	acct, ok, err := w.Account(name)
	if err != nil {
		return domain.Amount{}, err
	}
	if !ok {
		return domain.Amount{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, name)
	}

	// The running balance, not a sum over entries: an overdraft check happens on
	// every write and must not get slower as an account accumulates history.
	var minor int64
	err = w.tx.QueryRow(w.ctx,
		`SELECT amount_minor FROM balances WHERE ledger_id = $1 AND account = $2`,
		w.ledgerID, string(name)).Scan(&minor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return domain.Amount{}, fmt.Errorf("pgstore: reading balance of %q: %w", name, err)
	}

	sum := domain.FromMinor(acct.Currency, minor)
	if delta, ok := w.deltas[name]; ok {
		return sum.Add(delta)
	}
	return sum, nil
}

func (w *writer) Transaction(id domain.ID) (domain.RecordedTransaction, bool, error) {
	rec, ok := w.txs[id]
	if !ok {
		var err error
		rec, ok, err = readTransaction(w.ctx, w.tx, w.ledgerID, id)
		if err != nil {
			return domain.RecordedTransaction{}, false, err
		}
	}
	if ok {
		if reversal, reverted := w.revertedBy[id]; reverted {
			rec.RevertedBy = reversal
		}
	}
	return rec, ok, nil
}

func (w *writer) Idempotency(key string) (domain.IdempotencyRecord, bool, error) {
	for _, rec := range w.idem {
		if rec.Key == key {
			return rec, true, nil
		}
	}

	var (
		rec        domain.IdempotencyRecord
		hash       []byte
		txID       pgtype.UUID
		recordedAt time.Time
	)
	err := w.tx.QueryRow(w.ctx, `
		SELECT request_hash, seq, tx_id, recorded_at
		FROM idempotency WHERE ledger_id = $1 AND key = $2`,
		w.ledgerID, key).Scan(&hash, &rec.Seq, &txID, &recordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return domain.IdempotencyRecord{}, false, fmt.Errorf("pgstore: reading idempotency key: %w", err)
	}

	rec.Key = key
	rec.RequestHash = toHash(hash)
	rec.TxID = fromUUID(txID)
	rec.RecordedAt = readTime(recordedAt)
	return rec, true, nil
}

// Stage seals the event onto the end of the stream and folds its projection
// into the overlay. Nothing reaches the database until flush.
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

// flush writes everything staged. It runs inside the caller's transaction. The
// events and the projections derived from them therefore commit together or
// not at all. No window exists in which the log and the read model disagree.
func (w *writer) flush(ctx context.Context) error {
	if len(w.events) == 0 && len(w.idem) == 0 {
		return nil
	}

	batch := &pgx.Batch{}

	for _, event := range w.events {
		var key any
		if event.IdempotencyKey != "" {
			key = event.IdempotencyKey
		}
		batch.Queue(`
			INSERT INTO events (ledger_id, seq, event_id, type, payload,
			                    recorded_at, idempotency_key, prev_hash, hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			w.ledgerID, event.Seq, toUUID(event.ID), string(event.Type), string(event.Payload),
			event.RecordedAt, key, event.PrevHash[:], event.Hash[:])
	}

	for _, acct := range w.accounts {
		metadata, err := marshalMetadata(acct.Metadata)
		if err != nil {
			return err
		}
		batch.Queue(`
			INSERT INTO accounts (ledger_id, name, currency_code, currency_scale,
			                      normal, allow_negative, metadata, opened_at, opened_seq)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			w.ledgerID, string(acct.Name), acct.Currency.Code, int16(acct.Currency.Scale),
			acct.Normal.String(), acct.AllowNegative, metadata, acct.OpenedAt, acct.OpenedSeq)
	}

	for _, rec := range w.txs {
		metadata, err := marshalMetadata(rec.Metadata)
		if err != nil {
			return err
		}
		batch.Queue(`
			INSERT INTO transactions (ledger_id, tx_id, seq, effective_at, recorded_at,
			                          reference, metadata, reverts)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			w.ledgerID, toUUID(rec.ID), rec.Seq, rec.EffectiveAt, rec.RecordedAt,
			rec.Reference, metadata, toUUID(rec.Reverts))
	}

	for _, entry := range w.entries {
		batch.Queue(`
			INSERT INTO entries (ledger_id, seq, idx, account, amount_minor,
			                     currency_code, currency_scale, tx_id, reference,
			                     effective_at, recorded_at, reverts)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			w.ledgerID, entry.Seq, entry.Index, string(entry.Account), entry.Amount.Minor(),
			entry.Amount.Currency().Code, int16(entry.Amount.Currency().Scale),
			toUUID(entry.TxID), entry.Reference, entry.EffectiveAt, entry.RecordedAt, toUUID(entry.Reverts))
	}

	// Running balances move by the command's net effect per account, computed
	// once here rather than re-summed from history.
	for name, delta := range w.deltas {
		batch.Queue(`
			INSERT INTO balances (ledger_id, account, amount_minor, currency_code, currency_scale)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (ledger_id, account)
			DO UPDATE SET amount_minor = balances.amount_minor + EXCLUDED.amount_minor`,
			w.ledgerID, string(name), delta.Minor(),
			delta.Currency().Code, int16(delta.Currency().Scale))
	}

	for _, rec := range w.idem {
		batch.Queue(`
			INSERT INTO idempotency (ledger_id, key, request_hash, seq, tx_id, recorded_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			w.ledgerID, rec.Key, rec.RequestHash[:], rec.Seq, toUUID(rec.TxID), rec.RecordedAt)
	}

	if err := w.tx.SendBatch(ctx, batch).Close(); err != nil {
		return wrapWriteError(err)
	}

	// Link the reverted transactions last, guarded by "still NULL" so a second
	// reversal cannot slip past even if the command layer somehow allowed it.
	for original, reversal := range w.revertedBy {
		tag, err := w.tx.Exec(ctx, `
			UPDATE transactions SET reverted_by = $3
			WHERE ledger_id = $1 AND tx_id = $2 AND reverted_by IS NULL`,
			w.ledgerID, toUUID(original), toUUID(reversal))
		if err != nil {
			return wrapWriteError(err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: %s", domain.ErrAlreadyReverted, original)
		}
	}
	return nil
}

func marshalMetadata(metadata map[string]string) ([]byte, error) {
	if len(metadata) == 0 {
		return []byte(`{}`), nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("%w: metadata: %v", domain.ErrEncoding, err)
	}
	return encoded, nil
}

var _ app.Writer = (*writer)(nil)
