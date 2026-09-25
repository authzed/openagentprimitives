package channelevents

// WebhookInboundSubject carries a VERIFIED webhook delivery from webd (which
// owns the public HTTP surface) to channelsd (which owns the inbound
// pipeline). Cluster-wide, not session-scoped: a delivery arrives before any
// session exists, so it cannot be shaped like the ap.session.*.*.in.* subjects
// used elsewhere in this package — there is no session yet to scope it to.
const WebhookInboundSubject = "ap.channel.webhook_inbound"

// WebhookInboundQueueGroup makes delivery exactly-once across channelsd
// replicas. Without a queue group, N replicas would each create a session for
// the same pull-request event.
const WebhookInboundQueueGroup = "channelsd-webhook-inbound"

// WebhookInboundPayload is published by webd, once per verified webhook
// delivery, on WebhookInboundSubject. It names the Channel CR the delivery
// belongs to and carries the already-verified inbound content — webd has
// checked the delivery's signature before publishing, so channelsd trusts
// AuthzSubject and the rest of this payload without re-verifying.
type WebhookInboundPayload struct {
	// ChannelNamespace and ChannelName identify the Channel CR the webhook
	// delivery targets.
	ChannelNamespace string `json:"channelNamespace"`
	ChannelName      string `json:"channelName"`
	// ChannelKind is the registered channelkinds.Kind name (e.g. "github"),
	// carried for logging/diagnostics; the handler resolves the Channel CR by
	// name, not by this field.
	ChannelKind string `json:"channelKind"`
	// ChannelKey correlates this delivery to an AgentSession (e.g.
	// "pr:demo-org/platform#42").
	ChannelKey string `json:"channelKey"`
	// MessageText is the inbound content delivered to the agent.
	MessageText string `json:"messageText"`
	// AuthzSubject is the verified SpiceDB subject this delivery is
	// attributed to. Webhook deliveries have no per-user identity to resolve,
	// so this is used verbatim as the session's started_by subject — see
	// channelkinds.InboundEvent.AuthzSubject.
	AuthzSubject string `json:"authzSubject"`

	// RawDelivery is the verbatim provider payload webd verified, carried
	// across this NATS hop so channelsd can record it into trigger_delivery —
	// see channelkinds.InboundEvent.RawDelivery for why it cannot be
	// re-fetched instead.
	RawDelivery []byte `json:"rawDelivery,omitempty"`
	// DeliveryEvent is the provider's event-type header value for the same
	// delivery — see channelkinds.InboundEvent.DeliveryEvent.
	DeliveryEvent string `json:"deliveryEvent,omitempty"`
}

// WebhookRoutePattern is the ServeMux pattern webd registers for inbound
// channel webhooks, and WebhookPathFor builds a concrete path that matches
// it. They live together, in a leaf package, because THREE callers need this
// shape and only one of them serves it: webd's route, the App manifest that
// registers the URL with the provider, and the drift check that compares the
// registered URL against where we actually serve.
//
// Built by hand in each place, the first edit to the path silently breaks the
// other two — and the drift check, which exists precisely to catch a
// registered URL we no longer serve, would compare its own stale expectation
// against the stale registration, find them equal, and report no drift.
const WebhookRoutePattern = "/webhooks/{kind}/{ns}/{channel}"

func WebhookPathFor(kind, namespace, channel string) string {
	return "/webhooks/" + kind + "/" + namespace + "/" + channel
}
