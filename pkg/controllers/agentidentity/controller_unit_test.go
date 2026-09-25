package agentidentity

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// Registers static/oauth/federated so validateSpecShape's credkindregistry.Get
	// dispatch resolves in this package's (untagged) test binary.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

func TestValidateSpecShape(t *testing.T) {
	cases := []struct {
		name            string
		spec            *spiceboxv1alpha1.AgentIdentitySpec
		wantReason      string // "" means valid
		wantMsgContains string // if non-empty, checked against the returned message
	}{
		{
			name: "oauth credential with static block also set: SpecInvalid",
			spec: &spiceboxv1alpha1.AgentIdentitySpec{
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: "x", Type: "oauth",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"},
					},
					OAuth: &spiceboxv1alpha1.OAuthCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "s"}},
				}},
			},
			wantReason: spiceboxv1alpha1.ReasonSpecInvalid,
		},
		{
			name: "federated credential: not valid on this scope, rejected via credkind.ValidOnScope: SpecInvalid/message names where it IS valid",
			spec: &spiceboxv1alpha1.AgentIdentitySpec{
				Credentials: []spiceboxv1alpha1.AgentCredential{{
					Name: "linear", Type: "federated",
					Federated: &spiceboxv1alpha1.FederatedCredentialSource{
						Resource:          "r",
						ResourceServerURL: "https://x",
						IdPSecretRef:      spiceboxv1alpha1.SecretRef{Name: "s"},
					},
				}},
			},
			wantReason:      spiceboxv1alpha1.ReasonSpecInvalid,
			wantMsgContains: "is not valid here (valid on: [SessionUserIdentity])",
		},
		{
			name: "unknown credential type: rejected by the registry: SpecInvalid/message contains unknown credential type",
			spec: &spiceboxv1alpha1.AgentIdentitySpec{
				Credentials: []spiceboxv1alpha1.AgentCredential{{Name: "c", Type: "nosuch"}},
			},
			wantReason:      spiceboxv1alpha1.ReasonSpecInvalid,
			wantMsgContains: "unknown credential type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validateSpecShape(tc.spec)
			assert.Equal(t, tc.wantReason, reason)
			if tc.wantMsgContains != "" {
				assert.Contains(t, msg, tc.wantMsgContains)
			}
		})
	}
}
