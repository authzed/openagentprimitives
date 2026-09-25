package pipeline_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestRegistry_OrdersByOrderThenFiltersByPoint(t *testing.T) {
	r := pipeline.NewRegistry()
	// Register out of order; expect Hooks() to sort ascending by order.
	r.Register(&fakeHook{name: "scope", points: []pipeline.Point{pipeline.PreToolCall}}, 30)
	r.Register(&fakeHook{name: "trust", points: []pipeline.Point{pipeline.PreToolCall}}, 10)
	r.Register(&fakeHook{name: "auth", points: []pipeline.Point{pipeline.PreToolCall}}, 20)
	// A hook on a different point must not appear for PreToolCall.
	r.Register(&fakeHook{name: "leak", points: []pipeline.Point{pipeline.PostToolCall}}, 10)

	got := r.Hooks(pipeline.PreToolCall)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"trust", "auth", "scope"},
		[]string{got[0].Name(), got[1].Name(), got[2].Name()})

	assert.Len(t, r.Hooks(pipeline.PostToolCall), 1)
	assert.Empty(t, r.Hooks(pipeline.SessionEnd))
}

func TestRegistry_HookOnMultiplePoints(t *testing.T) {
	r := pipeline.NewRegistry()
	r.Register(&fakeHook{name: "scope", points: []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}}, 30)
	assert.Len(t, r.Hooks(pipeline.PreToolCall), 1)
	assert.Len(t, r.Hooks(pipeline.PostToolCall), 1)
}
