package scope_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func TestCheckScope_ToolAllowDeny(t *testing.T) {
	cases := []struct {
		name    string
		scope   scope.Scope
		tool    string
		wantOK  bool
		wantMsg string
	}{
		{
			name:   "empty scope: allow",
			scope:  scope.Scope{},
			tool:   "github.list_issues",
			wantOK: true,
		},
		{
			name: "allow glob matches",
			scope: scope.Scope{
				Tools: scope.ScopeTools{Allow: []string{"github.*"}},
			},
			tool:   "github.list_issues",
			wantOK: true,
		},
		{
			name: "allow glob does not match: deny",
			scope: scope.Scope{
				Tools: scope.ScopeTools{Allow: []string{"github.*"}},
			},
			tool:    "linear.get_issue",
			wantOK:  false,
			wantMsg: "tool linear.get_issue not in session allow-list",
		},
		{
			name: "deny glob matches: deny even if allow does too",
			scope: scope.Scope{
				Tools: scope.ScopeTools{
					Allow: []string{"github.*"},
					Deny:  []string{"github.delete_*"},
				},
			},
			tool:    "github.delete_repo",
			wantOK:  false,
			wantMsg: "tool github.delete_repo denied by session policy",
		},
		{
			name: "invalid glob pattern in Allow does not panic; treated as no-match",
			scope: scope.Scope{
				Tools: scope.ScopeTools{Allow: []string{"[bad-pattern"}},
			},
			tool:   "github.list_issues",
			wantOK: false, // pattern is invalid -> no match -> not in allow list
		},
		{
			name: "invalid glob pattern in Deny does not panic; treated as no-match",
			scope: scope.Scope{
				Tools: scope.ScopeTools{Deny: []string{"[bad-pattern"}},
			},
			tool:   "github.list_issues",
			wantOK: true, // pattern invalid -> no deny match -> allow (no Allow list constraint either)
		},
		{
			name: "empty tool name with allow list: denied",
			scope: scope.Scope{
				Tools: scope.ScopeTools{Allow: []string{"github.*"}},
			},
			tool:   "",
			wantOK: false,
		},
		{
			name: "both Allow and Deny match: Deny wins",
			scope: scope.Scope{
				Tools: scope.ScopeTools{
					Allow: []string{"github.delete_*"},
					Deny:  []string{"github.delete_*"},
				},
			},
			tool:    "github.delete_repo",
			wantOK:  false,
			wantMsg: "denied by session policy",
		},
		{
			name: "case-sensitivity: Github.* does NOT match github.list_issues",
			scope: scope.Scope{
				Tools: scope.ScopeTools{Allow: []string{"Github.*"}},
			},
			tool:   "github.list_issues",
			wantOK: false,
		},
		{
			name: "multiple Allow patterns: passes if any match",
			scope: scope.Scope{
				Tools: scope.ScopeTools{Allow: []string{"linear.*", "github.list_*"}},
			},
			tool:   "github.list_issues",
			wantOK: true,
		},
		{
			name: "multiple Deny patterns: denies if any match",
			scope: scope.Scope{
				Tools: scope.ScopeTools{
					Allow: []string{"github.*"},
					Deny:  []string{"github.delete_*", "github.force_*"},
				},
			},
			tool:    "github.force_push",
			wantOK:  false,
			wantMsg: "denied by session policy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := scope.CheckScope(tc.scope, tc.tool, json.RawMessage(`{}`))
			assert.Equal(t, tc.wantOK, r.OK, "msg=%s", r.Message)
			if !tc.wantOK && tc.wantMsg != "" {
				assert.Contains(t, r.Message, tc.wantMsg)
			}
		})
	}
}

func TestCheckScope_ArgConstraints_Equals(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{
			Tool:   "github.create_pr",
			Equals: map[string]string{"base": "main"},
		}},
	}
	cases := []struct {
		name   string
		tool   string
		args   string
		wantOK bool
	}{
		{"matching base passes", "github.create_pr", `{"base":"main"}`, true},
		{"non-matching base fails", "github.create_pr", `{"base":"develop"}`, false},
		{"missing arg fails", "github.create_pr", `{}`, false},
		{"different tool ignored", "github.list_issues", `{"base":"develop"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := scope.CheckScope(s, tc.tool, json.RawMessage(tc.args))
			assert.Equal(t, tc.wantOK, r.OK, "msg=%s", r.Message)
		})
	}
}

func TestCheckScope_ArgConstraints_Forbid(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{
			Tool:   "github.create_pr",
			Forbid: map[string]string{"force_push": "true"},
		}},
	}
	cases := []struct {
		name   string
		args   string
		wantOK bool
	}{
		{"force_push false passes", `{"force_push":"false"}`, true},
		{"force_push absent passes", `{}`, true},
		{"force_push true fails", `{"force_push":"true"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := scope.CheckScope(s, "github.create_pr", json.RawMessage(tc.args))
			assert.Equal(t, tc.wantOK, r.OK, "msg=%s", r.Message)
		})
	}
}

