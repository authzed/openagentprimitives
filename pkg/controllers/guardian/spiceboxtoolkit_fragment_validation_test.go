// pkg/controllers/guardian/spiceboxtoolkit_fragment_validation_test.go
//
// Untagged unit test for patchSpiceboxToolkitSchemaValidity, reached through
// the PatchSpiceboxToolkitSchemaValidityForTest bridge in export_test.go
// since the method itself is unexported. Mirrors the shape of
// bootstrap_validation_test.go (package guardian_test, fake client built the
// way spicedbbootstrap_controller_test.go does) rather than any MCPServer
// equivalent — no such file exists; that path is covered only through the
// integration-tagged controller tests.
package guardian_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/controllers/guardian"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// toolkitWithFragment builds a cluster-scoped SpiceboxToolkit fixture
// carrying a RawZed spicedbSchema fragment. SpiceboxToolkit is
// scope=Cluster (see the +kubebuilder:resource marker on the type), so it
// carries no namespace.
func toolkitWithFragment(name, rawZed string) *spiceboxv1alpha1.SpiceboxToolkit {
	return &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:          name,
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{RawZed: rawZed},
		},
	}
}

func TestPatchSpiceboxToolkitSchemaValidity_MarksBadAndClearsFixed(t *testing.T) {
	bad := toolkitWithFragment("bad", "definition broken {")
	fixed := toolkitWithFragment("fixed", "definition ok {}")
	conditions.Set(fixed, &fixed.Status.Conditions, metav1.Condition{
		Type:    spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid,
		Status:  metav1.ConditionFalse,
		Reason:  spiceboxv1alpha1.SpiceboxToolkitReasonFragmentInvalid,
		Message: "was broken",
	})
	untouched := toolkitWithFragment("untouched", "definition fine {}")

	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(bad, fixed, untouched).
		WithStatusSubresource(bad, fixed, untouched).
		Build()
	r := guardian.NewReconciler(c, nil, nil)

	all := []spiceboxv1alpha1.SpiceboxToolkit{*bad, *fixed, *untouched}
	guardian.PatchSpiceboxToolkitSchemaValidityForTest(r, context.Background(), all, []guardian.BadSpiceboxToolkitForTest{
		{Toolkit: bad, Reason: spiceboxv1alpha1.SpiceboxToolkitReasonFragmentInvalid, Err: errors.New("syntax error")},
	})

	var got spiceboxv1alpha1.SpiceboxToolkit

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(bad), &got))
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid)
	require.NotNil(t, cond, "a rejected fragment must be visible on its own CR, not only in operator logs")
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, spiceboxv1alpha1.SpiceboxToolkitReasonFragmentInvalid, cond.Reason)
	assert.Contains(t, cond.Message, "syntax error")

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(fixed), &got))
	cond = findCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "a fixed fragment clears without waiting on a schema write")

	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(untouched), &got))
	assert.Nil(t, findCondition(got.Status.Conditions, spiceboxv1alpha1.SpiceboxToolkitConditionSpiceDBSchemaValid),
		"an always-valid fragment gets no condition added just to say fine")
}
