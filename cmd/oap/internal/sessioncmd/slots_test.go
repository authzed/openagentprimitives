package sessioncmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeSlotRevokeClient is a minimal double for slotRevokeClient — no live
// SpiceDB connection, just canned standing answers, a canned held set, and a
// record of what got deleted so tests can assert a no-op issues no delete.
type fakeSlotRevokeClient struct {
	approveOK bool
	adminOK   bool
	held      []authz.SlotBinding
	deleted   []authz.Relation
}

func (f *fakeSlotRevokeClient) CheckApprove(_ context.Context, _, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	return f.approveOK, nil
}

func (f *fakeSlotRevokeClient) CheckPlatformPermission(_ context.Context, _ string, _ identity.CanonicalUserID, _ bool) (bool, error) {
	return f.adminOK, nil
}

func (f *fakeSlotRevokeClient) ListSlotGrants(_ context.Context, _, _ string) ([]authz.SlotBinding, error) {
	return f.held, nil
}

func (f *fakeSlotRevokeClient) Relations() authz.RelWriter { return f }

func (f *fakeSlotRevokeClient) WriteRelationships(_ context.Context, _ []authz.Relation) error {
	return nil
}

func (f *fakeSlotRevokeClient) DeleteRelationships(_ context.Context, rels []authz.Relation) error {
	f.deleted = append(f.deleted, rels...)
	return nil
}

func TestRevokeSlot(t *testing.T) {
	heldGrant := authz.SlotBinding{
		ResourceType: "widget", ResourceID: authz.TrustedObjectID("acme-widget-hash"), Permission: "read",
	}

	cases := []struct {
		name       string
		approveOK  bool
		adminOK    bool
		held       []authz.SlotBinding
		resourceID string
		wantErr    string // non-empty: RunE must fail with this substring
		wantOut    string // checked only when wantErr == ""
	}{
		{
			name:       "no standing to revoke: denied, issues no delete",
			approveOK:  false,
			adminOK:    false,
			held:       []authz.SlotBinding{heldGrant},
			resourceID: "acme-widget-hash",
			wantErr:    "may not revoke slots",
		},
		{
			name:       "standing present but operator pasted the raw value instead of the escaped id: errors, does not print revoked",
			approveOK:  true,
			held:       []authz.SlotBinding{heldGrant},
			resourceID: "https://github.com/acme/widget", // raw value, not the escaped id actually granted
			wantErr:    "no matching grant found",
		},
		{
			name:       "standing present and the grant matches: revokes and reports success",
			approveOK:  true,
			held:       []authz.SlotBinding{heldGrant},
			resourceID: "acme-widget-hash",
			wantOut:    "revoked widget:acme-widget-hash#read from agentsession:demo-ns/demo-session\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSlotRevokeClient{approveOK: tc.approveOK, adminOK: tc.adminOK, held: tc.held}
			var out strings.Builder

			err := revokeSlot(context.Background(), f, "demo-ns", "demo-session",
				identity.CanonicalFromTrusted("user:alice", "test fixture"), "widget", tc.resourceID, "read", &out)

			if tc.wantErr != "" {
				require.Error(t, err, "expected the revoke to fail rather than silently succeed")
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Empty(t, out.String(), "a failed revoke must not print a success line")
				assert.Empty(t, f.deleted, "a failed revoke must not issue a delete")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantOut, out.String())
			assert.Len(t, f.deleted, 1, "a successful revoke issues exactly one delete")
		})
	}
}
