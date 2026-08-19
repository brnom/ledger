package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/brnom/ledger/adapter/driven/memstore"
	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// These tests exercise the application layer's own rules: command validation,
// the window of business time, overdraft admission and idempotency. What a
// store must do with the result is checked separately, against every store, by
// the conformance suite in adapter/driven/storagetest.

var (
	brl  = domain.MustCurrency("BRL")
	usd  = domain.MustCurrency("USD")
	base = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
)

// clock is hand-advanced so recorded timestamps are deterministic. It is
// mutex-guarded because the concurrency tests read it from several goroutines.
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

type fixture struct {
	t      *testing.T
	ledger *app.Ledger
	store  app.Store
	clock  *clock
	ctx    context.Context
}

func newFixture(t *testing.T, opts ...app.Option) *fixture {
	t.Helper()
	store := memstore.New()
	t.Cleanup(func() { _ = store.Close() })

	clk := &clock{at: base}
	ledger, err := app.Open(store, "test", append([]app.Option{app.WithClock(clk.Now)}, opts...)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return &fixture{t: t, ledger: ledger, store: store, clock: clk, ctx: context.Background()}
}

func (fix *fixture) open(name domain.AccountName, normal domain.Normal, allowNegative bool) domain.Account {
	fix.t.Helper()
	acct, _, err := fix.ledger.OpenAccount(fix.ctx, app.OpenAccountCommand{
		Name: name, Currency: brl, Normal: normal, AllowNegative: allowNegative,
	})
	if err != nil {
		fix.t.Fatalf("OpenAccount(%q): %v", name, err)
	}
	return acct
}

// funded opens a credit-normal account and puts money in it, the other leg
// coming from the external account that represents the rest of the world.
func (fix *fixture) funded(name domain.AccountName, minor int64) {
	fix.t.Helper()
	if _, err := fix.ledger.Account(fix.ctx, "assets:cash"); err != nil {
		fix.open("assets:cash", domain.Debit, true)
	}
	fix.open(name, domain.Credit, false)
	fix.commit(app.CommitCommand{Postings: []domain.Posting{
		domain.Dr("assets:cash", domain.FromMinor(brl, minor)),
		domain.Cr(name, domain.FromMinor(brl, minor)),
	}})
}

func (fix *fixture) commit(cmd app.CommitCommand) app.Result {
	fix.t.Helper()
	res, err := fix.ledger.Commit(fix.ctx, cmd)
	if err != nil {
		fix.t.Fatalf("Commit: %v", err)
	}
	return res
}

// transfer moves minor units out of a credit-normal account into another.
func (fix *fixture) transfer(from, to domain.AccountName, minor int64) app.Result {
	fix.t.Helper()
	return fix.commit(app.CommitCommand{Postings: []domain.Posting{
		domain.Dr(from, domain.FromMinor(brl, minor)),
		domain.Cr(to, domain.FromMinor(brl, minor)),
	}})
}

// presented reads a balance the way the account's holder reads it.
func (fix *fixture) presented(name domain.AccountName) int64 {
	fix.t.Helper()
	amt, err := fix.ledger.PresentedBalance(fix.ctx, domain.BalanceQuery{Account: name})
	if err != nil {
		fix.t.Fatalf("PresentedBalance(%q): %v", name, err)
	}
	return amt.Minor()
}

func (fix *fixture) head() domain.Head {
	fix.t.Helper()
	head, err := fix.ledger.Head(fix.ctx)
	if err != nil {
		fix.t.Fatalf("Head: %v", err)
	}
	return head
}

func TestOpenRejectsBadArguments(t *testing.T) {
	if _, err := app.Open(nil, "main"); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("Open(nil store) = %v, want ErrInvalidID", err)
	}
	for _, id := range []string{"", "has space", "a/b"} {
		if _, err := app.Open(memstore.New(), id); !errors.Is(err, domain.ErrInvalidID) {
			t.Errorf("Open(%q) = %v, want ErrInvalidID", id, err)
		}
	}
	ledger, err := app.Open(memstore.New(), "main")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ledger.ID() != "main" {
		t.Errorf("ID() = %q, want %q", ledger.ID(), "main")
	}
}

