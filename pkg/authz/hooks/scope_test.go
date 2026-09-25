package hooks_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// disallowIssue returns a scope.Scope whose ResourceDisallowed("issue", id) is true.
func disallowIssue(id string) scope.Scope {
	return scope.Scope{
		Disallow: []scope.ScopeResource{
			{ResourceType: "issue", IDs: []string{id}},
		},
	}
}

func issueDecl() *hooks.ToolReadsDecl {
	return &hooks.ToolReadsDecl{ResourceType: "issue", IDArg: "id"}
}

func TestScope_PreToolCall_DisallowedIDInArgs_Denies(t *testing.T) {
	h := hooks.NewScope(hooks.ScopeDeps{
		Enabled:     true,
		LookupReads: func(string) *hooks.ToolReadsDecl { return issueDecl() },
		GetScope:    func(context.Context) (scope.Scope, bool, error) { return disallowIssue("ENG-1"), true, nil },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`)},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Contains(t, dec.Reason, "disallowed")
}

func TestScope_PreToolCall_IDOnlyInResult_NoOp(t *testing.T) {
	// idArg not present in args (id only comes back in the result) ⇒ pre-fetch is a no-op;
	// the post-fetch path handles it.
	h := hooks.NewScope(hooks.ScopeDeps{
		Enabled: true,
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", ResultIDField: "id"}
		},
		GetScope: func(context.Context) (scope.Scope, bool, error) { return disallowIssue("ENG-1"), true, nil },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{}}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

func TestScope_PostToolCall_DisallowedIDInResult_Denies(t *testing.T) {
	h := hooks.NewScope(hooks.ScopeDeps{
		Enabled: true,
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", ResultIDField: "id"}
		},
		GetScope: func(context.Context) (scope.Scope, bool, error) { return disallowIssue("ENG-1"), true, nil },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PostToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{}}`), Result: `{"id":"ENG-1"}`},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
}

func TestScope_ScopeReadFails_FailsClosed(t *testing.T) {
	h := hooks.NewScope(hooks.ScopeDeps{
		Enabled:     true,
		LookupReads: func(string) *hooks.ToolReadsDecl { return issueDecl() },
		GetScope:    func(context.Context) (scope.Scope, bool, error) { return scope.Scope{}, false, assert.AnError },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`)},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict, "scope doc unavailable ⇒ refuse to serve (fail closed)")
}

func TestScope_DisabledScope_NoOp(t *testing.T) {
	h := hooks.NewScope(hooks.ScopeDeps{Enabled: false})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`)},
	})
	require.Equal(t, pipeline.Allow, dec.Verdict)
}

func TestScope_Points(t *testing.T) {
	h := hooks.NewScope(hooks.ScopeDeps{Enabled: true})
	assert.ElementsMatch(t, []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}, h.Points())
	assert.Equal(t, "scope", h.Name())
}

// TestScope_PreToolCall_MalformedArgs_Denies guards against a fail-open within
// the gate: when the LLM-supplied tool args are not valid JSON, the
// arg-constraint pass must NOT evaluate against an empty map (which silently
// passes a Forbid constraint). Unparseable args ⇒ Deny, refusing to dispatch.
func TestScope_PreToolCall_MalformedArgs_Denies(t *testing.T) {
	// Scope with a Forbid constraint on tool "run" arg "danger"=="yes".
	// Against {} the constraint would pass (arg absent) — that's the bug.
	sc := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{
			{Tool: "run", Forbid: map[string]string{"danger": "yes"}},
		},
	}
	h := hooks.NewScope(hooks.ScopeDeps{
		Enabled:     true,
		LookupReads: func(string) *hooks.ToolReadsDecl { return nil },
		GetScope:    func(context.Context) (scope.Scope, bool, error) { return sc, true, nil },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		// Malformed JSON: a trailing comma + unterminated object.
		Tool: &pipeline.ToolCallInfo{Name: "run", Args: json.RawMessage(`{"args":{"danger":`)},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict, "unparseable args ⇒ Deny, not silent pass")
	assert.Contains(t, dec.Reason, "unparseable")
}

func TestScope_PreToolCall_ToolDeniedByPolicy_Denies(t *testing.T) {
	// CheckScope narrowing: tool not in allow list → deny
	sc := scope.Scope{
		Tools: scope.ScopeTools{Allow: []string{"allowed_tool"}},
	}
	h := hooks.NewScope(hooks.ScopeDeps{
		Enabled:     true,
		LookupReads: func(string) *hooks.ToolReadsDecl { return nil },
		GetScope:    func(context.Context) (scope.Scope, bool, error) { return sc, true, nil },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point: pipeline.PreToolCall,
		Tool:  &pipeline.ToolCallInfo{Name: "disallowed_tool", Args: json.RawMessage(`{}`)},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
}
