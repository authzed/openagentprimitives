package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The details link on a trigger's status surface is not the agent's to
// assemble. The seam that owns it knows the session, knows which artifact it
// delivered, and knows the address webd serves it at; a model hand-writing a
// URL from those three is the same shape as the hand-written provider calls
// this seam removed, and it fails the same way — silently, into a link a
// maintainer clicks once.
//
// These tests state that as behaviour: the framework fills the link, and the
// agent can still override it for the case where the result genuinely lives
// somewhere else.

const dlWebdBase = "https://ap.example"

// dlSessionWith builds the session context these tests drive the tool with:
// the trigger-status fixtures' cluster, plus a live deliveries store seeded
// with what the session already delivered.
func dlSessionWith(t *testing.T, kindName string, delivered ...deliveries.Item) *tool.SessionContext {
	t.Helper()
	sess := tsSession(tsClient(t, kindName))
	sess.State = state.NewRegistry(state.Deps{})
	store, ok := deliveries.TryFrom(sess)
	require.True(t, ok, "importing the deliveries package registers its state kind")
	if len(delivered) > 0 {
		require.NoError(t, store.Record(context.Background(), delivered...))
	}
	return sess
}

// dlConfig is tsConfig plus the webd base URL the framework composes against.
func dlConfig(kindName, webdBase string) meta.TriggerStatusConfig {
	cfg := tsConfig(kindName)
	cfg.WebdBaseURL = func() string { return webdBase }
	return cfg
}

func concludeWith(t *testing.T, cfg meta.TriggerStatusConfig, sess *tool.SessionContext, args string) tool.Result {
	t.Helper()
	res, err := meta.NewConcludeTriggerStatus(cfg).Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "result: %s", res.Content)
	return res
}

// TestConcludeTriggerStatus_DefaultsDetailsURLToTheDeliveredArtifact is the
// change's point: the agent supplies judgement, and the link to the full result
// comes from the framework that knows where the result is.
func TestConcludeTriggerStatus_DefaultsDetailsURLToTheDeliveredArtifact(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-details-default")
	sess := dlSessionWith(t, k.name, deliveries.Item{RenderName: "ar-report", ArtifactID: "artifact-report"})

	concludeWith(t, dlConfig(k.name, dlWebdBase), sess,
		`{"outcome":"problems_found","summary":"Two findings in the auth path."}`)

	want, err := channelkinds.ComposeArtifactViewURL(dlWebdBase, "default/sess-1", "artifact-report")
	require.NoError(t, err)
	require.Len(t, k.concluded, 1)
	assert.Equal(t, want, k.concluded[0].DetailsURL,
		"with no link supplied, the surface gets the durable link to what this session delivered")
}

// TestConcludeTriggerStatus_AgentSuppliedDetailsURLWins keeps the field
// agent-supplyable. A round whose real result lives somewhere the framework
// cannot know about — an external dashboard, a build log — must still be able
// to say so, and a default that silently overwrote it would be a worse link
// than no default at all.
func TestConcludeTriggerStatus_AgentSuppliedDetailsURLWins(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-details-supplied")
	sess := dlSessionWith(t, k.name, deliveries.Item{RenderName: "ar-report", ArtifactID: "artifact-report"})

	concludeWith(t, dlConfig(k.name, dlWebdBase), sess,
		`{"outcome":"clean","summary":"Nothing blocking.","details_url":"https://elsewhere.example/run/9"}`)

	require.Len(t, k.concluded, 1)
	assert.Equal(t, "https://elsewhere.example/run/9", k.concluded[0].DetailsURL)
}

// TestConcludeTriggerStatus_NoLinkableResult_ConcludesWithNoDetailsURL: every
// way of having nothing to link to ends the same — the trigger still gets its
// answer, without a link. An answer withheld because a link could not be built
// would be strictly worse than the silence this seam exists to end.
func TestConcludeTriggerStatus_NoLinkableResult_ConcludesWithNoDetailsURL(t *testing.T) {
	cases := []struct {
		name      string
		fakeName  string
		webdBase  string
		delivered []deliveries.Item
	}{
		{
			name:     "nothing delivered: conclusion published, no link",
			fakeName: "faketrigger-details-nodelivery",
			webdBase: dlWebdBase,
		},
		{
			name:      "delivered render carries no artifact id: conclusion published, no link",
			fakeName:  "faketrigger-details-noartifactid",
			webdBase:  dlWebdBase,
			delivered: []deliveries.Item{{RenderName: "ar-unlabelled"}},
		},
		{
			name:      "webd has no external URL yet: conclusion published, no link",
			fakeName:  "faketrigger-details-nowebd",
			webdBase:  "",
			delivered: []deliveries.Item{{RenderName: "ar-report", ArtifactID: "artifact-report"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := registerTriggerFake(t, tc.fakeName)
			sess := dlSessionWith(t, k.name, tc.delivered...)

			concludeWith(t, dlConfig(k.name, tc.webdBase), sess,
				`{"outcome":"clean","summary":"Nothing blocking."}`)

			require.Len(t, k.concluded, 1, "the trigger is answered either way")
			assert.Empty(t, k.concluded[0].DetailsURL)
		})
	}
}

// TestConcludeTriggerStatus_BlankDetailsURLIsAbsent: a model that emits
// `"details_url": "   "` meant to supply nothing. Treating whitespace as a
// value would publish a check run with an empty link, which the kind then
// refuses — failing the conclusion over punctuation.
func TestConcludeTriggerStatus_BlankDetailsURLIsAbsent(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-details-blank")
	sess := dlSessionWith(t, k.name, deliveries.Item{RenderName: "ar-report", ArtifactID: "artifact-report"})

	concludeWith(t, dlConfig(k.name, dlWebdBase), sess,
		`{"outcome":"clean","summary":"Nothing blocking.","details_url":"   "}`)

	want, err := channelkinds.ComposeArtifactViewURL(dlWebdBase, "default/sess-1", "artifact-report")
	require.NoError(t, err)
	require.Len(t, k.concluded, 1)
	assert.Equal(t, want, k.concluded[0].DetailsURL)
}

// TestConcludeTriggerStatus_NoDeliveryState_StillConcludes covers a session
// assembled without the deliveries Kind (a minimal context, a test fixture).
// The tool must not depend on state it may not have.
func TestConcludeTriggerStatus_NoDeliveryState_StillConcludes(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-details-nostate")

	concludeWith(t, dlConfig(k.name, dlWebdBase), tsSession(tsClient(t, k.name)),
		`{"outcome":"clean","summary":"Nothing blocking."}`)

	require.Len(t, k.concluded, 1)
	assert.Empty(t, k.concluded[0].DetailsURL)
}

// TestConcludeTriggerStatus_SchemaNoLongerAsksTheModelToAssembleALink: the
// schema is the instruction the model actually reads. Leaving it telling the
// model to supply the delivery thread would keep producing hand-written links
// that the default then never gets to replace.
func TestConcludeTriggerStatus_SchemaNoLongerAsksTheModelToAssembleALink(t *testing.T) {
	tl := meta.NewConcludeTriggerStatus(tsConfig("faketrigger-details-schema"))

	var schema struct {
		Properties struct {
			DetailsURL struct {
				Description string `json:"description"`
			} `json:"details_url"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema))

	desc := schema.Properties.DetailsURL.Description
	require.NotEmpty(t, desc)
	assert.NotContains(t, desc, "the thread you delivered it into",
		"the framework fills this in; the schema must stop pointing the model at the delivery thread")
	assert.NotContains(t, schema.Required, "details_url", "it stays optional")
}
