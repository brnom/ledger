package domain

import "time"

// TimePrecision is the resolution every timestamp in the ledger is stored at.
//
// This is not cosmetic. Event hashes cover their timestamps, and PostgreSQL's
// timestamptz holds microseconds. If the ledger hashed a nanosecond-precision
// time and then read it back from the database, the recomputed hash would not
// match the stored one and the chain would appear broken. Truncating on the
// way in makes an event's hash survive a round trip through storage.
const TimePrecision = time.Microsecond

// NormalizeTime puts a timestamp in the single representation the ledger uses:
// UTC, truncated to [TimePrecision].
func NormalizeTime(t time.Time) time.Time {
	return t.UTC().Truncate(TimePrecision)
}
