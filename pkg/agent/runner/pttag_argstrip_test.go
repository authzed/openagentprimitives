package runner

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
)

func TestStripArgTags_removesEnvelopeAndPreservesBigInt(t *testing.T) {
	inner := toolenvelope.WrapPt("secret data", "n1", "pt_1")
	args, err := json.Marshal(map[string]any{"text": inner, "big": json.Number("9007199254740993")})
	require.NoError(t, err)

	out := stripArgTags(args)

	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	assert.Equal(t, "secret data", got["text"], "the tool sees clean content, no pt markers")
	assert.NotContains(t, string(out), "pt-untrusted", "no envelope markers reach the tool")
	// The large-int sibling of the tagged field survives exactly (UseNumber),
	// even though the payload was re-marshaled because a tag was stripped.
	assert.Contains(t, string(out), "9007199254740993",
		"a large integer is not truncated when a sibling field was stripped")
}

func TestStripArgTags_untaggedIsByteIdentical(t *testing.T) {
	args := json.RawMessage(`{"text":"plain content","big":9007199254740993}`)
	out := stripArgTags(args)
	assert.Equal(t, string(args), string(out),
		"untagged args are returned byte-for-byte (no re-marshal, no precision risk)")
}

func TestStripArgTags_malformedJSONUnchanged(t *testing.T) {
	args := json.RawMessage(`not json`)
	assert.Equal(t, string(args), string(stripArgTags(args)),
		"unparseable args are handed to the tool exactly as sent")
}
