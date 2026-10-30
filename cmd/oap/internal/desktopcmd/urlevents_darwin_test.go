//go:build darwin && arm64

package desktopcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeliverURLEventDropsWhenFull pins the cgo callback's contract: the
// AppKit main thread must never block on the Go side, so once the buffer
// holds cap(urlEvents) undrained links the next one is dropped (false)
// rather than waited on.
func TestDeliverURLEventDropsWhenFull(t *testing.T) {
	// Swap in a fresh channel so a link queued by any other test (or by a
	// real Apple Event) cannot skew the count.
	orig := urlEvents
	urlEvents = make(chan string, 4)
	t.Cleanup(func() { urlEvents = orig })

	for i := range 4 {
		require.True(t, deliverURLEvent("oap://install/agent?v=1&ref=ghcr.io/acme/agents/demo-agent"),
			"send %d of 4 must fit the buffer", i+1)
	}
	assert.False(t, deliverURLEvent("oap://install/agent?v=1&ref=ghcr.io/acme/agents/overflow"),
		"5th send must be dropped, not block")

	assert.Len(t, urlEvents, 4, "the dropped link must not displace a queued one")
	assert.Equal(t, "oap://install/agent?v=1&ref=ghcr.io/acme/agents/demo-agent", <-urlEvents,
		"queued links are delivered in arrival order")
}
