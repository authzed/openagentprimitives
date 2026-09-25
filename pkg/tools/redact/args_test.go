package redact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// desc is the descriptor these tests attach when they drive the Redactor
// directly; the args helpers build their own via argDescriptor.
func desc(name string) Descriptor {
	return Descriptor{Description: "test secret", Kind: "env", Name: name}
}

// mustJSON renders a redacted args map so a test can assert on the exact
// bytes that would be serialized into a Decision — the surface a leak
// would actually escape through. HTML escaping is off so the redaction
// token stays readable as `<redacted id="1"/>` in assertions and diffs;
// it does not change whether a secret's bytes are present.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	require.NoError(t, enc.Encode(v), "marshal redacted args")
	return strings.TrimRight(buf.String(), "\n")
}

func TestStringify(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{name: "string passes through unchanged", in: "tok_abc", want: "tok_abc"},
		{name: "empty string yields empty (never registered)", in: "", want: ""},
		{name: "bool renders as true/false", in: true, want: "true"},
		{name: "float64 renders without exponent padding", in: float64(42), want: "42"},
		{name: "float64 keeps fractional precision", in: 1.5, want: "1.5"},
		{name: "int renders as decimal", in: 7, want: "7"},
		{name: "int64 renders as decimal", in: int64(-9), want: "-9"},
		{name: "nil has no single-string form: empty", in: nil, want: ""},
		{name: "map has no single-string form: empty", in: map[string]any{"a": 1}, want: ""},
		{name: "slice has no single-string form: empty", in: []any{1}, want: ""},
		{name: "unhandled scalar kind (int32) yields empty", in: int32(5), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Stringify(tc.in))
		})
	}
}

