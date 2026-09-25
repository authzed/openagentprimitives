package settings

import (
	"context"
	"fmt"
	"net/http"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ClusterIdentityProviderWebhook denies a ClusterIdentityProvider that
// is not the singleton name or whose spec is not fail-closed-valid.
type ClusterIdentityProviderWebhook struct{ base }

// NewClusterIdentityProviderWebhook constructs the handler. d must be the
// decoder for the scheme that includes the agentprimitives API types.
func NewClusterIdentityProviderWebhook(c client.Reader, d admission.Decoder) *ClusterIdentityProviderWebhook {
	return &ClusterIdentityProviderWebhook{base{Client: c, decoder: d}}
}

func (w *ClusterIdentityProviderWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var cidp v1.ClusterIdentityProvider
	if err := w.decoder.Decode(req, &cidp); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if cidp.Name != v1.ClusterIdentityProviderName {
		return admission.Denied(fmt.Sprintf("ClusterIdentityProvider must be named %q (singleton)", v1.ClusterIdentityProviderName))
	}
	if msg := IdPSpecError(&cidp.Spec); msg != "" {
		return admission.Denied(msg)
	}
	return admission.Allowed("")
}

// IdPSpecError returns "" when the spec is acceptable, else a human-readable
// denial reason. Exported so the status controller shares the same judgment
// and the two can never disagree.
func IdPSpecError(s *v1.ClusterIdentityProviderSpec) string {
	if s.Kind == "" {
		return "spec.kind is required"
	}
	if s.Kind == "google" && s.Issuer != "" {
		return "spec.issuer is pinned by kind=google and must be empty"
	}
	if s.Kind == "oidc" && s.Issuer == "" {
		return "spec.issuer is required for kind=oidc"
	}
	if len(s.AllowedEmailDomains) == 0 && !s.AllowAnyEmail {
		return "spec.allowedEmailDomains is empty; set domains or explicitly set allowAnyEmail=true (fail closed)"
	}
	if s.Federation != nil && s.Federation.Enabled {
		if s.Kind != "oidc" {
			return "spec.federation requires kind=oidc (ID-JAG needs a real token-exchange endpoint)"
		}
		if s.ClientSecretRef.Name == "" || s.ClientSecretRef.Key == "" {
			return "spec.federation requires a confidential client; set spec.clientSecretRef"
		}
	}
	return ""
}
