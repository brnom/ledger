package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/brnom/ledger/adapter/driven/memstore"
	"github.com/brnom/ledger/app"
	"github.com/brnom/ledger/domain"
)

// The property tests drive random command sequences and assert what must hold
// however the ledger got there. They are the part of the suite that finds the
// cases nobody thought to write down.

var propAccounts = []domain.AccountName{
	"liabilities:users:1",
	"liabilities:users:2",
	"revenue:fees",
}

// propLedger builds a book with a few accounts and some money in it. The
// clearing account is the only one allowed past zero, so every other account
// going negative is a bug rather than a scenario.
func propLedger(t *rapid.T) (*app.Ledger, context.Context) {
	ctx := context.Background()
	store := memstore.New()
	ledger, err := app.Open(store, "prop",
		app.WithClock(func() time.Time { return base }),
		app.WithBackdateLimit(90*24*time.Hour),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// The accounts start well in the past. A backdated entry therefore lands
	// inside their lifetime. It exercises backdating, not the check that an entry
	// may not predate the account it touches.
	open := func(name domain.AccountName, allowNegative bool) {
		if _, _, err := ledger.OpenAccount(ctx, app.OpenAccountCommand{
			Name: name, Currency: brl, Normal: domain.Credit, AllowNegative: allowNegative,
			EffectiveAt: base.Add(-60 * 24 * time.Hour),
		}); err != nil {
			t.Fatalf("OpenAccount(%q): %v", name, err)
		}
	}
	open("liabilities:clearing", true)
	for _, name := range propAccounts {
		open(name, false)
	}
	for _, name := range propAccounts[:2] {
		if _, err := ledger.Commit(ctx, app.CommitCommand{Postings: []domain.Posting{
			domain.Dr("liabilities:clearing", domain.FromMinor(brl, 1000)),
			domain.Cr(name, domain.FromMinor(brl, 1000)),
		}}); err != nil {
			t.Fatalf("funding %q: %v", name, err)
		}
	}
	return ledger, ctx
}

// A command either takes effect completely or leaves no trace, so a failure is
// never allowed to move the stream.
func mustNotWrite(t *rapid.T, ledger *app.Ledger, ctx context.Context, before domain.Head, err error) {
	after, headErr := ledger.Head(ctx)
	if headErr != nil {
		t.Fatalf("Head: %v", headErr)
	}
	if after.Seq != before.Seq {
		t.Fatalf("a command that failed with %v moved the stream from %d to %d",
			err, before.Seq, after.Seq)
	}
}

func TestPropertyLedgerInvariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ledger, ctx := propLedger(t)

		var committed []domain.ID
		keys := []string{"", "k1", "k2"}

		for range rapid.IntRange(1, 12).Draw(t, "commands") {
			before, err := ledger.Head(ctx)
			if err != nil {
				t.Fatalf("Head: %v", err)
			}

			switch rapid.SampledFrom([]string{"transfer", "backdate", "revert"}).Draw(t, "op") {
			case "transfer", "backdate":
				from := rapid.SampledFrom(propAccounts).Draw(t, "from")
				to := rapid.SampledFrom(propAccounts).Draw(t, "to")
				if from == to {
					continue
				}
				amount := rapid.Int64Range(1, 500).Draw(t, "amount")

				var effective time.Time
				if rapid.Bool().Draw(t, "backdated") {
					effective = base.Add(-time.Duration(rapid.IntRange(0, 48).Draw(t, "hours")) * time.Hour)
				}

				res, err := ledger.Commit(ctx, app.CommitCommand{
					EffectiveAt:    effective,
					IdempotencyKey: rapid.SampledFrom(keys).Draw(t, "key"),
					Postings: []domain.Posting{
						domain.Dr(from, domain.FromMinor(brl, amount)),
						domain.Cr(to, domain.FromMinor(brl, amount)),
					},
				})
				switch {
				case err == nil:
					if res.Replayed {
						// A replay answers with the original outcome and must not have written
						// anything of its own.
						mustNotWrite(t, ledger, ctx, before, nil)
					} else {
						committed = append(committed, res.TransactionID)
					}
				case errors.Is(err, domain.ErrInsufficientFunds),
					errors.Is(err, domain.ErrIdempotencyConflict):
					mustNotWrite(t, ledger, ctx, before, err)
				default:
					t.Fatalf("Commit: %v", err)
				}

			case "revert":
				if len(committed) == 0 {
					continue
				}
				id := rapid.SampledFrom(committed).Draw(t, "transaction")
				res, err := ledger.Revert(ctx, app.RevertCommand{TransactionID: id})
				switch {
				case err == nil:
					if !res.Replayed {
						committed = append(committed, res.TransactionID)
					}
				case errors.Is(err, domain.ErrAlreadyReverted),
					errors.Is(err, domain.ErrInsufficientFunds):
					mustNotWrite(t, ledger, ctx, before, err)
				default:
					t.Fatalf("Revert: %v", err)
				}
			}
		}

		checkConserved(t, ledger, ctx)
		checkNoUnpermittedOverdraft(t, ledger, ctx)
		checkChain(t, ledger, ctx)
		checkEveryPrefixBalances(t, ledger, ctx)
	})
}

