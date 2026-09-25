// pkg/controllers/agentclass/builderclass_test.go
//
// Two agent-builder rules (spec §1.0, §1.2), each decidable from the class
// plus the cluster tier's settings alone:
//
//  1. A class the cluster tier sanctions as a builder must declare an
//     explicit starter allowlist — a sanction with nobody named to use it
//     is a class nobody can ever legitimately start.
//  2. A class outside a workshop namespace must not reference a
//     workshop-labeled SpiceboxToolspec — the consumer half of the
//     webhook's prefix/label rule (Task 8 stamps the label on a
//     workshop-authored tool; this refuses it outside its build space).
package agentclass_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// reconcileBuilderClass builds a fake cluster holding ac plus extra objects
// (a ClusterAgentSettings, a SpiceboxClass, a SpiceboxToolspec — whatever a
// case needs), reconciles ac once, and returns the persisted object. Unlike
// reconcileSessionAuthz/reconcileClass, the reconcile key follows ac's own
// namespace rather than a hardcoded "default": the workshop-tool rule's
// whole point is a class namespace other than "default".
func reconcileBuilderClass(t *testing.T, ac *spiceboxv1alpha1.AgentClass, extra ...client.Object) spiceboxv1alpha1.AgentClass {
	t.Helper()
	objs := []client.Object{
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: ac.Namespace},
			Data:       map[string][]byte{"api-key": []byte("sk-test")},
		},
		ac,
	}
	objs = append(objs, extra...)
	c := fakeclient.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		Build()
	r := &agentclass.Reconciler{Client: c, APIReader: c}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ac.Namespace, Name: ac.Name},
	})
	require.NoError(t, err, "Reconcile must not error")
	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: ac.Namespace, Name: ac.Name}, &got), "Get after reconcile")
	return got
}

// builderClusterSettings sanctions exactly one (ns, name) pair as a builder
// class. The sidecar name is inert to this rule (it matters to the
// agentsession-side workshop hook, not this validator) but BuilderClassRef
// requires all three fields non-empty.
func builderClusterSettings(ns, name string) *spiceboxv1alpha1.ClusterAgentSettings {
	return &spiceboxv1alpha1.ClusterAgentSettings{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
		Spec: spiceboxv1alpha1.SettingsSpec{
			Limits: &spiceboxv1alpha1.SettingsLimits{
				BuilderClasses: &[]spiceboxv1alpha1.BuilderClassRef{
					{Namespace: ns, Name: name, SidecarToolbox: "builder-sidecar"},
				},
			},
		},
	}
}

// TestReconcile_SanctionedBuilderMustListStarters covers rule 1: the
// requirement applies only to a class the CLUSTER tier actually sanctioned,
// and an allowlist of any well-formed shape satisfies it.
func TestReconcile_SanctionedBuilderMustListStarters(t *testing.T) {
	cases := []struct {
		name        string
		sanctioned  bool
		starters    []string
		wantInvalid bool
	}{
		{
			name:        "sanctioned class with no allowedStarters: Invalid/BuilderClassInvalid",
			sanctioned:  true,
			wantInvalid: true,
		},
		{
			name:        "sanctioned class WITH allowedStarters: Valid",
			sanctioned:  true,
			starters:    []string{"user:abc123"},
			wantInvalid: false,
		},
		{
			name:        "unsanctioned class with no allowedStarters: Valid (the requirement is only for sanctioned classes)",
			sanctioned:  false,
			wantInvalid: false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := fmt.Sprintf("bc-starters-%d", i)
			ac := newClass(name)
			if len(tc.starters) > 0 {
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
					Session: &spiceboxv1alpha1.SessionAuthz{AllowedStarters: tc.starters},
				}
			}
			var extra []client.Object
			if tc.sanctioned {
				extra = append(extra, builderClusterSettings(ac.Namespace, name))
			}
			got := reconcileBuilderClass(t, ac, extra...)
			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			require.NotNil(t, cond, "Valid condition must be set")
			if tc.wantInvalid {
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonBuilderClassInvalid, cond.Reason)
				assert.Contains(t, cond.Message, "must list who may start it")
				return
			}
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "message: %s", cond.Message)
		})
	}
}

