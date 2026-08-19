// Package storagetest is the conformance suite every [app.Store] must pass.
//
// It exists so that "the in-memory store is the reference implementation" is a
// checked claim rather than an intention. The same tests run against memstore
// and against PostgreSQL. The two may read a bitemporal bound, a prefix match,
// or a concurrent write differently. That difference shows up as a failure,
// not as a surprise in production.
package storagetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// NewStore builds a store for one test. Implementations should return a store
// with no state the test did not put there.
type NewStore func(t *testing.T) app.Store

var (
	brl  = domain.MustCurrency("BRL")
	base = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
)

// clock is hand-advanced so recorded timestamps are deterministic.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (clk *clock) Now() time.Time {
	clk.mu.Lock()
	defer clk.mu.Unlock()
	return clk.at
}

func (clk *clock) Advance(by time.Duration) {
	clk.mu.Lock()
	defer clk.mu.Unlock()
	clk.at = clk.at.Add(by)
}

type harness struct {
	t      *testing.T
	ledger *app.Ledger
	store  app.Store
	clock  *clock
	ctx    context.Context
}

// ledgerCounter keeps each test on its own book. A store shared across tests,
// as a real database is, therefore still gives every test a clean slate.
var ledgerCounter atomic64

func newHarness(t *testing.T, newStore NewStore) *harness {
	t.Helper()
	store := newStore(t)
	clk := &clock{at: base}
	id := fmt.Sprintf("test-%d-%d", time.Now().UnixNano(), ledgerCounter.next())
	ledger, err := app.Open(store, id,
		app.WithClock(clk.Now),
		app.WithBackdateLimit(365*24*time.Hour),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return &harness{t: t, ledger: ledger, store: store, clock: clk, ctx: context.Background()}
}

func (h *harness) open(name domain.AccountName, allowNegative bool) {
	h.t.Helper()
	if _, _, err := h.ledger.OpenAccount(h.ctx, app.OpenAccountCommand{
		Name: name, Currency: brl, Normal: domain.Credit, AllowNegative: allowNegative,
	}); err != nil {
		h.t.Fatalf("OpenAccount(%q): %v", name, err)
	}
}

func (h *harness) transfer(from, to domain.AccountName, minor int64, effectiveAt time.Time) app.Result {
	h.t.Helper()
	res, err := h.ledger.Commit(h.ctx, app.CommitCommand{
		EffectiveAt: effectiveAt,
		Postings: []domain.Posting{
			domain.Dr(from, domain.FromMinor(brl, minor)),
			domain.Cr(to, domain.FromMinor(brl, minor)),
		},
	})
	if err != nil {
		h.t.Fatalf("Commit(%s -> %s, %d): %v", from, to, minor, err)
	}
	return res
}

func (h *harness) balance(query domain.BalanceQuery) int64 {
	h.t.Helper()
	amt, err := h.ledger.Balance(h.ctx, query)
	if err != nil {
		h.t.Fatalf("Balance(%+v): %v", query, err)
	}
	return amt.Minor()
}

// Run executes the whole conformance suite.
func Run(t *testing.T, newStore NewStore) {
	t.Run("AccountLifecycle", func(t *testing.T) { testAccountLifecycle(t, newStore) })
	t.Run("AccountPrefix", func(t *testing.T) { testAccountPrefix(t, newStore) })
	t.Run("Bitemporal", func(t *testing.T) { testBitemporal(t, newStore) })
	t.Run("EntryQueries", func(t *testing.T) { testEntryQueries(t, newStore) })
	t.Run("EntryPagination", func(t *testing.T) { testEntryPagination(t, newStore) })
	t.Run("Idempotency", func(t *testing.T) { testIdempotency(t, newStore) })
	t.Run("Revert", func(t *testing.T) { testRevert(t, newStore) })
	t.Run("RejectionLeavesNoTrace", func(t *testing.T) { testRejectionLeavesNoTrace(t, newStore) })
	t.Run("ChainVerifies", func(t *testing.T) { testChainVerifies(t, newStore) })
	t.Run("ReplayMatchesReadModel", func(t *testing.T) { testReplayMatchesReadModel(t, newStore) })
	t.Run("ConcurrentWriters", func(t *testing.T) { testConcurrentWriters(t, newStore) })
	t.Run("ConcurrentOverdraft", func(t *testing.T) { testConcurrentOverdraft(t, newStore) })
	t.Run("ConcurrentIdempotency", func(t *testing.T) { testConcurrentIdempotency(t, newStore) })
}

func testAccountLifecycle(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("liabilities:users:1", false)

	acct, err := h.ledger.Account(h.ctx, "liabilities:users:1")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if acct.Currency != brl || acct.Normal != domain.Credit || acct.AllowNegative {
		t.Errorf("account = %+v", acct)
	}
	if !acct.OpenedAt.Equal(base) || acct.OpenedSeq != 1 {
		t.Errorf("OpenedAt = %v, OpenedSeq = %d; want %v, 1", acct.OpenedAt, acct.OpenedSeq, base)
	}

	if _, err := h.ledger.Account(h.ctx, "nowhere:at:all"); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("Account(missing) = %v, want ErrAccountNotFound", err)
	}
	if _, _, err := h.ledger.OpenAccount(h.ctx, app.OpenAccountCommand{
		Name: "liabilities:users:1", Currency: brl, Normal: domain.Credit,
	}); !errors.Is(err, domain.ErrAccountExists) {
		t.Errorf("reopening = %v, want ErrAccountExists", err)
	}
}

