package github

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// TestGitHubSyncKind_AsksForOrgsAndRoundTripsThem: the org list is explicit
// and required — the synced set is the manifest's, never a credential's
// reach.
func TestGitHubSyncKind_AsksForOrgsAndRoundTripsThem(t *testing.T) {
	var k relsync.Configurable = &SyncKind{}
	screens := k.ConfigScreens(relsync.ExistingConfig{})
	require.Len(t, screens, 1)
	assert.Equal(t, "orgs", screens[0].Key)

	cfg, err := k.BuildConfig(map[string]string{"orgs": "acme, widgets"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"orgs":["acme","widgets"]}`, string(cfg),
		"surrounding whitespace is trimmed; the CR carries a clean list")
}

// TestGitHubSyncKind_RefusesAnEmptyOrgList: an empty org list must be
// refused at the seam, not written and then refused by the kind at sync
// time with a condition nobody is watching.
func TestGitHubSyncKind_RefusesAnEmptyOrgList(t *testing.T) {
	_, err := (&SyncKind{}).BuildConfig(map[string]string{"orgs": "  ,  "})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orgs")
}

// TestGitHubSyncKind_EndpointIsOptionalNotRequired: GHES is customer-hosted;
// github.com is not. The screen must be offered either way, because the
// wizard cannot know which the operator means.
func TestGitHubSyncKind_EndpointIsOptionalNotRequired(t *testing.T) {
	assert.False(t, (&SyncKind{}).NeedsEndpoint(),
		"github.com needs none; a GHES operator supplies one and the controller's credhost check gates it")
}

// TestGitHubSyncKind_PrefillsFromPriorConfig: a re-run shows the configured
// orgs so enter-through keeps them.
func TestGitHubSyncKind_PrefillsFromPriorConfig(t *testing.T) {
	screens := (&SyncKind{}).ConfigScreens(
		relsync.ExistingConfig{Config: json.RawMessage(`{"orgs":["acme"]}`)})
	require.Len(t, screens, 1)
	assert.Equal(t, "acme", screens[0].Default)
}
