package redact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactor_AssignsIDs(t *testing.T) {
	r := New()
	tok1 := r.Redact("secret-value-A", Descriptor{Description: "GH token", Kind: "env", Name: "GITHUB_TOKEN"})
	assert.Equal(t, `<redacted id="1"/>`, tok1)
	tok2 := r.Redact("other", Descriptor{Description: "other", Kind: "flag", Name: "x"})
	assert.Equal(t, `<redacted id="2"/>`, tok2)
}

func TestRedactor_Dedup(t *testing.T) {
	r := New()
	a := r.Redact("secret", Descriptor{Description: "d", Kind: "env", Name: "X"})
	b := r.Redact("secret", Descriptor{Description: "d", Kind: "env", Name: "X"})
	assert.Equal(t, a, b, "same value should dedupe")
}

func TestRedactor_EmitContainsDescriptors(t *testing.T) {
	r := New()
	r.Redact("v1", Descriptor{Description: "d1", Kind: "env", Name: "A"})
	r.Redact("v2", Descriptor{Description: "d2", Kind: "flag", Name: "B"})
	m := r.Emit()
	require.Len(t, m, 2)
	assert.Equal(t, "A", m["1"].Name)
	assert.Equal(t, "B", m["2"].Name)
}

func TestRedactor_RedactInString(t *testing.T) {
	r := New()
	r.RegisterSensitive("secret", Descriptor{Description: "d", Kind: "env", Name: "X"})
	got := r.RedactInString("prefix secret suffix")
	assert.Equal(t, `prefix <redacted id="1"/> suffix`, got)
}

func TestRedactor_RegisterSensitive_IgnoresEmpty(t *testing.T) {
	r := New()
	// Registering the empty string is a no-op: it can never be redacted
	// (RedactInString skips value=="") and would otherwise pollute the
	// descriptor map with a meaningless entry. Guard it so a caller that
	// stringifies an unhandled leaf to "" cannot silently register a
	// useless — and dangerously misleading — "sensitive" placeholder.
	r.RegisterSensitive("", Descriptor{Description: "d", Kind: "env", Name: "X"})
	assert.Empty(t, r.Emit(), "empty value must not produce a descriptor")
}
