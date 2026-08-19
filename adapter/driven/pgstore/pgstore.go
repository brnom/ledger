// Package pgstore implements [app.Store] on PostgreSQL.
//
// The design rests on three decisions:
//
// One writer per ledger. Every write transaction takes SELECT ... FOR UPDATE
// on the ledger's row before doing anything else. Serializing writes per book
// is what makes the event sequence gapless, which the hash chain depends on,
// and it lets a command read the state it is about to change with nothing able
// to slip in between.
//
// Projections in the same transaction as the events. The read model is written
// alongside the log rather than by a follower, so a balance is never stale and
// there is no lag to reason about. The log stays authoritative: the read model
// can be dropped and rebuilt from it, and the tests check that a rebuild
// reproduces it exactly.
//
// Payloads stored as json, not jsonb. jsonb reorders keys and rewrites
// numbers; the event hash covers the exact bytes, so jsonb would break every
// chain the moment it was read back.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// DefaultLimit caps a page when a query does not say.
const DefaultLimit = 1000

// Store is a PostgreSQL-backed [app.Store].
type Store struct {
	pool     *pgxpool.Pool
	ownsPool bool

	// ensured remembers which ledger rows this process has already created, so
	// the common path is one lock and no insert.
	ensured sync.Map
}

// Open connects to PostgreSQL and returns a store. The caller should call
// [Store.Migrate] before use.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connecting: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: connecting: %w", err)
	}
	return &Store{pool: pool, ownsPool: true}, nil
}

// New wraps an existing pool. The store does not close it.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool, for callers that need to share it.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close implements [app.Store].
func (s *Store) Close() error {
	if s.ownsPool {
		s.pool.Close()
	}
	return nil
}

// ensureLedger creates the ledger's row if it is missing.
//
// It runs in its own transaction, deliberately. Doing it inside the write
// transaction would be a race: a concurrent writer's uncommitted INSERT is
// invisible, so its SELECT ... FOR UPDATE would find no row and take no lock,
// and two writers would proceed at once.
func (s *Store) ensureLedger(ctx context.Context, ledgerID string) error {
	if _, ok := s.ensured.Load(ledgerID); ok {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO ledgers (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, ledgerID)
	if err != nil {
		return fmt.Errorf("pgstore: creating ledger %q: %w", ledgerID, err)
	}
	s.ensured.Store(ledgerID, struct{}{})
	return nil
}

// Update implements [app.Store].
func (s *Store) Update(ctx context.Context, ledgerID string, fn func(context.Context, app.Writer) error) error {
	if err := s.ensureLedger(ctx, ledgerID); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// The single-writer lock. Everything after this point, up to COMMIT, is
	// the only writer this ledger has.
	var locked string
	err = tx.QueryRow(ctx, `SELECT id FROM ledgers WHERE id = $1 FOR UPDATE`, ledgerID).Scan(&locked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the row for ledger %q is gone", domain.ErrConflict, ledgerID)
		}
		return fmt.Errorf("pgstore: locking ledger %q: %w", ledgerID, err)
	}

	writer, err := newWriter(ctx, ledgerID, tx)
	if err != nil {
		return err
	}
	if err := fn(ctx, writer); err != nil {
		return err
	}
	if err := writer.flush(ctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgstore: commit: %w", err)
	}
	return nil
}

// Head implements [app.Store].
func (s *Store) Head(ctx context.Context, ledgerID string) (domain.Head, error) {
	return readHead(ctx, s.pool, ledgerID)
}

// querier is the subset of pgx shared by a pool and a transaction, so the same
// read can serve an outside caller and a command mid-flight.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func readHead(ctx context.Context, db querier, ledgerID string) (domain.Head, error) {
	var (
		seq  int64
		hash []byte
	)
	err := db.QueryRow(ctx,
		`SELECT seq, hash FROM events WHERE ledger_id = $1 ORDER BY seq DESC LIMIT 1`,
		ledgerID).Scan(&seq, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Head{Hash: domain.GenesisHash}, nil
	}
	if err != nil {
		return domain.Head{}, fmt.Errorf("pgstore: reading head: %w", err)
	}
	return domain.Head{Seq: seq, Hash: toHash(hash)}, nil
}

