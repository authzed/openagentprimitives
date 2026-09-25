package slack

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStarterCache_PutGet(t *testing.T) {
	c := newStarterCache()
	_, ok := c.get("default/s1")
	assert.False(t, ok)

	c.put("default/s1", starterCoords{ChannelID: "C1", MessageTS: "1.1", AgentName: "hubspot-companies", StartedUnix: 1720000000})
	got, ok := c.get("default/s1")
	assert.True(t, ok)
	assert.Equal(t, "C1", got.ChannelID)
	assert.Equal(t, "1.1", got.MessageTS)
	assert.Equal(t, "hubspot-companies", got.AgentName)
	assert.Equal(t, int64(1720000000), got.StartedUnix)
}

func TestStartingMessageAt_TitleFreeFormatUnchanged(t *testing.T) {
	got := startingMessageAt("hubspot-companies", 1720000000)
	assert.Contains(t, got, "🤖 *hubspot-companies* started")
	assert.Contains(t, got, "<!date^1720000000^")
}
