package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

func TestTransactionValidate(t *testing.T) {
	brl := MustCurrency("BRL")
	usd := MustCurrency("USD")
	ten := FromMinor(brl, 1000)

	tests := []struct {
		name string
		tx   Transaction
		want error
	}{
		{
			name: "balanced transfer",
			tx:   NewTransfer("assets:cash", "expenses:fees", ten, testTime),
		},
		{
			name: "balanced three legs",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Cr("liabilities:users:1", FromMinor(brl, 10000)),
				Dr("assets:cash", FromMinor(brl, 9500)),
				Dr("expenses:fees", FromMinor(brl, 500)),
			}},
		},
		{
			name: "each currency balances on its own",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Cr("assets:brl", FromMinor(brl, 5000)),
				Dr("assets:fx:brl", FromMinor(brl, 5000)),
				Cr("assets:fx:usd", FromMinor(usd, 1000)),
				Dr("assets:usd", FromMinor(usd, 1000)),
			}},
		},
		{
			name: "unbalanced",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Cr("assets:cash", FromMinor(brl, 1000)),
				Dr("expenses:fees", FromMinor(brl, 999)),
			}},
			want: ErrInvalidTransaction,
		},
		{
			name: "one currency balances, the other does not",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Cr("assets:brl", FromMinor(brl, 5000)),
				Dr("assets:fx:brl", FromMinor(brl, 5000)),
				Dr("assets:usd", FromMinor(usd, 1000)),
			}},
			want: ErrInvalidTransaction,
		},
		{
			name: "same code different scale does not net out",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Cr("assets:a", FromMinor(Currency{"XTS", 2}, 1000)),
				Dr("assets:b", FromMinor(Currency{"XTS", 4}, 1000)),
			}},
			want: ErrInvalidTransaction,
		},
		{
			name: "single posting",
			tx:   Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{Dr("assets:cash", ten)}},
			want: ErrInvalidTransaction,
		},
		{
			name: "no postings",
			tx:   Transaction{ID: NewID(), EffectiveAt: testTime},
			want: ErrInvalidTransaction,
		},
		{
			name: "no id",
			tx: Transaction{EffectiveAt: testTime, Postings: []Posting{
				Cr("assets:cash", ten), Dr("expenses:fees", ten),
			}},
			want: ErrInvalidTransaction,
		},
		{
			name: "no effective time",
			tx: Transaction{ID: NewID(), Postings: []Posting{
				Cr("assets:cash", ten), Dr("expenses:fees", ten),
			}},
			want: ErrInvalidTransaction,
		},
		{
			name: "zero amount posting",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Dr("assets:cash", Zero(brl)), Cr("expenses:fees", Zero(brl)),
			}},
			want: ErrInvalidTransaction,
		},
		{
			name: "irreversible posting",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Dr("assets:cash", FromMinor(brl, math.MinInt64)),
				Dr("expenses:fees", FromMinor(brl, 1)),
			}},
			want: ErrInvalidTransaction,
		},
		{
			name: "bad account name",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
				Cr("assets cash", ten), Dr("expenses:fees", ten),
			}},
			want: ErrInvalidAccount,
		},
		{
			name: "oversized reference",
			tx: Transaction{ID: NewID(), EffectiveAt: testTime,
				Reference: strings.Repeat("r", MaxReferenceLen+1),
				Postings:  []Posting{Cr("assets:cash", ten), Dr("expenses:fees", ten)},
			},
			want: ErrInvalidTransaction,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.tx.Validate()
			if tt.want == nil && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Validate() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestTransactionValidateDetectsOverflow(t *testing.T) {
	brl := MustCurrency("BRL")
	tx := Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
		Dr("assets:a", FromMinor(brl, math.MaxInt64)),
		Dr("assets:b", FromMinor(brl, math.MaxInt64)),
		Cr("assets:c", FromMinor(brl, math.MaxInt64)),
		Cr("assets:d", FromMinor(brl, math.MaxInt64)),
	}}
	// The legs net to zero, but no summation order reaches that without
	// leaving int64. Reporting overflow beats reporting a balanced
	// transaction the storage layer cannot hold.
	if err := tx.Validate(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Validate() = %v, want ErrOverflow", err)
	}
}

