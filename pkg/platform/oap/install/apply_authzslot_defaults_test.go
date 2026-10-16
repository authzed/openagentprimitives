package install

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// agentClassWithSlots builds a bare AgentClass unstructured object whose
// spec.authz.slots is exactly the given slot maps — enough to exercise
// completeAuthzSlotDefaults without a real bundle or envtest.
func agentClassWithSlots(t *testing.T, slots []interface{}) *unstructured.Unstructured {
	t.Helper()
	ac := &unstructured.Unstructured{}
	ac.SetKind("AgentClass")
	ac.SetName("demo-class")
	require.NoError(t, unstructured.SetNestedSlice(ac.Object, slots, "spec", "authz", "slots"))
	return ac
}

func slotsOf(t *testing.T, ac *unstructured.Unstructured) []interface{} {
	t.Helper()
	slots, found, err := unstructured.NestedSlice(ac.Object, "spec", "authz", "slots")
	require.NoError(t, err)
	require.True(t, found)
	return slots
}

// TestCompleteAuthzSlotMembershipDefaults_FillsAbsentMembership pins the fix
// for the SSA-idempotency regression (TestInstall_OapSourceAnnotationIsSSAIdempotent,
// apply_integration_test.go): a bundle's slot that leaves membership unset
// (the normal case) must be completed with AuthzSlotMembershipDefault before
// the SSA-apply loop, because Slots is +listType=atomic and would otherwise
// have the applied list replace the live one with a slot lacking a field the
// apiserver has since defaulted.
func TestCompleteAuthzSlotMembershipDefaults_FillsAbsentMembership(t *testing.T) {
	ac := agentClassWithSlots(t, []interface{}{
		map[string]interface{}{"resourceType": "git_repo", "permission": "read"},
	})

	require.NoError(t, completeAuthzSlotDefaults(ac))

	slots := slotsOf(t, ac)
	require.Len(t, slots, 1)
	slot, ok := slots[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, v1alpha1.AuthzSlotMembershipDefault, slot["membership"],
		"a slot omitting membership must be completed with the default")
}

// TestCompleteAuthzSlotMembershipDefaults_LeavesEmptyStringFilled covers the
// same absent-value case represented as an explicit empty string (omitempty
// round-trips membership:"" the same as unset) rather than the key being
// missing entirely.
func TestCompleteAuthzSlotMembershipDefaults_LeavesEmptyStringFilled(t *testing.T) {
	ac := agentClassWithSlots(t, []interface{}{
		map[string]interface{}{"resourceType": "git_repo", "permission": "read", "membership": ""},
	})

	require.NoError(t, completeAuthzSlotDefaults(ac))

	slots := slotsOf(t, ac)
	slot, ok := slots[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, v1alpha1.AuthzSlotMembershipDefault, slot["membership"])
}

// TestCompleteAuthzSlotMembershipDefaults_LeavesExplicitDynamicAlone pins the
// other half of the contract: a slot that already spells membership out —
// including the non-default value — must be left exactly as the bundle
// author declared it. Completing a default must never overwrite a real
// choice.
func TestCompleteAuthzSlotMembershipDefaults_LeavesExplicitDynamicAlone(t *testing.T) {
	ac := agentClassWithSlots(t, []interface{}{
		map[string]interface{}{"resourceType": "git_repo", "permission": "read", "membership": "dynamic"},
	})

	require.NoError(t, completeAuthzSlotDefaults(ac))

	slots := slotsOf(t, ac)
	slot, ok := slots[0].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "dynamic", slot["membership"], "an explicit non-default membership must not be touched")
}

// TestCompleteAuthzSlotMembershipDefaults_NoSlotsIsUntouched covers a class
// with no authz.slots at all: the function must be a true no-op rather than
// fabricating an empty slots list (which would itself be an applied-field
// change from "absent" to "[]").
func TestCompleteAuthzSlotMembershipDefaults_NoSlotsIsUntouched(t *testing.T) {
	ac := &unstructured.Unstructured{}
	ac.SetKind("AgentClass")
	ac.SetName("demo-class")
	before := ac.DeepCopy()

	require.NoError(t, completeAuthzSlotDefaults(ac))

	assert.Equal(t, before.Object, ac.Object, "a class with no slots must be left byte-identical")
}

// TestCompleteAuthzSlotMembershipDefaults_MixedSlotsOnlyFillsAbsent exercises
// several slots at once: only the ones missing membership are completed, and
// the ones that already declare a value (default or otherwise) are untouched.
func TestCompleteAuthzSlotMembershipDefaults_MixedSlotsOnlyFillsAbsent(t *testing.T) {
	ac := agentClassWithSlots(t, []interface{}{
		map[string]interface{}{"resourceType": "git_repo", "permission": "read"},
		map[string]interface{}{"resourceType": "jira_issue", "permission": "write", "membership": "dynamic"},
		map[string]interface{}{"resourceType": "slack_channel", "permission": "read", "membership": "frozen"},
	})

	require.NoError(t, completeAuthzSlotDefaults(ac))

	slots := slotsOf(t, ac)
	require.Len(t, slots, 3)
	got := make([]string, len(slots))
	for i, s := range slots {
		slot := s.(map[string]interface{})
		got[i] = slot["membership"].(string)
	}
	assert.Equal(t, []string{"frozen", "dynamic", "frozen"}, got)
}

// TestCompleteAuthzSlotDefaults_FillsOccupancyAndRebind pins the new fields
// riding the same completion as membership: a slot that leaves occupancy
// and/or rebind unset must be completed with their +kubebuilder:default
// values, for the same SSA-idempotency reason membership is.
func TestCompleteAuthzSlotDefaults_FillsOccupancyAndRebind(t *testing.T) {
	ac := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"authz": map[string]any{"slots": []any{
			map[string]any{"resourceType": "git_repo", "permission": "push"},
		}}},
	}}
	require.NoError(t, completeAuthzSlotDefaults(ac))
	slots, _, _ := unstructured.NestedSlice(ac.Object, "spec", "authz", "slots")
	slot := slots[0].(map[string]any)
	assert.Equal(t, v1alpha1.AuthzSlotOccupancyDefault, slot["occupancy"])
	assert.Equal(t, v1alpha1.AuthzSlotRebindDefault, slot["rebind"])
	assert.Equal(t, v1alpha1.AuthzSlotMembershipDefault, slot["membership"])
}

// TestCompleteAuthzSlotMembershipDefaults_MalformedSlotIsAnError pins the
// error path: a slots entry that is not an object, or a membership that is
// not a string, is refused with a message naming the index — never guessed
// at, never skipped. A bundle that decodes to such a shape has already
// escaped the CRD's own validation, so the install must stop here loudly.
func TestCompleteAuthzSlotMembershipDefaults_MalformedSlotIsAnError(t *testing.T) {
	cases := []struct {
		name    string
		slots   []interface{}
		wantErr string
	}{
		{name: "a slot that is a string, not an object: refused by index", slots: []interface{}{"git_repo"}, wantErr: "spec.authz.slots[0] is string, not an object"},
		{name: "a slot whose membership is a number: refused by index", slots: []interface{}{map[string]interface{}{"resourceType": "git_repo", "membership": int64(1)}}, wantErr: "spec.authz.slots[0].membership"},
		{name: "a malformed second slot after a good first one: refused at index 1", slots: []interface{}{map[string]interface{}{"resourceType": "git_repo"}, true}, wantErr: "spec.authz.slots[1] is bool, not an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := agentClassWithSlots(t, tc.slots)
			err := completeAuthzSlotDefaults(ac)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
