package installcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCheckQuery(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		perm string
		objT string
		objI string
		subT string
		subI string
		subR string
	}{
		{"agentsession:default/foo#interact@user:abc", true, "interact", "agentsession", "default/foo", "user", "abc", ""},
		{"agentsession:default/foo#interact@slack_user:U1#user", true, "interact", "agentsession", "default/foo", "slack_user", "U1", "user"},
		{"agentsession:default/foo#interact", false, "", "", "", "", "", ""},
		{"agentsessiondefault/foo#interact@user:abc", false, "", "", "", "", "", ""},
		{"agentsession:default/foo@user:abc", false, "", "", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			res, perm, sub, err := parseCheckQuery(tc.in)
			if !tc.ok {
				assert.Error(t, err, "expected parse error")
				return
			}
			require.NoError(t, err, "parseCheckQuery")
			assert.Equal(t, tc.perm, perm, "permission")
			assert.Equal(t, tc.objT, res.ObjectType, "resource type")
			assert.Equal(t, tc.objI, res.ObjectId, "resource id")
			assert.Equal(t, tc.subT, sub.Object.ObjectType, "subject type")
			assert.Equal(t, tc.subI, sub.Object.ObjectId, "subject id")
			assert.True(t, strings.EqualFold(sub.OptionalRelation, tc.subR), "optional subject relation (case-insensitive)")
		})
	}
}
