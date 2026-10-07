package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/tools/toolkitstream"
)

// Locks the captured slice because besteffort.Log + callback chaining
// may stash work in different goroutines if the publisher contract
// ever changes; cheap insurance against -race flakes.
func TestBuildToolSessionEventPublisher_PublishesKindToolSessionEvent(t *testing.T) {
	var (
		mu       sync.Mutex
		captured []channelevents.Envelope
	)
	pub := func(_ context.Context, _ string, body []byte) error {
		mu.Lock()
		defer mu.Unlock()
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(body, &env))
		captured = append(captured, env)
		return nil
	}

	// logMode "off" isolates this test to the NATS publish path — the
	// publish boundary is unchanged regardless of the persist gate.
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/agent-1"}
	onEvent := buildToolSessionEventPublisher(
		context.Background(), pub, mem, scope, "off", "default", "agent-1", nil, nil)
	onEvent("tc-abc", "", "", toolkitstream.Event{Type: toolkitstream.EventTextDelta, Text: "hi"})

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, captured, 1)
	assert.Equal(t, channelevents.KindToolSessionEvent, captured[0].Kind)

	var pl channelevents.ToolSessionEventPayload
	require.NoError(t, json.Unmarshal(captured[0].Payload, &pl))
	assert.Equal(t, "tc-abc", pl.ToolCallRef)
	assert.Equal(t, "text_delta", pl.EventType)
	assert.Equal(t, "hi", pl.Text)
}

func TestBuildToolSessionEventPublisher_CarriesReasonAndOuterTool(t *testing.T) {
	var published [][]byte
	pub := func(_ context.Context, _ string, b []byte) error {
		published = append(published, b)
		return nil
	}
	emit := buildToolSessionEventPublisher(context.Background(), pub,
		nil /*mem*/, memory.Scope{} /*scope*/, spiceboxv1alpha1.ToolSessionLogOff /*no persist*/, "ns", "sess", nil, nil)

	emit("tc-1", "Have claude write the README", "claude",
		toolkitstream.Event{Type: toolkitstream.EventTextDelta, Text: "hi"})

	require.Len(t, published, 1)
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(published[0], &env))
	var pl channelevents.ToolSessionEventPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl))
	assert.Equal(t, "Have claude write the README", pl.Reason)
	assert.Equal(t, "claude", pl.OuterTool)
}

func TestBuildToolSessionEventPublisher_AllFieldsRoundTrip(t *testing.T) {
	var (
		mu       sync.Mutex
		captured []channelevents.Envelope
	)
	pub := func(_ context.Context, _ string, body []byte) error {
		mu.Lock()
		defer mu.Unlock()
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(body, &env))
		captured = append(captured, env)
		return nil
	}

	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/agent-1"}
	onEvent := buildToolSessionEventPublisher(
		context.Background(), pub, mem, scope, "off", "default", "agent-1", nil, nil)

	onEvent("tc-1", "", "", toolkitstream.Event{
		Type:    toolkitstream.EventToolUseStop,
		ToolID:  "tu_1",
		OK:      false,
		Summary: "exit code 1",
	})

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, captured, 1)
	var pl channelevents.ToolSessionEventPayload
	require.NoError(t, json.Unmarshal(captured[0].Payload, &pl))
	assert.Equal(t, "tool_use_stop", pl.EventType)
	assert.Equal(t, "tu_1", pl.ToolID)
	assert.False(t, pl.OK)
	assert.Equal(t, "exit code 1", pl.Summary)
}

func TestBuildToolSessionEventPublisher_InvokesOnResultForResultEvent(t *testing.T) {
	type call struct {
		tool string
		cost float64
		ok   bool
	}
	var got []call
	onResult := func(tool string, cost float64, ok bool) {
		got = append(got, call{tool, cost, ok})
	}
	pub := func(_ context.Context, _ string, _ []byte) error { return nil }

	emit := buildToolSessionEventPublisher(context.Background(), pub,
		nil, memory.Scope{}, spiceboxv1alpha1.ToolSessionLogOff, "ns", "sess", nil, onResult)

	// A non-result event must NOT trigger onResult.
	emit("tc-1", "reason", "claude-oauth",
		toolkitstream.Event{Type: toolkitstream.EventTextDelta, Text: "hi"})
	// A result event MUST trigger it once, carrying tool/cost/ok.
	emit("tc-1", "reason", "claude-oauth",
		toolkitstream.Event{Type: toolkitstream.EventResult, OK: true, CostUSD: 5.43})

	require.Len(t, got, 1, "onResult fires only on EventResult")
	assert.Equal(t, "claude-oauth", got[0].tool)
	assert.Equal(t, 5.43, got[0].cost)
	assert.True(t, got[0].ok)
}