func testAccountPrefix(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	for _, name := range []domain.AccountName{
		"assets:cash", "assets:cash:brl", "assets:receivable",
		"assets_frozen:cash", "assetsXfrozen:cash", "liabilities:users:1",
	} {
		h.open(name, true)
	}

	tests := []struct {
		prefix domain.AccountName
		want   []domain.AccountName
	}{
		// Sorted by byte value, which is what Go's string comparison does and what
		// the SQL store pins with COLLATE "C". ':' (0x3a) sorts before 'X' (0x58),
		// which sorts before '_' (0x5f).
		{"", []domain.AccountName{
			"assets:cash", "assets:cash:brl", "assets:receivable",
			"assetsXfrozen:cash", "assets_frozen:cash", "liabilities:users:1",
		}},
		// A prefix matches on segment boundaries, so neither assets_frozen nor
		// assetsXfrozen is under assets even though both start the same way.
		{"assets", []domain.AccountName{
			"assets:cash", "assets:cash:brl", "assets:receivable",
		}},
		{"assets:cash", []domain.AccountName{"assets:cash", "assets:cash:brl"}},
		// '_' is a valid character in an account name and a single-character
		// wildcard in SQL LIKE. If the store forgot to escape it, this prefix would
		// drag in assetsXfrozen as well.
		{"assets_frozen", []domain.AccountName{"assets_frozen:cash"}},
		{"nothing", nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.prefix), func(t *testing.T) {
			got, err := h.ledger.Accounts(h.ctx, tt.prefix)
			if err != nil {
				t.Fatalf("Accounts: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d accounts %v, want %d %v", len(got), names(got), len(tt.want), tt.want)
			}
			for i := range tt.want {
				if got[i].Name != tt.want[i] {
					t.Errorf("account %d = %q, want %q", i, got[i].Name, tt.want[i])
				}
			}
		})
	}
}

func names(accts []domain.Account) []domain.AccountName {
	out := make([]domain.AccountName, len(accts))
	for i, acct := range accts {
		out[i] = acct.Name
	}
	return out
}

