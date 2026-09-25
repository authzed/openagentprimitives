package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func TestQueryMemoryTool_Name(t *testing.T) {
	qt := meta.NewQueryMemory()
	assert.Equal(t, "query_memory", qt.Name())
}

// TestQueryMemoryTool_NoMemory_ResultIsTrusted is the Minor-gap regression
// test the reviewer asked for: query_memory has its own hand-written "memory
// not available" IsError result (not routed through tool.ArgParseError), so
// it needs its own coverage that the result stays Trusted (framework text,
// not third-party content) — the meta-tool-untrusted-by-default default
// would otherwise route it through content-guard inspection.
func TestQueryMemoryTool_NoMemory_ResultIsTrusted(t *testing.T) {
	qt := meta.NewQueryMemory()

	// A nil *tool.SessionContext is the easiest no-config path: no Mem
	// querier wired at all.
	res, err := qt.Execute(context.Background(), json.RawMessage(`{}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "memory not available")
	assert.True(t, res.Trusted, "query_memory's 'memory not available' result is framework text and must be Trusted")
}

// TestQueryMemoryTool_NilMem_ResultIsTrusted covers the sibling branch of the
// same guard: a non-nil SessionContext whose Mem field was never wired.
func TestQueryMemoryTool_NilMem_ResultIsTrusted(t *testing.T) {
	qt := meta.NewQueryMemory()
	sess := &tool.SessionContext{Namespace: "default", Name: "sess1"} // Mem left nil

	res, err := qt.Execute(context.Background(), json.RawMessage(`{}`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "memory not available")
	assert.True(t, res.Trusted)
}

// TestQueryMemoryTool_MalformedArgs_ResultIsTrusted exercises the
// tool.ArgParseError shared path directly through the real tool's Execute.
func TestQueryMemoryTool_MalformedArgs_ResultIsTrusted(t *testing.T) {
	qt := meta.NewQueryMemory()
	sess := &tool.SessionContext{Namespace: "default", Name: "sess1"}

	res, err := qt.Execute(context.Background(), json.RawMessage(`{not valid json`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted, "a malformed-args result must be Trusted (routes through tool.ArgParseError)")
}
