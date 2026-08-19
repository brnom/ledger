package domain

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// MaxScale is the largest number of decimal digits a [Currency] may declare.
const MaxScale = 18

// Currency identifies a unit of account together with the scale its minor
// units are expressed in: BRL has scale 2 (centavos), JPY has scale 0, BTC has
// scale 8 (satoshis).
//
// Currency is comparable, so it works as a map key. Two currencies with the
// same code but different scales are different currencies; the ledger refuses
// to mix them rather than silently rescaling.
type Currency struct {
	Code  string
	Scale uint8
}

// knownCurrencies maps a code to its conventional scale. It is deliberately
// short: it exists to stop the same code being used with two different scales
// in one deployment, not to be an exhaustive ISO 4217 table. Anything missing
// can still be built with [NewCurrency].
//
// Assets with very high precision are omitted on purpose. An [Amount] holds
// int64 minor units, so scale 18 caps a value at roughly 9.2 units — too low
// to be useful for, say, wei-denominated ETH.
var knownCurrencies = map[string]uint8{
	"ARS": 2, "AUD": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CLP": 0,
	"COP": 2, "EUR": 2, "GBP": 2, "JPY": 0, "KRW": 0, "MXN": 2,
	"USD": 2, "UYU": 2,

	"BTC": 8, "USDC": 6, "USDT": 6,
}

// NewCurrency returns the currency with the given code and scale.
func NewCurrency(code string, scale uint8) (Currency, error) {
	c := Currency{Code: code, Scale: scale}
	if err := c.Validate(); err != nil {
		return Currency{}, err
	}
	return c, nil
}

// CurrencyByCode returns the well-known currency with the given code and
// whether it is in the built-in table. It lets a caller accept a bare code
// like "BRL" without having to know that BRL means two decimal places.
func CurrencyByCode(code string) (Currency, bool) {
	scale, ok := knownCurrencies[code]
	if !ok {
		return Currency{}, false
	}
	return Currency{Code: code, Scale: scale}, true
}

// MustCurrency returns the well-known currency with the given code, panicking
// if it is not in the built-in table. It is meant for package-level variables
// and tests, where a bad code is a programming error rather than bad input.
func MustCurrency(code string) Currency {
	scale, ok := knownCurrencies[code]
	if !ok {
		panic(fmt.Sprintf("ledger: unknown currency %q; use NewCurrency", code))
	}
	return Currency{Code: code, Scale: scale}
}

// Validate reports whether the currency is well formed. Codes are 2 to 12
// characters of A-Z and 0-9, which covers ISO 4217 as well as the ticker-style
// codes used for crypto assets.
func (c Currency) Validate() error {
	if n := len(c.Code); n < 2 || n > 12 {
		return fmt.Errorf("%w: code %q must be 2-12 characters", ErrInvalidCurrency, c.Code)
	}
	for _, r := range c.Code {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return fmt.Errorf("%w: code %q must be uppercase A-Z0-9", ErrInvalidCurrency, c.Code)
		}
	}
	if c.Scale > MaxScale {
		return fmt.Errorf("%w: scale %d exceeds max %d", ErrInvalidCurrency, c.Scale, MaxScale)
	}
	if known, ok := knownCurrencies[c.Code]; ok && known != c.Scale {
		return fmt.Errorf("%w: %s has scale %d, not %d", ErrInvalidCurrency, c.Code, known, c.Scale)
	}
	return nil
}

// String returns the currency code.
func (c Currency) String() string { return c.Code }

// Amount is a fixed-point monetary value: an int64 count of minor units,
// tagged with the currency that gives those units meaning. Money is never a
// float here, and never a bare integer that could be added to a different
// currency by accident.
//
// The representable range is the int64 range scaled down by the currency's
// scale, so a scale-2 currency spans roughly ±92 quadrillion units. Every
// operation that could leave that range returns [ErrOverflow] instead of
// wrapping.
//
// The zero Amount has the zero Currency and is not usable in arithmetic; build
// values with [FromMinor], [Zero], or [ParseAmount].
type Amount struct {
	minor int64
	cur   Currency
}

// FromMinor returns an Amount of n minor units of cur.
func FromMinor(cur Currency, n int64) Amount {
	return Amount{minor: n, cur: cur}
}

// Zero returns the zero value of cur.
func Zero(cur Currency) Amount { return Amount{cur: cur} }

// Minor returns the value as a count of minor units.
func (a Amount) Minor() int64 { return a.minor }