// testBitemporal is the contract that separates this ledger from a single-axis
// one. A fact recorded now can take effect in the past. What the ledger
// reported before that fact arrived does not change.
func testBitemporal(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)

	day1 := base
	h.transfer("equity:opening-balances", "liabilities:users:1", 10000, day1)
	first, err := h.ledger.Head(h.ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	knownOnDay1 := h.balance(domain.BalanceQuery{Account: "liabilities:users:1"})

	// Three days later, a late settlement lands describing day 2.
	h.clock.Advance(72 * time.Hour)
	day2 := day1.Add(24 * time.Hour)
	h.transfer("equity:opening-balances", "liabilities:users:1", 2500, day2)

	tests := []struct {
		name  string
		query domain.BalanceQuery
		want  int64
	}{
		{"unbounded", domain.BalanceQuery{Account: "liabilities:users:1"}, -12500},
		{"effective on day 2", domain.BalanceQuery{
			Account: "liabilities:users:1", AsOfEffective: day2}, -12500},
		{"effective mid day 1", domain.BalanceQuery{
			Account: "liabilities:users:1", AsOfEffective: day1.Add(12 * time.Hour)}, -10000},
		{"effective before anything", domain.BalanceQuery{
			Account: "liabilities:users:1", AsOfEffective: day1.Add(-time.Second)}, 0},
		{"as recorded by sequence", domain.BalanceQuery{
			Account: "liabilities:users:1", AsOfSeq: first.Seq}, -10000},
		{"as recorded by timestamp", domain.BalanceQuery{
			Account: "liabilities:users:1", AsOfRecorded: day1.Add(time.Hour)}, -10000},
		{"a sequence bound wins over a timestamp bound", domain.BalanceQuery{
			Account:      "liabilities:users:1",
			AsOfSeq:      first.Seq,
			AsOfRecorded: base.Add(365 * 24 * time.Hour),
		}, -10000},
		{"both axes at once", domain.BalanceQuery{
			Account:       "liabilities:users:1",
			AsOfEffective: day2,
			AsOfSeq:       first.Seq,
		}, -10000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.balance(tt.query); got != tt.want {
				t.Errorf("balance = %d, want %d", got, tt.want)
			}
		})
	}

	if got := h.balance(domain.BalanceQuery{Account: "liabilities:users:1", AsOfSeq: first.Seq}); got != knownOnDay1 {
		t.Errorf("history changed: as of day 1 the balance is now %d, was %d", got, knownOnDay1)
	}
}

func testEntryQueries(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)
	h.open("liabilities:users:2", false)

	h.clock.Advance(time.Hour)
	first := h.transfer("equity:opening-balances", "liabilities:users:1", 5000, time.Time{})
	h.clock.Advance(time.Hour)
	second := h.transfer("liabilities:users:1", "liabilities:users:2", 1500, time.Time{})

	tests := []struct {
		name  string
		query domain.EntryQuery
		want  int
	}{
		{"everything", domain.EntryQuery{}, 4},
		{"by account", domain.EntryQuery{Account: "liabilities:users:1"}, 2},
		{"by prefix", domain.EntryQuery{AccountPrefix: "liabilities"}, 3},
		{"by transaction", domain.EntryQuery{TxID: first.TransactionID}, 2},
		{"by sequence range", domain.EntryQuery{FromSeq: second.Seq}, 2},
		{"by upper sequence", domain.EntryQuery{ToSeq: first.Seq}, 2},
		{"by effective window", domain.EntryQuery{
			EffectiveFrom: base.Add(90 * time.Minute)}, 2},
		{"by recorded window", domain.EntryQuery{
			RecordedTo: base.Add(90 * time.Minute)}, 2},
		{"empty window", domain.EntryQuery{
			EffectiveFrom: base.Add(48 * time.Hour)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := h.ledger.Entries(h.ctx, tt.query)
			if err != nil {
				t.Fatalf("Entries: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("got %d entries, want %d: %+v", len(got), tt.want, got)
			}
			for i := 1; i < len(got); i++ {
				if got[i-1].Seq > got[i].Seq ||
					(got[i-1].Seq == got[i].Seq && got[i-1].Index >= got[i].Index) {
					t.Errorf("entries are out of order at %d: %+v then %+v", i, got[i-1], got[i])
				}
			}
		})
	}

	if _, err := h.ledger.Entries(h.ctx, domain.EntryQuery{
		Account: "a:b", AccountPrefix: "a",
	}); !errors.Is(err, domain.ErrInvalidAccount) {
		t.Errorf("Account and AccountPrefix together = %v, want ErrInvalidAccount", err)
	}
}

