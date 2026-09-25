package core

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

const coreSecret = "SEEKRET-core-value"

// namedString stands in for any domain type declared over string — the walk
// must mask its content without flattening it to a plain string, because a
// structured consumer decodes it back into the same field.
type namedString string

// exportedOnly is the shape a Parsed view takes: every field readable, so the
// walk can visit each one and hand back a value of the same type.
type exportedOnly struct {
	Name  string
	Count int64
}

// hasUnexported cannot be walked field-by-field — reflect refuses to read
// `hidden` — so the only honest handling is to mask its rendering, which fmt
// DOES reach. The fixture puts the secret in the unreadable field on purpose.
type hasUnexported struct {
	Name   string
	hidden string
}

// newRedactor returns a Redactor primed with coreSecret plus the token it
// replaces it with, so cases can assert on the exact replacement without
// hardcoding an id that depends on registration order.
func newRedactor(t *testing.T) (*redact.Redactor, string) {
	t.Helper()
	r := redact.New()
	r.RegisterSensitive(coreSecret, redact.Descriptor{Description: "test secret", Kind: "env", Name: "TOKEN"})
	tok := r.RedactInString(coreSecret)
	require.NotEqual(t, coreSecret, tok, "the redactor must replace a registered value")
	return r, tok
}

// TestRedactAny_WalksEveryShape is the fail-open regression. RedactAny used to
// switch on string / map[string]any / []any and return everything else
// untouched, so a []string — what a stringList flag or a variadic positional
// binds to — carried its raw values straight through a walk that had
// "covered" the field.
//
// The rule the cases below pin: a value either comes back with its type
// intact and every string inside it masked, or it comes back as its masked
// rendering. Nothing is ever returned unexamined.
func TestRedactAny_WalksEveryShape(t *testing.T) {
	r, tok := newRedactor(t)

	cases := []struct {
		name string
		in   any
		want any
	}{
		{
			name: "string carrying the secret: masked in place",
			in:   "prefix " + coreSecret + " suffix",
			want: "prefix " + tok + " suffix",
		},
		{
			name: "[]string (a stringList slot): every element masked, still a []string",
			in:   []string{"safe", coreSecret},
			want: []string{"safe", tok},
		},
		{
			name: "[]any of mixed leaves: each leaf walked, still an []any",
			in:   []any{coreSecret, 1.5, nil},
			want: []any{tok, 1.5, nil},
		},
		{
			name: "nested map + slice: the walk descends to the leaf",
			in:   map[string]any{"outer": map[string]any{"inner": []any{coreSecret}}},
			want: map[string]any{"outer": map[string]any{"inner": []any{tok}}},
		},
		{
			name: "named string type: masked, and it keeps its type",
			in:   namedString(coreSecret),
			want: namedString(tok),
		},
		{
			name: "[]namedString: walked by shape, no type case needed for it",
			in:   []namedString{namedString(coreSecret)},
			want: []namedString{namedString(tok)},
		},
		{
			name: "byte slice: masked as the text it is, not shredded byte by byte",
			in:   []byte("payload " + coreSecret),
			want: []byte("payload " + tok),
		},
		{
			name: "scalar that is not sensitive: returned with its type intact",
			in:   int64(7),
			want: int64(7),
		},
		{
			name: "bool: returned with its type intact",
			in:   true,
			want: true,
		},
		{
			name: "struct with only exported fields: fields masked, type preserved",
			in:   exportedOnly{Name: coreSecret, Count: 3},
			want: exportedOnly{Name: tok, Count: 3},
		},
		{
			name: "nil: stays nil",
			in:   nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RedactAny(tc.in, r))
		})
	}
}

// TestRedactAny_NeverReturnsAValueItCannotWalk covers the shapes where
// preserving the type would mean preserving the secret. Each must come back
// masked; none may come back verbatim.
func TestRedactAny_NeverReturnsAValueItCannotWalk(t *testing.T) {
	r, tok := newRedactor(t)

	t.Run("scalar whose rendering IS the secret: masked, not emitted raw", func(t *testing.T) {
		num := redact.New()
		num.RegisterSensitive("4242", redact.Descriptor{Description: "account", Kind: "env", Name: "ACCT"})
		got := RedactAny(int64(4242), num)
		assert.NotEqual(t, int64(4242), got, "a scalar that IS a registered secret must not survive")
		assert.NotContains(t, fmt.Sprint(got), "4242", "no byte of it may survive")
	})

	t.Run("container whose element type cannot hold a token: masked rendering, never the raw map", func(t *testing.T) {
		num := redact.New()
		num.RegisterSensitive("4242", redact.Descriptor{Description: "account", Kind: "env", Name: "ACCT"})
		got := RedactAny(map[string]int{"acct": 4242}, num)
		assert.NotContains(t, fmt.Sprint(got), "4242",
			"an int-valued map holding a secret must be masked, not handed back")
	})

	t.Run("struct with an unreadable field: masked rendering reaches what reflect cannot", func(t *testing.T) {
		got := RedactAny(hasUnexported{Name: "visible", hidden: coreSecret}, r)
		s, ok := got.(string)
		require.True(t, ok, "a struct the walk cannot visit field-by-field comes back as its rendering, got %T", got)
		assert.NotContains(t, s, coreSecret, "the unreadable field's value must still be masked")
		assert.Contains(t, s, tok, "and replaced by the token")
	})

	t.Run("channel: no walkable content, and the value itself is not returned", func(t *testing.T) {
		got := RedactAny(make(chan int), r)
		assert.IsType(t, "", got, "an uninspectable value is rendered, not passed through")
	})
}

// TestRedactAny_PointersAreScrubbedInPlace pins the property core.Finalize
// depends on: Decision.Parsed is a pointer, and scrubbing it must leave the
// caller holding the same pointer with scrubbed contents.
func TestRedactAny_PointersAreScrubbedInPlace(t *testing.T) {
	r, tok := newRedactor(t)

	v := &exportedOnly{Name: coreSecret, Count: 1}
	got := RedactAny(v, r)
	assert.Same(t, v, got, "the same pointer comes back")
	assert.Equal(t, tok, v.Name, "and its contents are scrubbed in place")

	var nilPtr *exportedOnly
	assert.Equal(t, nilPtr, RedactAny(nilPtr, r), "a nil pointer walks without panicking")
}

// TestRedactAnyMap_MutatesInPlace pins the entry point both validators call:
// the caller keeps its map and finds it scrubbed.
func TestRedactAnyMap_MutatesInPlace(t *testing.T) {
	r, tok := newRedactor(t)

	m := map[string]any{"flag": coreSecret, "list": []string{coreSecret}}
	RedactAnyMap(m, r)
	assert.Equal(t, tok, m["flag"], "scalar value scrubbed in the caller's map")
	assert.Equal(t, []string{tok}, m["list"], "[]string value scrubbed in the caller's map")
}