const accountColumns = `name, currency_code, currency_scale, normal, allow_negative, metadata, opened_at, opened_seq`

func scanAccount(row pgx.Row) (domain.Account, error) {
	var (
		acct     domain.Account
		code     string
		scale    int16
		normal   string
		metadata map[string]string
		openedAt time.Time
	)
	err := row.Scan(&acct.Name, &code, &scale, &normal, &acct.AllowNegative, &metadata, &openedAt, &acct.OpenedSeq)
	if err != nil {
		return domain.Account{}, err
	}
	cur, err := domain.NewCurrency(code, uint8(scale))
	if err != nil {
		return domain.Account{}, err
	}
	acct.Currency = cur
	if acct.Normal, err = domain.ParseNormal(normal); err != nil {
		return domain.Account{}, err
	}
	if len(metadata) > 0 {
		acct.Metadata = metadata
	}
	acct.OpenedAt = readTime(openedAt)
	return acct, nil
}

// Account implements [app.Store].
func (s *Store) Account(ctx context.Context, ledgerID string, name domain.AccountName) (domain.Account, error) {
	return readAccount(ctx, s.pool, ledgerID, name)
}

func readAccount(ctx context.Context, db querier, ledgerID string, name domain.AccountName) (domain.Account, error) {
	row := db.QueryRow(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE ledger_id = $1 AND name = $2`,
		ledgerID, string(name))
	acct, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Account{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, name)
	}
	if err != nil {
		return domain.Account{}, fmt.Errorf("pgstore: reading account %q: %w", name, err)
	}
	return acct, nil
}

// Accounts implements [app.Store].
func (s *Store) Accounts(ctx context.Context, ledgerID string, prefix domain.AccountName) ([]domain.Account, error) {
	sql := `SELECT ` + accountColumns + ` FROM accounts WHERE ledger_id = $1`
	args := []any{ledgerID}
	if prefix != "" {
		// Match the prefix itself or anything below it, on segment
		// boundaries, so "assets" does not pull in "assets_frozen".
		sql += ` AND (name = $2 OR name LIKE $3)`
		args = append(args, string(prefix), escapeLike(string(prefix))+`:%`)
	}
	// COLLATE "C" makes this a byte-wise ordering, matching Go's string
	// comparison. Without it the result would depend on the database's
	// collation, which for punctuation-heavy account names is a real
	// difference and not a cosmetic one.
	sql += ` ORDER BY name COLLATE "C"`

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: listing accounts: %w", err)
	}
	defer rows.Close()

	var out []domain.Account
	for rows.Next() {
		acct, err := scanAccount(rows)
		if err != nil {
			return nil, fmt.Errorf("pgstore: listing accounts: %w", err)
		}
		out = append(out, acct)
	}
	return out, rows.Err()
}

// Transaction implements [app.Store].
func (s *Store) Transaction(ctx context.Context, ledgerID string, id domain.ID) (domain.RecordedTransaction, error) {
	tx, found, err := readTransaction(ctx, s.pool, ledgerID, id)
	if err != nil {
		return domain.RecordedTransaction{}, err
	}
	if !found {
		return domain.RecordedTransaction{}, fmt.Errorf("%w: %s", domain.ErrTransactionNotFound, id)
	}
	return tx, nil
}

func readTransaction(ctx context.Context, db querier, ledgerID string, id domain.ID) (domain.RecordedTransaction, bool, error) {
	var (
		rec         domain.RecordedTransaction
		effectiveAt time.Time
		recordedAt  time.Time
		metadata    map[string]string
		reverts     pgtype.UUID
		revertedBy  pgtype.UUID
	)
	err := db.QueryRow(ctx, `
		SELECT seq, effective_at, recorded_at, reference, metadata, reverts, reverted_by
		FROM transactions WHERE ledger_id = $1 AND tx_id = $2`,
		ledgerID, toUUID(id),
	).Scan(&rec.Seq, &effectiveAt, &recordedAt, &rec.Reference, &metadata, &reverts, &revertedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RecordedTransaction{}, false, nil
	}
	if err != nil {
		return domain.RecordedTransaction{}, false, fmt.Errorf("pgstore: reading transaction %s: %w", id, err)
	}

	rec.ID = id
	rec.EffectiveAt = readTime(effectiveAt)
	rec.RecordedAt = readTime(recordedAt)
	rec.Reverts = fromUUID(reverts)
	rec.RevertedBy = fromUUID(revertedBy)
	if len(metadata) > 0 {
		rec.Metadata = metadata
	}

	// The postings come from the read model rather than from the event, so a
	// transaction reads the same way whether or not the log is at hand.
	rows, err := db.Query(ctx, `
		SELECT account, amount_minor, currency_code, currency_scale
		FROM entries WHERE ledger_id = $1 AND tx_id = $2 ORDER BY seq, idx`,
		ledgerID, toUUID(id))
	if err != nil {
		return domain.RecordedTransaction{}, false, fmt.Errorf("pgstore: reading postings of %s: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			account string
			minor   int64
			code    string
			scale   int16
		)
		if err := rows.Scan(&account, &minor, &code, &scale); err != nil {
			return domain.RecordedTransaction{}, false, err
		}
		cur, err := domain.NewCurrency(code, uint8(scale))
		if err != nil {
			return domain.RecordedTransaction{}, false, err
		}
		rec.Postings = append(rec.Postings, domain.Posting{
			Account: domain.AccountName(account),
			Amount:  domain.FromMinor(cur, minor),
		})
	}
	return rec, true, rows.Err()
}

// Balance implements [app.Store]. Both time axes are pushed into SQL, so a
// bitemporal balance is one indexed aggregate rather than a scan in Go.
func (s *Store) Balance(ctx context.Context, ledgerID string, query domain.BalanceQuery) (domain.Amount, error) {
	var (
		asOfEffective any
		asOfSeq       any
		asOfRecorded  any
	)
	if !query.AsOfEffective.IsZero() {
		asOfEffective = query.AsOfEffective.UTC()
	}
	// A sequence bound is exact, so it supersedes a timestamp bound.
	if query.AsOfSeq > 0 {
		asOfSeq = query.AsOfSeq
	} else if !query.AsOfRecorded.IsZero() {
		asOfRecorded = query.AsOfRecorded.UTC()
	}

	var (
		code  string
		scale int16
		minor int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT a.currency_code, a.currency_scale,
		       COALESCE(SUM(e.amount_minor), 0)::bigint
		FROM accounts a
		LEFT JOIN entries e
		       ON e.ledger_id = a.ledger_id
		      AND e.account   = a.name
		      AND ($3::timestamptz IS NULL OR e.effective_at <= $3)
		      AND ($4::bigint      IS NULL OR e.seq          <= $4)
		      AND ($5::timestamptz IS NULL OR e.recorded_at  <= $5)
		WHERE a.ledger_id = $1 AND a.name = $2
		GROUP BY a.currency_code, a.currency_scale`,
		ledgerID, string(query.Account), asOfEffective, asOfSeq, asOfRecorded,
	).Scan(&code, &scale, &minor)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Amount{}, fmt.Errorf("%w: %q", domain.ErrAccountNotFound, query.Account)
	}
	if err != nil {
		return domain.Amount{}, wrapNumericOverflow(err, fmt.Sprintf("balance of %q", query.Account))
	}

	cur, err := domain.NewCurrency(code, uint8(scale))
	if err != nil {
		return domain.Amount{}, err
	}
	return domain.FromMinor(cur, minor), nil
}

