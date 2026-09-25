package channelevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestKindAgentUIOffer_ListMembership pins the kind into every predicate
// that gates it. Valid and Implemented are separate lists in kinds.go and a
// kind added to one but not the other is refused at the publish boundary
// with a message naming the other, which is a confusing failure to debug
// from a runner log.
func TestKindAgentUIOffer_ListMembership(t *testing.T) {
	assert.Equal(t, Kind("agent_ui_offer"), KindAgentUIOffer,
		"the wire value is persisted in the NATS subject leaf; changing it breaks in-flight consumers")
	assert.True(t, KindAgentUIOffer.Valid(), "agent_ui_offer must be Valid")
	assert.True(t, KindAgentUIOffer.Implemented(), "agent_ui_offer must be Implemented")
	assert.True(t, KindAgentUIOffer.RelayHandles(),
		"channelsd's outbound relay is the consumer; only the metaagent family answers false")
}
