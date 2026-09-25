package projectors

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestProvidersProjector(t *testing.T) {
	degraded := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterIdentityProviderName},
		Spec:       spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "oidc", Issuer: "https://idp.example.com", ClientID: "cid"},
		Status: spiceboxv1alpha1.ClusterIdentityProviderStatus{
			Conditions: []metav1.Condition{cond(spiceboxv1alpha1.ConditionIdPValid, metav1.ConditionFalse, "DiscoveryFailed")},
		},
	}
	// No conditions stamped → Unknown, never healthy by default.
	unstamped := &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "secondary"},
		Spec:       spiceboxv1alpha1.ClusterIdentityProviderSpec{Kind: "oidc", Issuer: "https://idp2.example.com", ClientID: "cid2"},
	}

	c := newClient(t, degraded, unstamped)
	rows, err := providersProjector{}.List(context.Background(), c)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	p := rowByName(t, rows, "default", "")
	assert.Equal(t, "cluster", p.Scope)
	assert.Equal(t, "Degraded", p.Status)
	assert.Equal(t, "DiscoveryFailed", p.StatusReason)
	assert.Equal(t, "oidc", badgeVal(p, "kind"))
	assert.Equal(t, "https://idp.example.com", badgeVal(p, "issuer"))
	assert.Equal(t, "kubectl edit clusteridentityprovider default", p.ManageCmd)

	u := rowByName(t, rows, "secondary", "")
	assert.Equal(t, "Unknown", u.Status)
}
