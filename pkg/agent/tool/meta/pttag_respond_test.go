package meta

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// The reply text may carry pt-untrusted envelopes (the model reused a datum).
// The leakage gate must see the TAGGED text to decide; the human must not see
// the markers.
func TestRespondExecuteStripsPtEnvelopeFromOutbound(t *testing.T) {
	var published channelevents.OutboundUserMessagePayload
	pub := func(_ context.Context, _ string, payload []byte) error {
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(payload, &env))
		require.NoError(t, json.Unmarshal(env.Payload, &published))
		return nil
	}
	var gateSaw string
	tt := newRespondTool(RespondConfig{
		Capabilities:      []string{"text", "markdown"},
		ChannelKind:       "fake",
		NATSPublish:       pub,
		NATSSubjectPrefix: "ap.session.default.foo",
		LeakageGate: func(_ context.Context, _ *tool.SessionContext, text string, _ []channelevents.AttachmentRef) error {
			gateSaw = text
			return nil
		},
	})

	reply := toolenvelope.WrapPt("the number is 42", "n1", "pt_1")
	args, err := json.Marshal(map[string]string{"text": reply})
	require.NoError(t, err)

	res, err := tt.Execute(memory.WithSystemApproval(context.Background(), "test"), args,
		&tool.SessionContext{Namespace: "default", Name: "foo"})
	require.NoError(t, err)
	assert.False(t, res.IsError)

	assert.Contains(t, gateSaw, "pt-untrusted", "the leakage gate must see the tagged text")
	assert.NotContains(t, published.Text, "pt-untrusted", "the human never sees pt markers")
	assert.Contains(t, published.Text, "the number is 42", "content survives the strip")
}
