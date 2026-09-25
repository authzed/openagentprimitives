// pkg/controllers/agentclass/subagents_capability_condition_test.go
//
// Reconcile-level coverage for AgentClassConditionSubagentsCapabilityGranted:
// a roster with no subagents capability grant is a real class that
// reconciles fully Valid=True — the whole point is that this condition
// warns WITHOUT blocking readiness, so the next author sees the omission
// instead of a silently inert roster.
package agentclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// rosterClass is a minimal, otherwise-valid AgentClass (same provider=test +
// AllowTestProvider=true shape as skillClass in skills_validation_test.go)
// declaring subagents and capabilities, so the test can vary just those two
// fields.
func rosterClass(name string, subagents []string, caps map[string]apiextensionsv1.JSON) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test", Name: "scripted",
				APIKey: spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 1, MaxTokens: 100, MaxDuration: metav1.Duration{Duration: time.Minute},
			},
			Subagents:    subagents,
			Capabilities: caps,
		},
	}
}

// reconcileAndGet builds a fake client over the given objects, reconciles
// className once, and returns the FULL post-reconcile object (unlike
// skills_validation_test.go's reconcileSkillClass, which returns only the
// Valid condition — this test needs to inspect a second, independent
// condition on the same object).
func reconcileAndGet(t *testing.T, className string, objs ...client.Object) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	placeholder := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("unused")},
	}
	all := append([]client.Object{placeholder}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		WithObjects(all...).Build()
	r := &agentclass.Reconciler{Client: c, AllowTestProvider: true, MaxDelegationDepth: 5}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: className, Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile must not return an error")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: className, Namespace: "default"}, &got))
	return &got
}

func TestAgentClass_SubagentsCapabilityCondition(t *testing.T) {
	cases := []struct {
		name           string
		subagents      []string
		caps           map[string]apiextensionsv1.JSON
		wantStatus     metav1.ConditionStatus
		wantReason     string
		wantValidTrue  bool
		seedRosterHelp bool // whether to seed a real "demo-helper" class for the roster to resolve against
	}{
		{
			name:          "no roster: condition True, reason SubagentsCapabilityGranted",
			wantStatus:    metav1.ConditionTrue,
			wantReason:    spiceboxv1alpha1.ReasonSubagentsCapabilityGranted,
			wantValidTrue: true,
		},
		{
			name:           "roster present, subagents capability granted: condition True",
			subagents:      []string{"demo-helper"},
			caps:           map[string]apiextensionsv1.JSON{"subagents": {Raw: []byte(`{}`)}},
			wantStatus:     metav1.ConditionTrue,
			wantReason:     spiceboxv1alpha1.ReasonSubagentsCapabilityGranted,
			wantValidTrue:  true,
			seedRosterHelp: true,
		},
		{
			name:           "roster present, subagents capability OMITTED: condition False, class still Valid=True",
			subagents:      []string{"demo-helper"},
			wantStatus:     metav1.ConditionFalse,
			wantReason:     spiceboxv1alpha1.ReasonSubagentsCapabilityMissing,
			wantValidTrue:  true,
			seedRosterHelp: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := rosterClass("ac-subagents-cap", tc.subagents, tc.caps)
			objs := []client.Object{ac}
			if tc.seedRosterHelp {
				objs = append(objs, rosterClass("demo-helper", nil, nil))
			}
			got := reconcileAndGet(t, ac.Name, objs...)

			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionSubagentsCapabilityGranted)
			require.NotNil(t, cond, "AgentClassConditionSubagentsCapabilityGranted must always be set")
			assert.Equal(t, tc.wantStatus, cond.Status, "reason=%s msg=%q", cond.Reason, cond.Message)
			assert.Equal(t, tc.wantReason, cond.Reason)

			validCond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			require.NotNil(t, validCond, "Valid condition must be set")
			if tc.wantValidTrue {
				assert.Equal(t, metav1.ConditionTrue, validCond.Status,
					"a missing subagents capability grant must NEVER block Valid — reason=%s msg=%q", validCond.Reason, validCond.Message)
			}
		})
	}
}