func TestOpenAccount(t *testing.T) {
	fix := newFixture(t)

	acct, res, err := fix.ledger.OpenAccount(fix.ctx, app.OpenAccountCommand{
		Name: "liabilities:users:1", Currency: brl, Normal: domain.Credit,
		Metadata: map[string]string{"owner": "9f3c"},
	})
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	if res.Seq != 1 || res.EventID.IsZero() {
		t.Errorf("Result = %+v, want seq 1 and an event id", res)
	}
	if !res.RecordedAt.Equal(base) {
		t.Errorf("RecordedAt = %s, want %s: the clock was not consulted", res.RecordedAt, base)
	}
	if acct.OpenedSeq != 1 {
		t.Errorf("OpenedSeq = %d, want 1", acct.OpenedSeq)
	}
	if !acct.OpenedAt.Equal(base) {
		t.Errorf("OpenedAt = %s, want %s", acct.OpenedAt, base)
	}

	// The account is readable through the store, not just returned.
	got, err := fix.ledger.Account(fix.ctx, "liabilities:users:1")
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if got.Normal != domain.Credit || got.Currency != brl {
		t.Errorf("Account = %+v, want a credit BRL account", got)
	}
}

func TestOpenAccountRejects(t *testing.T) {
	tests := []struct {
		name string
		cmd  app.OpenAccountCommand
		want error
	}{
		{
			name: "empty name",
			cmd:  app.OpenAccountCommand{Currency: brl, Normal: domain.Credit},
			want: domain.ErrInvalidAccount,
		},
		{
			name: "unknown normal",
			cmd:  app.OpenAccountCommand{Name: "assets:cash", Currency: brl},
			want: domain.ErrInvalidAccount,
		},
		{
			name: "zero currency",
			cmd:  app.OpenAccountCommand{Name: "assets:cash", Normal: domain.Debit},
			want: domain.ErrInvalidCurrency,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fix := newFixture(t)
			if _, _, err := fix.ledger.OpenAccount(fix.ctx, tt.cmd); !errors.Is(err, tt.want) {
				t.Errorf("OpenAccount = %v, want %v", err, tt.want)
			}
			if head := fix.head(); head.Seq != 0 {
				t.Errorf("stream is at %d, want 0: a rejected command wrote an event", head.Seq)
			}
		})
	}
}

func TestOpenAccountTwiceIsAnError(t *testing.T) {
	fix := newFixture(t)
	fix.open("assets:cash", domain.Debit, true)

	_, _, err := fix.ledger.OpenAccount(fix.ctx, app.OpenAccountCommand{
		Name: "assets:cash", Currency: brl, Normal: domain.Debit,
	})
	if !errors.Is(err, domain.ErrAccountExists) {
		t.Errorf("OpenAccount(duplicate) = %v, want ErrAccountExists", err)
	}
}

// An account may not be opened outside the window of business time the ledger
// accepts. An entry may not either, and for the same reason. Nobody can
// reproduce a balance dated before the account existed.
func TestEffectiveWindow(t *testing.T) {
	fix := newFixture(t, app.WithBackdateLimit(time.Hour), app.WithFutureLimit(time.Minute))

	_, _, err := fix.ledger.OpenAccount(fix.ctx, app.OpenAccountCommand{
		Name: "assets:cash", Currency: brl, Normal: domain.Debit,
		EffectiveAt: base.Add(-2 * time.Hour),
	})
	if !errors.Is(err, domain.ErrEffectiveOutOfRange) {
		t.Errorf("OpenAccount(too far back) = %v, want ErrEffectiveOutOfRange", err)
	}

	_, _, err = fix.ledger.OpenAccount(fix.ctx, app.OpenAccountCommand{
		Name: "assets:cash", Currency: brl, Normal: domain.Debit,
		EffectiveAt: base.Add(2 * time.Minute),
	})
	if !errors.Is(err, domain.ErrEffectiveOutOfRange) {
		t.Errorf("OpenAccount(too far forward) = %v, want ErrEffectiveOutOfRange", err)
	}

	// Inside the window it is a normal operation, not a grudging exception.
	if _, _, err := fix.ledger.OpenAccount(fix.ctx, app.OpenAccountCommand{
		Name: "assets:cash", Currency: brl, Normal: domain.Debit,
		EffectiveAt: base.Add(-30 * time.Minute),
	}); err != nil {
		t.Errorf("OpenAccount(inside the window): %v", err)
	}
}

