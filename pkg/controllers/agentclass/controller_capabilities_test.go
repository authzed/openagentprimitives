package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	_ "github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability" // register capabilities
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

func condStatus(ac *spiceboxv1alpha1.AgentClass, typ string) *metav1.Condition {
	return conditions.Find(ac.Status.Conditions, typ)
}

func TestValidateCapabilities(t *testing.T) {
	cases := []struct {
		name       string
		caps       map[string]apiextensionsv1.JSON
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "valid known capabilities: CapabilitiesValid=True",
			caps:       map[string]apiextensionsv1.JSON{"memory": {Raw: []byte(`{}`)}, "artifacts": {Raw: []byte(`{"renderers":["html"]}`)}},
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonCapabilitiesValid,
		},
		{
			name:       "unknown capability key: CapabilitiesValid=False/UnknownCapability",
			caps:       map[string]apiextensionsv1.JSON{"teleport": {Raw: []byte(`{}`)}},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonUnknownCapability,
		},
		{
			name:       "malformed config: CapabilitiesValid=False/InvalidCapabilityConfig",
			caps:       map[string]apiextensionsv1.JSON{"artifacts": {Raw: []byte(`{"renderers":"not-a-list"}`)}},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonInvalidCapabilityConfig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: tc.caps}}
			problems := validateCapabilities(ac) // controller helper under test
			if tc.wantStatus == metav1.ConditionTrue {
				assert.Empty(t, problems)
				conditions.SetTrue(ac, &ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionCapabilitiesValid, spiceboxv1alpha1.ReasonCapabilitiesValid)
			} else {
				require.NotEmpty(t, problems)
				conditions.SetFalse(ac, &ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionCapabilitiesValid, tc.wantReason, "…")
			}
			c := condStatus(ac, spiceboxv1alpha1.AgentClassConditionCapabilitiesValid)
			require.NotNil(t, c)
			assert.Equal(t, tc.wantStatus, c.Status)
			assert.Equal(t, tc.wantReason, c.Reason)
		})
	}
}
