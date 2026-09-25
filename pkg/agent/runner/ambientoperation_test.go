package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// The three-level ladder from the design: explicit wins, then the operation the
// in-progress plan item opened, then the session root. The point of the ladder
// is that level 3 always answers — which is what makes the audit graph total.
func TestResolveAmbientOperation_prefersExplicitThenPlanThenRoot(t *testing.T) {
	t.Run("an explicit operation_id is used verbatim", func(t *testing.T) {
		reg := operations.New(nil, nil)
		declared := reg.Begin("declared work")
		sess := &tool.SessionContext{Operations: reg}

		got := resolveAmbientOperation(sess, json.RawMessage(
			`{"operation_id":"`+declared.ID+`","_reason":"r","args":{}}`))

		assert.Equal(t, declared.ID, got,
			"an MCP/sandbox envelope already names its operation; ambient resolution must not override it")
		assert.Len(t, reg.All(), 1, "resolving an explicit id must not mint a root")
	})

	t.Run("a meta tool with no envelope falls to the session root", func(t *testing.T) {
		reg := operations.New(nil, nil)
		sess := &tool.SessionContext{Operations: reg}

		got := resolveAmbientOperation(sess, json.RawMessage(`{"query":"who owns billing"}`))

		require.NotEmpty(t, got, "every call must attribute somewhere — that is the whole point")
		root, ok := reg.Get(got)
		require.True(t, ok)
		assert.True(t, root.Root, "the fallback must be the root, not a fresh operation per call")
	})

	// The middle rung: work the agent has declared it is doing RIGHT NOW.
	// Without it every unattributed call in a planned session piles onto the
	// root, and the graph is total but useless — which is not what the slice is
	// for.
	t.Run("an in-progress plan item's operation beats the root", func(t *testing.T) {
		reg := operations.New(nil, nil)
		states := state.NewRegistry(state.Deps{
			Operations:       reg,
			AppendSystemNote: func(context.Context, map[string]any) error { return nil },
		})
		sess := &tool.SessionContext{Operations: reg, State: states}

		store, ok := plans.TryFrom(sess)
		require.True(t, ok, "the plans Kind must be registered for this fixture to mean anything")
		res, err := store.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{
			Items: []plans.Item{{ID: "s1", Label: "gather", Status: plans.StatusInProgress}},
		})
		require.NoError(t, err)
		require.NotNil(t, res.InProgress, "going in_progress must open an operation")

		got := resolveAmbientOperation(sess, json.RawMessage(`{"query":"who owns billing"}`))

		assert.Equal(t, res.InProgress.OperationID, got,
			"an unattributed call belongs to the step the agent says it is on")
		for _, op := range reg.All() {
			assert.False(t, op.Root, "the root must not even be minted while a plan item is live")
		}
	})

	t.Run("no registry wired resolves to nothing rather than panicking", func(t *testing.T) {
		assert.Empty(t, resolveAmbientOperation(&tool.SessionContext{}, json.RawMessage(`{}`)))
		assert.Empty(t, resolveAmbientOperation(nil, json.RawMessage(`{}`)))
	})

	t.Run("an empty operation_id is treated as absent, not as a valid id", func(t *testing.T) {
		reg := operations.New(nil, nil)
		sess := &tool.SessionContext{Operations: reg}

		got := resolveAmbientOperation(sess, json.RawMessage(`{"operation_id":"","_reason":"r","args":{}}`))

		root, ok := reg.Get(got)
		require.True(t, ok, "an empty id must not be handed on as if it addressed an operation")
		assert.True(t, root.Root)
	})
}

// The wiring. Resolution is worth nothing if the resolved operation never
// receives the call: before this, a meta tool made no audit entry at all, which
// is the hole "the audit graph is total" names.
func TestExecuteToolContained_recordsUnattributedCallsAgainstTheAmbientOperation(t *testing.T) {
	reg := operations.New(nil, nil)
	tl := echoTool{name: "query_memory"}
	// Registered on the Loop because ToolCallAuthz resolves a call's Permission
	// by NAME, not from the tool value passed in; an unregistered name resolves
	// to the zero Permission and fails closed before the tool ever runs.
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{tl}}
	sess := &tool.SessionContext{Namespace: "ns", Name: "s", Operations: reg}

	res, out := l.executeToolContained(context.Background(), sess, tl,
		"query_memory", json.RawMessage(`{"query":"who owns billing"}`), "use-1", "recalling context",
		containParams{})
	require.False(t, res.IsError, "fixture tool must succeed: %s", res.Content)
	require.Equal(t, containRanOK, out.Phase)

	root := reg.Root()
	got, ok := reg.Get(root.ID)
	require.True(t, ok)
	require.Len(t, got.Calls, 1, "the meta call must be recorded against the ambient operation")
	assert.Equal(t, "query_memory", got.Calls[0].Tool)
	assert.Equal(t, "recalling context", got.Calls[0].Reason,
		"the justification is what an audit reader correlates against the model trace")
	assert.False(t, got.Calls[0].CompletedAt.IsZero(),
		"a returned call must be marked complete, or it reads as in-flight forever "+
			"and pins the operation into the live activity tree")
}

// An MCP/sandbox call names its own operation and its dispatcher already
// records it. Recording centrally as well would double-count every external
// call — the failure this predicate exists to prevent.
func TestExecuteToolContained_doesNotDoubleRecordAnExplicitlyAttributedCall(t *testing.T) {
	reg := operations.New(nil, nil)
	declared := reg.Begin("declared work")
	tl := echoTool{name: "code_gh"}
	l := &Loop{AgentName: "demo-agent", Tools: []tool.Tool{tl}}
	sess := &tool.SessionContext{Namespace: "ns", Name: "s", Operations: reg}

	_, out := l.executeToolContained(context.Background(), sess, tl,
		"code_gh", json.RawMessage(`{"operation_id":"`+declared.ID+`","_reason":"r","args":{}}`),
		"use-2", "r", containParams{})
	require.Equal(t, containRanOK, out.Phase)

	got, ok := reg.Get(declared.ID)
	require.True(t, ok)
	assert.Empty(t, got.Calls,
		"the dispatcher owns recording for explicitly-attributed calls; this site must stay out")
}

// echoTool is a minimal successful tool: the choke point under test is the
// runner's, not any particular tool kind's.
type echoTool struct{ name string }

func (e echoTool) Name() string  { return e.name }
func (echoTool) Kind() tool.Kind { return tool.KindMeta }
func (echoTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (echoTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (echoTool) Description() string                           { return "echo" }
func (echoTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{"type":"object"}`) }
func (echoTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{Content: "ok"}, nil
}
