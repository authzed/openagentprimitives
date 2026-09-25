package authz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

type fakeApproverChecker struct {
	approveCalled bool
	ownerResult   map[string]bool
}

func (f *fakeApproverChecker) CheckApprove(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	f.approveCalled = true
	return true, nil // would WRONGLY authorize a session owner if consulted
}
func (f *fakeApproverChecker) CheckOwnerOnResource(_ context.Context, resType, resID string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	return f.ownerResult[resType+"/"+resID], nil
}

// CheckOnResource satisfies the approver-gate interface. These fakes model
// resource types whose approverPermission is the default (owner), so every
// permission delegates to the owner answer.
func (f *fakeApproverChecker) CheckOnResource(ctx context.Context, resType, resID, _ string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error) {
	return f.CheckOwnerOnResource(ctx, resType, resID, canonicalID, fullyConsistent)
}

// TestCheckApproverAuthorized_NoSelfApproval_Characterization pins that with a
// source resource the gate consults ONLY resource ownership — never the session
// approve-set — so a session owner who is not the resource owner cannot
// self-approve. Guards the 2026-07-02 regression across the C2 migration.
func TestCheckApproverAuthorized_NoSelfApproval_Characterization(t *testing.T) {
	resources := []ApproverResourceRef{{Type: "data", ID: "d1"}}
	canon := identity.CanonicalFromTrusted("owner@corp.example", "test fixture")
	cases := []struct {
		name        string
		resources   []ApproverResourceRef
		ownerOK     bool
		wantOK      bool
		wantApprove bool // was CheckApprove (session set) consulted?
	}{
		{"resource owner authorized, session set untouched", resources, true, true, false},
		{"non-owner denied, no self-approval, session set untouched", resources, false, false, false},
		{"no resources: session approve-set decides", nil, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeApproverChecker{ownerResult: map[string]bool{"data/d1": tc.ownerOK}}
			ok, err := CheckApproverAuthorized(context.Background(), f, "default", "s", tc.resources, canon)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantApprove, f.approveCalled)
		})
	}
}
