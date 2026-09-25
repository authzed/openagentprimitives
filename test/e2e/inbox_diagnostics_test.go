//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// A turn that produces nothing has two completely different causes, and the
// state dump could not tell them apart: channelsd never wrote the inbound, or
// it wrote it and no runner ever drained it. The first is a delivery bug, the
// second a wake/respawn bug — chasing the wrong one costs hours.
//
// The discriminator already exists in the data. channelsd writes an "inbox"
// turn; the runner writes an "inbox_done" marker at that index when it drains.
// An inbox with no matching inbox_done is a message that arrived and was never
// consumed.
func TestDescribeInboxTurns_flagsAnUndrainedInbound(t *testing.T) {
	got := describeInboxTurns([]memory.Turn{
		{Index: 0, Role: "inbox"},
		{Index: 0, Role: "inbox_done"},
		{Index: 4, Role: "inbox"},
	})

	assert.Contains(t, got, "index=4")
	assert.Contains(t, got, "NOT drained",
		"the undrained inbound is the whole point of this section")
	assert.NotContains(t, got, "index=0 ",
		"a drained inbound is normal and must not add noise")
}

func TestDescribeInboxTurns_saysWhenEveryInboundWasDrained(t *testing.T) {
	got := describeInboxTurns([]memory.Turn{
		{Index: 0, Role: "inbox"},
		{Index: 0, Role: "inbox_done"},
	})
	assert.Contains(t, got, "all 1 drained",
		"stating this positively is what rules OUT the wake bug at a glance")
}

// No inbox turn at all means channelsd never delivered — the opposite
// diagnosis, and it must be stated rather than rendering as an empty section.
func TestDescribeInboxTurns_saysWhenNothingWasEverDelivered(t *testing.T) {
	got := describeInboxTurns(nil)
	assert.Contains(t, got, "no inbound")
	assert.Contains(t, got, "channelsd",
		"naming the component that would have written it points at the next place to look")
}
