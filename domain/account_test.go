package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestAccountNameValidate(t *testing.T) {
	tests := []struct {
		name AccountName
		ok   bool
	}{
		{"assets", true},
		{"assets:cash", true},
		{"liabilities:users:9f3c:available", true},
		{"assets:acquirer:cielo-brasil", true},
		{"assets:fee_1.5", true},
		{"Assets:Cash", true},
		{"", false},
		{":assets", false},
		{"assets:", false},
		{"assets::cash", false},
		{"assets cash", false},
		{"assets/cash", false},
		{"contas:usuário", false},
		{AccountName(strings.Repeat("a", MaxAccountNameLen)), true},
		{AccountName(strings.Repeat("a", MaxAccountNameLen+1)), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.name), func(t *testing.T) {
			err := tt.name.Validate()
			if tt.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !tt.ok && !errors.Is(err, ErrInvalidAccount) {
				t.Errorf("Validate() = %v, want ErrInvalidAccount", err)
			}
		})
	}
}

func TestAccountNameHasPrefix(t *testing.T) {
	tests := []struct {
		name, prefix AccountName
		want         bool
	}{
		{"assets:cash", "assets", true},
		{"assets:cash", "assets:cash", true},
		{"assets:cash:brl", "assets:cash", true},
		{"assets_frozen:cash", "assets", false},
		{"assets", "assets:cash", false},
		{"liabilities:users", "assets", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.name)+" under "+string(tt.prefix), func(t *testing.T) {
			if got := tt.name.HasPrefix(tt.prefix); got != tt.want {
				t.Errorf("HasPrefix() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormal(t *testing.T) {
	if got := Debit.Sign(); got != 1 {
		t.Errorf("Debit.Sign() = %d, want 1", got)
	}
	if got := Credit.Sign(); got != -1 {
		t.Errorf("Credit.Sign() = %d, want -1", got)
	}
	if err := Normal(0).Validate(); !errors.Is(err, ErrInvalidAccount) {
		t.Errorf("Normal(0).Validate() = %v, want ErrInvalidAccount", err)
	}
	for _, s := range []string{"debit", "credit"} {
		got, err := ParseNormal(s)
		if err != nil || got.String() != s {
			t.Errorf("ParseNormal(%q) = %v, %v", s, got, err)
		}
	}
	if _, err := ParseNormal("Debit"); !errors.Is(err, ErrInvalidAccount) {
		t.Errorf("ParseNormal(Debit) = %v, want ErrInvalidAccount", err)
	}
}

func TestAccountPresented(t *testing.T) {
	brl := MustCurrency("BRL")
	// A customer wallet is a liability: internally it carries a credit
	// balance, but the customer must see a positive number.
	wallet := Account{Name: "liabilities:users:1", Currency: brl, Normal: Credit}
	got, err := wallet.Presented(FromMinor(brl, -15000))
	if err != nil || got.Minor() != 15000 {
		t.Errorf("credit account Presented(-150.00) = %v, %v; want 150.00", got, err)
	}

	cash := Account{Name: "assets:cash", Currency: brl, Normal: Debit}
	got, err = cash.Presented(FromMinor(brl, 15000))
	if err != nil || got.Minor() != 15000 {
		t.Errorf("debit account Presented(150.00) = %v, %v; want 150.00", got, err)
	}
}

func TestAccountValidate(t *testing.T) {
	brl := MustCurrency("BRL")
	valid := Account{Name: "assets:cash", Currency: brl, Normal: Debit}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}

	tests := []struct {
		name string
		acct Account
		want error
	}{
		{"bad name", Account{Name: "", Currency: brl, Normal: Debit}, ErrInvalidAccount},
		{"bad currency", Account{Name: "assets:cash", Currency: Currency{"brl", 2}, Normal: Debit}, ErrInvalidCurrency},
		{"bad normal", Account{Name: "assets:cash", Currency: brl}, ErrInvalidAccount},
		{"empty metadata key", Account{
			Name: "assets:cash", Currency: brl, Normal: Debit,
			Metadata: map[string]string{"": "v"},
		}, ErrInvalidAccount},
		{"oversized metadata value", Account{
			Name: "assets:cash", Currency: brl, Normal: Debit,
			Metadata: map[string]string{"k": strings.Repeat("v", MaxMetadataValueLen+1)},
		}, ErrInvalidAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.acct.Validate(); !errors.Is(err, tt.want) {
				t.Errorf("Validate() = %v, want %v", err, tt.want)
			}
		})
	}
}
