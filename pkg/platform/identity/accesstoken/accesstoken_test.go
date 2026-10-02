package accesstoken

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTokenValueShapeAndUniqueness(t *testing.T) {
	a, err := NewTokenValue()
	require.NoError(t, err)
	b, err := NewTokenValue()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(a, TokenPrefix))
	assert.Len(t, a, len(TokenPrefix)+43) // 32 bytes base64url, unpadded
	assert.NotEqual(t, a, b)
}

func TestHashTokenValueIsStableHexSHA256(t *testing.T) {
	h := HashTokenValue("oap_at_fixed")
	assert.Len(t, h, 64)
	assert.Equal(t, h, HashTokenValue("oap_at_fixed"))
	assert.NotEqual(t, h, HashTokenValue("oap_at_other"))
}

func TestRoleRelation(t *testing.T) {
	cases := []struct {
		role, want string
		wantErr    bool
	}{
		{role: RoleRead, want: RelationRoleRead},
		{role: RoleInteract, want: RelationRoleInteract},
		{role: RoleFull, want: RelationRoleFull},
		{role: "admin", wantErr: true},
		{role: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.role+": maps or errors", func(t *testing.T) {
			got, err := RoleRelation(tc.role)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNewTokenIDIsDNSSafe(t *testing.T) {
	id, err := NewTokenID()
	require.NoError(t, err)
	assert.Regexp(t, `^at-[0-9a-f]{12}$`, id)
}
