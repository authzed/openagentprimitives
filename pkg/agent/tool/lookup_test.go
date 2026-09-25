package tool

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// namedTool is a minimal Tool whose only interesting property is its name and
// an identity tag, so a lookup result can be traced back to the exact element
// it came from.
type namedTool struct {
	name string
	tag  string
}

func (n namedTool) Name() string                                { return n.name }
func (namedTool) Kind() Kind                                    { return KindMeta }
func (namedTool) Description() string                           { return "named test tool" }
func (namedTool) InputSchema() json.RawMessage                  { return json.RawMessage(`{}`) }
func (namedTool) Permission() authz.Permission                  { return authz.Permission{StateImpact: authz.Stateless} }
func (namedTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (namedTool) Execute(context.Context, json.RawMessage, *SessionContext) (Result, error) {
	return Result{}, nil
}

// TestLookupByName covers the lookup's whole behavior surface, including the
// duplicate-name case that previously made it disagree with the runner's own
// lookup: LookupByName built a map (LAST wins) while Loop.lookupTool scans
// linearly (FIRST wins). Both are consulted about the SAME tool call — this
// one for the failing tool's Origin() in request_credential_update, the
// runner's for permission / toolguard / MCP-spec routing — so a name that
// resolved to two different tools could open a credential card naming an
// upstream that never failed.
func TestLookupByName(t *testing.T) {
	cases := []struct {
		name    string
		tools   []Tool
		lookup  string
		wantOK  bool
		wantTag string
	}{
		{
			name:   "empty list: nothing resolves",
			tools:  nil,
			lookup: "anything",
			wantOK: false,
		},
		{
			name:    "single tool: resolves to it",
			tools:   []Tool{namedTool{name: "gh_issue", tag: "only"}},
			lookup:  "gh_issue",
			wantOK:  true,
			wantTag: "only",
		},
		{
			name: "multiple distinct tools: resolves the right one",
			tools: []Tool{
				namedTool{name: "gh_issue", tag: "gh"},
				namedTool{name: "jira_issue", tag: "jira"},
			},
			lookup:  "jira_issue",
			wantOK:  true,
			wantTag: "jira",
		},
		{
			name: "name absent from a populated list: not found",
			tools: []Tool{
				namedTool{name: "gh_issue", tag: "gh"},
				namedTool{name: "jira_issue", tag: "jira"},
			},
			lookup: "gitlab_issue",
			wantOK: false,
		},
		{
			name: "duplicate names: FIRST wins, matching Loop.lookupTool's linear scan",
			tools: []Tool{
				namedTool{name: "gh_issue", tag: "first"},
				namedTool{name: "gh_issue", tag: "second"},
			},
			lookup:  "gh_issue",
			wantOK:  true,
			wantTag: "first",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := LookupByName(tc.tools)(tc.lookup)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				assert.Nil(t, got, "a miss must return a nil Tool, not a zero-valued one")
				return
			}
			nt, isNamed := got.(namedTool)
			assert.True(t, isNamed, "lookup returned an unexpected concrete type %T", got)
			assert.Equal(t, tc.wantTag, nt.tag)
		})
	}
}

// TestLookupByName_AgreesWithLinearScan pins the invariant directly rather
// than by inspection: for a list containing duplicates, LookupByName must
// return the same element a first-match linear scan does. Loop.lookupTool
// lives in pkg/agent/runner (which imports this package, so it cannot be
// referenced here); the scan below is its behavior, restated as the contract.
func TestLookupByName_AgreesWithLinearScan(t *testing.T) {
	tools := []Tool{
		namedTool{name: "a", tag: "a1"},
		namedTool{name: "b", tag: "b1"},
		namedTool{name: "a", tag: "a2"},
		namedTool{name: "b", tag: "b2"},
	}
	firstMatch := func(name string) (Tool, bool) {
		for _, t := range tools {
			if t.Name() == name {
				return t, true
			}
		}
		return nil, false
	}
	lookup := LookupByName(tools)
	for _, name := range []string{"a", "b", "missing"} {
		wantTool, wantOK := firstMatch(name)
		gotTool, gotOK := lookup(name)
		assert.Equal(t, wantOK, gotOK, "found-ness must match the linear scan for %q", name)
		assert.Equal(t, wantTool, gotTool, "LookupByName must resolve %q to the same tool the runner's linear scan does", name)
	}
}
