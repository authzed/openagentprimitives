package toolenvelope_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
)

func TestStripPtJSON_removesEnvelopeAndPreservesBigInt(t *testing.T) {
	inner := toolenvelope.WrapPt("secret data", "n1", "pt_1")
	args, err := json.Marshal(map[string]any{"text": inner, "big": json.Number("9007199254740993")})
	require.NoError(t, err)

	out, had := toolenvelope.StripPtJSON(args)
	assert.True(t, had, "a tagged string leaf reports stripped")

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "secret data", got["text"], "the consumer sees clean content, no pt markers")
	assert.NotContains(t, string(out), "pt-untrusted", "no envelope markers survive")
	// The large-int sibling of the tagged field survives exactly (UseNumber),
	// even though the payload was re-marshaled because a tag was stripped.
	assert.Contains(t, string(out), "9007199254740993",
		"a large integer is not truncated when a sibling field was stripped")
}

func TestStripPtJSON_untaggedOrMalformedIsByteIdentical(t *testing.T) {
	for _, raw := range []string{`{"text":"plain content","big":9007199254740993}`, `not json`, ``} {
		out, had := toolenvelope.StripPtJSON(json.RawMessage(raw))
		assert.False(t, had, "%q: nothing stripped", raw)
		assert.Equal(t, raw, string(out), "%q: returned byte-for-byte (no re-marshal, no precision risk)", raw)
	}
}
