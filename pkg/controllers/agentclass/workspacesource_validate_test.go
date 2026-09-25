// package agentclass (internal, not agentclass_test) so this file can call
// the unexported validateWorkspaceSource directly.
package agentclass

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// TestValidateWorkspaceSource covers the four outcomes of the AgentClass
// controller's workspaceSource validator: no binding, a missing
// WorkspaceSource, a WorkspaceSource whose Valid condition isn't True, and a
// bound + Valid WorkspaceSource.
func TestValidateWorkspaceSource(t *testing.T) {
	ctx := context.Background()
	scheme := testfixtures.NewScheme(t)

	validWS := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-source", Namespace: "default"},
		Status: spiceboxv1alpha1.WorkspaceSourceStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.WorkspaceSourceConditionValid,
				Status:             metav1.ConditionTrue,
				Reason:             "OK",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	invalidWS := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-source", Namespace: "default"},
		Status: spiceboxv1alpha1.WorkspaceSourceStatus{
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.WorkspaceSourceConditionValid,
				Status:             metav1.ConditionFalse,
				Reason:             "BadSpec",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	noConditionWS := &spiceboxv1alpha1.WorkspaceSource{
		ObjectMeta: metav1.ObjectMeta{Name: "no-condition-source", Namespace: "default"},
	}

	c := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(validWS, invalidWS, noConditionWS).
		WithStatusSubresource(&spiceboxv1alpha1.WorkspaceSource{}).
		Build()

	cases := []struct {
		name       string
		ref        *spiceboxv1alpha1.AgentClassWorkspaceSourceRef
		wantReason string
	}{
		{name: "nil ref: no binding, no validation", ref: nil, wantReason: ""},
		{name: "missing WorkspaceSource: reason=WorkspaceSourceMissing",
			ref:        &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "does-not-exist"},
			wantReason: spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceMissing,
		},
		{name: "WorkspaceSource Valid=False: reason=WorkspaceSourceInvalid",
			ref:        &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "invalid-source"},
			wantReason: spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceInvalid,
		},
		{name: "WorkspaceSource has no Valid condition: reason=WorkspaceSourceInvalid",
			ref:        &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "no-condition-source"},
			wantReason: spiceboxv1alpha1.ReasonAgentClassWorkspaceSourceInvalid,
		},
		{name: "WorkspaceSource Valid=True: no error",
			ref:        &spiceboxv1alpha1.AgentClassWorkspaceSourceRef{Ref: "valid-source"},
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validateWorkspaceSource(ctx, c, "default", tc.ref)
			assert.Equal(t, tc.wantReason, reason)
			if tc.wantReason == "" {
				assert.Empty(t, msg, "no reason means no message either")
			} else {
				require.NotEmpty(t, msg, "a non-empty reason should carry an explanatory message")
			}
		})
	}
}