func TestCanonicalString(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{name: "scalar uses Stringify form", in: "tok_abc", want: "tok_abc"},
		{name: "nil has nothing to protect: empty", in: nil, want: ""},
		{name: "empty string has nothing to protect: empty", in: "", want: ""},
		{name: "map serializes to compact JSON", in: map[string]any{"k": "v"}, want: `{"k":"v"}`},
		{name: "slice serializes to compact JSON", in: []any{"a", "b"}, want: `["a","b"]`},
		{name: "nested composite serializes whole tree", in: map[string]any{"o": map[string]any{"k": "v"}}, want: `{"o":{"k":"v"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, canonicalString(tc.in))
		})
	}
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		name string
		path string
		want []pathSeg
		// wantErr marks a path that cannot be parsed. want then holds the
		// segments that DID parse — the ancestor prefix the redactors fall
		// back to rather than skipping the declaration entirely.
		wantErr bool
	}{
		{
			name: "dotted-only path yields key segments with index -1",
			path: "a.b.c",
			want: []pathSeg{{key: "a", index: -1}, {key: "b", index: -1}, {key: "c", index: -1}},
		},
		{
			name: "bracketed index emits key then index segment",
			path: "tags[2]",
			want: []pathSeg{{key: "tags", index: -1}, {key: "", index: 2}},
		},
		{
			name: "index followed by key continues the walk",
			path: "tags[2].name",
			want: []pathSeg{{key: "tags", index: -1}, {key: "", index: 2}, {key: "name", index: -1}},
		},
		{
			name: "consecutive indices emit one segment each",
			path: "grid[0][1]",
			want: []pathSeg{{key: "grid", index: -1}, {key: "", index: 0}, {key: "", index: 1}},
		},
		{
			name: "empty path yields no segments",
			path: "",
			want: []pathSeg{},
		},
		{
			name: "leading index with no key emits only the index",
			path: "[3]",
			want: []pathSeg{{key: "", index: 3}},
		},
		{
			name:    "non-integer index: error, segments truncated to the parsed prefix",
			path:    "tags[x]",
			want:    []pathSeg{{key: "tags", index: -1}},
			wantErr: true,
		},
		{
			name:    "unterminated bracket: error, segments truncated to the parsed prefix",
			path:    "tags[2",
			want:    []pathSeg{{key: "tags", index: -1}},
			wantErr: true,
		},
		{
			name:    "malformed index mid-path: error, later segments never parsed",
			path:    "creds[x].token",
			want:    []pathSeg{{key: "creds", index: -1}},
			wantErr: true,
		},
		{
			name:    "empty brackets: error, segments truncated to the parsed prefix",
			path:    "creds[].token",
			want:    []pathSeg{{key: "creds", index: -1}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePath(tc.path)
			if tc.wantErr {
				require.Error(t, err, "a malformed path must not parse silently")
				assert.Contains(t, err.Error(), tc.path, "the error must name the offending path")
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// mustParse is the walker-test shorthand for a path that is expected to be
// well-formed; a malformed one is a bug in the test, not the case under test.
func mustParse(t *testing.T, path string) []pathSeg {
	t.Helper()
	segs, err := parsePath(path)
	require.NoError(t, err, "fixture path %q must parse", path)
	return segs
}

// TestResolveLeaf_Failures pins the walker's fail-closed cases. Each one
// makes RedactLeafAtPath / RedactValueAtPath a no-op, so a spec that names
// a path the args do not have leaves the args untouched rather than
// panicking or redacting the wrong slot.
func TestResolveLeaf_Failures(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		path string
	}{
		{name: "empty path: not resolved", m: map[string]any{"a": "v"}, path: ""},
		{name: "absent top-level key: not resolved", m: map[string]any{"a": "v"}, path: "missing"},
		{name: "absent nested key: not resolved", m: map[string]any{"a": map[string]any{}}, path: "a.missing"},
		{name: "index into a map: shape mismatch, not resolved", m: map[string]any{"a": map[string]any{"k": "v"}}, path: "a[0]"},
		{name: "key into a slice: shape mismatch, not resolved", m: map[string]any{"a": []any{"v"}}, path: "a.k"},
		{name: "index past end of slice: not resolved", m: map[string]any{"a": []any{"v"}}, path: "a[5]"},
		{name: "descend through a scalar: not resolved", m: map[string]any{"a": "scalar"}, path: "a.b"},
		{name: "descend through nil: not resolved", m: map[string]any{"a": nil}, path: "a.b"},
		{name: "intermediate index past end: not resolved", m: map[string]any{"a": []any{}}, path: "a[0].k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := resolveLeaf(tc.m, mustParse(t, tc.path))
			assert.False(t, ok, "path %q must not resolve", tc.path)
		})
	}
}

func TestResolveLeaf_Successes(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		path string
		want any
	}{
		{name: "top-level key resolves to its value", m: map[string]any{"a": "v"}, path: "a", want: "v"},
		{name: "nested key resolves to the inner value", m: map[string]any{"a": map[string]any{"b": "v"}}, path: "a.b", want: "v"},
		{name: "array element resolves by index", m: map[string]any{"a": []any{"x", "y"}}, path: "a[1]", want: "y"},
		{name: "key under an array element resolves", m: map[string]any{"a": []any{map[string]any{"k": "v"}}}, path: "a[0].k", want: "v"},
		{name: "explicit nil value still resolves (key present)", m: map[string]any{"a": nil}, path: "a", want: nil},
		{name: "nested array of arrays resolves", m: map[string]any{"g": []any{[]any{"deep"}}}, path: "g[0][0]", want: "deep"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := resolveLeaf(tc.m, mustParse(t, tc.path))
			require.True(t, ok, "path %q must resolve", tc.path)
			assert.Equal(t, tc.want, ref.get())
		})
	}
}

// TestLeafRef_SetWritesThroughToRoot proves the walker hands back a live
// reference, not a copy — the whole point of resolveLeaf is that the
// caller's mutation lands in the map that gets serialized.
func TestLeafRef_SetWritesThroughToRoot(t *testing.T) {
	t.Run("map slot: set is visible at the root", func(t *testing.T) {
		m := map[string]any{"a": map[string]any{"b": "old"}}
		ref, ok := resolveLeaf(m, mustParse(t, "a.b"))
		require.True(t, ok)
		ref.set("new")
		assert.Equal(t, `{"a":{"b":"new"}}`, mustJSON(t, m))
	})
	t.Run("slice slot: set is visible at the root", func(t *testing.T) {
		m := map[string]any{"a": []any{"old", "keep"}}
		ref, ok := resolveLeaf(m, mustParse(t, "a[0]"))
		require.True(t, ok)
		ref.set("new")
		assert.Equal(t, `{"a":["new","keep"]}`, mustJSON(t, m))
	})
}

func TestDeepCopyJSONMap_IsIndependentOfSource(t *testing.T) {
	src := map[string]any{
		"scalar": "keep",
		"nested": map[string]any{"k": "orig"},
		"list":   []any{"a", map[string]any{"deep": "orig"}},
	}
	cp := DeepCopyJSONMap(src)
	require.Equal(t, src, cp, "copy must start out equal")

	// Mutate every level of the copy; the source must be untouched, because
	// callers redact the copy while keeping the originals for CEL evaluation.
	cp["scalar"] = "changed"
	cp["nested"].(map[string]any)["k"] = "changed"
	cp["list"].([]any)[0] = "changed"
	cp["list"].([]any)[1].(map[string]any)["deep"] = "changed"

	assert.Equal(t, "keep", src["scalar"])
	assert.Equal(t, "orig", src["nested"].(map[string]any)["k"])
	assert.Equal(t, "a", src["list"].([]any)[0])
	assert.Equal(t, "orig", src["list"].([]any)[1].(map[string]any)["deep"])
}

func TestDeepCopyJSONMap_NilInNilOut(t *testing.T) {
	assert.Nil(t, DeepCopyJSONMap(nil), "nil map must copy to nil, not an empty map")
}

// TestRedactLeafAtPath_UnresolvedPathIsNoOp is the agent-dispatch fail-safe:
// a spec naming a path the payload does not have must leave the payload
// byte-identical rather than redacting some other slot.
func TestRedactLeafAtPath_UnresolvedPathIsNoOp(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "absent key: payload unchanged, nothing emitted", path: "nope"},
		{name: "shape mismatch (index into map): payload unchanged, nothing emitted", path: "creds[0]"},
		{name: "index past end: payload unchanged, nothing emitted", path: "tags[9]"},
		{name: "empty path: payload unchanged, nothing emitted", path: ""},
		// Malformed bracket paths ("tags[x]", "tags[2") are NOT no-ops and are
		// covered in malformed_path_test.go instead: they fail closed onto the
		// enclosing value rather than skipping, which is the whole point.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{
				"creds": map[string]any{"token": "tok_abc"},
				"tags":  []any{"one"},
			}
			before := mustJSON(t, m)
			r := New()
			assert.NoError(t, RedactLeafAtPath(m, tc.path, r, tc.path),
				"a well-formed path that simply does not match is a no-op, not an error")
			assert.Equal(t, before, mustJSON(t, m), "unresolved path must not mutate args")
			assert.Empty(t, r.Emit(), "unresolved path must not emit a descriptor")
		})
	}
}

func TestRedactLeafAtPath_ReplacesOnlyTheAddressedLeaf(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		path string
		want string
	}{
		{
			name: "nested scalar: only that key becomes a token",
			m:    map[string]any{"creds": map[string]any{"token": "tok_abc", "user": "alice"}},
			path: "creds.token",
			want: `{"creds":{"token":"<redacted id=\"1\"/>","user":"alice"}}`,
		},
		{
			name: "array element: only that index becomes a token",
			m:    map[string]any{"tags": []any{"public", "tok_abc"}},
			path: "tags[1]",
			want: `{"tags":["public","<redacted id=\"1\"/>"]}`,
		},
		{
			name: "key under an array element: only that key becomes a token",
			m:    map[string]any{"items": []any{map[string]any{"k": "tok_abc", "keep": "v"}}},
			path: "items[0].k",
			want: `{"items":[{"k":"<redacted id=\"1\"/>","keep":"v"}]}`,
		},
		{
			name: "top-level scalar: that key becomes a token",
			m:    map[string]any{"apiKey": "tok_abc", "region": "us"},
			path: "apiKey",
			want: `{"apiKey":"<redacted id=\"1\"/>","region":"us"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			RedactLeafAtPath(tc.m, tc.path, r, tc.path)
			assert.Equal(t, tc.want, mustJSON(t, tc.m))

			emitted := r.Emit()
			require.Len(t, emitted, 1, "exactly one descriptor for one redacted leaf")
			assert.Equal(t, tc.path, emitted["1"].Name, "descriptor retains the call-site path")
			assert.Equal(t, "arg", emitted["1"].Kind)
		})
	}
}

