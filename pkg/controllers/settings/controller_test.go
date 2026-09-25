package settings

import (
	"context"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1.AddToScheme(s)
	return s
}

func TestClusterReconciler_SelfConsistent(t *testing.T) {
	cases := []struct {
		name       string
		casName    string
		spec       v1.SettingsSpec
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:    "well-known name, consistent: default model in catalog",
			casName: v1.ClusterAgentSettingsName,
			spec: v1.SettingsSpec{
				ModelCatalog: &[]v1.ModelCatalogEntry{{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				}},
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-opus-4-8"},
				},
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1.ReasonSettingsConsistent,
		},
		{
			name:    "well-known name, inconsistent: default model not in catalog",
			casName: v1.ClusterAgentSettingsName,
			spec: v1.SettingsSpec{
				ModelCatalog: &[]v1.ModelCatalogEntry{{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				}},
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-haiku-4-5"},
				},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1.ReasonSettingsSelfInconsistent,
		},
		{
			name:       "non-well-known name: NotSingleton",
			casName:    "other",
			spec:       v1.SettingsSpec{},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1.ReasonSettingsNotSingleton,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cas := &v1.ClusterAgentSettings{
				ObjectMeta: metav1.ObjectMeta{Name: tc.casName},
				Spec:       tc.spec,
			}
			c := fake.NewClientBuilder().
				WithScheme(testScheme()).
				WithObjects(cas).
				WithStatusSubresource(&v1.ClusterAgentSettings{}).
				Build()

			r := &ClusterReconciler{Client: c}
			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tc.casName},
			})
			require.NoError(t, err)

			var got v1.ClusterAgentSettings
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: tc.casName}, &got))

			cond := conditions.Find(got.Status.Conditions, v1.SettingsConditionSelfConsistent)
			require.NotNil(t, cond, "SelfConsistent condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
		})
	}
}

func TestNamespaceReconciler_SelfConsistent(t *testing.T) {
	cases := []struct {
		name       string
		asName     string
		namespace  string
		spec       v1.SettingsSpec
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "well-known name, consistent",
			asName:     v1.AgentSettingsName,
			namespace:  "team-a",
			spec:       v1.SettingsSpec{},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1.ReasonSettingsConsistent,
		},
		{
			name:      "well-known name, inconsistent: default model not in catalog",
			asName:    v1.AgentSettingsName,
			namespace: "team-a",
			spec: v1.SettingsSpec{
				ModelCatalog: &[]v1.ModelCatalogEntry{{
					Name: "claude-opus-4-8", Provider: "anthropic", Default: true,
					TokenRef: &v1.NamespacedSecretKeyRef{Namespace: "agentprimitives-system", Name: "k", Key: "token"},
				}},
				Defaults: &v1.SettingsDefaults{
					Model: &v1.DefaultModel{Provider: "anthropic", Name: "claude-haiku-4-5"},
				},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1.ReasonSettingsSelfInconsistent,
		},
		{
			name:       "non-well-known name: NotSingleton",
			asName:     "other",
			namespace:  "team-a",
			spec:       v1.SettingsSpec{},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1.ReasonSettingsNotSingleton,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			as := &v1.AgentSettings{
				ObjectMeta: metav1.ObjectMeta{Name: tc.asName, Namespace: tc.namespace},
				Spec:       tc.spec,
			}
			c := fake.NewClientBuilder().
				WithScheme(testScheme()).
				WithObjects(as).
				WithStatusSubresource(&v1.AgentSettings{}).
				Build()

			r := &NamespaceReconciler{Client: c}
			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: tc.asName, Namespace: tc.namespace},
			})
			require.NoError(t, err)

			var got v1.AgentSettings
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: tc.asName, Namespace: tc.namespace}, &got))

			cond := conditions.Find(got.Status.Conditions, v1.SettingsConditionSelfConsistent)
			require.NotNil(t, cond, "SelfConsistent condition must be set")
			assert.Equal(t, tc.wantStatus, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
		})
	}
}
