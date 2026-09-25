package runner_test

// status.parentExchange: the durable record a delegated child leaves for the
// agent that delegated to it. The properties that matter are the exchange
// number staying MONOTONIC across a reap (nothing else lets a parent tell a
// new question from the one it just answered) and the pending flag clearing
// only on a resume.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func readExchange(t *testing.T, c client.Client, key client.ObjectKey) *spiceboxv1alpha1.ParentExchange {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(context.Background(), key, &got))
	return got.Status.ParentExchange
}

func TestAskParent_NumbersEachQuestionFromTheDurableRecord(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	key := client.ObjectKeyFromObject(sess)
	sp := runner.NewStatusPatcher(c, key)
	ctx := context.Background()

	n, err := sp.AskParent(ctx, "which repo?")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "the first question is exchange 1")

	pe := readExchange(t, c, key)
	require.NotNil(t, pe)
	assert.Equal(t, int64(1), pe.Exchange)
	assert.True(t, pe.Pending)
	assert.Equal(t, "which repo?", pe.Question)

	// The answer landed; the child resumed.
	require.NoError(t, sp.ClearParentPending(ctx))
	pe = readExchange(t, c, key)
	require.NotNil(t, pe)
	assert.False(t, pe.Pending, "nothing is outstanding once the child resumes")
	assert.Equal(t, int64(1), pe.Exchange,
		"the NUMBER survives the clear — a fresh patcher (a new pod) increments from it, never from zero")

	// A FRESH patcher stands in for the next pod: the counter must come off
	// the CR, not out of process memory, or a child reaped mid-conversation
	// would re-ask as exchange 1 and its parent would wait out a timeout for
	// an advance that never comes.
	n, err = runner.NewStatusPatcher(c, key).AskParent(ctx, "and which branch?")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	pe = readExchange(t, c, key)
	require.NotNil(t, pe)
	assert.Equal(t, int64(2), pe.Exchange)
	assert.True(t, pe.Pending)
	assert.Equal(t, "and which branch?", pe.Question)
}

func TestClearParentPending_IsANoOpWhenNothingWasAsked(t *testing.T) {
	c, sess := newFakeClientWithSession(t)
	key := client.ObjectKeyFromObject(sess)

	// Every resume that drains a message calls this, on every session in the
	// cluster; the overwhelming majority never asked anything.
	require.NoError(t, runner.NewStatusPatcher(c, key).ClearParentPending(context.Background()))
	assert.Nil(t, readExchange(t, c, key), "an ordinary session must not grow the field by being resumed")
}

func TestAskParent_InLocalModeRefusesRatherThanReportingSuccess(t *testing.T) {
	// A kubectl-driven session has no AgentSession record to ask through and
	// no parent to read one. Reporting success would park the agent on a
	// question that went nowhere.
	_, err := runner.LocalStatusPatcher().AskParent(context.Background(), "anyone?")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no AgentSession record")
}
