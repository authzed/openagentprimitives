// pkg/controllers/agentclass/preferences_validate_test.go
//
// Untagged (unit tier): drives the real Reconcile against a fake client to pin
// the CALL SITE of preferences.ValidateSchema, not just the function — a
// class whose spec.userPreferences carries a default outside its own enum
// must be refused at reconcile, not only by the standalone validator in
// pkg/platform/preferences.
package agentclass_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestReconcile_InvalidUserPreferences_SetsValidFalse: an enum-typed
// preference whose default lies outside its own declared enum is
// self-inconsistent and must mark the class not-ready with a message that
// names the offending preference.
func TestReconcile_InvalidUserPreferences_SetsValidFalse(t *testing.T) {
	ac := newClass("prefs-invalid")
	ac.Spec.UserPreferences = []spiceboxv1alpha1.UserPreferenceSchema{
		{
			Name:    "language",
			Type:    "enum",
			Enum:    []spiceboxv1alpha1.PreferenceEnumValue{{Value: "en"}},
			Default: &apiextensionsv1.JSON{Raw: []byte(`"xx"`)}, // outside the enum
		},
	}

	got := reconcileClass(t, ac)

	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "the class must carry a Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a userPreferences default outside its own enum must refuse the class; message=%s", cond.Message)
	assert.Equal(t, spiceboxv1alpha1.ReasonUserPreferencesInvalid, cond.Reason)
	assert.Contains(t, cond.Message, `default for "language"`,
		"the message must name the offending preference")
}

// TestReconcile_ValidUserPreferences_Admits: the same shape of schema, but
// with a default inside its own enum, must not be refused for
// UserPreferencesInvalid — proving the gate above is discriminating rather
// than refusing any class that merely declares userPreferences.
func TestReconcile_ValidUserPreferences_Admits(t *testing.T) {
	ac := newClass("prefs-valid")
	ac.Spec.UserPreferences = []spiceboxv1alpha1.UserPreferenceSchema{
		{
			Name:    "language",
			Type:    "enum",
			Enum:    []spiceboxv1alpha1.PreferenceEnumValue{{Value: "en"}, {Value: "fr"}},
			Default: &apiextensionsv1.JSON{Raw: []byte(`"en"`)},
		},
	}

	got := reconcileClass(t, ac)

	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "the class must carry a Valid condition")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a self-consistent userPreferences list must not fail the class; message=%s", cond.Message)
}
