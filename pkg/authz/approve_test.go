package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type fakeApprover struct {
	approve map[string]bool
	owner   map[string]bool // canon|resType|resID
}

func (f *fakeApprover) CheckApprove(_ context.Context, _, _ string, canon identity.CanonicalUserID, _ bool) (bool, error) {
	return f.approve[canon.String()], nil
}
func (f *fakeApprover) CheckOwnerOnResource(_ context.Context, rt, rid string, canon identity.CanonicalUserID, _ bool) (bool, error) {
	return f.owner[canon.String()+"|"+rt+"|"+rid], nil
}

// CheckOnResource satisfies the approver-gate interface. These fakes model
// resource types whose approverPermission is the default (owner), so every
// permission delegates to the owner answer.
func (f *fakeApprover) CheckOnResource(ctx context.Context, resType, resID, _ string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return f.CheckOwnerOnResource(ctx, resType, resID, canonicalID, fullyConsistent)
}

// The approver-standing rule under test (see CheckApproverAuthorized):
// a resource-scoped approval is the RESOURCE OWNER's call — session
// standing is neither required nor sufficient, and owning ANY listed
// resource suffices (quorum = 1). Owner-only gates (no resources) still
// require agentsession#approve.
func TestCheckApproverAuthorized(t *testing.T) {
	ctx := context.Background()
	f := &fakeApprover{
		// alice: session approver + owner of repo:r1.
		// bob:   session approver only.
		// louis: owner of repo:r1 + repo:r2, NO session standing.
		approve: map[string]bool{"alice": true, "bob": true},
		owner: map[string]bool{
			"alice|repo|r1": true,
			"louis|repo|r1": true,
			"louis|repo|r2": true,
		},
	}
	r1 := []authz.ApproverResourceRef{{Type: "repo", ID: "r1"}}
	r12 := []authz.ApproverResourceRef{{Type: "repo", ID: "r1"}, {Type: "repo", ID: "r2"}}

	cases := []struct {
		name      string
		resources []authz.ApproverResourceRef
		canon     identity.CanonicalUserID
		want      bool
	}{
		{"owner-only gate: session approve alone suffices", nil, identity.CanonicalFromTrusted("bob", "test fixture"), true},
		{"owner-only gate: no session approve ⇒ denied", nil, identity.CanonicalFromTrusted("carol", "test fixture"), false},
		{"resource gate: resource owner WITHOUT session standing is authorized", r1, identity.CanonicalFromTrusted("louis", "test fixture"), true},
		{"resource gate: session approver who is not the resource owner is denied", r1, identity.CanonicalFromTrusted("bob", "test fixture"), false},
		{"resource gate: session approver who is also the owner is authorized", r1, identity.CanonicalFromTrusted("alice", "test fixture"), true},
		{"multi-resource gate: owning ANY listed resource authorizes (alice owns r1 only)", r12, identity.CanonicalFromTrusted("alice", "test fixture"), true},
		{"multi-resource gate: owner of none of the resources is denied", r12, identity.CanonicalFromTrusted("bob", "test fixture"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := authz.CheckApproverAuthorized(ctx, f, "ns", "s", tc.resources, tc.canon)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
}