// Entries implements [app.Store].
func (s *Store) Entries(ctx context.Context, ledgerID string, query domain.EntryQuery) ([]domain.Entry, error) {
	if query.Account != "" && query.AccountPrefix != "" {
		return nil, fmt.Errorf("%w: set Account or AccountPrefix, not both", domain.ErrInvalidAccount)
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}

	var conds condBuilder
	conds.add(`ledger_id = `, ledgerID)
	if query.Account != "" {
		conds.add(`account = `, string(query.Account))
	}
	if query.AccountPrefix != "" {
		conds.raw(`(account = ` + conds.next(string(query.AccountPrefix)) + ` OR account LIKE ` +
			conds.next(escapeLike(string(query.AccountPrefix))+`:%`) + `)`)
	}
	if !query.TxID.IsZero() {
		conds.add(`tx_id = `, toUUID(query.TxID))
	}
	if !query.EffectiveFrom.IsZero() {
		conds.add(`effective_at >= `, query.EffectiveFrom.UTC())
	}
	if !query.EffectiveTo.IsZero() {
		conds.add(`effective_at <= `, query.EffectiveTo.UTC())
	}
	if !query.RecordedFrom.IsZero() {
		conds.add(`recorded_at >= `, query.RecordedFrom.UTC())
	}
	if !query.RecordedTo.IsZero() {
		conds.add(`recorded_at <= `, query.RecordedTo.UTC())
	}
	if query.FromSeq > 0 {
		conds.add(`seq >= `, query.FromSeq)
	}
	if query.ToSeq > 0 {
		conds.add(`seq <= `, query.ToSeq)
	}
	if query.AfterSeq > 0 {
		conds.raw(`(seq, idx) > (` + conds.next(query.AfterSeq) + `, ` + conds.next(query.AfterIndex) + `)`)
	}

	sql := `SELECT seq, idx, account, amount_minor, currency_code, currency_scale,
	               tx_id, reference, effective_at, recorded_at, reverts
	        FROM entries WHERE ` + conds.where() +
		` ORDER BY seq, idx LIMIT ` + conds.next(limit)

	rows, err := s.pool.Query(ctx, sql, conds.args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: listing entries: %w", err)
	}
	defer rows.Close()

	var out []domain.Entry
	for rows.Next() {
		var (
			entry       domain.Entry
			minor       int64
			code        string
			scale       int16
			txID        pgtype.UUID
			reverts     pgtype.UUID
			effectiveAt time.Time
			recordedAt  time.Time
		)
		err := rows.Scan(&entry.Seq, &entry.Index, &entry.Account, &minor, &code, &scale,
			&txID, &entry.Reference, &effectiveAt, &recordedAt, &reverts)
		if err != nil {
			return nil, fmt.Errorf("pgstore: listing entries: %w", err)
		}
		cur, err := domain.NewCurrency(code, uint8(scale))
		if err != nil {
			return nil, err
		}
		entry.Amount = domain.FromMinor(cur, minor)
		entry.TxID = fromUUID(txID)
		entry.Reverts = fromUUID(reverts)
		entry.EffectiveAt = readTime(effectiveAt)
		entry.RecordedAt = readTime(recordedAt)
		out = append(out, entry)
	}
	return out, rows.Err()
}

