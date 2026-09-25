package hooks_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/stretchr/testify/assert"
)

// resolverFor returns an MCPSpecResolver that yields (spec, serverSideName) for
// any tool name. serverSideName is the allowlist key validator.Check uses.
func resolverFor(spec *mcpspec.Spec, serverSideName string) hooks.MCPSpecResolver {
	return func(string) (*mcpspec.Spec, string) { return spec, serverSideName }
}

func TestMcpTrust_AllowsWhenNoDenyRules(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{Name: "read_thing"}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "read_thing"))
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "read_thing", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

func TestMcpTrust_DeniesDestructiveWhenSpecDenies(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{
		Name:    "delete_thing",
		Effects: mcpspec.Effects{Destructive: true},
		Deny:    mcpspec.Deny{Effects: toolspec.DenyEffects{Destructive: true}},
	}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "delete_thing"))
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "delete_thing", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, "destructive")
}

// TestMcpTrust_UsesServerSideToolName pins the fix for the live-runner
// regression: the spec's allowlist is keyed by the SERVER-SIDE tool name
// (e.g. "get_record"), but the LLM calls it by the prefixed LLM name
// (e.g. "srv_get_record"). The resolver supplies the server-side name; the
// hook must pass THAT to validator.Check, otherwise the allowlist always misses
// and every MCP tool is wrongly denied.
func TestMcpTrust_UsesServerSideToolName(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{Name: "get_record"}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "get_record"))
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "srv_get_record", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict,
		"validator must be queried with the server-side name, not the LLM-prefixed name")
}

func TestMcpTrust_NoSpecForTool_Allows(t *testing.T) {
	h := hooks.NewMcpTrust(func(string) (*mcpspec.Spec, string) { return nil, "" })
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "meta_tool", Args: json.RawMessage(`{}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict, "no MCP spec (meta/sandbox tool) ⇒ McpTrust no-ops")
}

// TestMcpTrust_MalformedArgs_FailsClosed guards a validation bypass: swallowing
// an args-JSON unmarshal error leaves argsMap empty, which slips the call past
// every arg-based rule (allowedFields, constraints) unchecked. A tool restricted
// to AllowedFields: ["safe"] must DENY args it cannot parse, never treat them as
// an empty arg-less call.
func TestMcpTrust_MalformedArgs_FailsClosed(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{
		Name: "call_thing",
		Args: mcpspec.Args{AllowedFields: []string{"safe"}},
	}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "call_thing"))
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "call_thing", Args: json.RawMessage(`{bad json`)},
	})
	assertDenied(t, dec)
	assert.Contains(t, dec.Reason, "unparseable")
}

// TestMcpTrust_EmptyArgs_ValidatesNormally confirms the fix does not
// regress legitimate no-arg tool calls: empty (nil or zero-length) args
// JSON is NOT malformed and must still validate normally against the spec.
func TestMcpTrust_EmptyArgs_ValidatesNormally(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{Name: "noop_thing"}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "noop_thing"))

	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "noop_thing", Args: nil},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict, "nil args on a no-arg spec must validate normally, not be denied as malformed")

	dec = h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "noop_thing", Args: json.RawMessage{}},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict, "zero-length args on a no-arg spec must validate normally, not be denied as malformed")
}

// TestMcpTrust_WellFormedArgs_ValidatesAsBefore confirms well-formed args
// still flow through allowedFields enforcement unchanged by the fix.
func TestMcpTrust_WellFormedArgs_ValidatesAsBefore(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{
		Name: "call_thing",
		Args: mcpspec.Args{AllowedFields: []string{"safe"}},
	}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "call_thing"))

	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "call_thing", Args: json.RawMessage(`{"args":{"safe":"x"}}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict, "an allowed field must still validate as before")

	dec = h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "call_thing", Args: json.RawMessage(`{"args":{"evil":"x"}}`)},
	})
	assertDenied(t, dec)
	assert.Contains(t, dec.Reason, "not in allowedFields")
}