func TestCommitRecordsBothLegs(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	res := fix.transfer("liabilities:users:1", "revenue:fees", 250)
	if res.TransactionID.IsZero() {
		t.Error("Commit returned no transaction id")
	}
	if res.Replayed {
		t.Error("a first commit reported itself as a replay")
	}

	if got := fix.presented("liabilities:users:1"); got != 9750 {
		t.Errorf("user balance = %d, want 9750", got)
	}
	if got := fix.presented("revenue:fees"); got != 250 {
		t.Errorf("fees balance = %d, want 250", got)
	}
	if got := fix.presented("assets:cash"); got != 10000 {
		t.Errorf("cash balance = %d, want 10000: a transfer between two credit accounts moved cash", got)
	}

	tx, err := fix.ledger.Transaction(fix.ctx, res.TransactionID)
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if len(tx.Postings) != 2 || tx.Seq != res.Seq {
		t.Errorf("recorded transaction = %+v, want 2 postings at seq %d", tx, res.Seq)
	}
}

func TestCommitRejects(t *testing.T) {
	tests := []struct {
		name     string
		postings []domain.Posting
		want     error
	}{
		{
			name: "does not balance",
			postings: []domain.Posting{
				domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
				domain.Cr("revenue:fees", domain.FromMinor(brl, 99)),
			},
			want: domain.ErrInvalidTransaction,
		},
		{
			name:     "single leg",
			postings: []domain.Posting{domain.Dr("liabilities:users:1", domain.FromMinor(brl, 0))},
			want:     domain.ErrInvalidTransaction,
		},
		{
			name: "account was never opened",
			postings: []domain.Posting{
				domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
				domain.Cr("liabilities:ghost", domain.FromMinor(brl, 100)),
			},
			want: domain.ErrAccountNotFound,
		},
		{
			name: "posting is in another currency",
			postings: []domain.Posting{
				domain.Dr("liabilities:users:1", domain.FromMinor(usd, 100)),
				domain.Cr("revenue:fees", domain.FromMinor(usd, 100)),
			},
			want: domain.ErrCurrencyMismatch,
		},
		{
			name: "would overdraw an account that forbids it",
			postings: []domain.Posting{
				domain.Dr("liabilities:users:1", domain.FromMinor(brl, 10001)),
				domain.Cr("revenue:fees", domain.FromMinor(brl, 10001)),
			},
			want: domain.ErrInsufficientFunds,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fix := newFixture(t)
			fix.funded("liabilities:users:1", 10000)
			fix.open("revenue:fees", domain.Credit, false)
			before := fix.head()

			_, err := fix.ledger.Commit(fix.ctx, app.CommitCommand{Postings: tt.postings})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Commit = %v, want %v", err, tt.want)
			}
			if after := fix.head(); after.Seq != before.Seq {
				t.Errorf("stream moved from %d to %d: a rejected command wrote an event",
					before.Seq, after.Seq)
			}
			if got := fix.presented("liabilities:users:1"); got != 10000 {
				t.Errorf("balance = %d, want 10000: a rejected command moved money", got)
			}
		})
	}
}

// An account that is allowed to go negative is how the ledger models the rest
// of the world. A clearing account holds no funds of its own, and it goes
// negative while money is in flight. The account being funded from it may not.
func TestCommitAllowsNegativeWhenPermitted(t *testing.T) {
	fix := newFixture(t)
	fix.open("liabilities:clearing", domain.Credit, true)
	fix.open("liabilities:users:1", domain.Credit, false)

	fix.commit(app.CommitCommand{Postings: []domain.Posting{
		domain.Dr("liabilities:clearing", domain.FromMinor(brl, 500)),
		domain.Cr("liabilities:users:1", domain.FromMinor(brl, 500)),
	}})
	if got := fix.presented("liabilities:clearing"); got != -500 {
		t.Errorf("clearing balance = %d, want -500", got)
	}
	if got := fix.presented("liabilities:users:1"); got != 500 {
		t.Errorf("user balance = %d, want 500", got)
	}

	// The same movement in the other direction is refused, because that account
	// was opened without permission to go past zero.
	_, err := fix.ledger.Commit(fix.ctx, app.CommitCommand{Postings: []domain.Posting{
		domain.Dr("liabilities:users:1", domain.FromMinor(brl, 501)),
		domain.Cr("liabilities:clearing", domain.FromMinor(brl, 501)),
	}})
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Errorf("Commit = %v, want ErrInsufficientFunds", err)
	}
}