// Events implements [app.Store].
func (s *Store) Events(ctx context.Context, ledgerID string, fromSeq int64, limit int) ([]domain.Event, error) {
	if fromSeq < 1 {
		fromSeq = 1
	}
	if limit <= 0 {
		limit = DefaultLimit
	}

	rows, err := s.pool.Query(ctx, `
		SELECT seq, event_id, type, payload, recorded_at, idempotency_key, prev_hash, hash
		FROM events
		WHERE ledger_id = $1 AND seq >= $2
		ORDER BY seq
		LIMIT $3`, ledgerID, fromSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("pgstore: reading events: %w", err)
	}
	defer rows.Close()

	var out []domain.Event
	for rows.Next() {
		var (
			event      domain.Event
			eventID    pgtype.UUID
			payload    string
			key        *string
			prevHash   []byte
			hash       []byte
			recordedAt time.Time
		)
		err := rows.Scan(&event.Seq, &eventID, &event.Type, &payload, &recordedAt, &key, &prevHash, &hash)
		if err != nil {
			return nil, fmt.Errorf("pgstore: reading events: %w", err)
		}
		event.LedgerID = ledgerID
		event.ID = fromUUID(eventID)
		event.Payload = []byte(payload)
		event.RecordedAt = readTime(recordedAt)
		if key != nil {
			event.IdempotencyKey = *key
		}
		event.PrevHash = toHash(prevHash)
		event.Hash = toHash(hash)
		out = append(out, event)
	}
	return out, rows.Err()
}

