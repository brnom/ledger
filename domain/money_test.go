package domain

import (
	"errors"
	"math"
	"testing"
)

// testCur is deliberately outside knownCurrencies so tests are free to pick
// any scale without tripping the registry's consistency check.
func testCur(t *testing.T, scale uint8) Currency {
	t.Helper()
	cur, err := NewCurrency("XTS", scale)
	if err != nil {
		t.Fatalf("NewCurrency(XTS, %d): %v", scale, err)
	}
	return cur
}

func TestCurrencyValidate(t *testing.T) {
	tests := []struct {
		name string
		cur  Currency
		want error
	}{
		{"brl", Currency{"BRL", 2}, nil},
		{"jpy zero scale", Currency{"JPY", 0}, nil},
		{"btc", Currency{"BTC", 8}, nil},
		{"unknown code is fine", Currency{"XTS", 4}, nil},
		{"max scale", Currency{"XTS", MaxScale}, nil},
		{"too short", Currency{"B", 2}, ErrInvalidCurrency},
		{"too long", Currency{"ABCDEFGHIJKLM", 2}, ErrInvalidCurrency},
		{"empty", Currency{"", 2}, ErrInvalidCurrency},
		{"lowercase", Currency{"brl", 2}, ErrInvalidCurrency},
		{"punctuation", Currency{"BR-", 2}, ErrInvalidCurrency},
		{"scale too large", Currency{"XTS", MaxScale + 1}, ErrInvalidCurrency},
		{"known code wrong scale", Currency{"BRL", 3}, ErrInvalidCurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cur.Validate(); !errors.Is(err, tt.want) {
				t.Errorf("Validate() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestMustCurrencyPanicsOnUnknown(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustCurrency(ZZZ) did not panic")
		}
	}()
	MustCurrency("ZZZ")
}

func TestParseAmount(t *testing.T) {
	tests := []struct {
		name  string
		scale uint8
		in    string
		want  int64
		err   error
	}{
		{"integer with scale", 2, "12", 1200, nil},
		{"one fraction digit pads", 2, "12.3", 1230, nil},
		{"exact fraction digits", 2, "12.34", 1234, nil},
		{"negative", 2, "-12.34", -1234, nil},
		{"explicit plus", 2, "+12.34", 1234, nil},
		{"zero", 2, "0.00", 0, nil},
		{"negative zero is zero", 2, "-0.00", 0, nil},
		{"leading zeros", 2, "0000012.34", 1234, nil},
		{"scale zero", 0, "1234", 1234, nil},
		{"high scale", 8, "1.00000001", 100000001, nil},
		{"max int64", 0, "9223372036854775807", math.MaxInt64, nil},
		{"min int64", 0, "-9223372036854775808", math.MinInt64, nil},

		{"empty", 2, "", 0, ErrInvalidAmount},
		{"sign only", 2, "-", 0, ErrInvalidAmount},
		{"no integer digits", 2, ".5", 0, ErrInvalidAmount},
		{"point without fraction", 2, "12.", 0, ErrInvalidAmount},
		{"too many fraction digits", 2, "12.345", 0, ErrInvalidAmount},
		{"fraction on scale zero", 0, "12.3", 0, ErrInvalidAmount},
		{"two points", 2, "1.2.3", 0, ErrInvalidAmount},
		{"letters", 2, "12.3a", 0, ErrInvalidAmount},
		{"whitespace", 2, " 12.34", 0, ErrInvalidAmount},
		{"currency suffix", 2, "12.34 BRL", 0, ErrInvalidAmount},
		{"overflow positive", 0, "9223372036854775808", 0, ErrOverflow},
		{"overflow negative", 0, "-9223372036854775809", 0, ErrOverflow},
		{"overflow via scale", 2, "92233720368547758.08", 0, ErrOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := testCur(t, tt.scale)
			got, err := ParseAmount(cur, tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("ParseAmount(%q) error = %v, want %v", tt.in, err, tt.err)
			}
			if tt.err == nil && got.Minor() != tt.want {
				t.Errorf("ParseAmount(%q) = %d minor, want %d", tt.in, got.Minor(), tt.want)
			}
		})
	}
}

func TestParseAmountRejectsBadCurrency(t *testing.T) {
	if _, err := ParseAmount(Currency{"brl", 2}, "1.00"); !errors.Is(err, ErrInvalidCurrency) {
		t.Errorf("error = %v, want ErrInvalidCurrency", err)
	}
}

