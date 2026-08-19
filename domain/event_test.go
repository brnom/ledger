package domain

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustID(t *testing.T, s string) ID {
	t.Helper()
	id, err := ParseID(s)
	if err != nil {
		t.Fatalf("ParseID(%q): %v", s, err)
	}
	return id
}

// A fixed event, so hashes and encodings are reproducible across runs.
func goldenEvent(t *testing.T) Event {
	t.Helper()
	brl := MustCurrency("BRL")
	tx := Transaction{
		ID:          mustID(t, "01920000-0000-7000-8000-000000000001"),
		EffectiveAt: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
		Reference:   "e2e-abc",
		Postings: []Posting{
			Cr("liabilities:users:1", FromMinor(brl, 10000)),
			Dr("assets:cash", FromMinor(brl, 9500)),
			Dr("expenses:fees", FromMinor(brl, 500)),
		},
		Metadata: map[string]string{"channel": "pix", "acquirer": "cielo"},
	}
	payload, err := NewTransactionCommitted(tx)
	if err != nil {
		t.Fatalf("NewTransactionCommitted: %v", err)
	}
	event, err := NewEvent("main", payload, time.Date(2026, 8, 18, 12, 0, 1, 500000, time.UTC), "key-1")
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	event.ID = mustID(t, "01920000-0000-7000-8000-0000000000ff")
	event.Seal(1, GenesisHash)
	return event
}