// TestRedactLeafAtPath_SecretIsNotRecoverableFromArgs is the property that
// matters: after redaction the serialized args must contain no byte of the
// secret, whatever slot it lived in.
func TestRedactLeafAtPath_SecretIsNotRecoverableFromArgs(t *testing.T) {
	const secret = "tok_super_secret_value"
	m := map[string]any{"creds": map[string]any{"token": secret}}
	r := New()
	RedactLeafAtPath(m, "creds.token", r, "creds.token")
	assert.NotContains(t, mustJSON(t, m), secret, "secret must not survive in the emitted args")
}

func TestRedactValueAtPath_UnresolvedPathIsNoOp(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "absent key: payload unchanged, nothing emitted", path: "nope"},
		{name: "shape mismatch (key into slice): payload unchanged, nothing emitted", path: "tags.name"},
		{name: "index past end: payload unchanged, nothing emitted", path: "tags[9]"},
		{name: "empty path: payload unchanged, nothing emitted", path: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{
				"creds": map[string]any{"token": "tok_abc"},
				"tags":  []any{"one"},
			}
			before := mustJSON(t, m)
			r := New()
			assert.NoError(t, RedactValueAtPath(m, tc.path, r, tc.path),
				"a well-formed path that simply does not match is a no-op, not an error")
			assert.Equal(t, before, mustJSON(t, m), "unresolved path must not mutate args")
			assert.Empty(t, r.Emit(), "unresolved path must not emit a descriptor")
		})
	}
}

