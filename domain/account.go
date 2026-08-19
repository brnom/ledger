package domain

import (
	"fmt"
	"strings"
	"time"
)

// MaxAccountNameLen bounds an account name so it stays indexable.
const MaxAccountNameLen = 255

// AccountName is an account's natural key within a ledger: colon-separated
// segments that form a hierarchy, such as "liabilities:users:9f3c:available"
// or "assets:acquirer:cielo:receivable".
//
// The hierarchy is a naming convention the ledger preserves and can query by
// prefix; it carries no accounting meaning of its own. Names are
// case-sensitive, so "Users" and "users" are different accounts.
type AccountName string

// Validate reports whether the name is well formed: one or more non-empty
// segments joined by ':', each made of letters, digits, '_', '.', or '-'.
func (n AccountName) Validate() error {
	switch {
	case n == "":
		return fmt.Errorf("%w: name is empty", ErrInvalidAccount)
	case len(n) > MaxAccountNameLen:
		return fmt.Errorf("%w: name is %d bytes, max %d", ErrInvalidAccount, len(n), MaxAccountNameLen)
	}
	for _, seg := range strings.Split(string(n), ":") {
		if seg == "" {
			return fmt.Errorf("%w: %q has an empty segment", ErrInvalidAccount, n)
		}
		for _, r := range seg {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-'
			if !ok {
				return fmt.Errorf("%w: %q contains %q, want letters, digits, '_', '.' or '-'",
					ErrInvalidAccount, n, r)
			}
		}
	}
	return nil
}

// Segments splits the name on ':'.
func (n AccountName) Segments() []string { return strings.Split(string(n), ":") }

// HasPrefix reports whether the name sits under the given hierarchy prefix.
// It matches on segment boundaries, so "assets:cash" is under "assets" but
// "assets_frozen:cash" is not.
func (n AccountName) HasPrefix(prefix AccountName) bool {
	if n == prefix {
		return true
	}
	return strings.HasPrefix(string(n), string(prefix)+":")
}

func (n AccountName) String() string { return string(n) }

// Normal is the side of the book an account increases on.
//
// Balances are stored signed and debit-positive throughout the engine; Normal
// exists so a balance can be shown the way its holder reads it. A customer
// wallet is a liability of the operator, so it carries a credit balance
// internally, yet the customer expects to see a positive number.
type Normal int8

const (
	// Debit accounts increase on the debit side: assets and expenses.
	Debit Normal = iota + 1
	// Credit accounts increase on the credit side: liabilities, equity, and
	// revenue.
	Credit
)

// Sign returns +1 for a debit-normal account and -1 for a credit-normal one.
// Multiplying a signed internal balance by Sign yields the presentation
// balance, positive when the account holds what it is supposed to hold.
func (n Normal) Sign() int64 {
	if n == Credit {
		return -1
	}
	return 1
}

// Validate reports whether the value is a known normal balance.
func (n Normal) Validate() error {
	if n != Debit && n != Credit {
		return fmt.Errorf("%w: unknown normal balance %d", ErrInvalidAccount, int8(n))
	}
	return nil
}

func (n Normal) String() string {
	switch n {
	case Debit:
		return "debit"
	case Credit:
		return "credit"
	default:
		return fmt.Sprintf("Normal(%d)", int8(n))
	}
}

// ParseNormal parses "debit" or "credit".
func ParseNormal(text string) (Normal, error) {
	switch text {
	case "debit":
		return Debit, nil
	case "credit":
		return Credit, nil
	default:
		return 0, fmt.Errorf("%w: unknown normal balance %q", ErrInvalidAccount, text)
	}
}

// Account is a bucket that postings land in. Once opened, an account's
// currency and normal balance never change: rewriting them would silently
// reinterpret every entry already recorded against it.
type Account struct {
	Name     AccountName
	Currency Currency
	Normal   Normal

	// AllowNegative lets the balance go past zero in the account's normal
	// direction. It is false for accounts that must not be overdrawn, such as
	// a customer wallet, and true for the external and clearing accounts that
	// represent the other side of the world.
	AllowNegative bool

	Metadata map[string]string

	// OpenedAt is when the account starts to exist in business time, and
	// OpenedSeq is the event that recorded it.
	OpenedAt  time.Time
	OpenedSeq int64
}

// Validate reports whether the account definition is well formed.
func (a Account) Validate() error {
	if err := a.Name.Validate(); err != nil {
		return err
	}
	if err := a.Currency.Validate(); err != nil {
		return err
	}
	if err := a.Normal.Validate(); err != nil {
		return err
	}
	return validateMetadata(a.Metadata)
}

// Presented converts a signed, debit-positive internal balance into the
// account's own reading of it.
func (a Account) Presented(balance Amount) (Amount, error) {
	if a.Normal == Credit {
		return balance.Neg()
	}
	return balance, nil
}

// Limits on user-supplied metadata, which is stored verbatim and hashed into
// the event chain. The caps keep an event from becoming an unbounded blob.
const (
	MaxMetadataPairs    = 64
	MaxMetadataKeyLen   = 128
	MaxMetadataValueLen = 1024
)

func validateMetadata(metadata map[string]string) error {
	if len(metadata) > MaxMetadataPairs {
		return fmt.Errorf("%w: %d metadata keys, max %d", ErrInvalidAccount, len(metadata), MaxMetadataPairs)
	}
	for key, value := range metadata {
		switch {
		case key == "":
			return fmt.Errorf("%w: metadata has an empty key", ErrInvalidAccount)
		case len(key) > MaxMetadataKeyLen:
			return fmt.Errorf("%w: metadata key %q exceeds %d bytes", ErrInvalidAccount, key, MaxMetadataKeyLen)
		case len(value) > MaxMetadataValueLen:
			return fmt.Errorf("%w: metadata value for %q exceeds %d bytes", ErrInvalidAccount, key, MaxMetadataValueLen)
		}
	}
	return nil
}
