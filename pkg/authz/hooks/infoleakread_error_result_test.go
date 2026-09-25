package hooks_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The read gate skipped every result carrying isError, on the reasoning that
// "errored results carry no resource to scope-check / taint / gate".
//
// That reasoning holds for a call that actually failed. It does not hold for
// the flag, which the UPSTREAM SETS: an MCP server can return a full issue body
// with isError true, and the result was then delivered to the model with no view
// check and no taint recorded. A gate whose off-switch is a boolean the other
// side writes is not a gate.
//
// So the skip moves from the flag to the FACT: proceed, and treat a result whose
// resource cannot be resolved as nothing-to-gate rather than as a
// misconfiguration. A genuine failure resolves nothing and passes cleanly, which
// is the behaviour the skip was protecting; a failure carrying real content
// resolves its id and is checked like any other read.

func errorResultHook(t *testing.T, mode string, allow bool, tainted *[]infoleakagetaint.TaintRecord) *hooks.InfoLeakRead {
	t.Helper()
	return hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: mode,
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(_ context.Context, _, _, _ string) (bool, error) { return allow, nil },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			*tainted = append(*tainted, r)
			return nil
		},
	})
}

// A hostile upstream returns content under isError. The requester may see the
// resource, so the read is allowed — but it must be TAINTED, or the leakage
// tracker has no record that this session ever saw it.
func TestInfoLeakRead_ErrorResultWithAResolvableResource_IsStillTainted(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := errorResultHook(t, "enforcing", true, &tainted)

	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool: &pipeline.ToolCallInfo{
			Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`),
			Result: `{"title":"the confidential one"}`, IsError: true,
		},
	})

	assert.NotEqual(t, pipeline.Deny, dec.Verdict, "the requester holds view; the read is allowed")
	require.Len(t, tainted, 1,
		"content delivered under isError must still record that this session saw the resource")
	assert.Equal(t, "issue:ENG-1", tainted[0].ResourceType+":"+tainted[0].ResourceID)
}

// And the enforcing denial: a requester WITHOUT view must not receive the
// resource merely because the upstream flagged the response as an error.
func TestInfoLeakRead_ErrorResultWithoutView_IsDeniedInEnforcing(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := errorResultHook(t, "enforcing", false, &tainted)

	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool: &pipeline.ToolCallInfo{
			Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`),
			Result: `{"title":"the confidential one"}`, IsError: true,
		},
	})

	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"isError is set by the upstream; it cannot be the thing that opens the gate")
	assert.Empty(t, tainted, "a denied read records no taint, error or not")
}

// The counterweight, and the case the original skip existed for: a call that
// really failed carries no resolvable resource. It must pass cleanly rather
// than be treated as a misconfigured declaration, which in enforcing mode would
// deny every failed tool call in the session.
func TestInfoLeakRead_GenuineFailureWithNoResolvableResource_PassesCleanly(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := errorResultHook(t, "enforcing", false, &tainted)

	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool: &pipeline.ToolCallInfo{
			Name: "get_issue", Args: json.RawMessage(`{"args":{}}`),
			Result: "connection refused", IsError: true,
		},
	})

	assert.NotEqual(t, pipeline.Deny, dec.Verdict,
		"a real failure names no resource and must not read as a broken declaration")
	assert.Empty(t, tainted, "nothing was seen, so nothing is tainted")
}