func TestCheckScope_ArgConstraints_MalformedJSON(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "t", Equals: map[string]string{"k": "v"}}},
	}
	// Malformed args treated as empty map → Equals constraint fails (k missing).
	r := scope.CheckScope(s, "t", json.RawMessage(`{not json`))
	assert.False(t, r.OK)
}

func TestCheckScope_ArgConstraints_NumericVsString(t *testing.T) {
	// Document behavior: fmt.Sprintf("%v") collapses numeric 1 and string "1"
	// to the same surface, so this matches. If this behavior ever changes,
	// the test fails loudly.
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "t", Equals: map[string]string{"count": "1"}}},
	}
	r := scope.CheckScope(s, "t", json.RawMessage(`{"count":1}`))
	assert.True(t, r.OK, "numeric 1 should equal string \"1\" via fmt %%v")
}

func TestCheckScope_ArgConstraints_MultipleConstraintsAllMustPass(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{
			{Tool: "t", Equals: map[string]string{"a": "1"}},
			{Tool: "t", Equals: map[string]string{"b": "2"}},
		},
	}
	r := scope.CheckScope(s, "t", json.RawMessage(`{"a":"1","b":"3"}`))
	assert.False(t, r.OK, "one constraint failing means dispatch denied")
}

func TestCheckScope_ArgConstraints_DenyWithAllowGlob(t *testing.T) {
	// Tool matches an Allow glob, but its arg constraint denies. Final outcome: denied.
	s := scope.Scope{
		Tools: scope.ScopeTools{Allow: []string{"github.*"}},
		ArgConstraints: []scope.ArgConstraint{{
			Tool:   "github.create_pr",
			Forbid: map[string]string{"force_push": "true"},
		}},
	}
	r := scope.CheckScope(s, "github.create_pr", json.RawMessage(`{"force_push":"true"}`))
	assert.False(t, r.OK)
}

func TestCheckScope_ArgConstraints_EmptyConstraintPassesThrough(t *testing.T) {
	// A constraint entry with neither Equals nor Forbid nor CEL → pass-through (no-op).
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "t"}},
	}
	r := scope.CheckScope(s, "t", json.RawMessage(`{}`))
	assert.True(t, r.OK)
}

