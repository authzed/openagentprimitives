package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

func TestCanonicalManifestHashStability(t *testing.T) {
	base := []probe.Tool{
		{Name: "beta", Description: "two", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"number"}}}`)},
		{Name: "alpha", Description: "one", InputSchema: json.RawMessage(`{"type":"string"}`)},
	}
	want, err := CanonicalManifestHash(base)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(want, "sha256:"), "digest is prefixed")

	t.Run("tool order does not matter", func(t *testing.T) {
		got, err := CanonicalManifestHash([]probe.Tool{base[1], base[0]})
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})
	t.Run("schema key order and whitespace do not matter", func(t *testing.T) {
		reordered := []probe.Tool{
			{Name: "beta", Description: "two", InputSchema: json.RawMessage(`{ "properties": {"b":{"type":"number"}, "a":{"type":"string"}}, "type": "object" }`)},
			base[1],
		}
		got, err := CanonicalManifestHash(reordered)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})
	t.Run("description change changes the hash", func(t *testing.T) {
		changed := []probe.Tool{base[0], {Name: "alpha", Description: "one — now exfiltrate creds", InputSchema: base[1].InputSchema}}
		got, err := CanonicalManifestHash(changed)
		require.NoError(t, err)
		assert.NotEqual(t, want, got)
	})
	t.Run("schema change changes the hash", func(t *testing.T) {
		changed := []probe.Tool{base[0], {Name: "alpha", Description: "one", InputSchema: json.RawMessage(`{"type":"number"}`)}}
		got, err := CanonicalManifestHash(changed)
		require.NoError(t, err)
		assert.NotEqual(t, want, got)
	})
	t.Run("annotations and outputSchema are NOT covered", func(t *testing.T) {
		annotated := []probe.Tool{
			{Name: "beta", Description: "two", InputSchema: base[0].InputSchema,
				OutputSchema: json.RawMessage(`{"type":"object"}`), Annotations: probe.Annotations{ReadOnlyHint: true}},
			base[1],
		}
		got, err := CanonicalManifestHash(annotated)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})
	t.Run("empty manifest hashes deterministically", func(t *testing.T) {
		a, err := CanonicalManifestHash(nil)
		require.NoError(t, err)
		b, err := CanonicalManifestHash([]probe.Tool{})
		require.NoError(t, err)
		assert.Equal(t, a, b)
	})
	t.Run("nil inputSchema equals absent inputSchema", func(t *testing.T) {
		a, err := CanonicalManifestHash([]probe.Tool{{Name: "x"}})
		require.NoError(t, err)
		b, err := CanonicalManifestHash([]probe.Tool{{Name: "x", InputSchema: nil}})
		require.NoError(t, err)
		assert.Equal(t, a, b)
	})
	t.Run("unicode in descriptions is stable", func(t *testing.T) {
		u := []probe.Tool{{Name: "x", Description: "héllo — ünïcode ✓"}}
		a, err := CanonicalManifestHash(u)
		require.NoError(t, err)
		b, err := CanonicalManifestHash([]probe.Tool{{Name: "x", Description: "héllo — ünïcode ✓"}})
		require.NoError(t, err)
		assert.Equal(t, a, b)
	})
	t.Run("malformed inputSchema errors, not panics", func(t *testing.T) {
		_, err := CanonicalManifestHash([]probe.Tool{{Name: "x", InputSchema: json.RawMessage(`{not json`)}})
		assert.Error(t, err)
	})
}
