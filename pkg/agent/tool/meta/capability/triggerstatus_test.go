package capability

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

// TestTriggerStatus_OfferedOnlyForAKindThatReportsIt is the gate, stated in
// both directions and DERIVED from the registry rather than from a name list:
// every registered kind is asked whether it reports trigger status, and the
// capability must offer tools for exactly those that say yes.
//
// The negative direction is the load-bearing one. A capability that offered
// these tools for every channel-attached session would put a model in front of
// a status surface that does not exist, and the failure would land as a tool
// error at the end of a completed round rather than as an absent tool.
func TestTriggerStatus_OfferedOnlyForAKindThatReportsIt(t *testing.T) {
	c, ok := Lookup("trigger_status")
	require.True(t, ok, "the capability must be registered")
	assert.True(t, c.DefaultOn(),
		"a kind either has a trigger surface or does not; there is no operator choice to make, and an unanswered surface is never what anyone wants")

	kinds := registry.All()
	require.NotEmpty(t, kinds)

	var anyOffered, anyDeclined bool
	for _, k := range kinds {
		_, reports := registry.TriggerStatusReporterFor(k.Name())
		anyOffered = anyOffered || reports
		anyDeclined = anyDeclined || !reports

		t.Run(k.Name(), func(t *testing.T) {
			tools, skip := c.Offer(OfferContext{
				Ctx:     context.Background(),
				Granted: true,
				Enabled: true,
				Binding: &spiceboxv1alpha1.ChannelBinding{
					Name: "in", Kind: k.Name(), Key: "pr:demo-org/platform#42",
				},
			})
			assert.Nil(t, skip, "neither answer is a skip: a kind without a trigger surface is a normal state")
			if reports {
				assert.Equal(t, []string{"claim_trigger_status", "conclude_trigger_status"}, toolNames(tools))
				return
			}
			assert.Empty(t, tools)
		})
	}
	assert.True(t, anyOffered, "no registered kind reports trigger status, so the positive direction above is vacuous")
	assert.True(t, anyDeclined, "every registered kind reports trigger status, so the negative direction above is vacuous")
}

// TestTriggerStatus_InactiveWithoutAnInputBinding: a kubectl-driven session was
// started by a person running a command, not by an event with a status surface.
// Nothing to report on, and nothing to report about it.
func TestTriggerStatus_InactiveWithoutAnInputBinding(t *testing.T) {
	c, _ := Lookup("trigger_status")
	tools, skip := c.Offer(OfferContext{Ctx: context.Background(), Granted: true, Enabled: true, Binding: nil})
	assert.Nil(t, skip)
	assert.Empty(t, tools)
}

// TestTriggerStatus_ToolsAreWiredToTheSessionsOwnTrigger: the tools carry the
// binding rather than anything a class or a model supplied, and the description
// they show carries the KIND's own words for what it reports on.
func TestTriggerStatus_ToolsAreWiredToTheSessionsOwnTrigger(t *testing.T) {
	c, _ := Lookup("trigger_status")
	reporter, ok := registry.TriggerStatusReporterFor("github")
	require.True(t, ok, "the github kind must report trigger status for this test to mean anything")

	tools, skip := c.Offer(OfferContext{
		Ctx:     context.Background(),
		Granted: true,
		Enabled: true,
		Binding: &spiceboxv1alpha1.ChannelBinding{
			Name: "reviewbot-github", Kind: "github", Key: "pr:demo-org/platform#42",
		},
	})
	require.Nil(t, skip)
	require.Len(t, tools, 2)
	for _, tl := range tools {
		assert.Contains(t, tl.Description(), reporter.TriggerSurfaceKind(),
			"%s must describe itself in the kind's own words", tl.Name())
	}
}

// TestTriggerStatus_ParseConfig: the value set is closed and a typo fails at
// VALIDATION (CapabilitiesValid on the class) rather than silently publishing
// model text on a surface the operator believed was composed-only.
func TestTriggerStatus_ParseConfig(t *testing.T) {
	c, ok := Lookup("trigger_status")
	require.True(t, ok)

	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "absent config parses to the default (model text)", raw: ""},
		{name: "explicit model: accepted", raw: `{"publishedText":"model"}`, want: "model"},
		{name: "composed: accepted", raw: `{"publishedText":"composed"}`, want: "composed"},
		{name: "the common {enabled} envelope alone is tolerated", raw: `{"enabled":true}`},
		{name: "a typo'd value: refused, never a silent fallback to model text", raw: `{"publishedText":"compsed"}`, wantErr: true},
		{name: "a wrong-typed value: refused", raw: `{"publishedText":42}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.ParseConfig(json.RawMessage(tc.raw))
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			cfg, ok := got.(triggerStatusConfig)
			require.True(t, ok, "ParseConfig must return the type Offer reads, got %T", got)
			assert.Equal(t, tc.want, cfg.PublishedText)
		})
	}
}

// TestTriggerStatus_ComposedConfigReachesTheConcludeTool spans the join Task 1
// tested from below: the parsed config must actually flow into the tool the
// model reads, or the class opts in and nothing changes — the exact
// compiles-and-does-nothing failure shape this repo has shipped before.
func TestTriggerStatus_ComposedConfigReachesTheConcludeTool(t *testing.T) {
	c, ok := Lookup("trigger_status")
	require.True(t, ok)
	cfg, err := c.ParseConfig(json.RawMessage(`{"publishedText":"composed"}`))
	require.NoError(t, err)

	tools, skip := c.Offer(OfferContext{
		Ctx: context.Background(), Granted: true, Enabled: true, Config: cfg,
		Binding: &spiceboxv1alpha1.ChannelBinding{
			Name: "in", Kind: "github", Key: "pr:demo-org/platform#42",
		},
	})
	require.Nil(t, skip)
	require.Len(t, tools, 2)

	var conclude tool.Tool
	for _, tl := range tools {
		if tl.Name() == "conclude_trigger_status" {
			conclude = tl
		}
	}
	require.NotNil(t, conclude)

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	require.NoError(t, json.Unmarshal(conclude.InputSchema(), &schema))
	assert.NotContains(t, schema.Properties, "summary",
		"composed mode must remove the model-authored summary from the schema the model reads")
	assert.NotContains(t, schema.Properties, "details_url")
	assert.Equal(t, []string{"outcome"}, schema.Required)
}
