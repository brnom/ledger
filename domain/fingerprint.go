package domain

import (
	"crypto/sha256"
	"time"
)

// requestHashDomain separates request fingerprints from event hashes, so a
// value that hashes to one can never be mistaken for the other.
const requestHashDomain = "ledger.request.v1"

// Command identity.
//
// A fingerprint answers the only question idempotency turns on: are these two
// requests the same request? It covers what the caller supplied and nothing
// the ledger generates, so a retry of the same command fingerprints alike
// while a genuinely different command does not -- which is what lets a reused
// key be reported as a conflict rather than silently answered with someone
// else's result.
//
// That rule belongs to the ledger, not to the transport or the service that
// happens to apply it, which is why it is stated here and why the canonical
// encoding it depends on never has to leave this package.
type openAccountFingerprint struct {
	Kind          string            `json:"kind"`
	Name          AccountName       `json:"name"`
	Currency      string            `json:"currency"`
	Scale         uint8             `json:"scale"`
	Normal        string            `json:"normal"`
	AllowNegative bool              `json:"allow_negative"`
	EffectiveAt   string            `json:"effective_at,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

type commitFingerprint struct {
	Kind        string            `json:"kind"`
	ID          string            `json:"id,omitempty"`
	EffectiveAt string            `json:"effective_at,omitempty"`
	Postings    []PostingWire     `json:"postings"`
	Reference   string            `json:"reference,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type revertFingerprint struct {
	Kind        string `json:"kind"`
	RevertsID   string `json:"reverts_id"`
	EffectiveAt string `json:"effective_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// FingerprintOpenAccount identifies a request to open an account.
func FingerprintOpenAccount(name AccountName, cur Currency, normal Normal,
	allowNegative bool, effectiveAt time.Time, metadata map[string]string) ([32]byte, error) {

	return fingerprint(openAccountFingerprint{
		Kind:          "account.open",
		Name:          name,
		Currency:      cur.Code,
		Scale:         cur.Scale,
		Normal:        normal.String(),
		AllowNegative: allowNegative,
		EffectiveAt:   formatOptionalTime(effectiveAt),
		Metadata:      metadata,
	})
}

// FingerprintCommit identifies a request to commit a transaction.
func FingerprintCommit(id ID, effectiveAt time.Time, postings []Posting,
	reference string, metadata map[string]string) ([32]byte, error) {

	return fingerprint(commitFingerprint{
		Kind:        "transaction.commit",
		ID:          optionalID(id),
		EffectiveAt: formatOptionalTime(effectiveAt),
		Postings:    postingsToWire(postings),
		Reference:   reference,
		Metadata:    metadata,
	})
}

// FingerprintRevert identifies a request to revert a transaction.
func FingerprintRevert(txID ID, effectiveAt time.Time, reason string) ([32]byte, error) {
	return fingerprint(revertFingerprint{
		Kind:        "transaction.revert",
		RevertsID:   txID.String(),
		EffectiveAt: formatOptionalTime(effectiveAt),
		Reason:      reason,
	})
}

func fingerprint(v any) ([32]byte, error) {
	b, err := canonicalJSON(v)
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	writeChunk(h, []byte(requestHashDomain))
	writeChunk(h, b)
	return [32]byte(h.Sum(nil)), nil
}

// formatOptionalTime renders the caller's timestamp, keeping "unset" distinct
// from any particular instant. Resolving it to "now" before fingerprinting
// would give every retry a different fingerprint and defeat idempotency.
func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return NormalizeTime(t).Format(time.RFC3339Nano)
}

func optionalID(id ID) string {
	if id.IsZero() {
		return ""
	}
	return id.String()
}
