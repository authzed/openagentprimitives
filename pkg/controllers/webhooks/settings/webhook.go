package settings

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Paths the handlers are registered under (must match the ValidatingWebhookConfiguration).
const (
	PathAgentClass              = "/validate-agentclass"
	PathAgentSession            = "/validate-agentsession"
	PathClusterAgentSettings    = "/validate-clusteragentsettings"
	PathAgentSettings           = "/validate-agentsettings"
	PathClusterIdentityProvider = "/validate-clusteridentityprovider"
)

// base holds the shared read-only client and decoder every webhook handler
// needs. controller-runtime does not auto-inject the decoder, so callers build
// it with admission.NewDecoder(scheme) and pass it to the New*Webhook
// constructors.
type base struct {
	Client  client.Reader
	decoder admission.Decoder
}
