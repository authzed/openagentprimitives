package identitycmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestUserIdentityShow_CredentialsRouteThroughTheRegistry mirrors
// TestIdentityShow_CredentialsRouteThroughTheRegistry for `oap useridentity
// show`: same shared credentialRow/printCredentialRow helper, different
// owning CR. Both commands must degrade an unregistered type visibly rather
// than diverging on it.
func TestUserIdentityShow_CredentialsRouteThroughTheRegistry(t *testing.T) {
	ui := &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "u-abc123"},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject: "user:alice@example.com",
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "gh", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "u-abc123-gh", Key: "token"},
					},
				},
				{Name: "weird", Type: "nosuch"},
			},
		},
	}
	c := aptest.ClientBuilder(t).WithObjects(ui).Build()
	b := &kube.Bundle{Controller: c, Namespace: "default"}
	out := aptest.Run(t, newUserIdentityShowCmd(aptest.GlobalsFor(b)), "u-abc123")

	assert.Contains(t, out, "gh (Static token; secret=u-abc123-gh key=token)")
	assert.Contains(t, out, "weird (unknown (nosuch))",
		"an unrecognized type must still print a row, not vanish")
}