func TestCheckScope_ArgConstraints_CEL(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{
			Tool: "github.create_pr",
			CEL:  "args.title.size() < 80",
		}},
	}
	cases := []struct {
		name   string
		args   string
		wantOK bool
	}{
		{"short title passes", `{"title":"fix typo"}`, true},
		{"long title fails", `{"title":"` + repeatRune('x', 100) + `"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := scope.CheckScope(s, "github.create_pr", json.RawMessage(tc.args))
			assert.Equal(t, tc.wantOK, r.OK, "msg=%s", r.Message)
		})
	}
}

func repeatRune(r rune, n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

func TestCheckScope_ArgConstraints_CEL_CompileError(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "t", CEL: "args.x ===="}},
	}
	r := scope.CheckScope(s, "t", json.RawMessage(`{"x":1}`))
	assert.False(t, r.OK)
	assert.Contains(t, r.Message, "CEL")
}

func TestCheckScope_ArgConstraints_CEL_NonBoolReturn(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "t", CEL: "args.title"}},
	}
	r := scope.CheckScope(s, "t", json.RawMessage(`{"title":"hello"}`))
	assert.False(t, r.OK)
	assert.Contains(t, r.Message, "must return bool")
}

func TestCheckScope_ArgConstraints_CEL_MissingField(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "t", CEL: "args.nonexistent.size() < 10"}},
	}
	r := scope.CheckScope(s, "t", json.RawMessage(`{}`))
	assert.False(t, r.OK)
	assert.Contains(t, r.Message, "CEL")
}

func TestCheckScope_ArgConstraints_CEL_EmptyString_PassesThrough(t *testing.T) {
	// Constraint with empty CEL — pass-through (no-op).
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "t", CEL: ""}},
	}
	r := scope.CheckScope(s, "t", json.RawMessage(`{}`))
	assert.True(t, r.OK)
}

func TestCheckScope_ArgConstraints_CEL_DifferentToolIgnored(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "tool_a", CEL: "args.x > 100"}},
	}
	r := scope.CheckScope(s, "tool_b", json.RawMessage(`{"x":1}`))
	assert.True(t, r.OK, "constraint scoped to tool_a; tool_b not affected")
}

func TestCheckScope_ResourceInScope(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"foo/bar", "baz/qux"},
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo"}}},
		}},
	}
	cases := []struct {
		name   string
		refs   []scope.ResourceRef
		attrs  map[string]map[string]string
		wantOK bool
	}{
		{
			name:   "explicit ID in IDs list passes",
			refs:   []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
			wantOK: true,
		},
		{
			name:   "non-listed ID without matching pattern fails",
			refs:   []scope.ResourceRef{{ResourceType: "github_repo", ID: "other/repo"}},
			wantOK: false,
		},
		{
			name:   "ID matched by pattern attrs passes",
			refs:   []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/anything"}},
			attrs:  map[string]map[string]string{"foo/anything": {"owner": "foo", "name": "anything"}},
			wantOK: true,
		},
		{
			name:   "resource type not declared in scope: allow (no narrowing for that type)",
			refs:   []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1234"}},
			wantOK: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attrsLookup := func(r scope.ResourceRef) map[string]string {
				return tc.attrs[r.ID]
			}
			r := scope.CheckScopeWithRefs(s, "any.tool", json.RawMessage(`{}`), tc.refs, attrsLookup)
			assert.Equal(t, tc.wantOK, r.OK, "msg=%s", r.Message)
		})
	}
}

func TestCheckScopeWithRefs_PatternMissingKey(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo", "name": "lib-*"}}},
		}},
	}
	// Lookup returns owner but not name → pattern's "name" key missing → no match.
	attrs := func(r scope.ResourceRef) map[string]string { return map[string]string{"owner": "foo"} }
	r := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "x"}}, attrs)
	assert.False(t, r.OK)
}

func TestCheckScopeWithRefs_EmptyAttrsPattern(t *testing.T) {
	// Pattern with empty Attrs map is NOT a wildcard — doesn't match anything.
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{}}},
		}},
	}
	attrs := func(r scope.ResourceRef) map[string]string { return map[string]string{"owner": "foo"} }
	r := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "x"}}, attrs)
	assert.False(t, r.OK, "empty Attrs map is not a wildcard")
}

func TestCheckScopeWithRefs_GlobAttrMatching(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"name": "lib-*"}}},
		}},
	}
	attrs := func(r scope.ResourceRef) map[string]string { return map[string]string{"name": "lib-foo"} }
	r := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "x"}}, attrs)
	assert.True(t, r.OK)
}

func TestCheckScopeWithRefs_NilAttrsLookup(t *testing.T) {
	// nil AttrsLookup: only explicit IDs can match; patterns are inert.
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"foo/bar"},
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo"}}},
		}},
	}
	r1 := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}, nil)
	assert.True(t, r1.OK, "explicit ID match still works without attrs")
	r2 := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/other"}}, nil)
	assert.False(t, r2.OK, "pattern unreachable without AttrsLookup")
}

func TestCheckScopeWithRefs_AttrsLookupReturnsNil(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo"}}},
		}},
	}
	attrs := func(r scope.ResourceRef) map[string]string { return nil }
	r := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "x"}}, attrs)
	assert.False(t, r.OK)
}

func TestCheckScopeWithRefs_MultiplePatternsOneMatches(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			Patterns: []scope.ResourcePattern{
				{Attrs: map[string]string{"owner": "bar"}},
				{Attrs: map[string]string{"owner": "foo"}},
			},
		}},
	}
	attrs := func(r scope.ResourceRef) map[string]string { return map[string]string{"owner": "foo"} }
	r := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "x"}}, attrs)
	assert.True(t, r.OK)
}

func TestCheckScopeWithRefs_ExplicitIDAlsoMatchesPattern(t *testing.T) {
	// Both paths satisfy → allow.
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"foo/bar"},
			Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"owner": "foo"}}},
		}},
	}
	attrs := func(r scope.ResourceRef) map[string]string { return map[string]string{"owner": "foo"} }
	r := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}, attrs)
	assert.True(t, r.OK)
}

func TestCheckScopeWithRefs_TypeInScopeButEmptyIDsAndPatterns(t *testing.T) {
	// Strictest narrowing: type declared with empty IDs and Patterns → every resource of that type denied.
	s := scope.Scope{
		Resources: []scope.ScopeResource{{ResourceType: "github_repo"}},
	}
	r := scope.CheckScopeWithRefs(s, "t", json.RawMessage(`{}`), []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}}, nil)
	assert.False(t, r.OK)
}
