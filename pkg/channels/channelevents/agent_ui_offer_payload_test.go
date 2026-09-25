package channelevents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// TestAgentUIOfferPayload_RoundTrip pins the wire tag, not just the struct.
// The assert.Contains on the raw bytes is what makes this test see a
// json-tag rename at all — and it is still not enough on its own, because it
// pins the tag the producer writes without pinning that the consumer reads
// the same one. A separate spanning test (owned by the task that builds the
// consumer) is needed to close that gap; a round-trip through one struct is
// blind to a tag rename on both sides moving together.
func TestAgentUIOfferPayload_RoundTrip(t *testing.T) {
	in := channelevents.AgentUIOfferPayload{SessionRef: "demo-ns/demo-session"}
	b, err := json.Marshal(in)
	require.NoError(t, err, "marshal AgentUIOfferPayload")
	assert.Contains(t, string(b), `"sessionRef":"demo-ns/demo-session"`)

	var out channelevents.AgentUIOfferPayload
	require.NoError(t, json.Unmarshal(b, &out), "unmarshal AgentUIOfferPayload")
	assert.Equal(t, in, out)
}