// The overdraft check runs against the transaction's net effect on each
// account, not against each leg in turn. A check per leg would refuse a
// transaction that takes and returns within itself.
func TestCommitNetsPostingsPerAccount(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 100)
	fix.open("revenue:fees", domain.Credit, false)

	fix.commit(app.CommitCommand{Postings: []domain.Posting{
		domain.Dr("liabilities:users:1", domain.FromMinor(brl, 1000)),
		domain.Cr("liabilities:users:1", domain.FromMinor(brl, 950)),
		domain.Cr("revenue:fees", domain.FromMinor(brl, 50)),
	}})
	if got := fix.presented("liabilities:users:1"); got != 50 {
		t.Errorf("balance = %d, want 50", got)
	}
}

func TestCommitRejectsDuplicateTransactionID(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	id := domain.NewID()
	cmd := app.CommitCommand{
		ID: id,
		Postings: []domain.Posting{
			domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
			domain.Cr("revenue:fees", domain.FromMinor(brl, 100)),
		},
	}
	fix.commit(cmd)

	// No idempotency key, so this is not a retry -- it is a second transaction
	// claiming an identity that is taken.
	if _, err := fix.ledger.Commit(fix.ctx, cmd); !errors.Is(err, domain.ErrTransactionExists) {
		t.Errorf("Commit(duplicate id) = %v, want ErrTransactionExists", err)
	}
}

func TestIdempotentRetryDoesNotWriteTwice(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	cmd := app.CommitCommand{
		IdempotencyKey: "pay-1",
		Postings: []domain.Posting{
			domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
			domain.Cr("revenue:fees", domain.FromMinor(brl, 100)),
		},
	}
	first := fix.commit(cmd)
	if first.Replayed {
		t.Error("the first call reported itself as a replay")
	}

	fix.clock.Advance(time.Minute)
	second := fix.commit(cmd)
	if !second.Replayed {
		t.Error("the retry was not reported as a replay")
	}
	if second.Seq != first.Seq || second.TransactionID != first.TransactionID {
		t.Errorf("retry returned %+v, want the original %+v", second, first)
	}
	if head := fix.head(); head.Seq != first.Seq {
		t.Errorf("stream is at %d, want %d: the retry appended an event", head.Seq, first.Seq)
	}
	if got := fix.presented("liabilities:users:1"); got != 9900 {
		t.Errorf("balance = %d, want 9900: the retry moved money twice", got)
	}
}

// The fingerprint must cover what the caller sent, not what the ledger filled
// in. The caller supplies neither the transaction id nor the effective time
// here, so the ledger generates both. A retry a minute later still has to
// match.
func TestIdempotencyIgnoresGeneratedValues(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	cmd := app.CommitCommand{
		IdempotencyKey: "pay-xyz",
		Postings: []domain.Posting{
			domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
			domain.Cr("revenue:fees", domain.FromMinor(brl, 100)),
		},
	}
	first := fix.commit(cmd)
	fix.clock.Advance(time.Minute)
	second := fix.commit(cmd)

	if !second.Replayed || second.TransactionID != first.TransactionID {
		t.Errorf("retry = %+v, want a replay of %+v", second, first)
	}
}

func TestIdempotencyKeyReusedWithAnotherCommand(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	fix.commit(app.CommitCommand{
		IdempotencyKey: "pay-1",
		Postings: []domain.Posting{
			domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
			domain.Cr("revenue:fees", domain.FromMinor(brl, 100)),
		},
	})

	// Same key, a different amount: two different payments must never collapse
	// into one because a caller reused a key.
	_, err := fix.ledger.Commit(fix.ctx, app.CommitCommand{
		IdempotencyKey: "pay-1",
		Postings: []domain.Posting{
			domain.Dr("liabilities:users:1", domain.FromMinor(brl, 200)),
			domain.Cr("revenue:fees", domain.FromMinor(brl, 200)),
		},
	})
	if !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Errorf("Commit(reused key) = %v, want ErrIdempotencyConflict", err)
	}
}

func TestOpenAccountIsIdempotent(t *testing.T) {
	fix := newFixture(t)
	cmd := app.OpenAccountCommand{
		Name: "assets:cash", Currency: brl, Normal: domain.Debit,
		IdempotencyKey: "open-cash",
	}
	first, res, err := fix.ledger.OpenAccount(fix.ctx, cmd)
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}

	again, res2, err := fix.ledger.OpenAccount(fix.ctx, cmd)
	if err != nil {
		t.Fatalf("OpenAccount(retry): %v", err)
	}
	if !res2.Replayed || res2.Seq != res.Seq {
		t.Errorf("retry = %+v, want a replay of %+v", res2, res)
	}
	// The retry gets the account the first call opened, not a fresh one.
	if again.OpenedSeq != first.OpenedSeq || !again.OpenedAt.Equal(first.OpenedAt) {
		t.Errorf("retry returned %+v, want %+v", again, first)
	}
}

