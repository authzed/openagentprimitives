package memory

import (
	"encoding/json"
	"time"
)

// This file holds the canonical forms an Entry's fields take when two entries
// must be compared for SAMENESS across a storage round trip.
//
// A durable backend does not hand an entry back the way it took it: postgres
// stores content in JSONB, which re-emits object keys in its own order and
// whitespace, and created_at in TIMESTAMPTZ, which pgx truncates to
// microseconds. Comparing raw bytes and full-precision timestamps therefore
// answers "different" for two entries that are the same one — and only on
// postgres, since inmem holds entries verbatim and sqlite stores content as TEXT
// and created_at as integer nanoseconds. That asymmetry lets raw-byte
// comparisons ship green.
//
// Anything comparing a caller-held Entry against one that came back out of a
// backend belongs here rather than reaching for bytes.Equal.

// CanonicalContent normalizes JSON content so equality is invariant under a
// JSONB round trip: the trip through interface{} sorts object keys and
// normalizes numbers, and json.Marshal re-emits without whitespace. Applied to
// BOTH sides of a comparison, all it asks of the storage layer is semantic JSON
// equality — exactly what JSONB preserves.
//
// Invalid JSON falls back to the raw bytes, so a malformed payload still
// compares byte-for-byte rather than collapsing to equal-with-everything.
func CanonicalContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v) // json.Marshal sorts map keys
	if err != nil {
		return raw
	}
	return out
}

// CanonicalTime truncates t to the finest precision every backend can store.
// Postgres TIMESTAMPTZ holds microseconds and pgx truncates on the way in, so
// the nanoseconds a time.Now() CreatedAt carries do not survive the trip and
// must not participate in a sameness comparison. Truncation (not rounding) and
// UTC, to agree exactly with what the encoder did to the stored value.
func CanonicalTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}