// condBuilder assembles a WHERE clause with positional placeholders, so
// optional filters do not turn into string concatenation of user input.
type condBuilder struct {
	conds []string
	args  []any
}

// next records an argument and returns its placeholder.
func (b *condBuilder) next(arg any) string {
	b.args = append(b.args, arg)
	return fmt.Sprintf("$%d", len(b.args))
}

func (b *condBuilder) add(expr string, arg any) {
	b.conds = append(b.conds, expr+b.next(arg))
}

func (b *condBuilder) raw(cond string) { b.conds = append(b.conds, cond) }

func (b *condBuilder) where() string { return strings.Join(b.conds, " AND ") }

// escapeLike neutralizes the wildcards in a LIKE pattern so an account named
// "assets:100%" cannot match a subtree it does not own.
func escapeLike(pattern string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(pattern)
}

// readTime puts a timestamp back into the ledger's single representation. The
// values were written as UTC truncated to microseconds, so this restores the
// exact instant -- and, just as importantly, the same time.Location, so
// entries read back compare equal to the ones a replay produces.
func readTime(at time.Time) time.Time { return at.UTC() }

func toUUID(id domain.ID) pgtype.UUID {
	if id.IsZero() {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

func fromUUID(uuid pgtype.UUID) domain.ID {
	if !uuid.Valid {
		return domain.ID{}
	}
	return domain.ID(uuid.Bytes)
}

func toHash(raw []byte) [32]byte {
	var hash [32]byte
	copy(hash[:], raw)
	return hash
}

// wrapNumericOverflow turns PostgreSQL's out-of-range error into the ledger's
// own, so a balance too large for int64 reports the same way whichever store
// is underneath.
func wrapNumericOverflow(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22003" {
		return fmt.Errorf("%w: %s exceeds int64", domain.ErrOverflow, what)
	}
	return fmt.Errorf("pgstore: %s: %w", what, err)
}

// wrapWriteError maps the constraint violations the schema is there to enforce
// back onto the ledger's own errors, so callers see the same failure whichever
// layer caught it.
func wrapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("pgstore: write: %w", err)
	}
	switch {
	case pgErr.Code == "23505" && pgErr.ConstraintName == "transactions_reverts_once":
		return fmt.Errorf("%w: %s", domain.ErrAlreadyReverted, pgErr.Detail)
	case pgErr.Code == "23505" && pgErr.ConstraintName == "accounts_pkey":
		return fmt.Errorf("%w: %s", domain.ErrAccountExists, pgErr.Detail)
	case pgErr.Code == "23505" && pgErr.ConstraintName == "transactions_pkey":
		return fmt.Errorf("%w: %s", domain.ErrTransactionExists, pgErr.Detail)
	case pgErr.Code == "23505" && pgErr.ConstraintName == "idempotency_pkey":
		return fmt.Errorf("%w: %s", domain.ErrIdempotencyConflict, pgErr.Detail)
	case pgErr.Code == "23505" && pgErr.ConstraintName == "events_pkey":
		// Two writers reached the same sequence number, which the per-ledger
		// lock is supposed to prevent.
		return fmt.Errorf("%w: %s", domain.ErrConflict, pgErr.Detail)
	case pgErr.Code == "22003":
		return fmt.Errorf("%w: %s", domain.ErrOverflow, pgErr.Message)
	default:
		return fmt.Errorf("pgstore: write: %w", err)
	}
}

var _ app.Store = (*Store)(nil)
