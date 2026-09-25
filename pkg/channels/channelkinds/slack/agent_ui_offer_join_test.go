// pkg/channels/channelkinds/slack/agent_ui_offer_join_test.go
//
// The one test that spans show_agent_ui and the Slack button, and the reason
// it lives in package slack rather than beside either half: it needs to write
// agentUIOfferSender.client, an unexported field, AND to call the real tool.
// pkg/agent/tool/meta does not import this package (nor anything that does),
// so the dependency runs one way only.
package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// joinAgentUIMinter composes through the REAL ComposeAgentUIURL, so the URL
// asserted below is the one production composes rather than a literal this
// test invented.
type joinAgentUIMinter struct{ base string }

func (m joinAgentUIMinter) MintAgentUILink(sessionRef string) (string, error) {
	return channelkinds.ComposeAgentUIURL(m.base, sessionRef)
}

// TestShowAgentUIReachesASlackButton is the join: the agent's tool call and
// the button a non-browser channel renders are one contract, and neither
// half's own test can see it. Both halves stay green if the tool publishes
// KindSessionViewOffer by mistake — that kind's payload is byte-identical, so
// the sender decodes it happily and posts a button to the WRONG page — or if
// the payload's json tag drifts, because each half's own test builds its own
// stimulus. This test builds none: it starts at a real Execute and ends at the
// bytes slack-go would put on the wire.
func TestShowAgentUIReachesASlackButton(t *testing.T) {
	const (
		ns      = "demo-ns"
		name    = "demo-session"
		base    = "https://webd.example.test"
		wantURL = base + "/sessions?session=demo-ns%2Fdemo-session"
	)

	// --- Leg 1: the real tool.
	var gotSubject string
	var gotBytes []byte
	tl := meta.NewShowAgentUI(meta.ShowAgentUIConfig{
		NATSPublish: func(_ context.Context, subject string, payload []byte) error {
			gotSubject, gotBytes = subject, append([]byte(nil), payload...)
			return nil
		},
		ViewerCanInteract: func(context.Context) (bool, error) { return true, nil },
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{}`),
		&tool.SessionContext{Namespace: ns, Name: name})
	require.NoError(t, err, "Execute must not return a transport error")
	require.False(t, res.IsError, "Execute must not refuse: %s", res.Content)
	require.NotEmpty(t, gotBytes, "the tool must publish exactly one envelope")

	// Subject leaf and envelope kind are asserted separately and explicitly:
	// this is the pair a wrong-kind publish moves, and nothing downstream would
	// notice on its own. They look redundant because PublishOut derives the
	// subject from the kind — but a future refactor letting a caller pass a
	// subject would break exactly one of them, and that is the case worth
	// catching.
	assert.True(t, strings.HasSuffix(gotSubject, ".out.agent_ui_offer"),
		"published subject must be the agent_ui_offer leaf, got %q", gotSubject)
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(gotBytes, &env), "published bytes must be an Envelope")
	require.Equal(t, channelevents.KindAgentUIOffer, env.Kind, "envelope kind")

	// --- Leg 2: the real kind wiring, then the real sender, fed leg 1's
	// envelope. Nothing here constructs a payload.
	sender := (&Kind{}).SubChannelSender(channelkinds.SubChannelAgentUIOffer,
		channelkinds.Deps{AgentUIMinter: joinAgentUIMinter{base: base}})
	require.NotNil(t, sender, "the kind must wire an agent_ui_offer sender")
	s, ok := sender.(*agentUIOfferSender)
	require.True(t, ok, "SubChannelSender must return this package's agent_ui_offer sender")
	// The Slack client is swapped for the recording fake: newAgentUIOfferSender
	// builds a real client from Deps.Secret and this test asserts on rendered
	// blocks, not on Slack's API. The MINTER came through Deps untouched, which
	// is the wiring half this leg exists to pin.
	fake := &fakeSlackClient{}
	s.client = fake

	_, err = s.Send(context.Background(), sessionWithChannel("C01CHAN", "1700000000.000001"), env)
	require.NoError(t, err, "the sender must accept the envelope the tool published")
	require.Len(t, fake.postMessageCalls, 1, "exactly one chat.postMessage")

	blocks := msgOptionBlocksJSON(t, fake.postMessageCalls[0].options)
	assert.Contains(t, blocks, `"type":"button"`, "the handoff must render a button, not a bare URL")
	assert.Contains(t, blocks, wantURL, "the button must open the session the tool named")
}
