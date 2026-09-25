package permsurface

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func permCandidate(toolName, impact, permission, resourceType string) Candidate {
	return Candidate{
		ToolName: toolName,
		Permission: authz.Permission{
			StateImpact: authz.StateImpact(impact),
			Check:       &authz.PermissionCheck{Permission: permission, ResourceType: resourceType},
		},
	}
}

func handlesOf(t *testing.T, ds []Descriptor) []string {
	t.Helper()
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Handle.String())
	}
	return out
}

// stateless/passthrough are always-live infrastructure: never declared, never
// gated, and therefore never on the surface.
func TestEnumerate_excludesStatelessAndPassthrough(t *testing.T) {
	got := Enumerate([]Candidate{
		{ToolName: "update_plan", Permission: authz.Permission{StateImpact: authz.Passthrough}},
		{ToolName: "respond_to_user", Permission: authz.Permission{StateImpact: authz.Stateless}},
		permCandidate("gh_pr_view", "readonly", "read", "github_repo"),
	})
	assert.Equal(t, []string{"perm:read:github_repo"}, handlesOf(t, got))
}

// A tool with no Check falls back to a tool: handle.
func TestEnumerate_checklessToolYieldsToolHandle(t *testing.T) {
	got := Enumerate([]Candidate{
		{ToolName: "apply_workspace", Permission: authz.Permission{StateImpact: authz.External}},
	})
	require.Len(t, got, 1)
	assert.Equal(t, "tool:apply_workspace", got[0].Handle.String())
	assert.Equal(t, "apply_workspace", got[0].ToolName)
	assert.Equal(t, authz.External, got[0].StateImpact)
}

// Two tools naming the same (permission, resourceType) genuinely share one
// SpiceDB check, so they must merge into a single descriptor carrying both
// provenance entries.
func TestEnumerate_mergesProducersOfTheSameHandle(t *testing.T) {
	got := Enumerate([]Candidate{
		permCandidate("gh_pr_view", "readonly", "read", "github_repo"),
		permCandidate("gh_pr_list", "readonly", "read", "github_repo"),
	})
	require.Len(t, got, 1)
	assert.Equal(t, "perm:read:github_repo", got[0].Handle.String())
	assert.Equal(t, []Provenance{{Tool: "gh_pr_view"}, {Tool: "gh_pr_list"}}, got[0].Via)
}

// Divergent impacts on one handle resolve to MAX severity — fail-closed
// toward more approval, never less.
func TestEnumerate_stateImpactDivergenceTakesMaxSeverity(t *testing.T) {
	got := Enumerate([]Candidate{
		permCandidate("safe_reader", "readonly", "read", "github_repo"),
		permCandidate("risky_reader", "external", "read", "github_repo"),
	})
	require.Len(t, got, 1)
	assert.Equal(t, authz.External, got[0].StateImpact)
}

// Variants let ONE tool span several handles — strictly finer than tool-name
// gating, and the reason classes beat tool lists.
func TestEnumerate_variantsSpanMultipleHandles(t *testing.T) {
	got := Enumerate([]Candidate{{
		ToolName:   "tracker_update",
		Permission: authz.Permission{StateImpact: authz.Readwrite, Check: &authz.PermissionCheck{Permission: "write", ResourceType: "tracker_issue"}},
		Variants: []authz.PermissionVariant{{
			When: `args.op == "comment"`,
			Check: authz.Permission{
				StateImpact: authz.Readwrite,
				Check:       &authz.PermissionCheck{Permission: "comment", ResourceType: "tracker_issue"},
			},
		}},
	}})
	assert.ElementsMatch(t,
		[]string{"perm:comment:tracker_issue", "perm:write:tracker_issue"},
		handlesOf(t, got))

	for _, d := range got {
		if d.Handle.String() == "perm:comment:tracker_issue" {
			assert.Equal(t, []Provenance{{Tool: "tracker_update", Condition: `args.op == "comment"`}}, d.Via)
		}
	}
}

// Fail-closed: a candidate that cannot mint a handle is ABSENT from the
// surface. tool.ValidateEnvelope surfaces such a tool at startup.
func TestEnumerate_unhandleableCandidateIsExcluded(t *testing.T) {
	got := Enumerate([]Candidate{
		permCandidate("evil", "readwrite", "wr:ite", "github_repo"),
		{ToolName: "bad name", Permission: authz.Permission{StateImpact: authz.External}},
		permCandidate("gh_pr_view", "readonly", "read", "github_repo"),
	})
	assert.Equal(t, []string{"perm:read:github_repo"}, handlesOf(t, got))
}

