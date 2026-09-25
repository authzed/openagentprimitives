package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func classWithSlots(bound []spiceboxv1alpha1.BoundEntityType, slots []spiceboxv1alpha1.AuthzSlot) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{}
	ac.Spec.BoundEntities = bound
	if slots != nil {
		ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: slots}
	}
	return ac
}

// boundEntities is an AUTHZ field. Renaming it and silently ignoring the old
// spelling would make an agent's instance constraints vanish on upgrade —
// nothing would fail, the agent would simply be less constrained than its
// author wrote. That is the one outcome a rename must not produce.
//
// So the old field still deserializes, and its presence is a hard validation
// error naming the new one. Loud beats silent when the thing being dropped is a
// constraint.
func TestValidateSlotsMigration(t *testing.T) {
	oneBound := []spiceboxv1alpha1.BoundEntityType{{
		ResourceType: "tracker_issue", Permission: "write",
	}}
	oneSlot := []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: "tracker_issue", Permission: "write",
	}}

	cases := []struct {
		name       string
		bound      []spiceboxv1alpha1.BoundEntityType
		slots      []spiceboxv1alpha1.AuthzSlot
		wantReason bool
	}{
		{"neither set: valid", nil, nil, false},
		{"slots only: valid", nil, oneSlot, false},
		{"boundEntities set: rejected with a migration message", oneBound, nil, true},
		{"both set: rejected — one concept, one spelling", oneBound, oneSlot, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validateSlotsMigration(classWithSlots(tc.bound, tc.slots))
			if !tc.wantReason {
				assert.Empty(t, reason, "unexpected rejection: %s", msg)
				return
			}
			assert.Equal(t, spiceboxv1alpha1.ReasonBoundEntitiesRenamed, reason)
			assert.Contains(t, msg, "authz.slots", "the message must name the new field")
			assert.Contains(t, msg, "boundEntities", "and the old one, so it is greppable")
		})
	}
}

// The error has to be actionable without reading a spec: an operator seeing it
// should know exactly what to edit.
func TestValidateSlotsMigration_messageShowsTheMove(t *testing.T) {
	_, msg := validateSlotsMigration(classWithSlots(
		[]spiceboxv1alpha1.BoundEntityType{{ResourceType: "tracker_issue", Permission: "write"}}, nil))

	assert.Contains(t, msg, "spec.boundEntities")
	assert.Contains(t, msg, "spec.authz.slots")
}