func TestDrCr(t *testing.T) {
	brl := MustCurrency("BRL")
	ten := FromMinor(brl, 1000)

	dr := Dr("assets:cash", ten)
	if !dr.IsDebit() || dr.Amount.Minor() != 1000 {
		t.Errorf("Dr() = %v, want a +1000 debit", dr)
	}
	cr := Cr("assets:cash", ten)
	if cr.IsDebit() || cr.Amount.Minor() != -1000 {
		t.Errorf("Cr() = %v, want a -1000 credit", cr)
	}
	if got, want := dr.String(), "Dr assets:cash 10.00 BRL"; got != want {
		t.Errorf("Dr.String() = %q, want %q", got, want)
	}
	if got, want := cr.String(), "Cr assets:cash 10.00 BRL"; got != want {
		t.Errorf("Cr.String() = %q, want %q", got, want)
	}
}

func TestTransactionAccountsAndCurrencies(t *testing.T) {
	brl := MustCurrency("BRL")
	usd := MustCurrency("USD")
	tx := Transaction{ID: NewID(), EffectiveAt: testTime, Postings: []Posting{
		Cr("assets:brl", FromMinor(brl, 5000)),
		Dr("assets:fx:brl", FromMinor(brl, 5000)),
		Cr("assets:fx:usd", FromMinor(usd, 1000)),
		Dr("assets:usd", FromMinor(usd, 1000)),
		Dr("assets:brl", FromMinor(brl, 0)),
	}}

	gotAccts := tx.Accounts()
	wantAccts := []AccountName{"assets:brl", "assets:fx:brl", "assets:fx:usd", "assets:usd"}
	if len(gotAccts) != len(wantAccts) {
		t.Fatalf("Accounts() = %v, want %v", gotAccts, wantAccts)
	}
	for i := range wantAccts {
		if gotAccts[i] != wantAccts[i] {
			t.Errorf("Accounts()[%d] = %q, want %q", i, gotAccts[i], wantAccts[i])
		}
	}

	gotCurs := tx.Currencies()
	if len(gotCurs) != 2 || gotCurs[0] != brl || gotCurs[1] != usd {
		t.Errorf("Currencies() = %v, want [BRL USD]", gotCurs)
	}
}

func TestTransactionReverse(t *testing.T) {
	brl := MustCurrency("BRL")
	orig := Transaction{ID: NewID(), EffectiveAt: testTime, Reference: "e2e-123", Postings: []Posting{
		Cr("liabilities:users:1", FromMinor(brl, 10000)),
		Dr("assets:cash", FromMinor(brl, 9500)),
		Dr("expenses:fees", FromMinor(brl, 500)),
	}}
	if err := orig.Validate(); err != nil {
		t.Fatalf("original does not validate: %v", err)
	}

	later := testTime.Add(24 * time.Hour)
	rev := orig.Reverse(NewID(), later)
	if err := rev.Validate(); err != nil {
		t.Fatalf("reversal does not validate: %v", err)
	}
	if rev.ID == orig.ID {
		t.Error("reversal reused the original id; it must be a new transaction")
	}
	if !rev.EffectiveAt.Equal(later) {
		t.Errorf("reversal EffectiveAt = %v, want %v", rev.EffectiveAt, later)
	}
	for i := range orig.Postings {
		if rev.Postings[i].Account != orig.Postings[i].Account {
			t.Errorf("leg %d account changed", i)
		}
		if got, want := rev.Postings[i].Amount.Minor(), -orig.Postings[i].Amount.Minor(); got != want {
			t.Errorf("leg %d amount = %d, want %d", i, got, want)
		}
	}

	// Applying both leaves every account where it started.
	net := map[AccountName]int64{}
	for _, p := range append(append([]Posting{}, orig.Postings...), rev.Postings...) {
		net[p.Account] += p.Amount.Minor()
	}
	for acct, v := range net {
		if v != 0 {
			t.Errorf("account %q nets to %d after reversal, want 0", acct, v)
		}
	}
}

func TestNewTransferBalances(t *testing.T) {
	tx := NewTransfer("assets:cash", "expenses:fees", FromMinor(MustCurrency("BRL"), 1234), testTime)
	if err := tx.Validate(); err != nil {
		t.Fatalf("NewTransfer produced an invalid transaction: %v", err)
	}
	if tx.Postings[0].Account != "assets:cash" || tx.Postings[0].IsDebit() {
		t.Errorf("leg 0 = %v, want a credit of assets:cash", tx.Postings[0])
	}
	if tx.Postings[1].Account != "expenses:fees" || !tx.Postings[1].IsDebit() {
		t.Errorf("leg 1 = %v, want a debit of expenses:fees", tx.Postings[1])
	}
}