func testEntryPagination(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)
	const transfers = 25
	for i := 0; i < transfers; i++ {
		h.clock.Advance(time.Minute)
		h.transfer("equity:opening-balances", "liabilities:users:1", 100, time.Time{})
	}

	var (
		seen  []domain.Entry
		after domain.EntryQuery
	)
	for page := 0; page < 100; page++ {
		query := after
		query.Limit = 7
		got, err := h.ledger.Entries(h.ctx, query)
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		if len(got) == 0 {
			break
		}
		seen = append(seen, got...)
		last := got[len(got)-1]
		after = domain.EntryQuery{AfterSeq: last.Seq, AfterIndex: last.Index}
	}

	if len(seen) != transfers*2 {
		t.Fatalf("paging produced %d entries, want %d", len(seen), transfers*2)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1].Seq > seen[i].Seq ||
			(seen[i-1].Seq == seen[i].Seq && seen[i-1].Index >= seen[i].Index) {
			t.Fatalf("paging repeated or reordered an entry at %d", i)
		}
	}
}

func testIdempotency(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)

	cmd := app.CommitCommand{
		IdempotencyKey: "pay-1",
		Postings: []domain.Posting{
			domain.Dr("equity:opening-balances", domain.FromMinor(brl, 2500)),
			domain.Cr("liabilities:users:1", domain.FromMinor(brl, 2500)),
		},
	}
	first, err := h.ledger.Commit(h.ctx, cmd)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	h.clock.Advance(time.Hour)
	second, err := h.ledger.Commit(h.ctx, cmd)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !second.Replayed || second.TransactionID != first.TransactionID || second.Seq != first.Seq {
		t.Errorf("retry = %+v, want a replay of %+v", second, first)
	}
	if got := h.balance(domain.BalanceQuery{Account: "liabilities:users:1"}); got != -2500 {
		t.Errorf("balance = %d, want -2500: the retry was applied", got)
	}

	conflicting := cmd
	conflicting.Reference = "different"
	if _, err := h.ledger.Commit(h.ctx, conflicting); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Errorf("reused key with a new request = %v, want ErrIdempotencyConflict", err)
	}
}

func testRevert(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)
	committed := h.transfer("equity:opening-balances", "liabilities:users:1", 2500, time.Time{})

	h.clock.Advance(time.Hour)
	reverted, err := h.ledger.Revert(h.ctx, app.RevertCommand{
		TransactionID: committed.TransactionID, Reason: "chargeback",
	})
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if got := h.balance(domain.BalanceQuery{Account: "liabilities:users:1"}); got != 0 {
		t.Errorf("balance = %d after reversal, want 0", got)
	}

	original, err := h.ledger.Transaction(h.ctx, committed.TransactionID)
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if original.RevertedBy != reverted.TransactionID {
		t.Errorf("RevertedBy = %s, want %s", original.RevertedBy, reverted.TransactionID)
	}
	if len(original.Postings) != 2 {
		t.Errorf("original has %d postings, want 2", len(original.Postings))
	}

	reversal, err := h.ledger.Transaction(h.ctx, reverted.TransactionID)
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if reversal.Reverts != committed.TransactionID {
		t.Errorf("Reverts = %s, want %s", reversal.Reverts, committed.TransactionID)
	}

	if _, err := h.ledger.Revert(h.ctx, app.RevertCommand{
		TransactionID: committed.TransactionID,
	}); !errors.Is(err, domain.ErrAlreadyReverted) {
		t.Errorf("second Revert = %v, want ErrAlreadyReverted", err)
	}
	if _, err := h.ledger.Transaction(h.ctx, domain.NewID()); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("Transaction(unknown) = %v, want ErrTransactionNotFound", err)
	}
}

