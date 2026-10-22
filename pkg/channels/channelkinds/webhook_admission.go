package channelkinds

import (
	"context"
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// DeferredWebhookReceiver opts verified deliveries into durable admission before
// any session is started. Only provider code interprets readiness. Empty Ready
// with Done=false means retry; Done=true discards a superseded/unrelated event.
type DeferredWebhookReceiver interface {
	DeferWebhook(ch *api.Channel, event string) bool
	AdmitWebhook(context.Context, *api.Channel, WebhookSecrets, string, []byte) (WebhookAdmission, error)
}

type WebhookAdmission struct {
	Done   bool
	Reason string
	Ready  []AdmittedWebhook
}

// AdmittedWebhook carries separately the authenticated API context and original
// signed delivery. Context must be derived by the receiver, never by a model.
type AdmittedWebhook struct {
	ChannelKey         string
	MessageText        string
	ContextEvent       string
	Context            []byte
	External           map[string]string
	PreTurnAnnotations map[string]string
}
