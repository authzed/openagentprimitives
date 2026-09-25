package scope_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
)

func TestApplyDelta_AddResources(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"foo/bar"},
			Source:       scope.SourceDefault,
		}},
		ScopeVersion: 1,
	}
	d := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "baz/qux"}},
		},
	}
	next := scope.ApplyDelta(s, d, scope.SourceMetaagentApproved, time.Time{})
	require.Len(t, next.Resources, 1)
	assert.ElementsMatch(t, []string{"foo/bar", "baz/qux"}, next.Resources[0].IDs)
	assert.Equal(t, int64(2), next.ScopeVersion)
}

func TestApplyDelta_RemoveResources(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"foo/bar", "baz/qux"},
		}},
		ScopeVersion: 1,
	}
	d := scope.ScopeDelta{
		Remove: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
		},
	}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.Resources, 1)
	assert.Equal(t, []string{"baz/qux"}, next.Resources[0].IDs)
}

func TestApplyDelta_HardDenyResources_PopulateDisallow(t *testing.T) {
	s := scope.Scope{ScopeVersion: 1}
	d := scope.ScopeDelta{
		HardDeny: scope.ScopePartial{
			Resources: []scope.ResourceRef{
				{ResourceType: "linear_issue", ID: "ENG-*"},   // glob
				{ResourceType: "linear_issue", ID: "SEC-123"}, // concrete
				{ResourceType: "linear_team", ID: "ENG"},      // different type
			},
		},
	}
	next := scope.ApplyDelta(s, d, scope.SourceMetaagentApproved, time.Time{})

	// Disallow carries both ids under linear_issue + the team entry.
	require.Len(t, next.Disallow, 2, "one Disallow entry per resource type")
	var issue, team *scope.ScopeResource
	for i := range next.Disallow {
		switch next.Disallow[i].ResourceType {
		case "linear_issue":
			issue = &next.Disallow[i]
		case "linear_team":
			team = &next.Disallow[i]
		}
	}
	require.NotNil(t, issue)
	require.NotNil(t, team)
	assert.ElementsMatch(t, []string{"ENG-*", "SEC-123"}, issue.IDs)
	assert.Equal(t, []string{"ENG"}, team.IDs)

	// And the enforcement query: an ENG issue is disallowed, a non-ENG one is not.
	assert.True(t, next.ResourceDisallowed("linear_issue", "ENG-369"), "ENG-* glob blocks ENG-369")
	assert.True(t, next.ResourceDisallowed("linear_issue", "SEC-123"), "concrete id blocks SEC-123")
	assert.False(t, next.ResourceDisallowed("linear_issue", "L-140"), "L-140 is not under ENG → allowed")
	assert.False(t, next.ResourceDisallowed("linear_team", "PLATFORM"), "unmatched team id → allowed")
}

func TestResourceDisallowed_EmptyAndUnknown(t *testing.T) {
	var empty scope.Scope
	assert.False(t, empty.ResourceDisallowed("linear_issue", "ENG-1"), "empty scope disallows nothing")
	s := scope.Scope{Disallow: []scope.ScopeResource{{ResourceType: "linear_issue", IDs: []string{"ENG-*"}}}}
	assert.False(t, s.ResourceDisallowed("linear_issue", ""), "empty id never matches")
	assert.False(t, s.ResourceDisallowed("other_type", "ENG-1"), "type mismatch → not disallowed")
}

func TestIsGlobID(t *testing.T) {
	assert.True(t, scope.IsGlobID("ENG-*"))
	assert.True(t, scope.IsGlobID("ENG-?"))
	assert.True(t, scope.IsGlobID("[a-z]"))
	assert.False(t, scope.IsGlobID("ENG-369"))
	assert.False(t, scope.IsGlobID("L-140"))
}

func TestApplyDelta_AddTools(t *testing.T) {
	s := scope.Scope{ScopeVersion: 1}
	d := scope.ScopeDelta{
		Add: scope.ScopePartial{Tools: []string{"github.list_*"}},
	}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	assert.Equal(t, []string{"github.list_*"}, next.Tools.Allow)
}

func TestApplyDelta_IsIdempotent(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{
			ResourceType: "github_repo",
			IDs:          []string{"foo/bar"},
		}},
		ScopeVersion: 1,
	}
	d := scope.ScopeDelta{
		Add: scope.ScopePartial{
			Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
		},
	}
	once := scope.ApplyDelta(s, d, "", time.Time{})
	twice := scope.ApplyDelta(once, d, "", time.Time{})
	require.Len(t, twice.Resources, 1)
	assert.Equal(t, []string{"foo/bar"}, twice.Resources[0].IDs)
}

func TestApplyDelta_EmptyDelta_BumpsVersionOnly(t *testing.T) {
	s := scope.Scope{ScopeVersion: 3}
	next := scope.ApplyDelta(s, scope.ScopeDelta{}, "", time.Time{})
	assert.Equal(t, int64(4), next.ScopeVersion)
}