// testRejectionLeavesNoTrace checks that a failed command is truly atomic: no
// event, no entry, no advance of the stream.
func testRejectionLeavesNoTrace(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)
	h.transfer("equity:opening-balances", "liabilities:users:1", 1000, time.Time{})

	before, err := h.ledger.Head(h.ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	beforeEntries, err := h.ledger.Entries(h.ctx, domain.EntryQuery{})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}

	_, err = h.ledger.Commit(h.ctx, app.CommitCommand{
		IdempotencyKey: "doomed",
		Postings: []domain.Posting{
			domain.Dr("liabilities:users:1", domain.FromMinor(brl, 5000)),
			domain.Cr("equity:opening-balances", domain.FromMinor(brl, 5000)),
		},
	})
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("Commit = %v, want ErrInsufficientFunds", err)
	}

	after, err := h.ledger.Head(h.ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if after != before {
		t.Errorf("the stream advanced from %d to %d on a rejected command", before.Seq, after.Seq)
	}
	afterEntries, err := h.ledger.Entries(h.ctx, domain.EntryQuery{})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(afterEntries) != len(beforeEntries) {
		t.Errorf("a rejected command left %d entries behind", len(afterEntries)-len(beforeEntries))
	}

	// The idempotency key of a failed command must be free to reuse, or a
	// transient failure would poison the key forever.
	if _, err := h.ledger.Commit(h.ctx, app.CommitCommand{
		IdempotencyKey: "doomed",
		Postings: []domain.Posting{
			domain.Dr("liabilities:users:1", domain.FromMinor(brl, 500)),
			domain.Cr("equity:opening-balances", domain.FromMinor(brl, 500)),
		},
	}); err != nil {
		t.Errorf("reusing the key of a failed command = %v, want nil", err)
	}
}

func testChainVerifies(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)
	for i := 0; i < 30; i++ {
		h.clock.Advance(time.Second)
		h.transfer("equity:opening-balances", "liabilities:users:1", 10, time.Time{})
	}

	head, err := h.ledger.Head(h.ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	verified, err := h.ledger.Verify(h.ctx)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified != head {
		t.Errorf("Verify reached %+v, head is %+v", verified, head)
	}

	// The stream must be gapless, which is what the chain is built on.
	events, err := h.ledger.Events(h.ctx, 1, 10000)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if int64(len(events)) != head.Seq {
		t.Fatalf("read %d events, head is at %d", len(events), head.Seq)
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has sequence %d", i, e.Seq)
		}
	}
}

// testReplayMatchesReadModel is the property that keeps the event log
// authoritative. It matters most here, against a real database. An event that
// survives a storage round trip must hash the same and project the same
// entries as when it was written.
func testReplayMatchesReadModel(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)
	h.open("liabilities:users:2", false)

	var last domain.ID
	for i := 0; i < 20; i++ {
		h.clock.Advance(time.Minute)
		res, err := h.ledger.Commit(h.ctx, app.CommitCommand{
			Reference: fmt.Sprintf("ref-%d", i),
			Metadata:  map[string]string{"batch": "nightly", "seq": fmt.Sprint(i)},
			Postings: []domain.Posting{
				domain.Dr("equity:opening-balances", domain.FromMinor(brl, 300)),
				domain.Cr("liabilities:users:1", domain.FromMinor(brl, 200)),
				domain.Cr("liabilities:users:2", domain.FromMinor(brl, 100)),
			},
		})
		if err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
		last = res.TransactionID
	}
	if _, err := h.ledger.Revert(h.ctx, app.RevertCommand{TransactionID: last}); err != nil {
		t.Fatalf("Revert: %v", err)
	}

	events, err := h.ledger.Events(h.ctx, 1, 10000)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var replayed []domain.Entry
	for _, e := range events {
		// The hash is recomputed from what came back out of storage. Storage may
		// alter the payload bytes or the timestamp in transit, through jsonb
		// normalizing keys or through a precision mismatch. This is where that
		// shows.
		if err := e.Verify(); err != nil {
			t.Fatalf("event %d did not survive storage: %v", e.Seq, err)
		}
		proj, err := domain.Project(e)
		if err != nil {
			t.Fatalf("Project(%d): %v", e.Seq, err)
		}
		replayed = append(replayed, proj.Entries...)
	}

	stored, err := h.ledger.Entries(h.ctx, domain.EntryQuery{Limit: 10000})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(replayed) != len(stored) {
		t.Fatalf("replay produced %d entries, storage holds %d", len(replayed), len(stored))
	}
	for i := range stored {
		if replayed[i] != stored[i] {
			t.Errorf("entry %d differs:\n replay %+v\nstored %+v", i, replayed[i], stored[i])
		}
	}

	// And the balances served must equal what the replay implies.
	sums := map[domain.AccountName]int64{}
	for _, e := range replayed {
		sums[e.Account] += e.Amount.Minor()
	}
	for name, want := range sums {
		if got := h.balance(domain.BalanceQuery{Account: name}); got != want {
			t.Errorf("balance of %q: stored %d, replay %d", name, got, want)
		}
	}
}