// TestRedactValueAtPath_WholeCompositeIsReplaced is the validator semantics:
// a declared-sensitive composite collapses to ONE token, so no inner byte
// serializes into the Decision.
func TestRedactValueAtPath_WholeCompositeIsReplaced(t *testing.T) {
	m := map[string]any{
		"creds": map[string]any{"token": "tok_abc", "refresh": "rt_xyz"},
		"other": "public",
	}
	r := New()
	RedactValueAtPath(m, "creds", r, "creds")

	// The two inner scalars are registered first (ids 1 and 2) for free-text
	// scrubbing, then the whole composite is replaced by its own token (id 3).
	got := mustJSON(t, m)
	assert.Equal(t, `{"creds":"<redacted id=\"3\"/>","other":"public"}`, got)
	assert.NotContains(t, got, "tok_abc", "inner secret must not survive")
	assert.NotContains(t, got, "rt_xyz", "inner secret must not survive")
	assert.Len(t, r.Emit(), 3, "two inner leaves plus the whole composite")
}

// TestRedactValueAtPath_RegistersInnerLeavesForFreeTextScrubbing is the
// second half of the validator contract: the same secret reappearing in a
// CEL message or trace detail is scrubbed there too.
func TestRedactValueAtPath_RegistersInnerLeavesForFreeTextScrubbing(t *testing.T) {
	m := map[string]any{"creds": map[string]any{"token": "tok_abc", "nested": []any{"rt_xyz"}}}
	r := New()
	RedactValueAtPath(m, "creds", r, "creds")

	msg := r.RedactInString("call failed for token tok_abc and refresh rt_xyz")
	assert.NotContains(t, msg, "tok_abc", "inner scalar must be scrubbed from free text")
	assert.NotContains(t, msg, "rt_xyz", "inner slice scalar must be scrubbed from free text")
}

func TestRedactValueAtPath_NothingToProtectLeavesValueInPlace(t *testing.T) {
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
			RedactValueAtPath(tc.m, "apiKey", r, "apiKey")
			assert.Equal(t, tc.want, mustJSON(t, tc.m))
			assert.Empty(t, r.Emit(), "an empty canonical form must not emit a descriptor")
		})
	}
}

func TestRedactValueAtPath_ScalarAtArrayIndex(t *testing.T) {
	m := map[string]any{"tokens": []any{"public", "tok_abc"}}
	r := New()
	RedactValueAtPath(m, "tokens[1]", r, "tokens[1]")
	assert.Equal(t, `{"tokens":["public","<redacted id=\"1\"/>"]}`, mustJSON(t, m))
}

// TestRedactInString_MultiByteValues guards against a byte-oriented
// replacement corrupting or missing UTF-8 secrets.
func TestRedactInString_MultiByteValues(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		text   string
	}{
		{name: "cjk secret is replaced whole", secret: "秘密鍵", text: "key=秘密鍵 done"},
		{name: "emoji secret is replaced whole", secret: "🔑🔒", text: "key=🔑🔒 done"},
		{name: "accented latin secret is replaced whole", secret: "clé-privée", text: "key=clé-privée done"},
		{name: "secret adjacent to multi-byte context is replaced", secret: "tok_abc", text: "→tok_abc←"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			r.RegisterSensitive(tc.secret, desc("K"))
			got := r.RedactInString(tc.text)
			assert.NotContains(t, got, tc.secret, "secret must not survive")
			assert.Contains(t, got, `<redacted id="1"/>`, "a token must appear in its place")
			assert.True(t, utf8.ValidString(got), "replacement must not split a multi-byte rune")
		})
	}
}

func TestRedactInString_EveryOccurrenceIsReplaced(t *testing.T) {
	r := New()
	r.RegisterSensitive("tok_abc", desc("K"))
	got := r.RedactInString("tok_abc and again tok_abc and tok_abctok_abc")
	assert.NotContains(t, got, "tok_abc", "no occurrence may survive, including adjacent repeats")
}

func TestRedactInString_NoRegisteredValuesReturnsInputUnchanged(t *testing.T) {
	r := New()
	const s = "nothing sensitive here"
	assert.Equal(t, s, r.RedactInString(s))
}

func TestEmit_ReturnsAnIndependentCopy(t *testing.T) {
	r := New()
	r.Redact("tok_abc", desc("K"))
	m := r.Emit()
	require.Len(t, m, 1)
	// Mutating the emitted map must not corrupt the Redactor's own record.
	delete(m, "1")
	assert.Len(t, r.Emit(), 1, "Emit must hand back a copy, not the internal map")
}
