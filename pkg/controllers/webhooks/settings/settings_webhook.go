package settings

import (
	"context"
	"fmt"
	"net/http"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ClusterAgentSettingsWebhook denies a ClusterAgentSettings that is not the
// singleton name, or whose defaults contradict its own limits.
type ClusterAgentSettingsWebhook struct{ base }

// NewClusterAgentSettingsWebhook constructs the handler. d must be the decoder
// for the scheme that includes the agentprimitives API types.
func NewClusterAgentSettingsWebhook(c client.Reader, d admission.Decoder) *ClusterAgentSettingsWebhook {
	return &ClusterAgentSettingsWebhook{base{Client: c, decoder: d}}
}

func (w *ClusterAgentSettingsWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var cas v1.ClusterAgentSettings
	if err := w.decoder.Decode(req, &cas); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if cas.Name != v1.ClusterAgentSettingsName {
		return admission.Denied(fmt.Sprintf("ClusterAgentSettings must be named %q (singleton)", v1.ClusterAgentSettingsName))
	}
	if msg := SelfConsistencyError(&cas.Spec); msg != "" {
		return admission.Denied("a default violates its own limit: " + msg)
	}
	if msg := PinningKindsError(&cas.Spec); msg != "" {
		return admission.Denied("limits.pinning references an unregistered kind: " + msg)
	}
	if msg := ContentInspectorsError(&cas.Spec); msg != "" {
		return admission.Denied("limits.contentInspectors invalid: " + msg)
	}
	if msg := ModelCatalogError(&cas.Spec, true); msg != "" {
		return admission.Denied("modelCatalog invalid: " + msg)
	}
	if msg := ClassUserPreferencesError(&cas.Spec, true); msg != "" {
		return admission.Denied("classUserPreferences invalid: " + msg)
	}
	return admission.Allowed("")
}

// AgentSettingsWebhook denies an AgentSettings that is not the singleton name,
// or whose defaults contradict its own limits.
type AgentSettingsWebhook struct{ base }

// NewAgentSettingsWebhook constructs the handler. d must be the decoder for
// the scheme that includes the agentprimitives API types.
func NewAgentSettingsWebhook(c client.Reader, d admission.Decoder) *AgentSettingsWebhook {
	return &AgentSettingsWebhook{base{Client: c, decoder: d}}
}

func (w *AgentSettingsWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var as v1.AgentSettings
	if err := w.decoder.Decode(req, &as); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if as.Name != v1.AgentSettingsName {
		return admission.Denied(fmt.Sprintf("AgentSettings must be named %q (singleton)", v1.AgentSettingsName))
	}
	if msg := SelfConsistencyError(&as.Spec); msg != "" {
		return admission.Denied("a default violates its own limit: " + msg)
	}
	if msg := PinningKindsError(&as.Spec); msg != "" {
		return admission.Denied("limits.pinning references an unregistered kind: " + msg)
	}
	if msg := ContentInspectorsError(&as.Spec); msg != "" {
		return admission.Denied("limits.contentInspectors invalid: " + msg)
	}
	if msg := ModelCatalogError(&as.Spec, false); msg != "" {
		return admission.Denied("modelCatalog invalid: " + msg)
	}
	if msg := ClassUserPreferencesError(&as.Spec, false); msg != "" {
		return admission.Denied("classUserPreferences invalid: " + msg)
	}
	return admission.Allowed("")
}
