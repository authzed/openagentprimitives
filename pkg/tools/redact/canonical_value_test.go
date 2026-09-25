package redact

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonicalString is the single funnel every declared-sensitive value passes
// through on its way to a token. Anything it renders as "" is treated as
// "nothing to protect" and left in the args verbatim, with no descriptor
// emitted — so the set of values it cannot render is exactly the set of values
// that serialize in cleartext while the audit record stays silent about them.
//
// Two shapes fall through today:
//
//   - scalars Stringify does not enumerate (int32, json.Number, uint64, …).
//     Latent rather than live: MCP args arrive through encoding/json without
//     UseNumber, so every number is a float64. A single call to UseNumber, or
//     any in-process constructor of an args map, makes it live.
//   - composites, for RedactLeafAtPath specifically, which keys on Stringify
//     rather than canonicalString. Every composite stringifies to "", so all of
//     them collide on one cache entry and share one descriptor.

func TestCanonicalString_UnhandledScalarsCanonicalizeThroughJSON(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{name: "int32 renders through JSON, not empty", in: int32(4242), want: "4242"},
		{name: "json.Number renders as its raw number, not empty", in: json.Number("4242"), want: "4242"},
		{name: "uint64 renders through JSON, not empty", in: uint64(7), want: "7"},
		{name: "float32 renders through JSON, not empty", in: float32(1.5), want: "1.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, canonicalString(tc.in),
				"a declared-sensitive scalar must have SOME canonical form; %q means it is left in cleartext", "")
		})
	}
}

// TestRedactValueAtPath_UnhandledScalarIsRedactedAndRecorded is the end-to-end
// consequence: a scalar kind the stringifier does not enumerate must still be
// scrubbed from the emitted args AND recorded in the ID→descriptor map. Being
// absent from the descriptor map is the worse half — the audit record does not
// even show that a redaction was attempted.
func TestRedactValueAtPath_UnhandledScalarIsRedactedAndRecorded(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		literal string
	}{
		{name: "int32 secret: tokenized, descriptor emitted", value: int32(4242), literal: "4242"},
		{name: "json.Number secret: tokenized, descriptor emitted", value: json.Number("4242"), literal: "4242"},
		{name: "uint64 secret: tokenized, descriptor emitted", value: uint64(987654321), literal: "987654321"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{"apiKey": tc.value, "region": "us"}
			r := New()
			RedactValueAtPath(m, "apiKey", r, "apiKey")

			got := mustJSON(t, m)
			assert.NotContains(t, got, tc.literal, "a declared-sensitive value must not serialize in cleartext")
			assert.Contains(t, got, `<redacted id=`, "the value must be replaced by a token")
			assert.Len(t, r.Emit(), 1, "the audit record must show that a redaction happened")
		})
	}
}

// TestRedactValueAtPath_UnhandledScalarLeafIsScrubbedFromFreeText covers the
// other half of the validator contract: inner leaves are registered so the same
// secret reappearing in a CEL message or trace detail is scrubbed there too. A
// leaf the stringifier cannot render is never registered, so it survives.
func TestRedactValueAtPath_UnhandledScalarLeafIsScrubbedFromFreeText(t *testing.T) {
	m := map[string]any{"creds": map[string]any{"pin": int32(987654), "user": "operator"}}
	r := New()
	RedactValueAtPath(m, "creds", r, "creds")

	got := r.RedactInString("authentication failed for pin 987654")
	assert.NotContains(t, got, "987654", "an inner scalar leaf must be registered for free-text scrubbing")
}

// TestRedactLeafAtPath_DistinctCompositeLeavesGetDistinctDescriptors is the
// audit-integrity property RedactLeafAtPath loses by keying on Stringify: every
// composite stringifies to "", so two different secret objects dedupe onto one
// cache entry. The values are still scrubbed, but the emitted map claims both
// paths carried the FIRST path's value — a reader cannot tell the second
// redaction happened at all.
func TestRedactLeafAtPath_DistinctCompositeLeavesGetDistinctDescriptors(t *testing.T) {
	m := map[string]any{
		"primary":  map[string]any{"token": "tok_one"},
		"fallback": map[string]any{"token": "tok_two"},
	}
	r := New()
	RedactLeafAtPath(m, "primary", r, "primary")
	RedactLeafAtPath(m, "fallback", r, "fallback")

	got := mustJSON(t, m)
	assert.NotContains(t, got, "tok_one", "neither secret may survive")
	assert.NotContains(t, got, "tok_two", "neither secret may survive")

	emitted := r.Emit()
	require.Len(t, emitted, 2, "two distinct composite leaves must mint two descriptors, got %#v", emitted)
	assert.Equal(t, "primary", emitted["1"].Name)
	assert.Equal(t, "fallback", emitted["2"].Name)
	assert.Equal(t, `{"fallback":"<redacted id=\"2\"/>","primary":"<redacted id=\"1\"/>"}`, got,
		"each composite leaf must carry its own token, not share one")
}

// TestRedactLeafAtPath_NothingToProtectLeavesValueInPlace mirrors the
// RedactValueAtPath contract on the leaf path: a value with no canonical form
// (nil, the empty string) has nothing to protect, so replacing it with a token
// records a redaction that never happened and primes the Redactor's cache with
// the empty string — which RedactInString skips, making the descriptor a
// fail-open lie in exactly the way RegisterSensitive's guard exists to prevent.
func TestRedactLeafAtPath_NothingToProtectLeavesValueInPlace(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want string
	}{
		{
			name: "explicit nil: left in place, nothing emitted",
			m:    map[string]any{"apiKey": nil},
			want: `{"apiKey":null}`,
		},
		{
			name: "empty string: left in place, nothing emitted",
			m:    map[string]any{"apiKey": ""},
			want: `{"apiKey":""}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			RedactLeafAtPath(tc.m, "apiKey", r, "apiKey")
			assert.Equal(t, tc.want, mustJSON(t, tc.m))
			assert.Empty(t, r.Emit(), "an empty canonical form must not emit a descriptor")
		})
	}
}
