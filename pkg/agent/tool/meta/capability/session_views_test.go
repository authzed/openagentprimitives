package capability

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
)

func TestSessionViews_OptInDefaultOff(t *testing.T) {
	c := &sessionViewsCapability{}
	assert.Equal(t, "session_views", c.Name())
	assert.False(t, c.DefaultOn(), "session_views is opt-in — a class must explicitly list it")
	assert.False(t, c.Infrastructural())
}

func TestSessionViews_OffersNoTools(t *testing.T) {
	// It gates a browser affordance, not a tool. Offer must return no tools
	// (webd reads the GRANT directly via agentcaps; the runner injects nothing).
	tools, skip := (&sessionViewsCapability{}).Offer(OfferContext{})
	assert.Nil(t, skip)
	assert.Empty(t, tools)
}

func TestSessionViews_ParseConfig(t *testing.T) {
	cfg, err := (&sessionViewsCapability{}).ParseConfig(json.RawMessage(`{"interactions":["user_message"]}`))
	require.NoError(t, err)
	sv, ok := cfg.(agentcaps.SessionViewsConfig)
	require.True(t, ok, "ParseConfig must hand back the shared agentcaps config type, not a package-local clone")
	assert.Equal(t, []string{"user_message"}, sv.Interactions)
}

// DefaultOn() and the resolver agentcaps.ResolveSessionViews (which spells the
// same opt-in default privately) must never disagree: if they did, a class with
// no session_views key would offer no capability here while a browser view
// resolved one, or vice versa. Asserting the two agree on the absent-grant case
// pins that across future edits to either side.
func TestSessionViews_DefaultOnAgreesWithTheSharedResolver(t *testing.T) {
	c := &sessionViewsCapability{}
	require.False(t, c.DefaultOn(), "session_views is opt-in")

	views, err := agentcaps.ResolveSessionViews(nil) // no class ⇒ no grant
	require.NoError(t, err, "an absent grant is an ordinary answer, never an error")
	assert.False(t, views.Active,
		"an opt-in capability with no grant must resolve inactive — matching DefaultOn()=false")
}

func TestSessionViews_RegisteredAndValidates(t *testing.T) {
	// ValidateGrant must accept session_views (so the AgentClass controller's
	// CapabilitiesValid condition doesn't flag it as unknown).
	require.NoError(t, ValidateGrant("session_views", json.RawMessage(`{}`)))
	require.NoError(t, ValidateGrant("session_views", json.RawMessage(`{"interactions":["user_message"]}`)))
	require.Error(t, ValidateGrant("session_views", json.RawMessage(`{"interactions":"not-an-array"}`)))
}