func TestCanonicalJSONGolden(t *testing.T) {
	event := goldenEvent(t)
	const want = `{"effective_at":"2026-08-18T12:00:00Z","id":"01920000-0000-7000-8000-000000000001",` +
		`"metadata":{"acquirer":"cielo","channel":"pix"},` +
		`"postings":[` +
		`{"account":"liabilities:users:1","amount":"-100.00","currency":"BRL","scale":2},` +
		`{"account":"assets:cash","amount":"95.00","currency":"BRL","scale":2},` +
		`{"account":"expenses:fees","amount":"5.00","currency":"BRL","scale":2}],` +
		`"reference":"e2e-abc"}`
	if got := string(event.Payload); got != want {
		t.Errorf("canonical payload:\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalJSONIsDeterministic(t *testing.T) {
	opened := AccountOpened{
		Name: "assets:cash", Currency: "BRL", Scale: 2, Normal: "debit",
		OpenedAt: testTime,
		Metadata: map[string]string{
			"z": "1", "a": "2", "m": "3", "b": "4", "y": "5", "c": "6", "x": "7", "d": "8",
		},
	}
	first, err := canonicalJSON(opened)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	// Go randomizes map iteration order, so repeating the encode is a real
	// test that key ordering does not leak into the bytes we hash.
	for i := 0; i < 200; i++ {
		again, err := canonicalJSON(opened)
		if err != nil {
			t.Fatalf("canonicalJSON: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("encoding %d differs:\n got %s\nwant %s", i, again, first)
		}
	}
	if !strings.Contains(string(first), `"metadata":{"a":"2","b":"4","c":"6","d":"8"`) {
		t.Errorf("metadata keys are not sorted: %s", first)
	}
}

func TestCanonicalJSONRejectsFloats(t *testing.T) {
	// A float would make the hash depend on formatting rather than value, and
	// there is no legitimate reason for one to appear in a ledger payload.
	if _, err := canonicalJSON(map[string]any{"rate": 1.5}); !errors.Is(err, ErrEncoding) {
		t.Errorf("canonicalJSON(1.5) = %v, want ErrEncoding", err)
	}
	if _, err := canonicalJSON(map[string]any{"n": 42}); err != nil {
		t.Errorf("canonicalJSON(42) = %v, want nil", err)
	}
}

func TestCanonicalJSONEscapesAndNests(t *testing.T) {
	got, err := canonicalJSON(map[string]any{
		"b": []any{1, "two", nil, true},
		"a": map[string]any{"quote\"": "tab\t", "unicode": "ação"},
	})
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	const want = `{"a":{"quote\"":"tab\t","unicode":"ação"},"b":[1,"two",null,true]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestEventHashGolden pins the hashing scheme. If this fails, the scheme
// changed and every chain already in storage is invalidated: bump hashDomain
// deliberately rather than editing the expectation.
func TestEventHashGolden(t *testing.T) {
	event := goldenEvent(t)
	const want = "013e615c675a7b9962dfd7e3061401a8f61d86588ebab029110a8a5f00f2347a"
	if got := hex.EncodeToString(event.Hash[:]); got != want {
		t.Errorf("event hash = %s, want %s", got, want)
	}
}

func TestEventSealAndVerify(t *testing.T) {
	event := goldenEvent(t)
	if event.Seq != 1 {
		t.Errorf("Seq = %d, want 1", event.Seq)
	}
	if event.PrevHash != GenesisHash {
		t.Errorf("PrevHash = %x, want genesis", event.PrevHash)
	}
	if err := event.Verify(); err != nil {
		t.Errorf("Verify() = %v, want nil", err)
	}
}

func TestEventDetectsTampering(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{"payload", func(event *Event) {
			event.Payload = []byte(strings.Replace(string(event.Payload), "95.00", "99.00", 1))
		}},
		{"sequence", func(event *Event) { event.Seq = 2 }},
		{"ledger", func(event *Event) { event.LedgerID = "other" }},
		{"type", func(event *Event) { event.Type = EventAccountOpened }},
		{"recorded time", func(event *Event) { event.RecordedAt = event.RecordedAt.Add(time.Microsecond) }},
		{"idempotency key", func(event *Event) { event.IdempotencyKey = "key-2" }},
		{"event id", func(event *Event) { event.ID = NewID() }},
		{"prev hash", func(event *Event) { event.PrevHash[0] ^= 0xff }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := goldenEvent(t)
			tt.mutate(&event)
			if err := event.Verify(); !errors.Is(err, ErrChainBroken) {
				t.Errorf("Verify() after editing %s = %v, want ErrChainBroken", tt.name, err)
			}
		})
	}
}

// buildChain seals n trivial events into a valid chain.
func buildChain(t *testing.T, n int) []Event {
	t.Helper()
	brl := MustCurrency("BRL")
	events := make([]Event, 0, n)
	prev := GenesisHash
	for i := 0; i < n; i++ {
		tx := NewTransfer("assets:cash", "expenses:fees", FromMinor(brl, int64(i+1)*100), testTime)
		payload, err := NewTransactionCommitted(tx)
		if err != nil {
			t.Fatalf("NewTransactionCommitted: %v", err)
		}
		event, err := NewEvent("main", payload, testTime.Add(time.Duration(i)*time.Second), "")
		if err != nil {
			t.Fatalf("NewEvent: %v", err)
		}
		event.Seal(int64(i+1), prev)
		prev = event.Hash
		events = append(events, event)
	}
	return events
}

func TestVerifyChain(t *testing.T) {
	t.Run("intact", func(t *testing.T) {
		events := buildChain(t, 10)
		last, err := VerifyChain(events, 1, GenesisHash)
		if err != nil {
			t.Fatalf("VerifyChain() = %v, want nil", err)
		}
		if last != events[len(events)-1].Hash {
			t.Error("VerifyChain returned the wrong terminal hash")
		}
	})

	t.Run("verifiable in chunks", func(t *testing.T) {
		events := buildChain(t, 10)
		mid, err := VerifyChain(events[:4], 1, GenesisHash)
		if err != nil {
			t.Fatalf("first chunk: %v", err)
		}
		if _, err := VerifyChain(events[4:], 5, mid); err != nil {
			t.Fatalf("second chunk: %v", err)
		}
	})

	t.Run("gap in sequence", func(t *testing.T) {
		events := buildChain(t, 5)
		if _, err := VerifyChain(append(events[:2:2], events[3:]...), 1, GenesisHash); !errors.Is(err, ErrChainBroken) {
			t.Errorf("err = %v, want ErrChainBroken", err)
		}
	})

	t.Run("removed event breaks the link", func(t *testing.T) {
		events := buildChain(t, 5)
		// Renumber so the sequence looks contiguous; the chain still exposes
		// the removal, which is the whole point of linking hashes.
		spliced := append(events[:2:2], events[3:]...)
		for i := range spliced {
			spliced[i].Seq = int64(i + 1)
		}
		if _, err := VerifyChain(spliced, 1, GenesisHash); !errors.Is(err, ErrChainBroken) {
			t.Errorf("err = %v, want ErrChainBroken", err)
		}
	})

	t.Run("tampered event in the middle", func(t *testing.T) {
		events := buildChain(t, 5)
		events[2].Payload = []byte(strings.Replace(string(events[2].Payload), "3.00", "9.00", 1))
		if _, err := VerifyChain(events, 1, GenesisHash); !errors.Is(err, ErrChainBroken) {
			t.Errorf("err = %v, want ErrChainBroken", err)
		}
	})

	t.Run("wrong starting hash", func(t *testing.T) {
		events := buildChain(t, 3)
		if _, err := VerifyChain(events, 1, [32]byte{1}); !errors.Is(err, ErrChainBroken) {
			t.Errorf("err = %v, want ErrChainBroken", err)
		}
	})
}

func TestDecodePayloadRoundTrip(t *testing.T) {
	brl := MustCurrency("BRL")
	acct := Account{
		Name: "liabilities:users:1", Currency: brl, Normal: Credit,
		OpenedAt: testTime, Metadata: map[string]string{"kyc": "full"},
	}
	opened, err := NewAccountOpened(acct)
	if err != nil {
		t.Fatalf("NewAccountOpened: %v", err)
	}
	tx := NewTransfer("assets:cash", "expenses:fees", FromMinor(brl, 1234), testTime)
	committed, err := NewTransactionCommitted(tx)
	if err != nil {
		t.Fatalf("NewTransactionCommitted: %v", err)
	}
	reverted, err := NewTransactionReverted(tx, NewID(), testTime.Add(time.Hour), "chargeback")
	if err != nil {
		t.Fatalf("NewTransactionReverted: %v", err)
	}

	for _, want := range []Payload{opened, committed, reverted} {
		t.Run(string(want.EventType()), func(t *testing.T) {
			event, err := NewEvent("main", want, testTime, "")
			if err != nil {
				t.Fatalf("NewEvent: %v", err)
			}
			got, err := event.DecodePayload()
			if err != nil {
				t.Fatalf("DecodePayload: %v", err)
			}
			if got.EventType() != want.EventType() {
				t.Fatalf("type = %q, want %q", got.EventType(), want.EventType())
			}
			// Re-encoding what we decoded must reproduce the stored bytes, or
			// a replay would compute a different hash than the original.
			again, err := canonicalJSON(got)
			if err != nil {
				t.Fatalf("canonicalJSON: %v", err)
			}
			if string(again) != string(event.Payload) {
				t.Errorf("re-encode differs:\n got %s\nwant %s", again, event.Payload)
			}
		})
	}
}

func TestDecodePayloadRejectsUnknown(t *testing.T) {
	event := goldenEvent(t)
	event.Type = "account.frozen"
	if _, err := event.DecodePayload(); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("err = %v, want ErrUnknownEvent", err)
	}

	event = goldenEvent(t)
	event.Payload = []byte(`{"id":"01920000-0000-7000-8000-000000000001","surprise":true}`)
	if _, err := event.DecodePayload(); !errors.Is(err, ErrUnknownEvent) {
		t.Errorf("unknown field: err = %v, want ErrUnknownEvent", err)
	}
}

func TestAccountOpenedRoundTripsAccount(t *testing.T) {
	want := Account{
		Name: "liabilities:users:1", Currency: MustCurrency("BRL"), Normal: Credit,
		AllowNegative: true, OpenedAt: testTime,
	}
	payload, err := NewAccountOpened(want)
	if err != nil {
		t.Fatalf("NewAccountOpened: %v", err)
	}
	got, err := payload.Account()
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if got.Name != want.Name || got.Currency != want.Currency ||
		got.Normal != want.Normal || got.AllowNegative != want.AllowNegative ||
		!got.OpenedAt.Equal(want.OpenedAt) {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}
}

func TestNewEventValidation(t *testing.T) {
	ok, err := NewAccountOpened(Account{Name: "assets:cash", Currency: MustCurrency("BRL"), Normal: Debit, OpenedAt: testTime})
	if err != nil {
		t.Fatalf("NewAccountOpened: %v", err)
	}

	tests := []struct {
		name     string
		ledgerID string
		payload  Payload
		key      string
		want     error
	}{
		{"valid", "main", ok, "k", nil},
		{"valid with dashes", "tenant-42_x", ok, "", nil},
		{"empty ledger", "", ok, "", ErrInvalidID},
		{"ledger with colon", "main:sub", ok, "", ErrInvalidID},
		{"oversized ledger id", strings.Repeat("a", MaxLedgerIDLen+1), ok, "", ErrInvalidID},
		{"oversized idempotency key", "main", ok, strings.Repeat("k", MaxIdempotencyKeyLen+1), ErrInvalidID},
		{"invalid payload", "main", AccountOpened{Name: "bad name"}, "", ErrInvalidAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewEvent(tt.ledgerID, tt.payload, testTime, tt.key)
			if tt.want == nil && err != nil {
				t.Fatalf("NewEvent() = %v, want nil", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("NewEvent() = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestEventHashSurvivesTimeTruncation guards the reason timestamps are
// normalized at ingress: an event hashed with nanosecond precision would stop
// verifying once PostgreSQL handed it back with microsecond precision.
func TestEventHashSurvivesTimeTruncation(t *testing.T) {
	payload, err := NewTransactionCommitted(NewTransfer(
		"assets:cash", "expenses:fees", FromMinor(MustCurrency("BRL"), 100),
		time.Date(2026, 8, 18, 12, 0, 0, 123456789, time.UTC),
	))
	if err != nil {
		t.Fatalf("NewTransactionCommitted: %v", err)
	}
	event, err := NewEvent("main", payload, time.Date(2026, 8, 18, 12, 0, 0, 987654321, time.UTC), "")
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	event.Seal(1, GenesisHash)

	if event.RecordedAt.Nanosecond()%1000 != 0 {
		t.Errorf("RecordedAt = %v, want microsecond precision", event.RecordedAt)
	}
	// Simulate the storage round trip.
	event.RecordedAt = event.RecordedAt.Truncate(time.Microsecond)
	if err := event.Verify(); err != nil {
		t.Errorf("Verify() after a storage round trip = %v, want nil", err)
	}
}

// TestEventHashIsLocationIndependent guards the other half of normalization:
// the same instant expressed in another zone must hash identically.
func TestEventHashIsLocationIndependent(t *testing.T) {
	saoPaulo := time.FixedZone("-03", -3*3600)
	utc := time.Date(2026, 8, 18, 15, 0, 0, 0, time.UTC)
	local := utc.In(saoPaulo)

	txID := mustID(t, "01920000-0000-7000-8000-000000000001")
	hashAt := func(when time.Time) [32]byte {
		tx := NewTransfer("assets:cash", "expenses:fees", FromMinor(MustCurrency("BRL"), 100), when)
		tx.ID = txID
		payload, err := NewTransactionCommitted(tx)
		if err != nil {
			t.Fatalf("NewTransactionCommitted: %v", err)
		}
		event, err := NewEvent("main", payload, when, "")
		if err != nil {
			t.Fatalf("NewEvent: %v", err)
		}
		event.ID = mustID(t, "01920000-0000-7000-8000-0000000000ff")
		event.Seal(1, GenesisHash)
		return event.Hash
	}
	if hashAt(utc) != hashAt(local) {
		t.Error("the same instant hashed differently in two time zones")
	}
}
