// pkg/controllers/agentclass/identity_in_use_condition_test.go
//
// Reconcile-level coverage for AgentClassConditionAgentIdentityInUse: a class
// that binds an AgentIdentity carrying credentials but offers no tool to use
// them still reconciles Valid=True — the condition warns (and the monitoring
// watcher posts it) without blocking readiness.
package agentclass_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func inUseIdentity(name string, credNames ...string) *spiceboxv1alpha1.AgentIdentity {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
	}
	for _, n := range credNames {
		ai.Spec.Credentials = append(ai.Spec.Credentials, spiceboxv1alpha1.AgentCredential{
			Name: n,
			Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: n + "-secret", Key: "token"},
			},
		})
	}
	return ai
}

func TestAgentClass_AgentIdentityInUseCondition(t *testing.T) {
	cases := []struct {
		name        string
		identityRef string
		identity    *spiceboxv1alpha1.AgentIdentity
		wantStatus  metav1.ConditionStatus
		wantReason  string
	}{
		{
			name:       "no agentIdentity: condition True",
			wantStatus: metav1.ConditionTrue,
			wantReason: spiceboxv1alpha1.ReasonAgentIdentityInUse,
		},
		{
			name:        "identity with no credentials, no tools: condition True (nothing wasted)",
			identityRef: "demo-id",
			identity:    inUseIdentity("demo-id"),
			wantStatus:  metav1.ConditionTrue,
			wantReason:  spiceboxv1alpha1.ReasonAgentIdentityInUse,
		},
		{
			name:        "identity not found, no tools: condition True (nothing bound)",
			identityRef: "missing-id",
			wantStatus:  metav1.ConditionTrue,
			wantReason:  spiceboxv1alpha1.ReasonAgentIdentityInUse,
		},
		{
			name:        "identity with credentials, no tools: condition False/AgentIdentityUnused, class still Valid=True",
			identityRef: "demo-id",
			identity:    inUseIdentity("demo-id", "upstream-token"),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  spiceboxv1alpha1.ReasonAgentIdentityUnused,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := rosterClass("ac-identity-in-use", nil, nil)
			ac.Spec.AgentIdentity = tc.identityRef
			objs := []client.Object{ac}
			if tc.identity != nil {
				objs = append(objs, tc.identity)
			}
			got := reconcileAndGet(t, ac.Name, objs...)

			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionAgentIdentityInUse)
			require.NotNil(t, cond, "AgentClassConditionAgentIdentityInUse must be set on a Valid class")
			assert.Equal(t, tc.wantStatus, cond.Status, "reason=%s msg=%q", cond.Reason, cond.Message)
			assert.Equal(t, tc.wantReason, cond.Reason)
			if tc.wantStatus == metav1.ConditionFalse {
				assert.Contains(t, cond.Message, "upstream-token", "message must name the unused credential")
			}

			validCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			require.NotNil(t, validCond, "Valid condition must be set")
			assert.Equal(t, metav1.ConditionTrue, validCond.Status,
				"an unused AgentIdentity must NEVER block Valid — reason=%s msg=%q", validCond.Reason, validCond.Message)
		})
	}
}
