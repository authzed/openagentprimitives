package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The projection into the shared activation table reads the EFFECTIVE interact
// policy, so the runner and `oap agent authz` (which renders the same table
// through the same accessor) describe the same class the same way.
//
// Nothing in the runner queries hookActive("interact") today, so this asserts
// only the projection, not a behaviour change: it is here to keep the two
// readers of one table from drifting the moment something does.
func TestActivationConfig_ReadsTheEffectiveInteractPermission(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		derived  string
		want     string
	}{
		{
			name:    "derived only: the value the class reconciler published is projected",
			derived: "slack_channel:C0DEMO123#member",
			want:    "slack_channel:C0DEMO123#member",
		},
		{
			name:     "declared beats derived: an authored policy is never overridden",
			declared: "group:eng#member",
			derived:  "slack_channel:C0DEMO123#member",
			want:     "group:eng#member",
		},
		{name: "neither: nothing projected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &spiceboxv1alpha1.AgentClass{}
			if tc.declared != "" {
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
					Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: tc.declared},
				}
			}
			ac.Status.DerivedSessionInteractPermission = tc.derived

			l := &Loop{AgentClass: ac}
			assert.Equal(t, tc.want, l.activationConfig().InteractPermission)
		})
	}
}