// Deterministic ordering: severity descending, then resourceType, permission,
// toolName. Stable rendering and stable digests both depend on it.
func TestEnumerate_ordersBySeverityThenName(t *testing.T) {
	got := Enumerate([]Candidate{
		permCandidate("a_read", "readonly", "read", "github_repo"),
		{ToolName: "apply_workspace", Permission: authz.Permission{StateImpact: authz.External}},
		permCandidate("b_write", "readwrite", "write", "github_repo"),
		permCandidate("c_read", "readonly", "read", "tracker_issue"),
	})
	assert.Equal(t, []string{
		"tool:apply_workspace",   // external
		"perm:write:github_repo", // readwrite
		"perm:read:github_repo",  // readonly, github_repo sorts before tracker_issue
		"perm:read:tracker_issue",
	}, handlesOf(t, got))
}

func TestEnumerate_emptyInputYieldsEmptySurface(t *testing.T) {
	assert.Empty(t, Enumerate(nil))
}

// A check whose resourceIDTemplate has no {placeholder} names the SAME instance
// on every call — git.yaml keys git_repo read/write on the literal "workspace",
// the session's own checked-out copy. That instance is knowable at plan time,
// with no tool args, so an approval card can name it.
//
// It matters because a phase's declared slot names ONE instance (the remote
// URL) while its ceiling also touches this second, constant one. A card that
// shows only the declared slot tells an approver the phase reaches one
// repository when it will in fact also read and write a different object.
func TestEnumerate_ConstantResourceID_IsCaptured(t *testing.T) {
	got := Enumerate([]Candidate{{
		ToolName: "git",
		Permission: authz.Permission{
			StateImpact: authz.Readonly,
			Check: &authz.PermissionCheck{
				ResourceType:       "git_repo",
				Permission:         "read",
				ResourceIDTemplate: "workspace",
			},
		},
	}})
	require.Len(t, got, 1)
	assert.Equal(t, "workspace", got[0].ConstantResourceID,
		"a placeholder-free template resolves to one instance, known before any call")
}

// The converse: a template WITH a placeholder depends on tool args, so no
// instance is knowable at plan time and the field must stay empty rather than
// carrying the raw template. Emitting "{remote}" as though it were an object id
// would put a literal brace on an approval card.
func TestEnumerate_TemplatedResourceID_IsNotConstant(t *testing.T) {
	got := Enumerate([]Candidate{{
		ToolName: "git",
		Permission: authz.Permission{
			StateImpact: authz.External,
			Check: &authz.PermissionCheck{
				ResourceType:       "git_repo",
				Permission:         "push",
				ResourceIDTemplate: "{remote}",
			},
		},
	}})
	require.Len(t, got, 1)
	assert.Empty(t, got[0].ConstantResourceID, "an arg-dependent id is not knowable at plan time")
}

// A CEL expression is arg-dependent by construction (git.yaml's fetch/push use
// one), so it is never constant even though it carries no braces.
func TestEnumerate_ExprResourceID_IsNotConstant(t *testing.T) {
	got := Enumerate([]Candidate{{
		ToolName: "git",
		Permission: authz.Permission{
			StateImpact: authz.External,
			Check: &authz.PermissionCheck{
				ResourceType:   "git_repo",
				Permission:     "fetch",
				ResourceIDExpr: `has(args.remote) ? args.remote : ""`,
			},
		},
	}})
	require.Len(t, got, 1)
	assert.Empty(t, got[0].ConstantResourceID, "a CEL expression reads args, so it is never plan-time constant")
}

// Two producers of the SAME handle that disagree on the constant leave it
// empty. Claiming one of them would name an instance the other tool never
// touches, and a card must not assert an instance it cannot stand behind.
func TestEnumerate_DisagreeingConstants_ClearTheField(t *testing.T) {
	got := Enumerate([]Candidate{
		{ToolName: "git", Permission: authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "git_repo", Permission: "read", ResourceIDTemplate: "workspace"},
		}},
		{ToolName: "other", Permission: authz.Permission{
			StateImpact: authz.Readonly,
			Check:       &authz.PermissionCheck{ResourceType: "git_repo", Permission: "read", ResourceIDTemplate: "elsewhere"},
		}},
	})
	require.Len(t, got, 1, "same permission+type is one handle")
	assert.Empty(t, got[0].ConstantResourceID, "disagreeing producers cannot name a single instance")
}
