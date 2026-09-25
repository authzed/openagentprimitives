package scope_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

// TestScopeDelta_Shape covers the post-LLM shape derivation the metaagent
// Extract stage uses to label a parsed delta: Add present ⇒ widen; only
// Remove/HardDeny ⇒ narrow; both ⇒ mixed; empty ⇒ "".
func TestScopeDelta_Shape(t *testing.T) {
	res := []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}
	cases := []struct {
		name  string
		delta scope.ScopeDelta
		want  string
	}{
		{
			name:  "empty delta → \"\"",
			delta: scope.ScopeDelta{},
			want:  "",
		},
		{
			name:  "Add present → widen",
			delta: scope.ScopeDelta{Add: scope.ScopePartial{Resources: res}},
			want:  "widen",
		},
		{
			name:  "Add tools only → widen",
			delta: scope.ScopeDelta{Add: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
			want:  "widen",
		},
		{
			name:  "Remove only → narrow",
			delta: scope.ScopeDelta{Remove: scope.ScopePartial{Resources: res}},
			want:  "narrow",
		},
		{
			name:  "HardDeny only → narrow",
			delta: scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"linear.search_issues"}}},
			want:  "narrow",
		},
		{
			name: "Remove + HardDeny (no Add) → narrow",
			delta: scope.ScopeDelta{
				Remove:   scope.ScopePartial{Resources: res},
				HardDeny: scope.ScopePartial{Tools: []string{"x"}},
			},
			want: "narrow",
		},
		{
			name: "Add + HardDeny → mixed",
			delta: scope.ScopeDelta{
				Add:      scope.ScopePartial{Resources: res},
				HardDeny: scope.ScopePartial{Tools: []string{"x"}},
			},
			want: "mixed",
		},
		{
			name: "Add + Remove → mixed",
			delta: scope.ScopeDelta{
				Add:    scope.ScopePartial{Resources: res},
				Remove: scope.ScopePartial{Resources: res},
			},
			want: "mixed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.delta.Shape())
		})
	}
}