func TestRevert(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	paid := fix.transfer("liabilities:users:1", "revenue:fees", 300)
	fix.clock.Advance(time.Hour)

	res, err := fix.ledger.Revert(fix.ctx, app.RevertCommand{
		TransactionID: paid.TransactionID,
		Reason:        "chargeback",
	})
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if res.TransactionID == paid.TransactionID {
		t.Error("the reversal reused the original's id; it must be its own transaction")
	}
	if got := fix.presented("liabilities:users:1"); got != 10000 {
		t.Errorf("balance = %d, want 10000: the reversal did not undo the payment", got)
	}

	original, err := fix.ledger.Transaction(fix.ctx, paid.TransactionID)
	if err != nil {
		t.Fatalf("Transaction(original): %v", err)
	}
	if original.RevertedBy != res.TransactionID {
		t.Errorf("RevertedBy = %s, want %s", original.RevertedBy, res.TransactionID)
	}
	reversal, err := fix.ledger.Transaction(fix.ctx, res.TransactionID)
	if err != nil {
		t.Fatalf("Transaction(reversal): %v", err)
	}
	if reversal.Reverts != paid.TransactionID {
		t.Errorf("Reverts = %s, want %s", reversal.Reverts, paid.TransactionID)
	}
}

// A reversal dated now leaves the original's effect standing for the period
// between the two, which is the honest record of a correction discovered late.
func TestRevertLeavesHistoryStanding(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	paid := fix.transfer("liabilities:users:1", "revenue:fees", 300)
	fix.clock.Advance(time.Hour)
	if _, err := fix.ledger.Revert(fix.ctx, app.RevertCommand{TransactionID: paid.TransactionID}); err != nil {
		t.Fatalf("Revert: %v", err)
	}

	// As of the moment before the reversal, the money was still gone.
	amt, err := fix.ledger.Balance(fix.ctx, domain.BalanceQuery{
		Account: "liabilities:users:1", AsOfSeq: paid.Seq,
	})
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if amt.Minor() != -9700 {
		t.Errorf("balance as of seq %d = %d, want -9700: history was rewritten",
			paid.Seq, amt.Minor())
	}
}

func TestRevertRejects(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)

	if _, err := fix.ledger.Revert(fix.ctx, app.RevertCommand{TransactionID: domain.NewID()}); !errors.Is(err, domain.ErrTransactionNotFound) {
		t.Errorf("Revert(unknown) = %v, want ErrTransactionNotFound", err)
	}

	paid := fix.transfer("liabilities:users:1", "revenue:fees", 300)
	if _, err := fix.ledger.Revert(fix.ctx, app.RevertCommand{TransactionID: paid.TransactionID}); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	// A second correction has to be a new transaction, so the audit trail stays a
	// chain rather than a set.
	if _, err := fix.ledger.Revert(fix.ctx, app.RevertCommand{TransactionID: paid.TransactionID}); !errors.Is(err, domain.ErrAlreadyReverted) {
		t.Errorf("Revert(twice) = %v, want ErrAlreadyReverted", err)
	}
}

func TestRevertIsIdempotent(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)
	paid := fix.transfer("liabilities:users:1", "revenue:fees", 300)

	cmd := app.RevertCommand{TransactionID: paid.TransactionID, IdempotencyKey: "undo-1"}
	first, err := fix.ledger.Revert(fix.ctx, cmd)
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	fix.clock.Advance(time.Minute)
	second, err := fix.ledger.Revert(fix.ctx, cmd)
	if err != nil {
		t.Fatalf("Revert(retry): %v", err)
	}
	if !second.Replayed || second.TransactionID != first.TransactionID {
		t.Errorf("retry = %+v, want a replay of %+v", second, first)
	}
}

