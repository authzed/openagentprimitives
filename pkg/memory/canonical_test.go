package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestCanonicalContent covers the reshapings a JSONB round trip applies, and
// the ones it must NOT be read as applying: canonicalization has to erase the
// storage layer's byte choices without erasing a real difference in payload.
func TestCanonicalContent(t *testing.T) {
	cases := []struct {
		name  string
		a, b  string
		equal bool
	}{
		{
			name:  "reordered object keys: equal, this is what JSONB does",
			a:     `{"alpha":1,"zeta":2}`,
			b:     `{"zeta":2,"alpha":1}`,
			equal: true,
		},
		{
			name:  "added whitespace: equal, postgres re-emits with its own spacing",
			a:     `{"a":1,"b":[2,3]}`,
			b:     "{\n  \"a\": 1,\n  \"b\": [ 2, 3 ]\n}",
			equal: true,
		},
		{
			name:  "reordered keys nested inside an array element: equal",
			a:     `{"c":[{"x":1,"y":2}]}`,
			b:     `{"c":[{"y":2,"x":1}]}`,
			equal: true,
		},
		{
			name:  "different value: NOT equal, a rewrite is still a rewrite",
			a:     `{"a":1}`,
			b:     `{"a":2}`,
			equal: false,
		},
		{
			name:  "reordered ARRAY elements: NOT equal, arrays are ordered",
			a:     `{"a":[1,2]}`,
			b:     `{"a":[2,1]}`,
			equal: false,
		},
		{
			name:  "an added key: NOT equal",
			a:     `{"a":1}`,
			b:     `{"a":1,"b":2}`,
			equal: false,
		},
		{
			name:  "invalid JSON compared with itself: equal (falls back to raw bytes)",
			a:     `not json`,
			b:     `not json`,
			equal: true,
		},
		{
			name:  "invalid JSON differing: NOT equal, the fallback is byte-for-byte",
			a:     `not json`,
			b:     `also not json`,
			equal: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := string(memory.CanonicalContent(json.RawMessage(tc.a)))
			cb := string(memory.CanonicalContent(json.RawMessage(tc.b)))
			if tc.equal {
				assert.Equal(t, ca, cb)
			} else {
				assert.NotEqual(t, ca, cb)
			}
		})
	}

	assert.Nil(t, memory.CanonicalContent(nil), "nil content canonicalizes to nil")
	assert.Nil(t, memory.CanonicalContent(json.RawMessage{}), "empty content canonicalizes to nil")
}

// TestCanonicalTime pins the truncation to what the postgres encoder does to a
// value on the way in — truncate to the microsecond, in UTC — so a caller's
// full-precision timestamp still compares equal to the stored one.
func TestCanonicalTime(t *testing.T) {
	ns := time.Date(2026, 8, 11, 12, 0, 0, 123456789, time.UTC)
	us := time.Date(2026, 8, 11, 12, 0, 0, 123456000, time.UTC)
	assert.True(t, memory.CanonicalTime(ns).Equal(memory.CanonicalTime(us)),
		"sub-microsecond precision does not survive TIMESTAMPTZ and must not decide sameness")

	assert.False(t, memory.CanonicalTime(ns).Equal(memory.CanonicalTime(ns.Add(time.Microsecond))),
		"a whole microsecond apart is a real difference and must survive")

	east := time.FixedZone("UTC+2", 2*60*60)
	assert.True(t, memory.CanonicalTime(ns).Equal(memory.CanonicalTime(ns.In(east))),
		"the same instant in another zone is the same instant")
	assert.Equal(t, time.UTC, memory.CanonicalTime(ns.In(east)).Location(),
		"canonical form is UTC")
}