func testConcurrentWriters(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)

	const (
		writers = 8
		each    = 15
	)
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_, err := h.ledger.Commit(context.Background(), app.CommitCommand{
					Reference: fmt.Sprintf("w%d-%d", w, i),
					Postings: []domain.Posting{
						domain.Dr("equity:opening-balances", domain.FromMinor(brl, 10)),
						domain.Cr("liabilities:users:1", domain.FromMinor(brl, 10)),
					},
				})
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Commit: %v", err)
	}

	const moved = writers * each * 10
	if got := h.balance(domain.BalanceQuery{Account: "liabilities:users:1"}); got != -moved {
		t.Errorf("balance = %d, want %d", got, -moved)
	}

	// Concurrency must not put a hole in the sequence, or the chain breaks.
	head, err := h.ledger.Head(h.ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	events, err := h.ledger.Events(h.ctx, 1, 100000)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if int64(len(events)) != head.Seq {
		t.Fatalf("head is at %d but only %d events exist", head.Seq, len(events))
	}
	if _, err := h.ledger.Verify(h.ctx); err != nil {
		t.Errorf("Verify after concurrent writes: %v", err)
	}
}

// testConcurrentOverdraft points more writers at an account than its balance
// can satisfy. Exactly as many must succeed as the funds cover: a
// check-then-write race would let extra ones through.
func testConcurrentOverdraft(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)
	h.open("liabilities:users:2", false)
	h.transfer("equity:opening-balances", "liabilities:users:1", 1000, time.Time{})

	const writers = 24
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			_, err := h.ledger.Commit(context.Background(), app.CommitCommand{
				Reference: fmt.Sprintf("w%d", w),
				Postings: []domain.Posting{
					domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
					domain.Cr("liabilities:users:2", domain.FromMinor(brl, 100)),
				},
			})
			switch {
			case err == nil:
				mu.Lock()
				succeeded++
				mu.Unlock()
			case errors.Is(err, domain.ErrInsufficientFunds):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(w)
	}
	wg.Wait()

	if succeeded != 10 {
		t.Errorf("%d writers got through, want exactly 10", succeeded)
	}
	if got := h.balance(domain.BalanceQuery{Account: "liabilities:users:1"}); got != 0 {
		t.Errorf("source balance = %d, want 0", got)
	}
	if got := h.balance(domain.BalanceQuery{Account: "liabilities:users:2"}); got != -1000 {
		t.Errorf("destination balance = %d, want -1000", got)
	}
}

func testConcurrentIdempotency(t *testing.T, newStore NewStore) {
	h := newHarness(t, newStore)
	h.open("equity:opening-balances", true)
	h.open("liabilities:users:1", false)

	cmd := app.CommitCommand{
		IdempotencyKey: "one-and-only",
		Postings: []domain.Posting{
			domain.Dr("equity:opening-balances", domain.FromMinor(brl, 500)),
			domain.Cr("liabilities:users:1", domain.FromMinor(brl, 500)),
		},
	}

	const callers = 24
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []app.Result
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := h.ledger.Commit(context.Background(), cmd)
			if err != nil {
				t.Errorf("Commit: %v", err)
				return
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()

	writes := 0
	for _, res := range results {
		if !res.Replayed {
			writes++
		}
		if res.TransactionID != results[0].TransactionID {
			t.Fatalf("callers saw different transactions: %s and %s",
				res.TransactionID, results[0].TransactionID)
		}
	}
	if writes != 1 {
		t.Errorf("%d callers wrote, want exactly 1", writes)
	}
	if got := h.balance(domain.BalanceQuery{Account: "liabilities:users:1"}); got != -500 {
		t.Errorf("balance = %d, want -500: the transfer landed more than once", got)
	}
}
