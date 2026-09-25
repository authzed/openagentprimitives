package identitycmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind.Kinds so
	// credentialRow's credkindregistry.Get(cred.Type) resolves here.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// TestIdentityShow_CredentialsRouteThroughTheRegistry is an end-to-end guard
// on the `oap identity show` wiring: credentialRow/printCredentialRow are
// unit-tested on their own, but this proves the command's RunE actually
// calls them for every credential — a known type prints its DisplayName, and
// an unregistered type still prints a visibly-marked row instead of being
// dropped or panicking the command.
func TestIdentityShow_CredentialsRouteThroughTheRegistry(t *testing.T) {
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: "gh", Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-agent-gh", Key: "token"},
					},
				},
				{Name: "weird", Type: "nosuch"},
			},
		},
	}
	c := aptest.ClientBuilder(t).WithObjects(ai).Build()
	b := &kube.Bundle{Controller: c, Namespace: "default"}
	out := aptest.Run(t, newIdentityShowCmd(aptest.GlobalsFor(b)), "demo-agent")

	assert.Contains(t, out, "gh (Static token; secret=demo-agent-gh key=token)")
	assert.Contains(t, out, "weird (unknown (nosuch))",
		"an unrecognized type must still print a row, not vanish")
}