// workshopToolFixture returns a SpiceboxClass + SpiceboxToolspec pair a
// ToolBundle can reference: the class carries one "git" tool, the toolspec
// narrows it (already Valid=True, as validateBundles requires before this
// rule ever runs), and carries LabelWorkshopNamespace=wsLabel when wsLabel
// is non-empty.
func workshopToolFixture(className, toolspecName, wsLabel string) (*spiceboxv1alpha1.SpiceboxClass, *spiceboxv1alpha1.SpiceboxToolspec) {
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: className},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Image: "spicebox-sandbox:dev",
			Resources: spiceboxv1alpha1.SpiceboxResources{
				CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), EphemeralStorage: resource.MustParse("16Mi"),
			},
			Tools: []spiceboxv1alpha1.SpiceboxTool{{Name: "git", Command: []string{"/usr/bin/git"}}},
		},
	}
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: toolspecName},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             toolspecName,
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "git", Revision: "X"},
			AllowSubcommands: []string{"log"},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			Conditions: []metav1.Condition{{
				Type: spiceboxv1alpha1.SpiceboxToolspecConditionValid, Status: metav1.ConditionTrue,
				Reason: "OK", LastTransitionTime: metav1.Now(),
			}},
		},
	}
	if wsLabel != "" {
		ts.Labels = map[string]string{spiceboxv1alpha1.LabelWorkshopNamespace: wsLabel}
	}
	return cls, ts
}

// TestReconcile_WorkshopToolCannotEscapeItsBuildSpace covers rule 2: a
// workshop-labeled toolspec is refused to any class outside the labeled
// namespace, fine for a class inside it, and untouched for an ordinary
// (unlabeled) toolspec.
func TestReconcile_WorkshopToolCannotEscapeItsBuildSpace(t *testing.T) {
	cases := []struct {
		name        string
		classNS     string
		wsLabel     string
		wantInvalid bool
	}{
		{
			name:        "class outside a workshop referencing a workshop-labeled toolspec: Invalid/ReferencesWorkshopTool",
			classNS:     "default",
			wsLabel:     "ws-abc",
			wantInvalid: true,
		},
		{
			name:        "class inside the workshop namespace referencing its own workshop tool: Valid",
			classNS:     "ws-abc",
			wsLabel:     "ws-abc",
			wantInvalid: false,
		},
		{
			name:        "class referencing an unlabeled toolspec: Valid",
			classNS:     "default",
			wsLabel:     "",
			wantInvalid: false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clsName := fmt.Sprintf("bc-tool-cls-%d", i)
			tsName := fmt.Sprintf("bc-tool-ts-%d", i)
			cls, ts := workshopToolFixture(clsName, tsName, tc.wsLabel)

			ac := newClass(fmt.Sprintf("bc-tool-%d", i))
			ac.Namespace = tc.classNS
			// userPassthrough: no AgentIdentity/credential-coverage fixture
			// needed, so the case turns only on the workshop-label rule.
			ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeUserPassthrough
			ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{
				{Name: "code", Class: clsName, Toolspecs: []string{tsName}},
			}

			got := reconcileBuilderClass(t, ac, cls, ts)
			cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
			require.NotNil(t, cond, "Valid condition must be set")
			if tc.wantInvalid {
				assert.Equal(t, metav1.ConditionFalse, cond.Status)
				assert.Equal(t, spiceboxv1alpha1.ReasonReferencesWorkshopTool, cond.Reason)
				assert.Contains(t, cond.Message, "build space")
				return
			}
			assert.Equal(t, metav1.ConditionTrue, cond.Status, "message: %s", cond.Message)
		})
	}
}
