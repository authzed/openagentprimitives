package scope_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func TestClassifySkipped_OutOfEnvelopeResourceType(t *testing.T) {
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{
			{ResourceType: "github_repo", Permission: "read"},
		},
	}
	proposed := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{
				{ResourceType: "github_repo", ID: "foo/bar"},
				{ResourceType: "linear_issue", ID: "L-1"},
			},
		},
	}
	requesterPerms := scope.RequesterPerms{
		AllowedIDsByType: map[string]map[string]bool{
			"github_repo": {"foo/bar": true},
		},
	}
	applied, skipped := scope.ClassifySkipped(proposed, env, requesterPerms)
	require.Len(t, applied.Add.Resources, 1)
	assert.Equal(t, "github_repo:foo/bar", applied.Add.Resources[0].String())
	require.Len(t, skipped, 1)
	assert.Equal(t, scope.ReasonOutOfEnvelopeRtype, skipped[0].Reason)
	assert.Contains(t, skipped[0].RequestFragment, "linear_issue:L-1")
}

func TestClassifySkipped_RequesterLacksPerm(t *testing.T) {
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{
			{ResourceType: "github_repo", Permission: "read"},
		},
	}
	proposed := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{
				{ResourceType: "github_repo", ID: "private/internal"},
			},
		},
	}
	requesterPerms := scope.RequesterPerms{
		AllowedIDsByType: map[string]map[string]bool{
			"github_repo": {"public/repo": true},
		},
	}
	applied, skipped := scope.ClassifySkipped(proposed, env, requesterPerms)
	assert.Empty(t, applied.Add.Resources)
	require.Len(t, skipped, 1)
	assert.Equal(t, scope.ReasonRequesterLacksPerm, skipped[0].Reason)
}

func TestClassifySkipped_ToolOutOfEnvelope(t *testing.T) {
	env := scope.AgentClassEnvelope{
		Tools: []scope.EnvelopeTool{
			{Name: "github.list_issues"},
			{Name: "github.create_pr"},
		},
	}
	proposed := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Tools: []string{"github.list_issues", "slack.send_message"},
		},
	}
	applied, skipped := scope.ClassifySkipped(proposed, env, scope.RequesterPerms{})
	assert.Equal(t, []string{"github.list_issues"}, applied.Add.Tools)
	require.Len(t, skipped, 1)
	assert.Equal(t, scope.ReasonOutOfEnvelopeTool, skipped[0].Reason)
}

func TestClassifySkipped_GlobToolMatch(t *testing.T) {
	env := scope.AgentClassEnvelope{
		Tools: []scope.EnvelopeTool{{Name: "github.list_issues"}, {Name: "github.create_pr"}},
	}
	proposed := scope.ScopeDelta{Add: scope.ScopePartial{Tools: []string{"github.*"}}}
	applied, skipped := scope.ClassifySkipped(proposed, env, scope.RequesterPerms{})
	assert.Equal(t, []string{"github.*"}, applied.Add.Tools)
	assert.Empty(t, skipped)
}

func TestClassifySkipped_HardDeny_NoRequesterPermRequired(t *testing.T) {
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo", Permission: "read"}},
	}
	// Requester has no perms; HardDeny should still apply.
	proposed := scope.ScopeDelta{HardDeny: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "any/repo"}},
	}}
	applied, skipped := scope.ClassifySkipped(proposed, env, scope.RequesterPerms{})
	require.Len(t, applied.HardDeny.Resources, 1)
	assert.Empty(t, skipped)
}

func TestClassifySkipped_RemoveDoesNotRequirePerm(t *testing.T) {
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo", Permission: "read"}},
	}
	// Requester has no perms; Remove should still apply (purely subtractive).
	proposed := scope.ScopeDelta{Remove: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "any/repo"}},
	}}
	applied, skipped := scope.ClassifySkipped(proposed, env, scope.RequesterPerms{})
	require.Len(t, applied.Remove.Resources, 1)
	assert.Empty(t, skipped)
}

func TestClassifySkipped_MultipleSkipped(t *testing.T) {
	env := scope.AgentClassEnvelope{
		Tools:         []scope.EnvelopeTool{{Name: "github.list_issues"}},
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}},
	}
	proposed := scope.ScopeDelta{Add: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1"}, {ResourceType: "jira_issue", ID: "J-1"}},
		Tools:     []string{"slack.send"},
	}}
	_, skipped := scope.ClassifySkipped(proposed, env, scope.RequesterPerms{})
	require.Len(t, skipped, 3, "two out-of-envelope resources + one out-of-envelope tool")
}

func TestClassifySkipped_EmptyInput_EmptyOutput(t *testing.T) {
	applied, skipped := scope.ClassifySkipped(scope.ScopeDelta{}, scope.AgentClassEnvelope{}, scope.RequesterPerms{})
	assert.True(t, applied.IsEmpty())
	assert.Empty(t, skipped)
}

func TestClassifySkipped_ResourcePatternsPassThrough(t *testing.T) {
	env := scope.AgentClassEnvelope{
		BoundEntities: []scope.EnvelopeBoundEntity{{ResourceType: "github_repo"}},
	}
	proposed := scope.ScopeDelta{Add: scope.ScopePartial{
		ResourcePatterns: []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo"}}},
	}}
	applied, skipped := scope.ClassifySkipped(proposed, env, scope.RequesterPerms{})
	require.Len(t, applied.Add.ResourcePatterns, 1)
	assert.Empty(t, skipped, "patterns are not validated in v1; passed through")
}

func TestClassifySkipped_ArgConstraintsPassThrough(t *testing.T) {
	proposed := scope.ScopeDelta{Add: scope.ScopePartial{
		ArgConstraints: []scope.ArgConstraint{{Tool: "x"}},
	}}
	applied, skipped := scope.ClassifySkipped(proposed, scope.AgentClassEnvelope{}, scope.RequesterPerms{})
	require.Len(t, applied.Add.ArgConstraints, 1)
	assert.Empty(t, skipped, "ArgConstraints not validated in v1; passed through")
}
