package settings

import (
	"context"
	"net/http"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/settingswiring"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// AgentSessionWebhook denies an AgentSession whose class model is
// specified-but-disallowed, or whose model is unresolvable at session
// runtime (ForSession=true makes a missing model fatal). Fails open
// when the class is not found or settings resolution errors — the
// AgentSession controller is the real gate.
type AgentSessionWebhook struct{ base }

// NewAgentSessionWebhook constructs the handler. d must be the decoder for the
// scheme that includes the agentprimitives API types; build it with
// admission.NewDecoder(scheme).
func NewAgentSessionWebhook(c client.Reader, d admission.Decoder) *AgentSessionWebhook {
	return &AgentSessionWebhook{base{Client: c, decoder: d}}
}

func (w *AgentSessionWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var sess v1.AgentSession
	if err := w.decoder.Decode(req, &sess); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	var class v1.AgentClass
	if err := w.Client.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: sess.Spec.Class}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			// Class missing is the AgentSession controller's gate, not ours.
			return admission.Allowed("class not found; deferring to controller")
		}
		return admission.Allowed("settings resolution unavailable; deferring to controller")
	}

	_, vs, err := settingswiring.ResolveForSession(ctx, w.Client, &class, &sess)
	if err != nil {
		return admission.Allowed("settings resolution unavailable; deferring to controller")
	}
	if f := settingswiring.FirstFatal(vs); f != nil {
		return admission.Denied(f.Reason + ": " + f.Message)
	}
	return admission.Allowed("")
}
