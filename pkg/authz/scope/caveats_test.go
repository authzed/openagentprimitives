package scope_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func TestDetectCaveats_SearchToolCanReturn(t *testing.T) {
	env := scope.AgentClassEnvelope{
		Tools: []scope.EnvelopeTool{
			{Name: "linear.get_issue"},
			{Name: "linear.search_issues"},
		},
		BoundEntities: []scope.EnvelopeBoundEntity{
			{ResourceType: "linear_issue", Permission: "read"},
		},
	}
	toolReads := map[string]scope.ToolReads{
		"linear.get_issue":     {ResourceType: "linear_issue", IDArg: "issueId", QueryStyle: false},
		"linear.search_issues": {ResourceType: "linear_issue", IDArg: "", QueryStyle: true},
	}
	d := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1234"}},
		},
	}
	caveats := scope.DetectCaveats(d, env, toolReads)
	require.Len(t, caveats, 1)
	assert.Equal(t, scope.CaveatSearchToolCanReturn, caveats[0].Category)
	assert.Contains(t, caveats[0].AppliedFragment, "L-1234")
}

func TestDetectCaveats_PatternNotEnumerable(t *testing.T) {
	d := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			ResourcePatterns: []scope.ResourcePattern{{Attrs: map[string]string{"owner": "internal-*"}}},
		},
	}
	caveats := scope.DetectCaveats(d, scope.AgentClassEnvelope{}, nil)
	require.Len(t, caveats, 1)
	assert.Equal(t, scope.CaveatPatternNotEnumerable, caveats[0].Category)
}

func TestDetectCaveats_ListToolReturnsParentTaint(t *testing.T) {
	env := scope.AgentClassEnvelope{
		Tools: []scope.EnvelopeTool{{Name: "linear.list_team_issues"}},
	}
	toolReads := map[string]scope.ToolReads{
		"linear.list_team_issues": {ResourceType: "linear_issue", ParentType: "linear_team"},
	}
	d := scope.ScopeDelta{HardDeny: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1"}},
	}}
	caveats := scope.DetectCaveats(d, env, toolReads)
	require.Len(t, caveats, 1)
	assert.Equal(t, scope.CaveatListToolReturnsParentTaint, caveats[0].Category)
}

func TestDetectCaveats_NoCaveats_CleanDisallow(t *testing.T) {
	env := scope.AgentClassEnvelope{
		Tools: []scope.EnvelopeTool{{Name: "linear.get_issue"}},
	}
	toolReads := map[string]scope.ToolReads{
		"linear.get_issue": {ResourceType: "linear_issue", IDArg: "issueId"}, // ID-style only
	}
	d := scope.ScopeDelta{HardDeny: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1"}},
	}}
	caveats := scope.DetectCaveats(d, env, toolReads)
	assert.Empty(t, caveats, "ID-style tool produces no caveats")
}

func TestDetectCaveats_MultipleCaveatsForOneItem(t *testing.T) {
	// Both a search-style AND a parent-tainting list tool exist for the same type.
	env := scope.AgentClassEnvelope{
		Tools: []scope.EnvelopeTool{{Name: "linear.search_issues"}, {Name: "linear.list_team_issues"}},
	}
	toolReads := map[string]scope.ToolReads{
		"linear.search_issues":    {ResourceType: "linear_issue", QueryStyle: true},
		"linear.list_team_issues": {ResourceType: "linear_issue", ParentType: "linear_team"},
	}
	d := scope.ScopeDelta{HardDeny: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1"}},
	}}
	caveats := scope.DetectCaveats(d, env, toolReads)
	require.Len(t, caveats, 2)
}

func TestDetectCaveats_EmptyToolReads(t *testing.T) {
	env := scope.AgentClassEnvelope{Tools: []scope.EnvelopeTool{{Name: "any.tool"}}}
	d := scope.ScopeDelta{HardDeny: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "x", ID: "y"}},
	}}
	caveats := scope.DetectCaveats(d, env, nil)
	assert.Empty(t, caveats, "no toolReads means no per-tool caveats can be detected")
}