// A debit account reads as it is stored. A credit account reads inverted. This
// is the only place presentation happens, and it is why balances can be held
// signed everywhere else.
func TestPresentedBalance(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)

	signed, err := fix.ledger.Balance(fix.ctx, domain.BalanceQuery{Account: "liabilities:users:1"})
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if signed.Minor() != -10000 {
		t.Errorf("signed balance = %d, want -10000", signed.Minor())
	}
	if got := fix.presented("liabilities:users:1"); got != 10000 {
		t.Errorf("presented balance = %d, want 10000", got)
	}
	if got := fix.presented("assets:cash"); got != 10000 {
		t.Errorf("presented balance of a debit account = %d, want 10000", got)
	}

	if _, err := fix.ledger.PresentedBalance(fix.ctx, domain.BalanceQuery{Account: "liabilities:ghost"}); !errors.Is(err, domain.ErrAccountNotFound) {
		t.Errorf("PresentedBalance(unknown) = %v, want ErrAccountNotFound", err)
	}
}

func TestQueries(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("liabilities:users:2", domain.Credit, false)
	fix.transfer("liabilities:users:1", "liabilities:users:2", 400)

	accts, err := fix.ledger.Accounts(fix.ctx, "liabilities:users")
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(accts) != 2 {
		t.Errorf("Accounts(prefix) returned %d accounts, want 2", len(accts))
	}

	entries, err := fix.ledger.Entries(fix.ctx, domain.EntryQuery{Account: "liabilities:users:1"})
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("Entries returned %d, want 2 (the funding and the transfer)", len(entries))
	}
	if entries[0].Seq > entries[1].Seq {
		t.Error("entries came back out of recorded order")
	}

	events, err := fix.ledger.Events(fix.ctx, 1, 100)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != int(fix.head().Seq) {
		t.Errorf("Events returned %d, want %d", len(events), fix.head().Seq)
	}
}

func TestVerify(t *testing.T) {
	fix := newFixture(t)

	// An empty ledger verifies, and reports that it holds nothing.
	head, err := fix.ledger.Verify(fix.ctx)
	if err != nil {
		t.Fatalf("Verify(empty): %v", err)
	}
	if head.Seq != 0 {
		t.Errorf("Verify(empty) = %+v, want the zero head", head)
	}

	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)
	fix.transfer("liabilities:users:1", "revenue:fees", 100)

	head, err = fix.ledger.Verify(fix.ctx)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if head != fix.head() {
		t.Errorf("Verify = %+v, want the stream head %+v", head, fix.head())
	}
}

// The lock the store takes per ledger is what makes this safe. The engine's
// part is to read the balance inside that lock. N callers may spend from an
// account that can fund only some of them, and exactly that many must get
// through.
func TestConcurrentSpendersCannotOverdraw(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 1000)
	fix.open("revenue:fees", domain.Credit, false)

	const (
		callers = 25
		each    = 100 // funds cover exactly ten of these
	)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ok      int
		refused int
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fix.ledger.Commit(fix.ctx, app.CommitCommand{
				Postings: []domain.Posting{
					domain.Dr("liabilities:users:1", domain.FromMinor(brl, each)),
					domain.Cr("revenue:fees", domain.FromMinor(brl, each)),
				},
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, domain.ErrInsufficientFunds):
				refused++
			default:
				t.Errorf("Commit: %v", err)
			}
		}()
	}
	wg.Wait()

	if ok != 10 || refused != callers-10 {
		t.Errorf("%d succeeded and %d were refused, want 10 and %d", ok, refused, callers-10)
	}
	if got := fix.presented("liabilities:users:1"); got != 0 {
		t.Errorf("balance = %d, want 0", got)
	}
}

// N callers sharing one idempotency key must produce exactly one write, and
// all of them must be told the same outcome.
func TestConcurrentRetriesOfOneKey(t *testing.T) {
	fix := newFixture(t)
	fix.funded("liabilities:users:1", 10000)
	fix.open("revenue:fees", domain.Credit, false)
	before := fix.head()

	const callers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []app.Result
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := fix.ledger.Commit(fix.ctx, app.CommitCommand{
				IdempotencyKey: "pay-1",
				Postings: []domain.Posting{
					domain.Dr("liabilities:users:1", domain.FromMinor(brl, 100)),
					domain.Cr("revenue:fees", domain.FromMinor(brl, 100)),
				},
			})
			if err != nil {
				t.Errorf("Commit: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			results = append(results, res)
		}()
	}
	wg.Wait()

	if head := fix.head(); head.Seq != before.Seq+1 {
		t.Errorf("stream advanced by %d, want 1", head.Seq-before.Seq)
	}
	for _, res := range results {
		if res.TransactionID != results[0].TransactionID || res.Seq != results[0].Seq {
			t.Fatalf("callers were told different outcomes: %+v and %+v", res, results[0])
		}
	}
}
