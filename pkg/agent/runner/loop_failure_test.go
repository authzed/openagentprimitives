package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	lifecyclekind "github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// TestFail_EmitsRunnerTerminalThroughTheSequencer proves l.fail's durable half:
// the RunnerTerminal{Phase: Failed, Reason, Message} event actually reaches the
// signed lifecycle log through applyEvent's AppendLog effect, not just the CR
// status write. Without this, a bug in fail's emitLifecycleEvent call (a wrong
// Phase, a dropped Message) would go unnoticed by any test — WriteFailed and
// the lifecycle append are two separate records of the same fact, and only the
// CR-status half had a behavioral test.
func TestFail_EmitsRunnerTerminalThroughTheSequencer(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	lifecycleMem := memory.NewLocal(inmem.NewBackend())

	l := &Loop{
		SessionKey:      memory.NamespacedName{Namespace: "default", Name: "sess1"},
		LifecycleMemory: lifecycleMem,
		Status:          LocalStatusPatcher(),
	}

	require.NoError(t, l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionStalled, "the model gave up"))

	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	events, err := lifecyclekind.Events(ctx, lifecycleMem, scope)
	require.NoError(t, err, "read the session's lifecycle log")

	var terminals []lifecyclecore.RunnerTerminal
	for _, ev := range events {
		if rt, ok := ev.(lifecyclecore.RunnerTerminal); ok {
			terminals = append(terminals, rt)
		}
	}
	require.Len(t, terminals, 1, "exactly one RunnerTerminal recorded, got events %v", events)
	assert.Equal(t, lifecyclecore.PhaseFailed, terminals[0].Phase)
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentSessionStalled, terminals[0].Reason)
	assert.Equal(t, "the model gave up", terminals[0].Message)

	// The CR-side write still ran too — fail's other durable half.
	assert.NotNil(t, l.Status.LocalFailure(), "WriteFailed must also have run")
}
