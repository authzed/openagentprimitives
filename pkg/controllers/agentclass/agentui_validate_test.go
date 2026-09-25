// pkg/controllers/agentclass/agentui_validate_test.go
//
// An AgentClass that opts into an AgentUI (spec.agentUI.ref) is parked
// Valid=False until that AgentUI exists in the same namespace AND is itself
// Valid=True. Before this gate existed, a typo'd or Valid=False ref reported
// Valid=True/AllReferencesResolve and the only symptom was an Info log at
// session start plus an empty browser-tool surface.
//
// These tests drive the full Reconcile (fake client) with
// AllowTestProvider=true so a class that passes every OTHER check reaches
// Valid=True — isolating the agentUI gate as the only thing that can flip it.
// Driving Reconcile rather than calling the unexported validator is what makes
// the CALL SITE part of what is pinned: deleting the validateAgentUI call from
// Reconcile fails the missing/invalid rows here, not just a compile error.
package agentclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// agentUIClass builds a minimal, otherwise-valid AgentClass. A nil grant means
// spec.agentUI is absent entirely — the "unaffected" row.
func agentUIClass(name string, grant *spiceboxv1alpha1.AgentClassUIGrant) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Model: &spiceboxv1alpha1.ModelConfig{
				Provider: "test",
				Name:     "scripted",
				APIKey:   spiceboxv1alpha1.SecretKeyRef{Name: "placeholder", Key: "api-key"},
			},
			SystemPrompt: spiceboxv1alpha1.PromptSource{Inline: "you are an agent"},
			Budget: &spiceboxv1alpha1.BudgetConfig{
				MaxTurns: 1, MaxTokens: 100, MaxDuration: metav1.Duration{Duration: time.Minute},
			},
			AgentUI: grant,
		},
	}
}

// agentUIObj builds an AgentUI carrying the given Valid condition status. An
// empty status string means "no Valid condition at all yet" — the freshly
// created, not-yet-reconciled case.
func agentUIObj(name string, valid metav1.ConditionStatus, reason string) *spiceboxv1alpha1.AgentUI {
	aui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{{Name: "main"}},
		},
	}
	if valid != "" {
		aui.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.AgentUIConditionValid,
			Status:             valid,
			Reason:             reason,
			LastTransitionTime: metav1.Now(),
		}}
	}
	return aui
}

// reconcileAgentUIClass builds a fake client over the given objects,
// reconciles the named class, and returns its observed Valid condition.
func reconcileAgentUIClass(t *testing.T, className string, objs ...client.Object) *metav1.Condition {
	t.Helper()
	placeholder := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "placeholder", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("unused")},
	}
	all := append([]client.Object{placeholder}, objs...)
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		WithObjects(all...).Build()
	r := &agentclass.Reconciler{Client: c, AllowTestProvider: true}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: className, Namespace: "default"},
	})
	require.NoError(t, err, "Reconcile must not return an error (failures surface on Valid)")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: className, Namespace: "default"}, &got), "Get after Reconcile")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition must be set")
	return cond
}

func TestAgentClass_AgentUI_Validation(t *testing.T) {
	cases := []struct {
		name       string
		grant      *spiceboxv1alpha1.AgentClassUIGrant
		objs       []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string // substring; "" = no substring check
	}{
		{
			name:       "no spec.agentUI at all: gate is a no-op, class Valid=True",
			grant:      nil,
			objs:       []client.Object{agentUIObj("some-ui", metav1.ConditionTrue, "SpecOK")},
			wantStatus: metav1.ConditionTrue,
		},
		{
			name:       "spec.agentUI.ref names no AgentUI: Valid=False/AgentUIMissing",
			grant:      &spiceboxv1alpha1.AgentClassUIGrant{Ref: "typoed-ui"},
			objs:       nil,
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentClassAgentUIMissing,
			wantMsg:    "AgentUI/typoed-ui not found",
		},
		{
			name:       "AgentUI exists but Valid=False: Valid=False/AgentUIInvalid",
			grant:      &spiceboxv1alpha1.AgentClassUIGrant{Ref: "broken-ui"},
			objs:       []client.Object{agentUIObj("broken-ui", metav1.ConditionFalse, spiceboxv1alpha1.ReasonAgentUIInvalidDefault)},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentClassAgentUIInvalid,
			wantMsg:    "is not Valid",
		},
		{
			// GrantUnresolved is Valid=Unknown: the AgentUI controller could
			// not compute the eligible-tools ceiling this reconcile. Admitting
			// the class on an unknown ceiling is the fail-open answer, so this
			// parks too — the AgentUI watch recovers it when status settles.
			name:       "AgentUI Valid=Unknown (GrantUnresolved): Valid=False/AgentUIInvalid",
			grant:      &spiceboxv1alpha1.AgentClassUIGrant{Ref: "pending-ui"},
			objs:       []client.Object{agentUIObj("pending-ui", metav1.ConditionUnknown, spiceboxv1alpha1.ReasonAgentUIGrantUnresolved)},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentClassAgentUIInvalid,
		},
		{
			name:       "AgentUI exists with no Valid condition yet: Valid=False/AgentUIInvalid",
			grant:      &spiceboxv1alpha1.AgentClassUIGrant{Ref: "fresh-ui"},
			objs:       []client.Object{agentUIObj("fresh-ui", "", "")},
			wantStatus: metav1.ConditionFalse,
			wantReason: spiceboxv1alpha1.ReasonAgentClassAgentUIInvalid,
		},
		{
			name:       "AgentUI exists and Valid=True: gate passes, class Valid=True",
			grant:      &spiceboxv1alpha1.AgentClassUIGrant{Ref: "good-ui", GrantedTools: []string{"crm_list_leads"}},
			objs:       []client.Object{agentUIObj("good-ui", metav1.ConditionTrue, spiceboxv1alpha1.ReasonAgentUISpecOK)},
			wantStatus: metav1.ConditionTrue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objs := append([]client.Object{agentUIClass("ac-agentui", tc.grant)}, tc.objs...)
			cond := reconcileAgentUIClass(t, "ac-agentui", objs...)
			assert.Equal(t, tc.wantStatus, cond.Status, "Valid status (reason=%s msg=%s)", cond.Reason, cond.Message)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, cond.Reason)
			}
			if tc.wantMsg != "" {
				assert.Contains(t, cond.Message, tc.wantMsg)
			}
		})
	}
}
