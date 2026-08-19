package domain

import (
	"encoding/hex"
	"testing"
	"time"
)

// Command identity is what idempotency is built on, so it is pinned rather
// than merely exercised. A change to one of these hashes stops every recorded
// idempotency key from matching its own command. The ledger would answer a
// retry as a new request, or a genuinely new request as a replay. The failure
// is meant to be loud.
func TestFingerprintGolden(t *testing.T) {
	brl := MustCurrency("BRL")
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	id, err := ParseID("0195f3c0-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatalf("ParseID: %v", err)
	}

	tests := []struct {
		name string
		fn   func() ([32]byte, error)
		want string
	}{
		{
			name: "open account",
			fn: func() ([32]byte, error) {
				return FingerprintOpenAccount("liabilities:users:1", brl, Credit,
					false, at, map[string]string{"owner": "9f3c"})
			},
			want: "34f5cba2c55922fbe298bcd26608bf2254c4fbba685e451cd0922cb790b76102",
		},
		{
			name: "commit",
			fn: func() ([32]byte, error) {
				return FingerprintCommit(id, at, []Posting{
					Dr("assets:cash", FromMinor(brl, 10000)),
					Cr("liabilities:users:1", FromMinor(brl, 10000)),
				}, "pix-e2e-1", nil)
			},
			want: "8b1559da85ca69e5934283fe85f204c9be62a64496b4e35c456f6f4a2e0d615e",
		},
		{
			name: "revert",
			fn: func() ([32]byte, error) {
				return FingerprintRevert(id, at, "chargeback")
			},
			want: "b6ec216a752cf732c19dbea10bbaa5e8b077a046e7f2d245589bd3bdd4003b39",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp, err := tt.fn()
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			if got := hex.EncodeToString(fp[:]); got != tt.want {
				t.Errorf("fingerprint = %s, want %s", got, tt.want)
			}
		})
	}
}

// What the caller did not supply must not enter the fingerprint, or no retry
// would ever match. The ledger fills in a transaction id and an effective time
// when the caller omits them. Those differ on every attempt.
func TestFingerprintOmitsUnsetFields(t *testing.T) {
	brl := MustCurrency("BRL")
	postings := []Posting{
		Dr("assets:cash", FromMinor(brl, 100)),
		Cr("liabilities:users:1", FromMinor(brl, 100)),
	}

	bare, err := FingerprintCommit(ID{}, time.Time{}, postings, "", nil)
	if err != nil {
		t.Fatalf("FingerprintCommit: %v", err)
	}
	again, err := FingerprintCommit(ID{}, time.Time{}, postings, "", nil)
	if err != nil {
		t.Fatalf("FingerprintCommit: %v", err)
	}
	if bare != again {
		t.Error("the same command fingerprinted twice differently")
	}

	withID, err := FingerprintCommit(NewID(), time.Time{}, postings, "", nil)
	if err != nil {
		t.Fatalf("FingerprintCommit: %v", err)
	}
	if withID == bare {
		t.Error("supplying a transaction id did not change the fingerprint")
	}

	withTime, err := FingerprintCommit(ID{}, time.Now(), postings, "", nil)
	if err != nil {
		t.Fatalf("FingerprintCommit: %v", err)
	}
	if withTime == bare {
		t.Error("supplying an effective time did not change the fingerprint")
	}
}

// Fingerprints are taken below microsecond truncation, so two timestamps the
// ledger would store identically cannot produce two different identities.
func TestFingerprintNormalizesTime(t *testing.T) {
	brl := MustCurrency("BRL")
	base := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC)

	first, err := FingerprintOpenAccount("assets:cash", brl, Debit, true, base, nil)
	if err != nil {
		t.Fatalf("FingerprintOpenAccount: %v", err)
	}
	second, err := FingerprintOpenAccount("assets:cash", brl, Debit, true,
		base.Add(999*time.Nanosecond).In(time.FixedZone("BRT", -3*3600)), nil)
	if err != nil {
		t.Fatalf("FingerprintOpenAccount: %v", err)
	}
	if first != second {
		t.Error("two timestamps the ledger stores identically fingerprinted differently")
	}
}
