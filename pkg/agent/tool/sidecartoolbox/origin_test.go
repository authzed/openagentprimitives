package sidecartoolbox

import (
	"context"
	"encoding/json"
	"testing"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTool returns a fixed Result on every call, optionally returning an
// error. Counter exposes how many times Execute fired for assertions.
type stubTool struct {
	res   agenttool.Result
	err   error
	calls int
	name  string
	desc  string
}

func (s *stubTool) Name() string                                  { return s.name }
func (s *stubTool) Kind() agenttool.Kind                          { return agenttool.KindMCP }
func (s *stubTool) Description() string                           { return s.desc }
func (s *stubTool) InputSchema() json.RawMessage                  { return json.RawMessage("{}") }
func (s *stubTool) Permission() authz.Permission                  { return authz.Permission{} }
func (s *stubTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (s *stubTool) Execute(_ context.Context, _ json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	s.calls++
	return s.res, s.err
}

func TestOriginTool_ImplementsOriginTool(t *testing.T) {
	o := &originTool{sidecarRef: "mybox"}
	var ot agenttool.OriginTool = o
	assert.Equal(t, "sidecartoolbox/mybox", ot.Origin())
}

func TestOriginTool_DelegatesNameAndDescription(t *testing.T) {
	inner := &stubTool{name: "echo_ping", desc: "sends a ping"}
	o := &originTool{inner: inner, sidecarRef: "echo"}
	assert.Equal(t, "echo_ping", o.Name())
	assert.Equal(t, "sends a ping", o.Description())
}

func TestOriginTool_DelegatesExecute(t *testing.T) {
	inner := &stubTool{res: agenttool.Result{Content: "pong"}}
	o := &originTool{inner: inner, sidecarRef: "echo"}
	res, err := o.Execute(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "pong", res.Content)
	assert.Equal(t, 1, inner.calls)
}

// TestSynthesizedToolsCarrySidecarOrigin verifies that every tool returned
// by the real Synthesize call implements agenttool.OriginTool with
// Origin() == "sidecartoolbox/<ref>". This catches a missing-wrap regression
// that the pure-struct tests above cannot catch: if Synthesize ever stops
// applying the &originTool{} wrapper, this test fails while the others pass.
func TestSynthesizedToolsCarrySidecarOrigin(t *testing.T) {
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "echo",
		Ref:  "mybox",
		Port: 19999,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "echo"},
				{Name: "ping"},
			},
		},
	}
	live := []probe.Tool{
		{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
		{Name: "ping", InputSchema: []byte(`{"type":"object"}`)},
	}

	tools, err := Synthesize(rt, live, nil)
	require.NoError(t, err, "Synthesize must succeed")
	require.Len(t, tools, 2, "one tool per allowlisted entry")

	for i, tool := range tools {
		ot, ok := tool.(agenttool.OriginTool)
		require.Truef(t, ok, "tool[%d] (%T) must implement agenttool.OriginTool — missing &originTool{} wrap in Synthesize?", i, tool)
		assert.Equalf(t, "sidecartoolbox/mybox", ot.Origin(),
			"tool[%d] Origin() must be sidecartoolbox/<ref>", i)
		// Name must carry the synthesizer's "<prefix>_<name>" form.
		assert.Containsf(t, tool.Name(), "echo_", "tool[%d] Name() should carry the sidecar prefix", i)
	}
}

// TestSynthesizedToolsAreCancellable verifies that every tool returned by the
// real Synthesize call satisfies tool.Cancellable — sidecartoolbox tools wrap
// mcp.MCPTool (via the originTool field, not embedding), so without a
// forwarding Cancel method the wrapper would NOT inherit MCPTool's Cancel by
// promotion. Locks the P1b requirement that sidecar tools are as
// interruptible as plain MCP tools.
func TestSynthesizedToolsAreCancellable(t *testing.T) {
	rt := spiceboxv1alpha1.ResolvedSidecarToolbox{
		Name: "echo",
		Ref:  "mybox",
		Port: 19999,
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{Name: "echo"},
			},
		},
	}
	live := []probe.Tool{
		{Name: "echo", InputSchema: []byte(`{"type":"object"}`)},
	}

	tools, err := Synthesize(rt, live, nil)
	require.NoError(t, err, "Synthesize must succeed")
	require.Len(t, tools, 1)

	c, ok := tools[0].(agenttool.Cancellable)
	require.Truef(t, ok, "tool (%T) must implement agenttool.Cancellable — originTool needs a forwarding Cancel", tools[0])
	assert.NoError(t, c.Cancel(context.Background()))
}

// TestOriginTool_CancelForwardsToInner confirms the forwarder delegates to
// inner.Cancel when inner implements tool.Cancellable, and degrades to a
// harmless no-op when it does not — never panicking either way.
func TestOriginTool_CancelForwardsToInner(t *testing.T) {
	t.Run("inner is Cancellable: forwards and returns inner's result", func(t *testing.T) {
		inner := &cancellableStubTool{stubTool: stubTool{name: "echo"}}
		o := &originTool{inner: inner, sidecarRef: "echo"}

		require.NoError(t, o.Cancel(context.Background()))
		assert.Equal(t, 1, inner.cancelCalls)
	})

	t.Run("inner is not Cancellable: no-op, no panic", func(t *testing.T) {
		inner := &stubTool{name: "echo"}
		o := &originTool{inner: inner, sidecarRef: "echo"}

		assert.NoError(t, o.Cancel(context.Background()))
	})
}

// cancellableStubTool extends stubTool with a Cancel method so the forwarder
// test can distinguish "forwarded" from "no-op".
type cancellableStubTool struct {
	stubTool
	cancelCalls int
}

func (s *cancellableStubTool) Cancel(context.Context) error {
	s.cancelCalls++
	return nil
}
