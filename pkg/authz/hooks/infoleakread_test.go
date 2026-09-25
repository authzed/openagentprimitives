package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInfoLeakRead_Enforcing_RequesterLacksView_Denies(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(_ context.Context, subj, perm, res string) (bool, error) { return false, nil },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			tainted = append(tainted, r)
			return nil
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool:      &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Empty(t, tainted, "denied read in enforcing mode does NOT record taint")
}

func TestInfoLeakRead_Enforcing_RequesterHasView_AllowsAndTaints(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(_ context.Context, subj, perm, res string) (bool, error) { return true, nil },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			tainted = append(tainted, r)
			return nil
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool:      &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, tainted, 1)
	assert.Equal(t, "ENG-1", tainted[0].ResourceID)
}

func TestInfoLeakRead_Logging_RequesterLacksView_AllowsAuditsTaints(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: "logging",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(_ context.Context, subj, perm, res string) (bool, error) { return false, nil },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			tainted = append(tainted, r)
			return nil
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool:      &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, tainted, 1, "logging mode records taint even on denied view")
	assert.NotEmpty(t, dec.Audit)
}

func TestInfoLeakRead_DisabledMode_NoOp(t *testing.T) {
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{Mode: "disabled"})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool:      &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

// TestInfoLeakRead_UnmappedTool_NoSessionRef_FailsClosedInEnforcing: with no
// session ref there is no floor to build, so enforcing must fail closed (deny)
// rather than silently let undeclared data out; logging is a no-op.
func TestInfoLeakRead_UnmappedTool_NoSessionRef_FailsClosedInEnforcing(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		wantVerdict pipeline.Verdict
	}{
		{"enforcing", pipeline.Deny},
		{"logging", pipeline.Allow},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			var tainted []infoleakagetaint.TaintRecord
			h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
				Mode:        tc.mode, // no SessionRef
				LookupReads: func(string) *hooks.ToolReadsDecl { return nil },
				AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
					tainted = append(tainted, r)
					return nil
				},
			})
			dec := h.Eval(context.Background(), pipeline.Input{
				Point:     pipeline.PostToolCall,
				Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
				Tool:      &pipeline.ToolCallInfo{Name: "unknown_tool", Args: json.RawMessage(`{}`)},
			})
			assert.Equal(t, tc.wantVerdict, dec.Verdict)
			assert.Empty(t, tainted, "no session ref ⇒ no floor taint")
		})
	}
}

// TestInfoLeakRead_UnmappedTool_WithSessionRef_FallsToFloor: an undeclared tool
// with a session ref falls to the coarse floor — a session-participant taint
// (agentsession#unknown_provenance) + ALLOW, never a hard deny — in BOTH
// enforcing and logging. This is the invariant: undeclared data is no more
// restricted than the coarse floor.
func TestInfoLeakRead_UnmappedTool_WithSessionRef_FallsToFloor(t *testing.T) {
	for _, mode := range []string{"enforcing", "logging"} {
		t.Run(mode+": taints the session floor and allows", func(t *testing.T) {
			var tainted []infoleakagetaint.TaintRecord
			h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
				Mode:        mode,
				SessionRef:  "ns1/sess1",
				LookupReads: func(string) *hooks.ToolReadsDecl { return nil },
				AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
					tainted = append(tainted, r)
					return nil
				},
			})
			dec := h.Eval(context.Background(), pipeline.Input{
				Point:     pipeline.PostToolCall,
				Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
				Tool:      &pipeline.ToolCallInfo{Name: "sidecar_read_thing", UseID: "u1", Args: json.RawMessage(`{}`)},
			})
			assert.NotEqual(t, pipeline.Deny, dec.Verdict, "undeclared tool must not hard-deny when a floor is available")
			require.Len(t, tainted, 1, "must record one session-floor taint")
			assert.Equal(t, "agentsession", tainted[0].ResourceType)
			assert.Equal(t, "ns1/sess1", tainted[0].ResourceID)
			assert.Equal(t, "unknown_provenance", tainted[0].Permission)
			assert.Equal(t, "sidecar_read_thing", tainted[0].ToolName)
			assert.Equal(t, "u1", tainted[0].ToolUseID)
			if assert.NotEmpty(t, dec.Audit) {
				assert.Equal(t, "unmapped_tool_floored", dec.Audit[0].Kind)
			}
		})
	}
}

// TestInfoLeakRead_NoTaintDecl_Bypasses: a declared opt-out (the wiring maps
// toolResourceMap.noTaint to a bypass-only decl) must BYPASS entirely — no
// taint, no floor, no deny — distinct from an undeclared tool (nil decl).
func TestInfoLeakRead_NoTaintDecl_Bypasses(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode:        "enforcing",
		SessionRef:  "ns1/sess1",
		LookupReads: func(string) *hooks.ToolReadsDecl { return &hooks.ToolReadsDecl{BypassRequesterCheck: true} },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			tainted = append(tainted, r)
			return nil
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool:      &pipeline.ToolCallInfo{Name: "notaint_tool", UseID: "u1", Args: json.RawMessage(`{}`)},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, tainted, "a noTaint tool opts out of tracking; it must not floor-taint")
	assert.Empty(t, dec.Audit)
}

