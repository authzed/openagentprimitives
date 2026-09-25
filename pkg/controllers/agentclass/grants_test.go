// pkg/controllers/agentclass/grants_test.go
//
// Pure-function tests for grants helpers — no envtest required.
// Exercises the slice-2 task-13 contract that writeAgentSessionGrants
// is well-defined even when the resolved tool list is empty: extractGrantPairs
// returns no pairs, and we don't panic.
package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// checkPerm returns a readonly Permission with a Check binding to the
// given (resourceType, permission) pair.
func checkPerm(rt, perm string) *authz.Permission {
	return &authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: rt,
			Permission:   perm,
		},
	}
}

// TestExtractGrantPairs collapses the five original extractGrantPairs
// cases into one table. Each case constructs a list of MCPServers and
// asserts the dedup'd, sorted GrantPair slice the helper returns.
//
// This covers the slice-2 task-13 silent-crash scenario (empty/non-Check
// tool lists) plus the slice-4 invariant that PermissionVariants
// contribute pairs alongside the singular fallback Permission — the
// guardian schema composer needs the union, otherwise variant-routed
// approvals target nonexistent relations.
func TestExtractGrantPairs(t *testing.T) {
	cases := []struct {
		name    string
		servers []spiceboxv1alpha1.MCPServer
		want    []spiceboxv1alpha1.GrantPair
	}{
		{
			name:    "nil server list → zero pairs (no panic for empty tool list)",
			servers: nil,
			want:    nil,
		},
		{
			name: "tools with stateless/passthrough/nil permissions → zero pairs",
			servers: []spiceboxv1alpha1.MCPServer{{
				Spec: spiceboxv1alpha1.MCPServerSpec{
					Tools: []spiceboxv1alpha1.MCPServerTool{
						{Name: "ping", Permission: &authz.Permission{StateImpact: authz.Stateless}},
						{Name: "echo", Permission: &authz.Permission{StateImpact: authz.Passthrough}},
						{Name: "no_perm_at_all"}, // Permission==nil
					},
				},
			}},
			want: nil,
		},
		{
			name: "passthrough fallback + variant with Check → variant pair only",
			servers: []spiceboxv1alpha1.MCPServer{{
				Spec: spiceboxv1alpha1.MCPServerSpec{
					Tools: []spiceboxv1alpha1.MCPServerTool{
						{
							Name:       "search_crm_objects",
							Permission: &authz.Permission{StateImpact: authz.Passthrough},
							PermissionVariants: []authz.PermissionVariant{
								{
									When: `args.objectType == "contacts"`,
									Check: authz.Permission{
										StateImpact: authz.Readonly,
										Check: &authz.PermissionCheck{
											ResourceType: "crm_company",
											Permission:   "contact_access",
										},
									},
								},
								{
									When:  `args.objectType == "companies"`,
									Check: authz.Permission{StateImpact: authz.Passthrough},
								},
							},
						},
					},
				},
			}},
			want: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "crm_company", Permission: "contact_access"},
			},
		},
		{
			name: "fallback Check + variant Check → both pairs, sorted",
			servers: []spiceboxv1alpha1.MCPServer{{
				Spec: spiceboxv1alpha1.MCPServerSpec{
					Tools: []spiceboxv1alpha1.MCPServerTool{
						{
							Name:       "mixed_tool",
							Permission: checkPerm("github_repo", "read"),
							PermissionVariants: []authz.PermissionVariant{
								{
									When: `args.kind == "private"`,
									Check: authz.Permission{
										StateImpact: authz.Readonly,
										Check: &authz.PermissionCheck{
											ResourceType: "github_repo",
											Permission:   "admin",
										},
									},
								},
							},
						},
					},
				},
			}},
			want: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "admin"},
				{ResourceType: "github_repo", Permission: "read"},
			},
		},
		{
			name: "duplicates across tools/servers → dedup + sort by (resource, permission)",
			servers: []spiceboxv1alpha1.MCPServer{
				{
					Spec: spiceboxv1alpha1.MCPServerSpec{
						Tools: []spiceboxv1alpha1.MCPServerTool{
							{Name: "a", Permission: checkPerm("github_repo", "read")},
							{Name: "b", Permission: checkPerm("github_repo", "read")}, // dup
							{Name: "c", Permission: checkPerm("linear_issue", "read")},
						},
					},
				},
				{
					Spec: spiceboxv1alpha1.MCPServerSpec{
						Tools: []spiceboxv1alpha1.MCPServerTool{
							{Name: "d", Permission: checkPerm("github_repo", "admin")},
						},
					},
				},
			},
			want: []spiceboxv1alpha1.GrantPair{
				{ResourceType: "github_repo", Permission: "admin"},
				{ResourceType: "github_repo", Permission: "read"},
				{ResourceType: "linear_issue", Permission: "read"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractGrantPairs(tc.servers)
			if tc.want == nil {
				// Accept nil or empty slice — both are "no pairs."
				assert.Empty(t, got, "extractGrantPairs should return no pairs")
				return
			}
			assert.Equal(t, tc.want, got, "extractGrantPairs result")
		})
	}
}

// extractSlotPairs emits ONE pair per declared permission, because the schema
// composer turns each pair into a slot_grant_<permission> relation and a grant
// can only be written for a relation the schema carries.
//
// A slot used to be one (type, permission) pair, so approving anything else on
// that type bound a relation the schema never emitted, failed at SpiceDB with
// FailedPrecondition, and left the call escalating on every attempt.
func TestExtractSlotPairs_OnePairPerDeclaredPermission(t *testing.T) {
	cls := &spiceboxv1alpha1.AgentClass{}
	cls.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{{
			ResourceType: "git_repo",
			Description:  "a repository",
			Permission:   "push",
			Permissions:  []string{"read", "write"},
		}},
	}

	got := extractSlotPairs(cls)

	var perms []string
	for _, p := range got {
		assert.Equal(t, "git_repo", p.ResourceType, "every pair names the slot's type")
		perms = append(perms, p.Permission)
	}
	assert.ElementsMatch(t, []string{"push", "read", "write"}, perms,
		"each declared permission needs its own slot_grant relation in the composed schema")
}

// A slot naming no resource type emits nothing rather than a pair with a blank
// side, which the composer would turn into a malformed relation.
func TestExtractSlotPairs_SkipsATypelessSlot(t *testing.T) {
	cls := &spiceboxv1alpha1.AgentClass{}
	cls.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{{Permission: "push"}},
	}
	assert.Empty(t, extractSlotPairs(cls))
}
