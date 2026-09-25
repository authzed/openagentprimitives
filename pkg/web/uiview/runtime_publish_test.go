package uiview_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// capturedPublish records every envelope a test's rt.Publish receives, so
// assertions can inspect the kind/payload without depending on any
// transport.
type capturedPublish struct {
	envs []channelevents.Envelope
}

func (c *capturedPublish) fn(_ context.Context, env channelevents.Envelope) error {
	c.envs = append(c.envs, env)
	return nil
}

// failingPutMemory wraps a real memory.Memory and forces its NEXT Put to
// fail, then reverts to delegating — used only to prove Runtime.Write's
// publish-AFTER-record ordering: a failed durable write must never be
// followed by a push (mutation-check 3, task-9-brief).
type failingPutMemory struct {
	memory.Memory
	failNext bool
}

func (m *failingPutMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if m.failNext {
		m.failNext = false
		return memory.Entry{}, errors.New("forced Put failure")
	}
	return m.Memory.Put(ctx, e)
}

func TestRuntimeWritePublishesUIViewUpdateOnSuccess(t *testing.T) {
	ctx, mem, _ := newMemFixture(t)
	ui := writableUIFixture(t)
	pub := &capturedPublish{}
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeUIClient(t, ui), Mem: mem, Publish: pub.fn,
	}

	_, err := rt.Write(ctx, "panel", uicomponents.Node{
		Component: "ap:markdown",
		Props:     map[string]json.RawMessage{"body": json.RawMessage(`"agent copy"`)},
	})
	require.NoError(t, err)

	require.Len(t, pub.envs, 1, "a successful Write must publish exactly one push")
	env := pub.envs[0]
	assert.Equal(t, channelevents.KindUIViewUpdate, env.Kind)
	assert.Equal(t, "demo-ns", env.Session.Namespace)
	assert.Equal(t, "demo-session", env.Session.Name)

	var payload channelevents.UIViewUpdatePayload
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, "panel", payload.Hook)
	assert.False(t, payload.UpdatedAt.IsZero())
}

// TestRuntimeClearPublishesUIViewUpdate is the "updated cue holds for
// removals" floor: a Clear changes the durable record exactly as a Write
// does, so a browser watching for the push must be told about it exactly
// the same way — there is no separate "something was removed" envelope.
func TestRuntimeClearPublishesUIViewUpdate(t *testing.T) {
	ctx, mem, _ := newMemFixture(t)
	ui := writableUIWithDefaultFixture(t)
	pub := &capturedPublish{}
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: ui.Name,
		Client: newFakeUIClient(t, ui), Mem: mem, Publish: pub.fn,
	}

	_, err := rt.Clear(ctx, "panel")
	require.NoError(t, err)

	require.Len(t, pub.envs, 1, "a successful Clear must publish exactly one push")
	env := pub.envs[0]
	assert.Equal(t, channelevents.KindUIViewUpdate, env.Kind)

	var payload channelevents.UIViewUpdatePayload
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, "panel", payload.Hook)
	assert.False(t, payload.UpdatedAt.IsZero())
}

func TestRuntimeWriteWithNilPublishDegradesToMemoryOnly(t *testing.T) {
	ctx, mem, _ := newMemFixture(t)
	ui := writableUIFixture(t)
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeUIClient(t, ui), Mem: mem, // Publish left nil
	}

	assert.NotPanics(t, func() {
		_, err := rt.Write(ctx, "panel", uicomponents.Node{Component: "ap:text", Props: map[string]json.RawMessage{"text": json.RawMessage(`"x"`)}})
		require.NoError(t, err)
	})
}

func TestRuntimeWriteSurvivesAPublishFailure(t *testing.T) {
	ctx, mem, _ := newMemFixture(t)
	ui := writableUIFixture(t)
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeUIClient(t, ui), Mem: mem,
		Publish: func(context.Context, channelevents.Envelope) error { return errors.New("nats down") },
	}

	_, err := rt.Write(ctx, "panel", uicomponents.Node{Component: "ap:text", Props: map[string]json.RawMessage{"text": json.RawMessage(`"x"`)}})
	require.NoError(t, err, "the memory write already succeeded; a push failure must not fail the tool call")
}

// TestRuntimeWriteNeverPublishesWhenTheDurableRecordFails is mutation-check
// 3 from task-9-brief.md: publish must happen AFTER uiviewmodel.Record, not
// before. A push that preceded the write would let a browser observe a
// state no reconnect could ever reproduce once the write failed.
func TestRuntimeWriteNeverPublishesWhenTheDurableRecordFails(t *testing.T) {
	ctx, realMem, _ := newMemFixture(t)
	mem := &failingPutMemory{Memory: realMem, failNext: true}
	ui := writableUIFixture(t)
	pub := &capturedPublish{}
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeUIClient(t, ui), Mem: mem, Publish: pub.fn,
	}

	_, err := rt.Write(ctx, "panel", uicomponents.Node{
		Component: "ap:markdown",
		Props:     map[string]json.RawMessage{"body": json.RawMessage(`"agent copy"`)},
	})
	require.Error(t, err, "the forced Put failure must surface as a Write error")
	assert.Empty(t, pub.envs, "a failed durable write must never be followed by a push")
}

// TestRuntimeWriteNeverPublishesOnARejectedFragment proves Publish is never
// reached on the reject-before-record path (a non-writable slot) — the
// ordering guarantee only matters once there is something durable to have
// published.
func TestRuntimeWriteNeverPublishesOnARejectedFragment(t *testing.T) {
	ctx, mem, _ := newMemFixture(t)
	ui := writableUIFixture(t) // "notes" is NOT agentWritable
	pub := &capturedPublish{}
	rt := &uiview.Runtime{
		Namespace: "demo-ns", Session: "demo-session", UIName: "demo-ui",
		Client: newFakeUIClient(t, ui), Mem: mem, Publish: pub.fn,
	}

	_, err := rt.Write(ctx, "notes", uicomponents.Node{Component: "ap:text", Props: map[string]json.RawMessage{"text": json.RawMessage(`"nope"`)}})
	require.Error(t, err)
	assert.Empty(t, pub.envs)
}
