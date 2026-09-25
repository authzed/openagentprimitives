package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestIdentitiesProjector(t *testing.T) {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "bot", Namespace: "ns1"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{Name: "gh", Type: "static"},
				{Name: "linear", Type: "oauth"},
			},
		},
		Status: spiceboxv1alpha1.AgentIdentityStatus{
			ResolvedCredentials: []string{"gh"},
			Conditions:          []metav1.Condition{cond(spiceboxv1alpha1.AgentIdentityConditionValid, metav1.ConditionTrue, "AllReferencesResolve")},
		},
	}
	noStatus := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "ns1"},
	}

	c := newClient(t, ai, noStatus)
	rows, err := identitiesProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	b := rowByName(t, rows, "bot", "")
	assert.Equal(t, "namespaced", b.Scope)
	assert.Equal(t, "Valid", b.Status)
	assert.Equal(t, 2, countVal(b, "credentials"))
	assert.Equal(t, 1, countVal(b, "resolved"))
	// distinct credential types surfaced as badges (sorted: oauth, static).
	assert.Equal(t, "oauth", b.Badges[0].Value)
	assert.Equal(t, "static", b.Badges[1].Value)
	assert.Equal(t, "kubectl edit agentidentity bot -n ns1", b.ManageCmd)

	f := rowByName(t, rows, "fresh", "")
	assert.Equal(t, "Unknown", f.Status)
}