func TestAmountFormat(t *testing.T) {
	tests := []struct {
		scale uint8
		minor int64
		want  string
	}{
		{2, 1234, "12.34"},
		{2, -1234, "-12.34"},
		{2, 0, "0.00"},
		{2, 5, "0.05"},
		{2, -5, "-0.05"},
		{2, 100, "1.00"},
		{0, 1234, "1234"},
		{0, -1234, "-1234"},
		{8, 1, "0.00000001"},
		{0, math.MaxInt64, "9223372036854775807"},
		{0, math.MinInt64, "-9223372036854775808"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FromMinor(testCur(t, tt.scale), tt.minor).Format()
			if got != tt.want {
				t.Errorf("Format() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAmountString(t *testing.T) {
	if got, want := FromMinor(MustCurrency("BRL"), -1234).String(), "-12.34 BRL"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestAmountArithmetic(t *testing.T) {
	brl := MustCurrency("BRL")
	usd := MustCurrency("USD")

	t.Run("add", func(t *testing.T) {
		got, err := FromMinor(brl, 1000).Add(FromMinor(brl, 234))
		if err != nil || got.Minor() != 1234 {
			t.Errorf("Add() = %v, %v; want 1234 minor", got, err)
		}
	})
	t.Run("sub", func(t *testing.T) {
		got, err := FromMinor(brl, 1000).Sub(FromMinor(brl, 234))
		if err != nil || got.Minor() != 766 {
			t.Errorf("Sub() = %v, %v; want 766 minor", got, err)
		}
	})
	t.Run("mismatch is not silently coerced", func(t *testing.T) {
		if _, err := FromMinor(brl, 1).Add(FromMinor(usd, 1)); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("Add across currencies err = %v, want ErrCurrencyMismatch", err)
		}
		if _, err := FromMinor(brl, 1).Sub(FromMinor(usd, 1)); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("Sub across currencies err = %v, want ErrCurrencyMismatch", err)
		}
		if _, err := FromMinor(brl, 1).Cmp(FromMinor(usd, 1)); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("Cmp across currencies err = %v, want ErrCurrencyMismatch", err)
		}
	})

	overflows := []struct {
		name string
		fn   func() (Amount, error)
	}{
		{"add positive", func() (Amount, error) {
			return FromMinor(brl, math.MaxInt64).Add(FromMinor(brl, 1))
		}},
		{"add negative", func() (Amount, error) {
			return FromMinor(brl, math.MinInt64).Add(FromMinor(brl, -1))
		}},
		{"sub", func() (Amount, error) {
			return FromMinor(brl, math.MinInt64).Sub(FromMinor(brl, 1))
		}},
		{"neg min int64", func() (Amount, error) {
			return FromMinor(brl, math.MinInt64).Neg()
		}},
		{"abs min int64", func() (Amount, error) {
			return FromMinor(brl, math.MinInt64).Abs()
		}},
		{"mul", func() (Amount, error) {
			return FromMinor(brl, math.MaxInt64).MulInt(2)
		}},
		{"mul min by -1", func() (Amount, error) {
			return FromMinor(brl, -1).MulInt(math.MinInt64)
		}},
	}
	for _, tt := range overflows {
		t.Run("overflow "+tt.name, func(t *testing.T) {
			if _, err := tt.fn(); !errors.Is(err, ErrOverflow) {
				t.Errorf("err = %v, want ErrOverflow", err)
			}
		})
	}
}

func TestAmountNoNearMissOverflow(t *testing.T) {
	brl := MustCurrency("BRL")
	// Boundary values must succeed. An overflow check that fires too early is as
	// wrong as a missing one.
	if got, err := FromMinor(brl, math.MaxInt64-1).Add(FromMinor(brl, 1)); err != nil || got.Minor() != math.MaxInt64 {
		t.Errorf("Add to MaxInt64 = %v, %v", got, err)
	}
	if got, err := FromMinor(brl, math.MinInt64+1).Sub(FromMinor(brl, 1)); err != nil || got.Minor() != math.MinInt64 {
		t.Errorf("Sub to MinInt64 = %v, %v", got, err)
	}
	if got, err := FromMinor(brl, 0).MulInt(math.MinInt64); err != nil || got.Minor() != 0 {
		t.Errorf("zero * MinInt64 = %v, %v", got, err)
	}
}

func TestAmountSignAndCmp(t *testing.T) {
	brl := MustCurrency("BRL")
	if got := FromMinor(brl, -1).Sign(); got != -1 {
		t.Errorf("Sign(-1) = %d", got)
	}
	if got := Zero(brl).Sign(); got != 0 {
		t.Errorf("Sign(0) = %d", got)
	}
	if got := FromMinor(brl, 1).Sign(); got != 1 {
		t.Errorf("Sign(1) = %d", got)
	}
	if !Zero(brl).IsZero() {
		t.Error("Zero().IsZero() = false")
	}
	if got, _ := FromMinor(brl, 1).Cmp(FromMinor(brl, 2)); got != -1 {
		t.Errorf("Cmp(1,2) = %d", got)
	}
	if got, _ := FromMinor(brl, 2).Cmp(FromMinor(brl, 2)); got != 0 {
		t.Errorf("Cmp(2,2) = %d", got)
	}
	if FromMinor(brl, 1).Equal(FromMinor(MustCurrency("USD"), 1)) {
		t.Error("Equal across currencies = true")
	}
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		name   string
		minor  int64
		ratios []int64
		want   []int64
	}{
		{"indivisible cents go to the front", 5, []int64{1, 1, 1}, []int64{2, 2, 1}},
		{"exact split", 100, []int64{3, 7}, []int64{30, 70}},
		{"weighted with remainder", 100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{"marketplace fee", 10000, []int64{95, 5}, []int64{9500, 500}},
		{"negative amount keeps sign", -5, []int64{1, 1, 1}, []int64{-2, -2, -1}},
		{"zero amount", 0, []int64{1, 2}, []int64{0, 0}},
		{"zero ratio gets nothing", 100, []int64{0, 1}, []int64{0, 100}},
		{"single part gets everything", 7, []int64{3}, []int64{7}},
		{"min int64 does not overflow", math.MinInt64, []int64{1, 1}, []int64{math.MinInt64 / 2, math.MinInt64 / 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cur := testCur(t, 2)
			got, err := FromMinor(cur, tt.minor).Allocate(tt.ratios)
			if err != nil {
				t.Fatalf("Allocate: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d parts, want %d", len(got), len(tt.want))
			}
			var sum int64
			for i := range got {
				if got[i].Minor() != tt.want[i] {
					t.Errorf("part %d = %d, want %d", i, got[i].Minor(), tt.want[i])
				}
				if got[i].Currency() != cur {
					t.Errorf("part %d currency = %v, want %v", i, got[i].Currency(), cur)
				}
				sum += got[i].Minor()
			}
			if sum != tt.minor {
				t.Errorf("parts sum to %d, want %d: allocation lost money", sum, tt.minor)
			}
		})
	}
}

func TestAllocateInvalid(t *testing.T) {
	amount := FromMinor(MustCurrency("BRL"), 100)
	for _, tt := range []struct {
		name   string
		ratios []int64
	}{
		{"no ratios", nil},
		{"empty ratios", []int64{}},
		{"negative ratio", []int64{1, -1}},
		{"all zero", []int64{0, 0}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := amount.Allocate(tt.ratios); !errors.Is(err, ErrInvalidAmount) {
				t.Errorf("err = %v, want ErrInvalidAmount", err)
			}
		})
	}
}

func FuzzParseAmount(f *testing.F) {
	for _, s := range []string{
		"0", "0.00", "12.34", "-12.34", "+1", ".5", "1.", "1.2.3", "abc", "",
		"-", "9223372036854775807", "-9223372036854775808", "9223372036854775808",
		"000000000000000000000000001.00", "1e5", "０.０",
	} {
		for _, scale := range []uint8{0, 2, 8} {
			f.Add(scale, s)
		}
	}
	f.Fuzz(func(t *testing.T, scale uint8, s string) {
		cur, err := NewCurrency("XTS", scale%(MaxScale+1))
		if err != nil {
			t.Fatalf("NewCurrency: %v", err)
		}
		got, err := ParseAmount(cur, s)
		if err != nil {
			return
		}
		// Anything that parses must round-trip through its canonical form.
		again, err := ParseAmount(cur, got.Format())
		if err != nil {
			t.Fatalf("ParseAmount(%q) accepted %q but rejected its own Format() %q: %v",
				cur, s, got.Format(), err)
		}
		if again != got {
			t.Errorf("round trip of %q: %v -> %q -> %v", s, got, got.Format(), again)
		}
	})
}

func FuzzAmountFormatRoundTrip(f *testing.F) {
	f.Add(uint8(2), int64(0))
	f.Add(uint8(2), int64(-1))
	f.Add(uint8(0), int64(math.MaxInt64))
	f.Add(uint8(8), int64(math.MinInt64))
	f.Fuzz(func(t *testing.T, scale uint8, minor int64) {
		cur, err := NewCurrency("XTS", scale%(MaxScale+1))
		if err != nil {
			t.Fatalf("NewCurrency: %v", err)
		}
		want := FromMinor(cur, minor)
		got, err := ParseAmount(cur, want.Format())
		if err != nil {
			t.Fatalf("ParseAmount(%q) = %v", want.Format(), err)
		}
		if got != want {
			t.Errorf("round trip: %d minor -> %q -> %d minor", minor, want.Format(), got.Minor())
		}
	})
}

func FuzzAllocateConservesTotal(f *testing.F) {
	f.Add(int64(5), int64(1), int64(1), int64(1))
	f.Add(int64(-100), int64(0), int64(7), int64(3))
	f.Add(int64(math.MinInt64), int64(1), int64(1), int64(1))
	f.Fuzz(func(t *testing.T, minor, r0, r1, r2 int64) {
		cur := Currency{"XTS", 2}
		ratios := []int64{r0, r1, r2}
		parts, err := FromMinor(cur, minor).Allocate(ratios)
		if err != nil {
			return
		}
		// The invariant that makes Allocate safe for payment splits: the parts
		// reconstruct the original exactly, with no unit created or lost.
		sum := Zero(cur)
		for _, part := range parts {
			sum, err = sum.Add(part)
			if err != nil {
				t.Fatalf("summing parts of %d overflowed: %v", minor, err)
			}
		}
		if sum.Minor() != minor {
			t.Errorf("Allocate(%d, %v) parts sum to %d", minor, ratios, sum.Minor())
		}
	})
}