// Currency returns the currency the value is denominated in.
func (a Amount) Currency() Currency { return a.cur }

// IsZero reports whether the value is zero, regardless of currency.
func (a Amount) IsZero() bool { return a.minor == 0 }

// Sign returns -1, 0, or +1 as the value is negative, zero, or positive.
func (a Amount) Sign() int {
	switch {
	case a.minor < 0:
		return -1
	case a.minor > 0:
		return 1
	default:
		return 0
	}
}

// Add returns a+b, or [ErrCurrencyMismatch] if the currencies differ.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.cur != b.cur {
		return Amount{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, a.cur, b.cur)
	}
	sum := a.minor + b.minor
	// Overflow iff both operands share a sign that the result does not.
	if (a.minor^sum)&(b.minor^sum) < 0 {
		return Amount{}, fmt.Errorf("%w: %s + %s", ErrOverflow, a, b)
	}
	return Amount{minor: sum, cur: a.cur}, nil
}

// Sub returns a-b, or [ErrCurrencyMismatch] if the currencies differ.
func (a Amount) Sub(b Amount) (Amount, error) {
	if a.cur != b.cur {
		return Amount{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, a.cur, b.cur)
	}
	diff := a.minor - b.minor
	// Overflow iff the operands differ in sign and the result takes b's.
	if (a.minor^b.minor)&(a.minor^diff) < 0 {
		return Amount{}, fmt.Errorf("%w: %s - %s", ErrOverflow, a, b)
	}
	return Amount{minor: diff, cur: a.cur}, nil
}

// Neg returns -a. It overflows only on the single unrepresentable value
// math.MinInt64, whose negation has no int64 counterpart.
func (a Amount) Neg() (Amount, error) {
	if a.minor == math.MinInt64 {
		return Amount{}, fmt.Errorf("%w: -%s", ErrOverflow, a)
	}
	return Amount{minor: -a.minor, cur: a.cur}, nil
}

// Abs returns the magnitude of a.
func (a Amount) Abs() (Amount, error) {
	if a.minor >= 0 {
		return a, nil
	}
	return a.Neg()
}

// MulInt returns a*n.
func (a Amount) MulInt(n int64) (Amount, error) {
	prod := a.minor * n
	if a.minor != 0 && (prod/a.minor != n || (a.minor == -1 && n == math.MinInt64)) {
		return Amount{}, fmt.Errorf("%w: %s * %d", ErrOverflow, a, n)
	}
	return Amount{minor: prod, cur: a.cur}, nil
}