// TestInfoLeakRead_CheckUnavailable covers the three ways the requester view
// check can fail to produce a verdict — the resolver errors, the resolver
// yields no principal at all (a kubectl-driven session has no channel
// identity), or the SpiceDB check itself errors. Enforcing mode fails closed;
// logging mode, which exists precisely so that nothing changes, must audit the
// unevaluated check and let the read through, exactly as it does for the
// "requester lacks view" verdict.
func TestInfoLeakRead_CheckUnavailable(t *testing.T) {
	const boom = "spicedb unavailable"

	cases := []struct {
		name         string
		mode         string
		requester    func(context.Context, identity.CanonicalUserID) (string, error)
		check        func(context.Context, string, string, string) (bool, error)
		wantVerdict  pipeline.Verdict
		wantTainted  bool
		wantReason   string // read_unchecked reason; "" ⇒ no audit expected
		wantNoChecks bool   // the checker must not be invoked at all
	}{
		{
			name:        "logging + requester resolver errors: Allow, taint recorded, read_unchecked(requester_resolution_failed)",
			mode:        "logging",
			requester:   func(context.Context, identity.CanonicalUserID) (string, error) { return "", errors.New(boom) },
			check:       func(context.Context, string, string, string) (bool, error) { return true, nil },
			wantVerdict: pipeline.Allow,
			wantTainted: true,
			wantReason:  "requester_resolution_failed",
		},
		{
			name:         "logging + no requester on the call: Allow, taint recorded, read_unchecked(no_requester), checker untouched",
			mode:         "logging",
			requester:    func(context.Context, identity.CanonicalUserID) (string, error) { return "", nil },
			check:        func(context.Context, string, string, string) (bool, error) { return true, nil },
			wantVerdict:  pipeline.Allow,
			wantTainted:  true,
			wantReason:   "no_requester",
			wantNoChecks: true,
		},
		{
			name: "logging + SpiceDB check errors: Allow, taint recorded, read_unchecked(check_failed)",
			mode: "logging",
			requester: func(_ context.Context, p identity.CanonicalUserID) (string, error) {
				return p.SubjectRef().String(), nil
			},
			check:       func(context.Context, string, string, string) (bool, error) { return false, errors.New(boom) },
			wantVerdict: pipeline.Allow,
			wantTainted: true,
			wantReason:  "check_failed",
		},
		{
			name:        "enforcing + requester resolver errors: Deny, no taint",
			mode:        "enforcing",
			requester:   func(context.Context, identity.CanonicalUserID) (string, error) { return "", errors.New(boom) },
			check:       func(context.Context, string, string, string) (bool, error) { return true, nil },
			wantVerdict: pipeline.Deny,
		},
		{
			name:         "enforcing + no requester on the call: Deny, no taint, checker untouched",
			mode:         "enforcing",
			requester:    func(context.Context, identity.CanonicalUserID) (string, error) { return "", nil },
			check:        func(context.Context, string, string, string) (bool, error) { return true, nil },
			wantVerdict:  pipeline.Deny,
			wantNoChecks: true,
		},
		{
			name: "enforcing + SpiceDB check errors: Deny, no taint",
			mode: "enforcing",
			requester: func(_ context.Context, p identity.CanonicalUserID) (string, error) {
				return p.SubjectRef().String(), nil
			},
			check:       func(context.Context, string, string, string) (bool, error) { return false, errors.New(boom) },
			wantVerdict: pipeline.Deny,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tainted []infoleakagetaint.TaintRecord
			var checkedSubjects []string
			h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
				Mode: tc.mode,
				LookupReads: func(string) *hooks.ToolReadsDecl {
					return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
				},
				Requester: tc.requester,
				Check: func(ctx context.Context, subj, perm, res string) (bool, error) {
					checkedSubjects = append(checkedSubjects, subj)
					return tc.check(ctx, subj, perm, res)
				},
				AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
					tainted = append(tainted, r)
					return nil
				},
			})

			dec := h.Eval(context.Background(), pipeline.Input{
				Point:     pipeline.PostToolCall,
				Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
				Tool:      &pipeline.ToolCallInfo{Name: "get_issue", Args: json.RawMessage(`{"args":{"id":"ENG-1"}}`), Result: `{}`},
			})

			assert.Equal(t, tc.wantVerdict, dec.Verdict)
			if tc.wantTainted {
				assert.Len(t, tainted, 1, "an unevaluated check in logging mode still records taint for the respond-time gate")
			} else {
				assert.Empty(t, tainted, "a denied read does NOT record taint")
			}
			if tc.wantNoChecks {
				assert.Empty(t, checkedSubjects,
					"an empty requester must short-circuit: SpiceDB's subject parser rejects %q and turns a missing identity into an opaque parse error", "")
			}
			if tc.wantReason != "" {
				require.NotEmpty(t, dec.Audit, "logging mode must audit the unevaluated check")
				last := dec.Audit[len(dec.Audit)-1]
				assert.Equal(t, "read_unchecked", last.Kind)
				assert.Equal(t, tc.wantReason, last.Fields["reason"])
			}
		})
	}
}

func TestInfoLeakRead_Points(t *testing.T) {
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{Mode: "disabled"})
	assert.Equal(t, []pipeline.Point{pipeline.PostToolCall}, h.Points())
	assert.Equal(t, "info_leak_read", h.Name())
}

// TestInfoLeakRead_TaintRecord_CarriesUseID verifies Fix I1: a tool call with
// UseID="tu-1" produces a taint record whose ToolUseID == "tu-1".
func TestInfoLeakRead_TaintRecord_CarriesUseID(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(_ context.Context, subj, perm, res string) (bool, error) { return true, nil },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			tainted = append(tainted, r)
			return nil
		},
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool: &pipeline.ToolCallInfo{
			Name:  "get_issue",
			Args:  json.RawMessage(`{"args":{"id":"ENG-1"}}`),
			UseID: "tu-1",
		},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, tainted, 1)
	assert.Equal(t, "tu-1", tainted[0].ToolUseID,
		"ToolUseID must flow from pipeline.ToolCallInfo.UseID into the taint record (Fix I1)")
}
