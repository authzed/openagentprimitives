package subagentrequest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestCheckMonotonicIdentity(t *testing.T) {
	const alice, bob = "user:alice", "user:bob"

	cases := []struct {
		name                        string
		parentMode, childMode       string
		parentStarter, childStarter string
		wantErr                     string
	}{
		{
			name:       "agent parent, agent child: allowed",
			parentMode: v1.IdentityModeAgent, childMode: v1.IdentityModeAgent,
		},
		{
			name:       "passthrough parent, agent child: narrowing is allowed",
			parentMode: v1.IdentityModeUserPassthrough, childMode: v1.IdentityModeAgent,
			parentStarter: alice,
		},
		{
			name:       "passthrough parent, passthrough child, same human: allowed",
			parentMode: v1.IdentityModeUserPassthrough, childMode: v1.IdentityModeUserPassthrough,
			parentStarter: alice, childStarter: alice,
		},
		{
			name:       "agent parent, passthrough child: WIDENING, refused",
			parentMode: v1.IdentityModeAgent, childMode: v1.IdentityModeUserPassthrough,
			childStarter: alice,
			wantErr:      "cannot widen",
		},
		{
			name:       "passthrough parent, passthrough child, DIFFERENT human: refused",
			parentMode: v1.IdentityModeUserPassthrough, childMode: v1.IdentityModeUserPassthrough,
			parentStarter: alice, childStarter: bob,
			wantErr: "different starter",
		},
		{
			name:       "passthrough child with no starter: refused",
			parentMode: v1.IdentityModeUserPassthrough, childMode: v1.IdentityModeUserPassthrough,
			parentStarter: alice, childStarter: "",
			wantErr: "no starter",
		},
		{
			name:       "unrecognized child mode: refused",
			parentMode: v1.IdentityModeAgent, childMode: "dynamic",
			wantErr: "unrecognized child identity mode",
		},
		{
			name:       "unrecognized parent mode: refused",
			parentMode: "dynamic", childMode: v1.IdentityModeAgent,
			wantErr: "unrecognized parent identity mode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMonotonicIdentity(tc.parentMode, tc.childMode, tc.parentStarter, tc.childStarter)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
