package capability

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// A SPLIT-CHANNEL session is one whose inbound and outbound are different
// transports: a webhook arrives on one Channel and every reply is rendered on
// another. It is not an edge case — the pipeline REFUSES to create a session
// for an input-only Channel unless an output Channel resolves, so every session
// a webhook ever spawns has this shape.
//
// The two bindings answer different questions, and the split matters because
// they disagree. The trigger-status capability wants the INPUT binding: which
// system raised this work, and can its status be reported back. Everything that
// shapes a REPLY wants the OUTPUT binding: which transport will render it, what
// it can carry, whether it has a live-view surface. Reading the input binding
// for an outbound question describes a channel nothing will be rendered on.

// splitBindings returns the pair a github→slack session carries: an input
// binding that advertises nothing (github is input-only, so its kind reports no
// capabilities at all) and an output binding carrying slack's.
func splitBindings() (in, out *spiceboxv1alpha1.ChannelBinding) {
	in = &spiceboxv1alpha1.ChannelBinding{
		Name: "reviewbot-gh", Kind: "github", Key: "pr:demo-org/demo-repo#4",
		Capabilities:      nil,
		NATSSubjectPrefix: "ap.session.default.review-1",
	}
	out = &spiceboxv1alpha1.ChannelBinding{
		Name: "reviewbot-slack", Kind: "slack", Key: "thread:C0DEMO:1700000000.1",
		Capabilities:      []string{"text", "markdown", "asset:text/html"},
		NATSSubjectPrefix: "ap.session.default.review-1",
	}
	return in, out
}

// offerOn builds one capability's tools for a split-channel session.
func offerOn(t *testing.T, c Capability, cfg Config) []tool.Tool {
	t.Helper()
	in, out := splitBindings()
	tools, skip := c.Offer(OfferContext{
		Config:     cfg,
		Class:      &spiceboxv1alpha1.AgentClass{},
		Session:    &spiceboxv1alpha1.AgentSession{},
		Binding:    in,
		OutBinding: out,
		Env: RunnerEnv{
			ChannelAttached: true,
			Artifacts:       artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil),
			NATSPublish:     func(context.Context, string, []byte) error { return nil },
			SubjectPrefix:   "ap.session.default.review-1",
		},
	})
	require.Nil(t, skip, "the capability must contribute tools for this session")
	return tools
}

func toolNamed(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %q was not offered; got %d tools", name, len(tools))
	return nil
}

// TestSplitChannel_RespondToUserIsShapedByTheChannelThatRendersTheReply is the
// defect that kept a review report out of a thread.
//
// respond_to_user only offers `attached` when its channel advertises an asset:*
// capability. Shaped from the INPUT binding, a webhook-spawned session asks
// github — which advertises nothing — and the field is absent from the schema
// altogether. The prompt can instruct the agent to attach its report all it
// likes; there is no argument to put it in, so the report stays behind and the
// reply goes out as text. The reply is rendered by SLACK, which carries
// attachments perfectly well.
func TestSplitChannel_RespondToUserIsShapedByTheChannelThatRendersTheReply(t *testing.T) {
	tools := offerOn(t, channelInteractionCapability{}, nil)
	respond := toolNamed(t, tools, "respond_to_user")

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(respond.InputSchema(), &schema), "respond_to_user schema must parse")

	assert.Contains(t, schema.Properties, "attached",
		"slack renders this session's replies and carries text/html, so the agent must have a field to attach its report in")
	assert.Contains(t, respond.Description(), "slack",
		"and the tool must describe the transport the user will actually read it on")
}

// TestSplitChannel_ArtifactOfferViewTargetsTheChannelThatRendersTheOffer is the
// same mistake on the other tool, and the more dangerous direction: github has
// no live-view surface, so asking the input binding suppresses an offer slack
// would have rendered — and tells the agent its channel cannot do live views
// when the one its reader is on can.
func TestSplitChannel_ArtifactOfferViewTargetsTheChannelThatRendersTheOffer(t *testing.T) {
	// Register the renderer this case needs rather than leaning on the html
	// package's init: other tests in this package Reset the process-wide
	// renderer registry and restore it EMPTY, so a test that depends on a blank
	// import passes alone and fails in company.
	registerStandaloneRenderer(t)

	tools := offerOn(t, artifactsCapability{}, artifactsConfig{Renderers: []string{"fakestandalone"}})
	offer := toolNamed(t, tools, "artifact_offer_view")

	// The tool names its channel in every answer it gives, which is the one
	// observable that says which binding it was built from.
	assert.Contains(t, offer.Description(), "live-view",
		"sanity: this is the offer tool")

	res, err := offer.Execute(t.Context(), json.RawMessage(`{"artifact_id":"artifact-nope"}`),
		&tool.SessionContext{Namespace: "default", Name: "review-1"})
	require.NoError(t, err)
	assert.NotContains(t, res.Content, "github has no live-view surface",
		"the offer is rendered by slack, which has one; refusing on the input channel's behalf loses the live view entirely")
}
