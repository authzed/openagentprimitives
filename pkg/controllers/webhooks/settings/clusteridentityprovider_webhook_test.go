package settings

import (
	"context"
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestIdPSpecError_Federation(t *testing.T) {
	base := func() *v1.ClusterIdentityProviderSpec {
		return &v1.ClusterIdentityProviderSpec{
			Kind: "oidc", Issuer: "https://idp.example.com", ClientID: "ap",
			ClientSecretRef: v1.ClusterSecretKeyRef{Namespace: "ns", Name: "s", Key: "k"},
			AllowAnyEmail:   true,
		}
	}
	t.Run("oidc + confidential client: federation allowed", func(t *testing.T) {
		s := base()
		s.Federation = &v1.FederationConfig{Enabled: true}
		assert.Empty(t, IdPSpecError(s))
	})
	t.Run("google + federation: denied (needs oidc)", func(t *testing.T) {
		s := base()
		s.Kind = "google"
		s.Issuer = ""
		s.Federation = &v1.FederationConfig{Enabled: true}
		assert.Contains(t, IdPSpecError(s), "federation requires kind=oidc")
	})
	t.Run("federation + no client secret: denied", func(t *testing.T) {
		s := base()
		s.ClientSecretRef = v1.ClusterSecretKeyRef{}
		s.Federation = &v1.FederationConfig{Enabled: true}
		assert.Contains(t, IdPSpecError(s), "confidential client")
	})
}

func TestClusterIdentityProviderWebhook(t *testing.T) {
	// validBase is a spec with all required fields populated so individual
	// cases only vary the field under test.
	validBase := v1.ClusterIdentityProviderSpec{
		Kind:     "google",
		ClientID: "client-id-123",
		ClientSecretRef: v1.ClusterSecretKeyRef{
			Namespace: "agentprimitives-system",
			Name:      "idp-secret",
			Key:       "client-secret",
		},
		AllowedEmailDomains: []string{"example.com"},
	}

	cases := []struct {
		name        string
		cidpName    string
		spec        v1.ClusterIdentityProviderSpec
		wantAllowed bool
		wantContain string
	}{
		{
			name:        "non-singleton name → denied",
			cidpName:    "my-idp",
			spec:        validBase,
			wantAllowed: false,
			wantContain: "singleton",
		},
		{
			name:     "empty kind → denied",
			cidpName: v1.ClusterIdentityProviderName,
			spec: func() v1.ClusterIdentityProviderSpec {
				s := validBase
				s.Kind = ""
				return s
			}(),
			wantAllowed: false,
			wantContain: "spec.kind is required",
		},
		{
			name:     "oidc without issuer → denied",
			cidpName: v1.ClusterIdentityProviderName,
			spec: func() v1.ClusterIdentityProviderSpec {
				s := validBase
				s.Kind = "oidc"
				s.Issuer = ""
				return s
			}(),
			wantAllowed: false,
			wantContain: "spec.issuer is required for kind=oidc",
		},
		{
			name:     "google with issuer → denied",
			cidpName: v1.ClusterIdentityProviderName,
			spec: func() v1.ClusterIdentityProviderSpec {
				s := validBase
				s.Kind = "google"
				s.Issuer = "https://accounts.google.com"
				return s
			}(),
			wantAllowed: false,
			wantContain: "pinned",
		},
		{
			name:     "no domains, allowAnyEmail=false → denied (fail closed)",
			cidpName: v1.ClusterIdentityProviderName,
			spec: func() v1.ClusterIdentityProviderSpec {
				s := validBase
				s.AllowedEmailDomains = nil
				s.AllowAnyEmail = false
				return s
			}(),
			wantAllowed: false,
			wantContain: "allowedEmailDomains",
		},
		{
			name:     "no domains, allowAnyEmail=true → allowed",
			cidpName: v1.ClusterIdentityProviderName,
			spec: func() v1.ClusterIdentityProviderSpec {
				s := validBase
				s.AllowedEmailDomains = nil
				s.AllowAnyEmail = true
				return s
			}(),
			wantAllowed: true,
		},
		{
			name:     "happy google → allowed",
			cidpName: v1.ClusterIdentityProviderName,
			spec: func() v1.ClusterIdentityProviderSpec {
				s := validBase
				s.Kind = "google"
				s.ClientID = "google-client-id"
				s.AllowedEmailDomains = []string{"example.com"}
				return s
			}(),
			wantAllowed: true,
		},
		{
			name:     "happy oidc → allowed",
			cidpName: v1.ClusterIdentityProviderName,
			spec: func() v1.ClusterIdentityProviderSpec {
				s := validBase
				s.Kind = "oidc"
				s.Issuer = "https://idp.example.com"
				s.ClientID = "oidc-client-id"
				s.AllowedEmailDomains = []string{"example.com"}
				return s
			}(),
			wantAllowed: true,
		},
	}

	sc := scheme()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cidp := &v1.ClusterIdentityProvider{
				ObjectMeta: metav1.ObjectMeta{Name: tc.cidpName},
				Spec:       tc.spec,
			}
			c := fake.NewClientBuilder().WithScheme(sc).Build()
			h := NewClusterIdentityProviderWebhook(c, admission.NewDecoder(sc))

			resp := h.Handle(context.Background(), reqFor(t, cidp))
			assert.Equal(t, tc.wantAllowed, resp.Allowed)
			if tc.wantContain != "" {
				assert.Contains(t, resp.Result.Message, tc.wantContain)
			}
		})
	}
}