// Cmp compares a and b, returning -1, 0, or +1. It reports
// [ErrCurrencyMismatch] rather than ordering across currencies.
func (a Amount) Cmp(b Amount) (int, error) {
	if a.cur != b.cur {
		return 0, fmt.Errorf("%w: %s <=> %s", ErrCurrencyMismatch, a.cur, b.cur)
	}
	switch {
	case a.minor < b.minor:
		return -1, nil
	case a.minor > b.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether a and b have the same currency and value. Unlike
// [Amount.Cmp] it treats a currency mismatch as inequality, not an error, so
// it is safe in test assertions and map lookups.
func (a Amount) Equal(b Amount) bool { return a == b }

// Allocate splits a across len(ratios) parts in proportion to ratios,
// distributing the indivisible remainder by the largest-remainder method. The
// parts always sum back to a exactly: no minor unit is created or lost, which
// is what a payment split or a fee breakdown requires.
//
// Ratios must be non-negative and sum to a positive value.
func (a Amount) Allocate(ratios []int64) ([]Amount, error) {
	if len(ratios) == 0 {
		return nil, fmt.Errorf("%w: no ratios given", ErrInvalidAmount)
	}
	total := new(big.Int)
	for i, r := range ratios {
		if r < 0 {
			return nil, fmt.Errorf("%w: ratio %d is negative", ErrInvalidAmount, i)
		}
		total.Add(total, big.NewInt(r))
	}
	if total.Sign() == 0 {
		return nil, fmt.Errorf("%w: ratios sum to zero", ErrInvalidAmount)
	}

	// Work on the magnitude so that truncation rounds consistently toward
	// zero, then restore the sign. Taking the magnitude in big.Int keeps
	// math.MinInt64 representable.
	mag := new(big.Int).Abs(big.NewInt(a.minor))
	neg := a.minor < 0

	type part struct {
		idx       int
		remainder *big.Int
	}
	out := make([]Amount, len(ratios))
	parts := make([]part, len(ratios))
	assigned := new(big.Int)

	for i, r := range ratios {
		share, rem := new(big.Int), new(big.Int)
		share.Mul(mag, big.NewInt(r))
		share.QuoRem(share, total, rem)
		assigned.Add(assigned, share)
		out[i] = Amount{minor: share.Int64(), cur: a.cur}
		parts[i] = part{idx: i, remainder: rem}
	}

	// Hand the leftover units to the largest remainders, breaking ties by
	// index so the result is deterministic and replayable.
	leftover := new(big.Int).Sub(mag, assigned).Int64()
	sort.SliceStable(parts, func(i, j int) bool {
		if c := parts[i].remainder.Cmp(parts[j].remainder); c != 0 {
			return c > 0
		}
		return parts[i].idx < parts[j].idx
	})
	for i := int64(0); i < leftover; i++ {
		out[parts[i].idx].minor++
	}

	if neg {
		for i := range out {
			out[i].minor = -out[i].minor
		}
	}
	return out, nil
}

// Format returns the value as a bare decimal string with exactly
// [Currency.Scale] fraction digits and no currency code: "-1234.05". It is the
// inverse of [ParseAmount] and the form used in canonical event encoding.
func (a Amount) Format() string {
	var u uint64
	if a.minor < 0 {
		// Negate in unsigned space so math.MinInt64 does not overflow.
		u = uint64(-(a.minor + 1)) + 1
	} else {
		u = uint64(a.minor)
	}

	d := strconv.FormatUint(u, 10)
	if sc := int(a.cur.Scale); sc > 0 {
		if len(d) <= sc {
			d = strings.Repeat("0", sc-len(d)+1) + d
		}
		d = d[:len(d)-sc] + "." + d[len(d)-sc:]
	}
	if a.minor < 0 {
		d = "-" + d
	}
	return d
}

// String returns the value with its currency code: "-1234.05 BRL".
func (a Amount) String() string { return a.Format() + " " + a.cur.Code }

// ParseAmount parses a decimal string as an amount of cur. It accepts an
// optional sign, at least one integer digit, and at most [Currency.Scale]
// fraction digits.
//
// More fraction digits than the currency has is an error, not a rounding
// opportunity: silently dropping a digit is how money goes missing, so the
// caller has to decide how to round.
func ParseAmount(cur Currency, s string) (Amount, error) {
	if err := cur.Validate(); err != nil {
		return Amount{}, err
	}

	rest := s
	neg := false
	if rest != "" && (rest[0] == '-' || rest[0] == '+') {
		neg = rest[0] == '-'
		rest = rest[1:]
	}

	intPart, fracPart, hasPoint := strings.Cut(rest, ".")
	switch {
	case intPart == "":
		return Amount{}, fmt.Errorf("%w: %q has no integer digits", ErrInvalidAmount, s)
	case hasPoint && fracPart == "":
		return Amount{}, fmt.Errorf("%w: %q has a point but no fraction digits", ErrInvalidAmount, s)
	case len(fracPart) > int(cur.Scale):
		return Amount{}, fmt.Errorf("%w: %q has %d fraction digits, %s allows %d",
			ErrInvalidAmount, s, len(fracPart), cur.Code, cur.Scale)
	}
	if !isDigits(intPart) || !isDigits(fracPart) {
		return Amount{}, fmt.Errorf("%w: %q is not a decimal number", ErrInvalidAmount, s)
	}

	digits := intPart + fracPart + strings.Repeat("0", int(cur.Scale)-len(fracPart))
	u, err := strconv.ParseUint(strings.TrimLeft(digits, "0"), 10, 64)
	if err != nil && strings.TrimLeft(digits, "0") != "" {
		return Amount{}, fmt.Errorf("%w: %q does not fit in %s", ErrOverflow, s, cur.Code)
	}

	if neg {
		// -math.MinInt64 has no positive counterpart, so admit its magnitude
		// as the one unsigned value above MaxInt64 that is representable.
		if u > uint64(math.MaxInt64)+1 {
			return Amount{}, fmt.Errorf("%w: %q does not fit in %s", ErrOverflow, s, cur.Code)
		}
		if u == uint64(math.MaxInt64)+1 {
			return Amount{minor: math.MinInt64, cur: cur}, nil
		}
		return Amount{minor: -int64(u), cur: cur}, nil
	}
	if u > uint64(math.MaxInt64) {
		return Amount{}, fmt.Errorf("%w: %q does not fit in %s", ErrOverflow, s, cur.Code)
	}
	return Amount{minor: int64(u), cur: cur}, nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
