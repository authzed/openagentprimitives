package scope_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func TestScope_RoundTripJSON(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"foo/bar", "baz/qux"},
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo", "name": "lib-*"}}},
			Source:       scope.SourceDefault,
		}},
		Tools: scope.ScopeTools{
			Allow: []string{"github.*"},
			Deny:  []string{"github.delete_*"},
		},
		ArgConstraints: []scope.ArgConstraint{{
			Tool:   "github.create_pr",
			Equals: map[string]string{"base": "main"},
			Forbid: map[string]string{"force_push": "true"},
			CEL:    "args.title.size() < 200",
		}},
		ScopeVersion: 7,
	}

	raw, err := json.Marshal(s)
	require.NoError(t, err)

	var back scope.Scope
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, s, back)
}

func TestResourceRef_String(t *testing.T) {
	r := scope.ResourceRef{ResourceType: "github_repo", ID: "foo/bar"}
	assert.Equal(t, "github_repo:foo/bar", r.String())
}

func TestScope_OmitemptyBehavior(t *testing.T) {
	// Empty Scope marshals to a minimal JSON object — only scopeVersion appears.
	raw, err := json.Marshal(scope.Scope{})
	require.NoError(t, err)
	// scopeVersion is NOT marked omitempty, so it'll appear as 0.
	// resources, tools, argConstraints, expiry are omitempty.
	assert.JSONEq(t, `{"scopeVersion":0}`, string(raw))
}

func TestResourceRef_StringWithEmptyID(t *testing.T) {
	r := scope.ResourceRef{ResourceType: "github_repo", ID: ""}
	// Don't crash; produce "<type>:"
	assert.Equal(t, "github_repo:", r.String())
}

func TestZeroValueScope_HasZeroVersion(t *testing.T) {
	var s scope.Scope
	assert.Equal(t, int64(0), s.ScopeVersion)
}

func TestScopeDelta_RoundTripJSON(t *testing.T) {
	d := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
			Tools:     []string{"github.list_*"},
		},
		Remove: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "old/repo"}},
		},
		HardDeny: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1234"}},
			Tools:     []string{"github.delete_repo"},
		},
	}

	raw, err := json.Marshal(d)
	require.NoError(t, err)

	var back scope.ScopeDelta
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, d, back)
}

func TestScopePartial_FlattenResources(t *testing.T) {
	p := scope.ScopePartial{
		Resources: []scope.ResourceRef{
			{ResourceType: "github_repo", ID: "foo/bar"},
			{ResourceType: "github_repo", ID: "baz/qux"},
		},
		ResourcePatterns: []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo"}}},
	}
	got := p.FlattenResources()
	require.Len(t, got, 2)
	assert.Equal(t, "github_repo:foo/bar", got[0].String())
	assert.Equal(t, "github_repo:baz/qux", got[1].String())
}

func TestScopePartial_IsEmpty(t *testing.T) {
	assert.True(t, scope.ScopePartial{}.IsEmpty())
	assert.False(t, scope.ScopePartial{Tools: []string{"x"}}.IsEmpty())
	assert.False(t, scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "t", ID: "i"}}}.IsEmpty())
	assert.False(t, scope.ScopePartial{ResourcePatterns: []scope.ResourcePattern{{Attrs: map[string]string{"k": "v"}}}}.IsEmpty())
	assert.False(t, scope.ScopePartial{ArgConstraints: []scope.ArgConstraint{{Tool: "t"}}}.IsEmpty())
}

func TestScopeDelta_IsEmpty(t *testing.T) {
	assert.True(t, scope.ScopeDelta{}.IsEmpty())
	assert.False(t, scope.ScopeDelta{Add: scope.ScopePartial{Tools: []string{"x"}}}.IsEmpty())
	assert.False(t, scope.ScopeDelta{Remove: scope.ScopePartial{Tools: []string{"x"}}}.IsEmpty())
	assert.False(t, scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"x"}}}.IsEmpty())
}

func TestMetaagentOutput_RoundTripJSON(t *testing.T) {
	m := scope.MetaagentOutput{
		Delta: scope.ScopeDelta{
			Add: scope.ScopePartial{
				Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
			},
		},
		Skipped: []scope.SkippedItem{{
			RequestFragment: "linear_issue:L-1",
			Reason:          scope.ReasonOutOfEnvelopeRtype,
			Explanation:     "Linear isn't set up.",
		}},
		Caveats: []scope.CaveatItem{{
			AppliedFragment: "disallow github_repo:bad/repo",
			Category:        scope.CaveatSearchToolCanReturn,
			Explanation:     "Search tools could still return it.",
		}},
		ApproverSummary: "If you approve, the agent will be able to read foo/bar.",
		SkippedExplain:  "...",
		CaveatExplain:   "...",
	}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	var back scope.MetaagentOutput
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, m, back)
}

func TestMetaagentOutput_CannotAddress(t *testing.T) {
	m := scope.MetaagentOutput{
		CannotAddress:        true,
		CannotAddressMessage: "I couldn't help with that.",
	}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	var back scope.MetaagentOutput
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, m, back)
	assert.True(t, back.CannotAddress)
}

func TestMetaagentOutput_AllEmpty_RoundTrips(t *testing.T) {
	m := scope.MetaagentOutput{}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	var back scope.MetaagentOutput
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, m, back)
}
