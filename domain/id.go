package domain

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// ID is a UUIDv7: 48 bits of Unix milliseconds followed by 74 bits of
// randomness. Time-ordered identifiers keep B-tree inserts at the right edge
// of the index instead of scattering them, which matters when every write is
// an append.
//
// The ledger generates its own rather than taking a dependency, so the core
// package needs nothing outside the standard library.
type ID [16]byte

// NewID returns a new time-ordered ID.
func NewID() ID { return newIDAt(time.Now()) }

func newIDAt(at time.Time) ID {
	var id ID
	// crypto/rand.Read is documented never to fail as of Go 1.24.
	rand.Read(id[6:])

	ms := uint64(at.UnixMilli())
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], ms)
	copy(id[0:6], buf[2:8])

	id[6] = (id[6] & 0x0f) | 0x70 // version 7
	id[8] = (id[8] & 0x3f) | 0x80 // RFC 4122 variant
	return id
}

// Time returns the millisecond timestamp encoded in the ID.
func (id ID) Time() time.Time {
	var buf [8]byte
	copy(buf[2:8], id[0:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(buf[:]))).UTC()
}

// IsZero reports whether the ID is unset.
func (id ID) IsZero() bool { return id == ID{} }

// String returns the canonical hyphenated hex form.
func (id ID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], id[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], id[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], id[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], id[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], id[10:16])
	return string(buf[:])
}

// ParseID parses the canonical hyphenated hex form.
func ParseID(text string) (ID, error) {
	var id ID
	if len(text) != 36 || text[8] != '-' || text[13] != '-' || text[18] != '-' || text[23] != '-' {
		return ID{}, fmt.Errorf("%w: %q is not a UUID", ErrInvalidID, text)
	}
	src := []byte(text[0:8] + text[9:13] + text[14:18] + text[19:23] + text[24:36])
	if _, err := hex.Decode(id[:], src); err != nil {
		return ID{}, fmt.Errorf("%w: %q is not a UUID", ErrInvalidID, text)
	}
	return id, nil
}

// MarshalText implements [encoding.TextMarshaler], which is what makes IDs
// render as strings in JSON payloads.
func (id ID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText implements [encoding.TextUnmarshaler].
func (id *ID) UnmarshalText(text []byte) error {
	parsed, err := ParseID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
