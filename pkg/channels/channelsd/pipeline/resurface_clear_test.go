package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// TestClearPendingPromptOnResolve: a resolved prompt must stop being
// outstanding, or every surface that attaches would be re-posted a card whose
// decision has already landed.
func TestClearPendingPromptOnResolve(t *testing.T) {
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	sessKey := client.ObjectKey{Namespace: "ns", Name: "s"}
	mem := newTestMemory(t)
	p := &Pipeline{Mem: mem}
	notePendingInteractionRequest(t, mem, sessKey, fixtureInteractionCategory, "r1",
		channelevents.InteractionRequestPayload{})
	require.Len(t, outstandingPrompts(t, p, sessKey), 1, "precondition: the prompt is outstanding")

	p.clearPendingPrompt(context.Background(), "ns", "s", "r1")

	assert.Empty(t, outstandingPrompts(t, p, sessKey))
}

// TestClearPendingPromptWithoutMemoryIsNoop: an unwired facade must not panic —
// the clear runs on the decision path, and taking that down would strand the
// session rather than merely leaving a stale card.
func TestClearPendingPromptWithoutMemoryIsNoop(t *testing.T) {
	assert.NotPanics(t, func() {
		(&Pipeline{}).clearPendingPrompt(context.Background(), "ns", "s", "r1")
	})
}