// TestMcpTrust_NullInnerArgs_RegressionForNoArgBinding pins the fix for a
// real authz bug: an AgentUI Binding declared with no Args marshals its
// envelope's inner "args" key as JSON null, not an object (encoding/json's
// nil-RawMessage MarshalJSON emits "null"). Before the fix, the type
// assertion on argsMap["args"] silently failed and the allowedFields check
// fell through to evaluating the WRAPPER's own keys (operation_id, _reason,
// args) — none of which are ever in a spec's allowedFields — denying every
// no-arg call regardless of the tool's actual constraints.
func TestMcpTrust_NullInnerArgs_RegressionForNoArgBinding(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{
		Name: "call_thing",
		Args: mcpspec.Args{AllowedFields: []string{"safe"}},
	}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "call_thing"))
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name: "call_thing",
			Args: json.RawMessage(`{"operation_id":"op-1","_reason":"invoke a no-arg binding","args":null}`),
		},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict,
		"a null inner args must validate as a legitimate zero-argument call, not fall through to the wrapper's own keys: got %v", dec.Reason)
}

// TestMcpTrust_AbsentArgsKey_ValidatesAsEmpty confirms the same treatment
// applies when the envelope omits the "args" key entirely, not just sets it
// to null — both mean "the caller supplied no arguments."
func TestMcpTrust_AbsentArgsKey_ValidatesAsEmpty(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{
		Name: "call_thing",
		Args: mcpspec.Args{AllowedFields: []string{"safe"}},
	}}}
	h := hooks.NewMcpTrust(resolverFor(spec, "call_thing"))
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool: &pipeline.ToolCallInfo{
			Name: "call_thing",
			Args: json.RawMessage(`{"operation_id":"op-1","_reason":"why"}`),
		},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict,
		"an envelope missing the args key entirely must validate as a zero-argument call: got %v", dec.Reason)
}

// TestMcpTrust_MalformedInnerArgs_FailsClosed proves a present-but-non-object,
// non-null inner "args" (a string, number, boolean, or array — never
// legitimate; SEP-1913 args are always an object or absent) is denied rather
// than silently treated as empty or as the wrapper. Each case's expected
// substring is distinct so the assertion can't be satisfied by a generic
// "malformed" message that doesn't actually name the offending shape.
func TestMcpTrust_MalformedInnerArgs_FailsClosed(t *testing.T) {
	spec := &mcpspec.Spec{Tools: []mcpspec.Tool{{Name: "call_thing"}}}
	cases := []struct {
		name         string
		args         string
		expectedKind string
	}{
		{name: "inner args is a string", args: `{"operation_id":"op-1","_reason":"r","args":"not an object"}`, expectedKind: "a string"},
		{name: "inner args is a number", args: `{"operation_id":"op-1","_reason":"r","args":42}`, expectedKind: "a number"},
		{name: "inner args is a boolean", args: `{"operation_id":"op-1","_reason":"r","args":true}`, expectedKind: "a boolean"},
		{name: "inner args is an array", args: `{"operation_id":"op-1","_reason":"r","args":[1,2,3]}`, expectedKind: "an array"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hooks.NewMcpTrust(resolverFor(spec, "call_thing"))
			dec := h.Eval(context.Background(), pipeline.Input{
				Point: pipeline.PreToolCall,
				Tool:  &pipeline.ToolCallInfo{Name: "call_thing", Args: json.RawMessage(tc.args)},
			})
			assertDenied(t, dec)
			assert.Contains(t, dec.Reason, tc.expectedKind)
		})
	}
}

func assertDenied(t *testing.T, dec pipeline.Decision) {
	t.Helper()
	assert.Equal(t, pipeline.Deny, dec.Verdict)
}

func TestMcpTrust_Points(t *testing.T) {
	h := hooks.NewMcpTrust(func(string) (*mcpspec.Spec, string) { return nil, "" })
	assert.Equal(t, []pipeline.Point{pipeline.PreToolCall}, h.Points())
	assert.Equal(t, "mcp_trust", h.Name())
}
