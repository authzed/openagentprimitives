package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPinnedMessageStatus_OmitsEmptyAndRoundTrips(t *testing.T) {
	// A nil PinnedMessage must not serialize a "pinnedMessage" key.
	var st AgentSessionStatus
	b, err := json.Marshal(st)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "pinnedMessage", "a nil projection must be omitted")

	st.PinnedMessage = &PinnedMessageStatus{
		Badge: OpeningBadgeProblemsFound,
		Body:  "2 findings",
		Link:  "https://webd.example/view",
	}
	b, err = json.Marshal(st)
	require.NoError(t, err)

	var back AgentSessionStatus
	require.NoError(t, json.Unmarshal(b, &back))
	require.NotNil(t, back.PinnedMessage)
	assert.Equal(t, OpeningBadgeProblemsFound, back.PinnedMessage.Badge)
	assert.Equal(t, "2 findings", back.PinnedMessage.Body)
}

func TestOpeningBadge_OutcomeStringsFoldDirectly(t *testing.T) {
	// The concluded-outcome badges share the TriggerOutcome vocabulary.
	assert.EqualValues(t, "clean", OpeningBadgeClean)
	assert.EqualValues(t, "problems_found", OpeningBadgeProblemsFound)
	assert.EqualValues(t, "could_not_finish", OpeningBadgeCouldNotFinish)
}
