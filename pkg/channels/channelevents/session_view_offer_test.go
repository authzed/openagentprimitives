package channelevents_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestSessionViewOfferPayload_RoundTrip(t *testing.T) {
	in := channelevents.SessionViewOfferPayload{SessionRef: "default/sess-1"}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"sessionRef":"default/sess-1"`)

	var out channelevents.SessionViewOfferPayload
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, in, out)
}
