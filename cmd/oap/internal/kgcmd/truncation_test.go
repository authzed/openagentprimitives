package kgcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

func factsN(n int) []memory.KGFact {
	out := make([]memory.KGFact, n)
	for i := range out {
		out[i] = memory.KGFact{UUID: "u", Name: "knows", Fact: "a knows b"}
	}
	return out
}

func entitiesN(n int) []memory.KGEntity {
	out := make([]memory.KGEntity, n)
	for i := range out {
		out[i] = memory.KGEntity{UUID: "u", Name: "thing", Summary: "a thing"}
	}
	return out
}

// TestRenderFactsTruncation pins that `oap kg search` reports a ranking the
// --limit cut short, and stays silent when it did not.
func TestRenderFactsTruncation(t *testing.T) {
	th := aptest.PlainTheme()

	t.Run("truncated fact listing names a larger --limit", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderFacts(&buf, th, factsN(2), true, 2))
		assert.Contains(t, buf.String(), "--limit 4")
	})

	t.Run("complete fact listing says nothing about --limit", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderFacts(&buf, th, factsN(2), false, 20))
		assert.NotContains(t, buf.String(), "--limit")
	})
}

// TestRenderEntitiesTruncation pins the same pair for `oap kg related`.
func TestRenderEntitiesTruncation(t *testing.T) {
	th := aptest.PlainTheme()

	t.Run("truncated entity listing names a larger --limit", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderEntities(&buf, th, entitiesN(3), true, 3))
		assert.Contains(t, buf.String(), "--limit 6")
	})

	t.Run("complete entity listing says nothing about --limit", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, renderEntities(&buf, th, entitiesN(3), false, 20))
		assert.NotContains(t, buf.String(), "--limit")
	})
}

// TestKGJSONPayloadsCarryTruncation pins the machine-readable half. The kg
// commands hand-build their --json document, so the fact has to be put there
// explicitly — a notice that lives only in the table leaves a script with the
// same silent partial the human had.
func TestKGJSONPayloadsCarryTruncation(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{name: "truncated facts", payload: kgFactsPayload(factsN(2), true), want: `"truncated":true`},
		{name: "complete facts: present and false, never absent", payload: kgFactsPayload(factsN(2), false), want: `"truncated":false`},
		{name: "truncated entities", payload: kgEntitiesPayload(entitiesN(2), true), want: `"truncated":true`},
		{name: "complete entities: present and false, never absent", payload: kgEntitiesPayload(entitiesN(2), false), want: `"truncated":false`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.payload)
			require.NoError(t, err, "Marshal payload")
			assert.Contains(t, string(b), tc.want)
		})
	}
}
