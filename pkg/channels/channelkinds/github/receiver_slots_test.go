package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestTriggerSlotInstances_NodeIDPresentYieldsExactlyOneInstance is driven
// from the package's own real-payload fixture (testdata/pull_request_opened.json),
// which carries the pull request's node_id alongside fields prEvent does not
// decode — the same reasoning receiver_facts_test.go gives for using a
// fixture over a synthetic body built by marshaling into prEvent.
func TestTriggerSlotInstances_NodeIDPresentYieldsExactlyOneInstance(t *testing.T) {
	got, err := receiver{}.TriggerSlotInstances(testChannel(t), "pull_request", fixtureBody(t, "pull_request_opened.json"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, channelkinds.TriggerSlotInstance{
		ResourceType: "github_pull_request",
		ResourceID:   "PR_kwDOJ3xR-M6ZqBcT",
	}, got[0])
}

// TestTriggerSlotInstances_MissingNodeIDIsALoudError reuses
// pull_request_fork.json, which carries a valid repo and number but (like
// every fixture in this package before this task) no node_id — exactly the
// shape a real delivery would never produce but a payload change upstream
// could.
func TestTriggerSlotInstances_MissingNodeIDIsALoudError(t *testing.T) {
	_, err := receiver{}.TriggerSlotInstances(testChannel(t), "pull_request", fixtureBody(t, "pull_request_fork.json"))
	require.Error(t, err)
	assert.ErrorContains(t, err, "demo-org/platform", "must name the repository")
	assert.ErrorContains(t, err, "43", "must name the PR number")
	assert.ErrorContains(t, err, "default", "must name the Channel namespace")
	assert.ErrorContains(t, err, "demo-reviewbot-gh", "must name the Channel name")
}

func TestTriggerSlotInstances_IgnoresOtherEventTypes(t *testing.T) {
	got, err := receiver{}.TriggerSlotInstances(testChannel(t), "ping", fixtureBody(t, "pull_request_opened.json"))
	require.NoError(t, err, "an uninteresting event is not an error")
	assert.Empty(t, got)
}

// TestTriggerSlotInstances_RefusesUndecodableBody covers json.Unmarshal's
// error branch, asserting the decode branch's OWN message rather than merely
// that some error came back — the next guard (missing node_id) also errors on
// this input, since an empty prEvent decodes to an empty node_id too.
func TestTriggerSlotInstances_RefusesUndecodableBody(t *testing.T) {
	_, err := receiver{}.TriggerSlotInstances(testChannel(t), "pull_request", []byte("not json"))
	require.Error(t, err)
	assert.ErrorContains(t, err, "decode pull_request payload",
		"must fail at the decode step, not fall through to the missing-node_id guard")
}