// Double entry means the book sums to zero in every currency, whatever was
// done to it. If this ever fails, money was created or destroyed.
func checkConserved(t *rapid.T, ledger *app.Ledger, ctx context.Context) {
	accounts, err := ledger.Accounts(ctx, "")
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	var total int64
	for _, acct := range accounts {
		amt, err := ledger.Balance(ctx, domain.BalanceQuery{Account: acct.Name})
		if err != nil {
			t.Fatalf("Balance(%q): %v", acct.Name, err)
		}
		total += amt.Minor()
	}
	if total != 0 {
		t.Fatalf("the book sums to %d, want 0", total)
	}
}

// An account opened without permission to go past zero must not end up past
// it. The order the commands arrived in makes no difference.
func checkNoUnpermittedOverdraft(t *rapid.T, ledger *app.Ledger, ctx context.Context) {
	accounts, err := ledger.Accounts(ctx, "")
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, acct := range accounts {
		if acct.AllowNegative {
			continue
		}
		amt, err := ledger.PresentedBalance(ctx, domain.BalanceQuery{Account: acct.Name})
		if err != nil {
			t.Fatalf("PresentedBalance(%q): %v", acct.Name, err)
		}
		if amt.Sign() < 0 {
			t.Fatalf("%q is overdrawn at %s and was not allowed to be", acct.Name, amt)
		}
	}
}

func checkChain(t *rapid.T, ledger *app.Ledger, ctx context.Context) {
	verified, err := ledger.Verify(ctx)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	head, err := ledger.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if verified != head {
		t.Fatalf("Verify stopped at %+v but the stream is at %+v", verified, head)
	}
}

// The one that matters most is this. For every prefix of the log, the balance
// as of that point equals the sum of exactly the entries recorded up to it.
// The prefix starts at 1 because AsOfSeq is zero-means-unbounded, so "as of 0"
// is the whole book rather than an empty one. This is the bitemporal claim --
// "what did we believe, back then" -- reduced to arithmetic, and it is what a
// single-timestamp ledger cannot answer.
func checkEveryPrefixBalances(t *rapid.T, ledger *app.Ledger, ctx context.Context) {
	head, err := ledger.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	accounts, err := ledger.Accounts(ctx, "")
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}

	for _, acct := range accounts {
		entries, err := ledger.Entries(ctx, domain.EntryQuery{Account: acct.Name})
		if err != nil {
			t.Fatalf("Entries(%q): %v", acct.Name, err)
		}
		for seq := int64(1); seq <= head.Seq; seq++ {
			var want int64
			for _, entry := range entries {
				if entry.Seq <= seq {
					want += entry.Amount.Minor()
				}
			}
			got, err := ledger.Balance(ctx, domain.BalanceQuery{Account: acct.Name, AsOfSeq: seq})
			if err != nil {
				t.Fatalf("Balance(%q, as of %d): %v", acct.Name, seq, err)
			}
			if got.Minor() != want {
				t.Fatalf("balance of %q as of seq %d = %d, want %d",
					acct.Name, seq, got.Minor(), want)
			}
		}
	}
}
