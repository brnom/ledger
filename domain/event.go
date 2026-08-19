package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// EventType names a kind of fact the ledger records. Types are strings on the
// wire and in storage. A reader written today can therefore skip an event kind
// added tomorrow instead of failing to parse the stream.
type EventType string

// The event types the ledger records.
const (
	EventAccountOpened        EventType = "account.opened"
	EventTransactionCommitted EventType = "transaction.committed"
	EventTransactionReverted  EventType = "transaction.reverted"
)

// MaxIdempotencyKeyLen bounds a caller-supplied idempotency key.
const MaxIdempotencyKeyLen = 255

// hashDomain separates this ledger's hashes from any other use of SHA-256, and
// pins the hashing scheme to a version. Changing how events are hashed means
// changing this string, which makes the break explicit rather than silently
// invalidating every chain in storage.
const hashDomain = "ledger.event.v1"

// Payload is the typed body of an event.
type Payload interface {
	// EventType reports which event this payload belongs to.
	EventType() EventType
	// Validate reports whether the payload is internally consistent.
	Validate() error
}

// PostingWire is a posting in its stored form. Amounts travel as decimal
// strings with their currency and scale alongside. A JSON number would invite
// a float somewhere down the line. A bare minor-unit integer would be
// meaningless if a reader misread the currency's scale.
type PostingWire struct {
	Account  AccountName `json:"account"`
	Amount   string      `json:"amount"`
	Currency string      `json:"currency"`
	Scale    uint8       `json:"scale"`
}

