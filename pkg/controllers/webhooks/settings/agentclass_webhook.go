package settings

import (
	"context"
	"net/http"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/settingswiring"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// AgentClassWebhook denies an AgentClass whose specified (not inherited)
// model/toolkit/MCP is disallowed by the effective settings. A *missing* model
// is NOT denied here (a default tier may supply it before any session runs).
// Fails open on resolution errors — the Plan 2 controller is the real gate.
type AgentClassWebhook struct{ base }

// NewAgentClassWebhook constructs the handler. d must be the decoder for the
// scheme that includes the agentprimitives API types; build it with
// admission.NewDecoder(scheme).
func NewAgentClassWebhook(c client.Reader, d admission.Decoder) *AgentClassWebhook {
	return &AgentClassWebhook{base{Client: c, decoder: d}}
}

func (w *AgentClassWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var class v1.AgentClass
	if err := w.decoder.Decode(req, &class); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	_, vs, err := settingswiring.ResolveForClass(ctx, w.Client, &class)
	if err != nil {
		// Fail open on resolution errors — the controller is the real gate.
		return admission.Allowed("settings resolution unavailable; deferring to controller")
	}
	if f := settingswiring.FirstFatal(vs); f != nil {
		return admission.Denied(f.Reason + ": " + f.Message)
	}
	return admission.Allowed("")
}