func TestApplyDelta_RemoveNonexistentID_NoOp(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{ResourceType: "github_repo", IDs: []string{"foo/bar"}}},
	}
	d := scope.ScopeDelta{Remove: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "not-present"}},
	}}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.Resources, 1)
	assert.Equal(t, []string{"foo/bar"}, next.Resources[0].IDs)
}

func TestApplyDelta_RemoveAllIDs_LeavesEmptyEntry(t *testing.T) {
	s := scope.Scope{
		Resources: []scope.ScopeResource{{ResourceType: "github_repo", IDs: []string{"foo/bar"}}},
	}
	d := scope.ScopeDelta{Remove: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "foo/bar"}},
	}}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.Resources, 1, "type entry remains even when IDs is empty")
	assert.Empty(t, next.Resources[0].IDs)
}

func TestApplyDelta_AddToNewType_CreatesEntry(t *testing.T) {
	s := scope.Scope{}
	d := scope.ScopeDelta{Add: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "L-1"}},
	}}
	next := scope.ApplyDelta(s, d, scope.SourceMetaagentApproved, time.Time{})
	require.Len(t, next.Resources, 1)
	assert.Equal(t, "linear_issue", next.Resources[0].ResourceType)
	assert.Equal(t, []string{"L-1"}, next.Resources[0].IDs)
	assert.Equal(t, scope.SourceMetaagentApproved, next.Resources[0].Source)
}

func TestApplyDelta_HardDenyTools_AddsToToolsDeny(t *testing.T) {
	s := scope.Scope{}
	d := scope.ScopeDelta{HardDeny: scope.ScopePartial{Tools: []string{"github.delete_repo"}}}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	assert.Equal(t, []string{"github.delete_repo"}, next.Tools.Deny)
}

func TestApplyDelta_HardDenyResources_DoesNotModifySessionScopeResources(t *testing.T) {
	// HardDeny.Resources is for SpiceDB tuples; Layer 2 doc Resources stays
	// as the positive list. The next-Scope should NOT have HardDeny resources
	// appearing as in-scope.
	s := scope.Scope{
		Resources: []scope.ScopeResource{{ResourceType: "github_repo", IDs: []string{"foo/bar"}}},
	}
	d := scope.ScopeDelta{HardDeny: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "github_repo", ID: "bad/repo"}},
	}}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.Resources, 1)
	assert.Equal(t, []string{"foo/bar"}, next.Resources[0].IDs, "bad/repo must NOT appear in scope.Resources")
}

func TestApplyDelta_AddArgConstraints_Appends(t *testing.T) {
	s := scope.Scope{
		ArgConstraints: []scope.ArgConstraint{{Tool: "a"}},
	}
	d := scope.ScopeDelta{Add: scope.ScopePartial{
		ArgConstraints: []scope.ArgConstraint{{Tool: "b"}},
	}}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.ArgConstraints, 2)
	assert.Equal(t, "a", next.ArgConstraints[0].Tool)
	assert.Equal(t, "b", next.ArgConstraints[1].Tool)
}

func TestApplyDelta_RemoveTool_FromAllow(t *testing.T) {
	s := scope.Scope{Tools: scope.ScopeTools{Allow: []string{"a", "b", "c"}}}
	d := scope.ScopeDelta{Remove: scope.ScopePartial{Tools: []string{"b"}}}
	next := scope.ApplyDelta(s, d, "", time.Time{})
	assert.Equal(t, []string{"a", "c"}, next.Tools.Allow)
}

func TestApplyDelta_AddAndHardDenySameItem_NoCrash(t *testing.T) {
	// Conflict: same resource appears in both Add and HardDeny. ApplyDelta
	// doesn't resolve the conflict (SpiceDB layer enforces deny; doc shows
	// the add). Just verify it produces a valid Scope and doesn't crash.
	d := scope.ScopeDelta{
		Add:      scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "t", ID: "x"}}},
		HardDeny: scope.ScopePartial{Resources: []scope.ResourceRef{{ResourceType: "t", ID: "x"}}},
	}
	require.NotPanics(t, func() {
		_ = scope.ApplyDelta(scope.Scope{}, d, scope.SourceMetaagentApproved, time.Time{})
	})
}