// AccountOpened records an account entering the book.
type AccountOpened struct {
	Name          AccountName       `json:"name"`
	Currency      string            `json:"currency"`
	Scale         uint8             `json:"scale"`
	Normal        string            `json:"normal"`
	AllowNegative bool              `json:"allow_negative"`
	OpenedAt      time.Time         `json:"opened_at"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// EventType implements [Payload].
func (AccountOpened) EventType() EventType { return EventAccountOpened }

// Validate implements [Payload].
func (opened AccountOpened) Validate() error {
	// Check the name before rebuilding the account so a bad name is reported as
	// such rather than as whatever the reconstruction trips over first.
	if err := opened.Name.Validate(); err != nil {
		return err
	}
	acct, err := opened.Account()
	if err != nil {
		return err
	}
	return acct.Validate()
}

// Account rebuilds the domain account this payload describes.
func (opened AccountOpened) Account() (Account, error) {
	cur, err := NewCurrency(opened.Currency, opened.Scale)
	if err != nil {
		return Account{}, err
	}
	normal, err := ParseNormal(opened.Normal)
	if err != nil {
		return Account{}, err
	}
	return Account{
		Name:          opened.Name,
		Currency:      cur,
		Normal:        normal,
		AllowNegative: opened.AllowNegative,
		Metadata:      opened.Metadata,
		OpenedAt:      NormalizeTime(opened.OpenedAt),
	}, nil
}

// NewAccountOpened builds the payload that opens acct.
func NewAccountOpened(acct Account) (AccountOpened, error) {
	if err := acct.Validate(); err != nil {
		return AccountOpened{}, err
	}
	return AccountOpened{
		Name:          acct.Name,
		Currency:      acct.Currency.Code,
		Scale:         acct.Currency.Scale,
		Normal:        acct.Normal.String(),
		AllowNegative: acct.AllowNegative,
		OpenedAt:      NormalizeTime(acct.OpenedAt),
		Metadata:      acct.Metadata,
	}, nil
}

// TransactionCommitted records a balanced transaction landing in the book.
type TransactionCommitted struct {
	ID          ID                `json:"id"`
	EffectiveAt time.Time         `json:"effective_at"`
	Postings    []PostingWire     `json:"postings"`
	Reference   string            `json:"reference,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// EventType implements [Payload].
func (TransactionCommitted) EventType() EventType { return EventTransactionCommitted }

// Validate implements [Payload].
func (committed TransactionCommitted) Validate() error {
	tx, err := committed.Transaction()
	if err != nil {
		return err
	}
	return tx.Validate()
}

// Transaction rebuilds the domain transaction this payload describes.
func (committed TransactionCommitted) Transaction() (Transaction, error) {
	postings, err := postingsFromWire(committed.Postings)
	if err != nil {
		return Transaction{}, err
	}
	return Transaction{
		ID:          committed.ID,
		EffectiveAt: NormalizeTime(committed.EffectiveAt),
		Postings:    postings,
		Reference:   committed.Reference,
		Metadata:    committed.Metadata,
	}, nil
}

// NewTransactionCommitted builds the payload that commits tx.
func NewTransactionCommitted(tx Transaction) (TransactionCommitted, error) {
	if err := tx.Validate(); err != nil {
		return TransactionCommitted{}, err
	}
	return TransactionCommitted{
		ID:          tx.ID,
		EffectiveAt: NormalizeTime(tx.EffectiveAt),
		Postings:    postingsToWire(tx.Postings),
		Reference:   tx.Reference,
		Metadata:    tx.Metadata,
	}, nil
}

// TransactionReverted records the compensating transaction that undoes an
// earlier one. The original is never touched. This is a new transaction that
// names what it cancels.
type TransactionReverted struct {
	ID          ID                `json:"id"`
	RevertsID   ID                `json:"reverts_id"`
	EffectiveAt time.Time         `json:"effective_at"`
	Postings    []PostingWire     `json:"postings"`
	Reason      string            `json:"reason,omitempty"`
	Reference   string            `json:"reference,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// EventType implements [Payload].
func (TransactionReverted) EventType() EventType { return EventTransactionReverted }

// Validate implements [Payload].
func (reverted TransactionReverted) Validate() error {
	if reverted.RevertsID.IsZero() {
		return fmt.Errorf("%w: reversal %s names no original transaction", ErrInvalidTransaction, reverted.ID)
	}
	if reverted.ID == reverted.RevertsID {
		return fmt.Errorf("%w: transaction %s reverts itself", ErrInvalidTransaction, reverted.ID)
	}
	tx, err := reverted.Transaction()
	if err != nil {
		return err
	}
	return tx.Validate()
}

// Transaction rebuilds the compensating transaction this payload describes.
func (reverted TransactionReverted) Transaction() (Transaction, error) {
	postings, err := postingsFromWire(reverted.Postings)
	if err != nil {
		return Transaction{}, err
	}
	return Transaction{
		ID:          reverted.ID,
		EffectiveAt: NormalizeTime(reverted.EffectiveAt),
		Postings:    postings,
		Reference:   reverted.Reference,
		Metadata:    reverted.Metadata,
	}, nil
}

// NewTransactionReverted builds the payload that reverts original.
func NewTransactionReverted(original Transaction, id ID, effectiveAt time.Time, reason string) (TransactionReverted, error) {
	rev := original.Reverse(id, effectiveAt)
	if err := rev.Validate(); err != nil {
		return TransactionReverted{}, err
	}
	return TransactionReverted{
		ID:          rev.ID,
		RevertsID:   original.ID,
		EffectiveAt: NormalizeTime(rev.EffectiveAt),
		Postings:    postingsToWire(rev.Postings),
		Reason:      reason,
		Reference:   rev.Reference,
	}, nil
}

func postingsToWire(postings []Posting) []PostingWire {
	out := make([]PostingWire, len(postings))
	for i, posting := range postings {
		out[i] = PostingWire{
			Account:  posting.Account,
			Amount:   posting.Amount.Format(),
			Currency: posting.Amount.Currency().Code,
			Scale:    posting.Amount.Currency().Scale,
		}
	}
	return out
}

func postingsFromWire(wire []PostingWire) ([]Posting, error) {
	out := make([]Posting, len(wire))
	for i, wirePosting := range wire {
		cur, err := NewCurrency(wirePosting.Currency, wirePosting.Scale)
		if err != nil {
			return nil, fmt.Errorf("posting %d: %w", i, err)
		}
		amt, err := ParseAmount(cur, wirePosting.Amount)
		if err != nil {
			return nil, fmt.Errorf("posting %d: %w", i, err)
		}
		out[i] = Posting{Account: wirePosting.Account, Amount: amt}
	}
	return out, nil
}

// Event is one immutable fact in a ledger's stream, together with the chain
// linkage that makes the stream tamper-evident.
//
// Events are appended and never modified. Seq is contiguous from 1 within a
// ledger. Each event's PrevHash is the hash of the event before it. A change
// to any event, or its removal, therefore breaks every hash that follows.
type Event struct {
	// Seq is the event's position in its ledger's stream, starting at 1 with no
	// gaps. It doubles as the "recorded" axis of the ledger's bitemporal model:
	// an event with a lower Seq was known earlier.
	Seq int64

	// ID identifies the event itself, distinct from any identifier inside the
	// payload.
	ID ID

	// LedgerID names the stream. One ledger is one book with one chain.
	LedgerID string

	Type EventType

	// Payload is the canonical JSON encoding of a [Payload]. It is stored and
	// hashed byte for byte, so it must never be re-serialized in transit.
	Payload []byte

	// RecordedAt is when the ledger learned the fact. Business time lives in the
	// payload instead, which is what lets a correction be recorded now and take
	// effect in the past.
	RecordedAt time.Time

	// IdempotencyKey is the caller's key for the command that produced this
	// event, empty if none was supplied.
	IdempotencyKey string

	PrevHash [32]byte
	Hash     [32]byte
}

// GenesisHash is the PrevHash of the first event in a ledger.
var GenesisHash = [32]byte{}

// NewEvent builds an unsealed event carrying payload. The returned event has
// no Seq and no hashes. [Event.Seal] assigns those when the event is appended,
// because its position in the stream is not known until then.
func NewEvent(ledgerID string, payload Payload, recordedAt time.Time, idempotencyKey string) (Event, error) {
	if err := ValidateLedgerID(ledgerID); err != nil {
		return Event{}, err
	}
	if len(idempotencyKey) > MaxIdempotencyKeyLen {
		return Event{}, fmt.Errorf("%w: idempotency key is %d bytes, max %d",
			ErrInvalidID, len(idempotencyKey), MaxIdempotencyKeyLen)
	}
	if err := payload.Validate(); err != nil {
		return Event{}, err
	}
	encoded, err := canonicalJSON(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{
		ID:             NewID(),
		LedgerID:       ledgerID,
		Type:           payload.EventType(),
		Payload:        encoded,
		RecordedAt:     NormalizeTime(recordedAt),
		IdempotencyKey: idempotencyKey,
	}, nil
}

// Seal fixes the event at position seq behind prev and computes its hash. It
// is called once, by the store, inside the transaction that appends the event.
func (e *Event) Seal(seq int64, prev [32]byte) {
	e.Seq = seq
	e.PrevHash = prev
	e.Hash = e.computeHash()
}

// Verify recomputes the event's hash and reports whether it still matches.
func (e Event) Verify() error {
	if got := e.computeHash(); got != e.Hash {
		return fmt.Errorf("%w: event %d (%s) hashes to %x, stored as %x",
			ErrChainBroken, e.Seq, e.ID, got[:8], e.Hash[:8])
	}
	return nil
}

// computeHash digests the event's fields as length-prefixed chunks. The
// prefixes make the encoding unambiguous: without them, two different events
// could concatenate to the same bytes and collide.
func (e Event) computeHash() [32]byte {
	hasher := sha256.New()
	writeChunk(hasher, []byte(hashDomain))
	writeChunk(hasher, e.PrevHash[:])
	var seq [8]byte
	binary.BigEndian.PutUint64(seq[:], uint64(e.Seq))
	writeChunk(hasher, seq[:])
	writeChunk(hasher, e.ID[:])
	writeChunk(hasher, []byte(e.LedgerID))
	writeChunk(hasher, []byte(e.Type))
	writeChunk(hasher, []byte(e.RecordedAt.Format(time.RFC3339Nano)))
	writeChunk(hasher, []byte(e.IdempotencyKey))
	writeChunk(hasher, e.Payload)
	return [32]byte(hasher.Sum(nil))
}

func writeChunk(hasher interface{ Write([]byte) (int, error) }, chunk []byte) {
	var lengthPrefix [8]byte
	binary.BigEndian.PutUint64(lengthPrefix[:], uint64(len(chunk)))
	hasher.Write(lengthPrefix[:])
	hasher.Write(chunk)
}

// DecodePayload returns the typed payload carried by the event.
func (e Event) DecodePayload() (Payload, error) {
	var target Payload
	switch e.Type {
	case EventAccountOpened:
		target = &AccountOpened{}
	case EventTransactionCommitted:
		target = &TransactionCommitted{}
	case EventTransactionReverted:
		target = &TransactionReverted{}
	default:
		return nil, fmt.Errorf("%w: event %d has unknown type %q", ErrUnknownEvent, e.Seq, e.Type)
	}
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return nil, fmt.Errorf("%w: event %d: %v", ErrUnknownEvent, e.Seq, err)
	}
	// Return the value, not the pointer, so callers type-switch on the same types
	// they constructed.
	switch decoded := target.(type) {
	case *AccountOpened:
		return *decoded, nil
	case *TransactionCommitted:
		return *decoded, nil
	case *TransactionReverted:
		return *decoded, nil
	default:
		panic("unreachable")
	}
}

// VerifyChain checks that events form an unbroken chain: contiguous sequence
// numbers, each event linked to the one before it, and every hash matching its
// contents. prev is the hash the chain must start from. That is [GenesisHash]
// for a whole stream, or the hash of the last verified event when the caller
// checks one chunk at a time.
func VerifyChain(events []Event, startSeq int64, prev [32]byte) ([32]byte, error) {
	for i, event := range events {
		switch {
		case event.Seq != startSeq+int64(i):
			return prev, fmt.Errorf("%w: expected event %d, found %d",
				ErrChainBroken, startSeq+int64(i), event.Seq)
		case event.PrevHash != prev:
			return prev, fmt.Errorf("%w: event %d links to %x, expected %x",
				ErrChainBroken, event.Seq, event.PrevHash[:8], prev[:8])
		}
		if err := event.Verify(); err != nil {
			return prev, err
		}
		prev = event.Hash
	}
	return prev, nil
}

// MaxLedgerIDLen bounds a ledger identifier.
const MaxLedgerIDLen = 128

func ValidateLedgerID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: ledger id is empty", ErrInvalidID)
	}
	if len(id) > MaxLedgerIDLen {
		return fmt.Errorf("%w: ledger id is %d bytes, max %d", ErrInvalidID, len(id), MaxLedgerIDLen)
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("%w: ledger id %q contains %q", ErrInvalidID, id, r)
		}
	}
	return nil
}

// canonicalJSON encodes v in a form that is byte-identical for equal values:
// object keys sorted, no insignificant whitespace, no floating-point numbers.
//
// The event hash covers these bytes, so the encoding has to be a function of
// the value alone. Go's struct field order would already be stable. A
// canonical form also survives another implementation decoding and re-encoding
// the payload. That is exactly what an auditor who verifies the chain
// independently does.
func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncoding, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEncoding, err)
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch node := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if node {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		// Floating-point has no place in a ledger, and its shortest representation
		// is not stable enough to hash. Money is already encoded as a decimal
		// string. Anything else numeric is a count.
		if strings.ContainsAny(node.String(), ".eE") {
			return fmt.Errorf("%w: non-integer number %s in payload", ErrEncoding, node)
		}
		buf.WriteString(node.String())
	case string:
		// Delegate string escaping to encoding/json so the output stays valid JSON
		// for every rune, including the ones that need \u escapes.
		esc, err := json.Marshal(node)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrEncoding, err)
		}
		buf.Write(esc)
	case []any:
		buf.WriteByte('[')
		for i, item := range node {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, node[key]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("%w: unsupported type %T in payload", ErrEncoding, value)
	}
	return nil
}
