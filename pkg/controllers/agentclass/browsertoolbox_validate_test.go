//go:build integration

// pkg/controllers/agentclass/browsertoolbox_validate_test.go
//
// Phase A validation of AgentClass.spec.browserToolboxes: existence-only.
// Unlike validateSidecarToolboxes, this does NOT read the BrowserToolbox's
// own Valid condition -- Phase A has no BrowserToolbox reconciler to stamp
// one (it arrives in Phase B), and the CRD's field-level markers mean the
// API server already rejects a structurally-invalid BrowserToolbox.
package agentclass_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// TestAgentClassBrowserToolbox covers the two outcomes of Phase A browser
// toolbox validation: a class referencing a BrowserToolbox that does not
// exist lands Valid=False/AgentClassBrowserToolboxMissing, and creating the
// referenced BrowserToolbox then re-reconciling flips the class to
// Valid=True.
func TestAgentClassBrowserToolbox(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()
	r := &agentclass.Reconciler{Client: env.Client, APIReader: env.Client}

	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	}), "create llm-creds")

	ac := newClass("ac-btb-missing")
	ac.Spec.BrowserToolboxes = []spiceboxv1alpha1.AgentClassBrowserToolboxRef{
		{Name: "web", Ref: "missing-btb"},
	}
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-btb-missing"}})
	require.NoError(t, err, "Reconcile")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-btb-missing"}, &got),
		"Get AgentClass")
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "Valid status")
	assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassBrowserToolboxMissing, cond.Reason, "Valid reason")
	assert.Contains(t, cond.Message, "missing-btb", "message should name the missing ref")

	// Now create the referenced BrowserToolbox and reconcile again -- the
	// class must flip to Valid=True without needing a Valid condition on
	// the BrowserToolbox itself (Phase A is existence-only).
	btb := &spiceboxv1alpha1.BrowserToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-btb", Namespace: "default"},
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{
				Mode:  "allowlist",
				Sites: []spiceboxv1alpha1.BrowserSite{{Host: "example.com"}},
			},
		},
	}
	require.NoError(t, env.Client.Create(ctx, btb), "create BrowserToolbox")

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ac-btb-missing"}})
	require.NoError(t, err, "Reconcile after BrowserToolbox created")

	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ac-btb-missing"}, &got),
		"Get AgentClass after BrowserToolbox created")
	assert.True(t,
		hasCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid, metav1.ConditionTrue),
		"want Valid=True; got conditions=%+v", got.Status.Conditions)
}