// TestApplyDelta_AddNeverDowngradesAnExistingSource: adding an ID to a type
// already in scope must not re-tag where that type came from.
//
// The case that matters is the last row. PromoteObservedSlots re-runs after
// every dispatch round once a fact exists, so with last-writer-wins a
// git_commit entry a human cleared as `approved` was re-tagged `observed` on
// the very next round — the audit trail then claiming an observation put in
// scope what a person waived a gate for.
func TestApplyDelta_AddNeverDowngradesAnExistingSource(t *testing.T) {
	cases := []struct {
		name     string
		existing scope.Source
		adding   scope.Source
		want     scope.Source
	}{
		{
			name:     "empty existing source: filled, there is no provenance to protect",
			existing: "", adding: scope.SourceObserved, want: scope.SourceObserved,
		},
		{
			name:     "adding with no source: existing preserved",
			existing: scope.SourceDefault, adding: "", want: scope.SourceDefault,
		},
		{
			name:     "a repeat from the same source: unchanged",
			existing: scope.SourceObserved, adding: scope.SourceObserved, want: scope.SourceObserved,
		},
		{
			name:     "a human decision is never re-tagged by an automated source",
			existing: scope.SourceApproved, adding: scope.SourceObserved, want: scope.SourceApproved,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := scope.Scope{Resources: []scope.ScopeResource{{
				ResourceType: "git_commit",
				IDs:          []string{"ba03f5969a"},
				Source:       tc.existing,
			}}}
			d := scope.ScopeDelta{Add: scope.ScopePartial{
				Resources: []scope.ResourceRef{{ResourceType: "git_commit", ID: "0f1e2d3c4b"}},
			}}
			next := scope.ApplyDelta(s, d, tc.adding, time.Time{})

			require.Len(t, next.Resources, 1)
			assert.Equal(t, tc.want, next.Resources[0].Source)
			assert.ElementsMatch(t, []string{"ba03f5969a", "0f1e2d3c4b"}, next.Resources[0].IDs,
				"the ID still lands whatever happens to the Source tag")
		})
	}
}

// TestApplyDelta_RemoveAllIDsClearsTheStaleSource: a fully-revoked type keeps
// its entry but must not keep its tag.
//
// This only bites because applyAdd is set-once. Left behind, the tag would be
// PERMANENT — the next source to re-populate the type could never label its own
// work, and would silently inherit whatever the previous occupant was tagged.
func TestApplyDelta_RemoveAllIDsClearsTheStaleSource(t *testing.T) {
	s := scope.Scope{Resources: []scope.ScopeResource{{
		ResourceType: "git_commit",
		IDs:          []string{"ba03f5969a"},
		Source:       scope.SourceApproved,
	}}}
	d := scope.ScopeDelta{Remove: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "git_commit", ID: "ba03f5969a"}},
	}}

	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.Resources, 1, "the entry stays: absence would WIDEN, not revoke")
	assert.Empty(t, next.Resources[0].IDs)
	assert.Empty(t, string(next.Resources[0].Source), "a type naming nothing carries no provenance")

	// And the next source to fill it can now tag its own work, which is the
	// whole reason the clear is needed under set-once.
	refilled := scope.ApplyDelta(next, scope.ScopeDelta{Add: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "git_commit", ID: "0f1e2d3c4b"}},
	}}, scope.SourceObserved, time.Time{})
	assert.Equal(t, scope.SourceObserved, refilled.Resources[0].Source,
		"the re-populating source must not inherit the revoked entry's tag")
}

// A partial removal leaves the type still naming something, so its provenance
// is still a true claim and must survive.
func TestApplyDelta_PartialRemoveKeepsTheSource(t *testing.T) {
	s := scope.Scope{Resources: []scope.ScopeResource{{
		ResourceType: "git_commit",
		IDs:          []string{"ba03f5969a", "0f1e2d3c4b"},
		Source:       scope.SourceApproved,
	}}}
	d := scope.ScopeDelta{Remove: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "git_commit", ID: "ba03f5969a"}},
	}}

	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.Resources, 1)
	assert.Equal(t, []string{"0f1e2d3c4b"}, next.Resources[0].IDs)
	assert.Equal(t, scope.SourceApproved, next.Resources[0].Source,
		"the type still names an instance, so who put it in scope is still true")
}

// An entry emptied of IDs but still carrying a live pattern still reaches
// resources, so it keeps its tag.
func TestApplyDelta_RemoveAllIDsKeepsSourceWhenAPatternRemains(t *testing.T) {
	s := scope.Scope{Resources: []scope.ScopeResource{{
		ResourceType: "linear_issue",
		IDs:          []string{"ENG-1"},
		Patterns:     []scope.ResourcePattern{{Attrs: map[string]string{"team": "eng"}}},
		Source:       scope.SourceApproved,
	}}}
	d := scope.ScopeDelta{Remove: scope.ScopePartial{
		Resources: []scope.ResourceRef{{ResourceType: "linear_issue", ID: "ENG-1"}},
	}}

	next := scope.ApplyDelta(s, d, "", time.Time{})
	require.Len(t, next.Resources, 1)
	assert.Empty(t, next.Resources[0].IDs)
	assert.Equal(t, scope.SourceApproved, next.Resources[0].Source,
		"a live pattern still reaches resources, so the provenance still describes something")
}
