package channelevents

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		kind   Kind
		wantOK bool
		want   AuthzClass
	}{
		{
			name:   "permission request: decision_request to specific user, not sensitive",
			kind:   KindPermissionRequest,
			wantOK: true,
			want: AuthzClass{
				Category:        CategoryDecisionRequest,
				DefaultAudience: AudienceSpecificUser,
				Severity:        SeverityInfo,
				Sensitive:       false,
			},
		},
		{
			name:   "non-authz kind returns ok=false",
			kind:   KindUserMessage,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Classify(tc.kind)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}
