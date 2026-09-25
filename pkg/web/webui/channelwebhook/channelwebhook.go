// Package channelwebhook mounts ONE route that accepts inbound HTTP
// deliveries for ANY channel kind implementing channelkinds.WebhookReceiver
// (see pkg/channels/channelkinds/webhook.go). There is no kind-specific
// BRANCHING: adding a webhook-capable transport means implementing
// WebhookReceiver and blank-importing the kind elsewhere — this file does
// not change. One provider header (X-GitHub-Delivery) is read, never
// branched on, purely to correlate a log line with the provider's own
// delivery log — see deliveryIDHeader's doc for why that one literal earns
// its place here.
//
// Verification is synchronous (the kind's Verify runs inline, before this
// handler responds), because the upstream provider's delivery timeout is far
// shorter than an agent's review turn: a bad signature must show up as a 401
// in the provider's own delivery log, not time out. Everything after
// verification is asynchronous — a verified delivery is handed to NATS
// (channelevents.WebhookInboundSubject) for channelsd to pick up, and this
// handler returns as soon as the publish succeeds.
//
// The response status is a contract with the upstream provider's retry
// machinery (GitHub's, today): 401 and 413 must never be retried (a forged,
// misconfigured, or oversized delivery must not be replayed), 404/400/204
// are terminal (nothing about retrying would help), and 503 is the one case
// worth retrying — the delivery was genuine, we simply failed to carry it.
package channelwebhook

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	kindregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// maxBodyBytes caps how much of a delivery body this handler will read into
// memory. GitHub caps its own payloads at 25MB; 5MB covers pull-request /
// issue-comment events with headroom while bounding a hostile or
// misbehaving sender.
const maxBodyBytes = 5 << 20

// deliveryIDHeader is the header GitHub (and compatible providers) stamp on
// every delivery with a per-attempt UUID, read ONLY to enrich log lines — it
// is never branched on, and its absence changes no response. This is the
// one provider-shaped literal in this package: the two most security-
// relevant log lines here (unknown kind, nil receiver) have no Channel and
// no receiver to ask for a correlation id, so there is no seam to read it
// through instead. It stops here — a header that changed BEHAVIOR (e.g. an
// event-type or signature header) would belong on WebhookReceiver, not as a
// second literal in this file.
const deliveryIDHeader = "X-GitHub-Delivery"

// UI is the channelwebhook WebUI plug-in. Unlike most WebUIs in this tree it
// is not self-registering via init() + webui.Deps casting: its two
// dependencies (a k8s client and a NATS publish func) are validated once at
// construction (see New), so a misconfigured webd fails loudly at startup
// instead of mounting a handler that nil-panics on delivery.
type UI struct {
	k8s      client.Client
	publish  func(subject string, payload []byte) error
	kindDeps channelkinds.Deps
}

// New constructs the channelwebhook plug-in. Both k8s and publish are
// required: a nil k8s client can never resolve a Channel, and a nil publish
// func would nil-panic the first time a verified delivery reached step 7.
// Refusing here — once, at webd startup — is the fail-closed alternative to
// discovering either gap on the first live GitHub delivery.
func New(k8s client.Client, publish func(subject string, payload []byte) error) (webui.WebUI, error) {
	if k8s == nil {
		return nil, errors.New("channelwebhook: k8s client is required")
	}
	if publish == nil {
		return nil, errors.New("channelwebhook: NATS publish func is required")
	}
	return &UI{
		k8s:     k8s,
		publish: publish,
		// kindDeps is the construction-time context handed to
		// Kind.WebhookReceiver — the per-delivery Channel and credentials
		// Secret are NOT here; they're passed directly to Verify/Translate
		// per call (see channelkinds.WebhookReceiver), since one kind's
		// receiver serves every Channel of that kind.
		kindDeps: channelkinds.Deps{K8sClient: k8s, NATSPublish: publish},
	}, nil
}

func (u *UI) Name() string { return "channelwebhook" }

func (u *UI) Routes(webui.Deps) []webui.Route {
	return []webui.Route{{
		Origin:  webui.OriginTrusted,
		Pattern: channelevents.WebhookRoutePattern,
		Methods: []string{http.MethodPost},
		// AuthHandlerManaged: the credential is carried IN the request (a
		// signature over the body, verified below), not in an ambient
		// cookie — so there is no CSRF vector, and the framework must not
		// impose cookie auth or restrict this to GET/HEAD.
		Auth:    webui.AuthHandlerManaged,
		Handler: http.HandlerFunc(u.handle),
	}}
}

// handle implements the status-code contract documented on the package. Every
// non-2xx path logs at INFO+ with the kind/ns/channel and, where available,
// the upstream delivery id — per the no-silent-errors rule, a caller getting
// a 4xx/5xx must always leave a matching log line an operator can find.
func (u *UI) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)
	kindName, ns, chName := r.PathValue("kind"), r.PathValue("ns"), r.PathValue("channel")
	deliveryID := r.Header.Get(deliveryIDHeader)

	// Step 1: resolve the kind. An unknown kind is a permanent 404 — no
	// retry schedule will ever make it known.
	k, ok := kindregistry.Get(kindName)
	if !ok {
		logger.Info("webhook: unknown channel kind", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID)
		http.Error(w, "unknown channel kind", http.StatusNotFound)
		return
	}

	// Step 2: the kind must actually be webhook-routable. nil is a valid,
	// common answer (see channelkinds.Kind.WebhookReceiver) — fail closed
	// with the same 404 a caller sees for an unregistered kind, rather than
	// falling through to a nil-pointer call below.
	rcv := k.WebhookReceiver(u.kindDeps)
	if rcv == nil {
		logger.Info("webhook: kind has no webhook receiver", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID)
		http.Error(w, "kind is not webhook-routable", http.StatusNotFound)
		return
	}

	// Step 3: resolve the Channel CR, and check its kind matches the URL.
	// The mismatch check is a real security control, not tidiness: without
	// it, a delivery addressed at /webhooks/<urlKind>/... whose Channel is
	// actually a DIFFERENT kind would still get verified by urlKind's
	// receiver against that other kind's Secret — e.g. a Slack channel's
	// signing secret used to validate a GitHub HMAC. Both failure shapes
	// (Channel missing, or Channel present but the wrong kind) return the
	// identical 404 body, so a caller cannot use the response to probe
	// which Channels exist under a different kind.
	var ch spiceboxv1alpha1.Channel
	if err := u.k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: chName}, &ch); err != nil {
		logger.Info("webhook: Channel lookup failed", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID, "err", err.Error())
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}
	if ch.Spec.Kind != kindName {
		logger.Info("webhook: URL kind does not match Channel kind", "urlKind", kindName, "channelKind", ch.Spec.Kind, "ns", ns, "channel", chName, "deliveryID", deliveryID)
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}

	// Body is capped BEFORE it is handed to Verify, so a hostile or
	// misbehaving sender cannot force this handler to buffer an unbounded
	// payload. Read one byte past the cap (rather than exactly maxBodyBytes)
	// so an oversized delivery is DETECTABLE: reading exactly the cap and
	// silently truncating would hand a real HMAC receiver a body that no
	// longer matches its signature, which fails Verify and answers 401 — the
	// one status this route's whole contract promises will never be retried.
	// An operator reading that 401 would go hunting a rotated secret for a
	// delivery that was simply too big; 413 says the true, terminal reason.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		logger.Info("webhook: body read failed", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID, "err", err.Error())
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		logger.Info("webhook: body exceeds size cap", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID, "capBytes", maxBodyBytes)
		http.Error(w, "delivery too large", http.StatusRequestEntityTooLarge)
		return
	}

	// Step 4: read the Channel's credentials Secret. webd holds no
	// cluster-wide Secret access — it is browser-facing, and standing
	// privilege on every Secret in the cluster is what that refusal buys. The
	// read is authorized instead by a Role the operator stamps per Channel, in
	// the Channel's OWN namespace, naming this one Secret by resourceName:
	// pkg/controllers/channel/webhookrbac.go. A Forbidden here therefore means
	// the Channel has not been reconciled yet (or the operator could not stamp
	// the grant — it logs and requeues); the 500 tells the provider to retry,
	// which is the right answer for a grant that is on its way.
	var sec corev1.Secret
	if err := u.k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: ch.Spec.CredentialsRef.SecretName}, &sec); err != nil {
		logger.Info("webhook: credentials Secret read failed",
			"kind", kindName, "ns", ns, "channel", chName, "secret", ch.Spec.CredentialsRef.SecretName, "deliveryID", deliveryID, "err", err.Error())
		http.Error(w, "channel credentials unavailable", http.StatusInternalServerError)
		return
	}

	req := channelkinds.WebhookRequest{Headers: r.Header, Body: body}

	// Step 5: verify. This MUST run before Translate (see the
	// WebhookReceiver interface doc) — a signature mismatch is always a
	// 401, and 401 must never be retried: a forged or misconfigured
	// delivery replayed by the provider would just fail the same way
	// forever, and only muddies the provider's own delivery log, which is
	// the only external signal an operator has for hook health.
	//
	// Deliberately not narrowed to errors.Is(err, channelkinds.
	// ErrWebhookUnauthenticated): ANY non-nil error here maps to 401. The
	// interface doc documents exactly one failure mode for Verify, so this is
	// the fail-closed default for whatever a receiver returns instead — never
	// a 5xx that would tell the provider to retry a delivery that failed
	// authentication for an unanticipated reason.
	if err := rcv.Verify(ctx, channelkinds.WebhookSecrets{Data: sec.Data}, req); err != nil {
		logger.Info("webhook: verification failed", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID, "err", err.Error())
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	// Step 6: translate. (nil, nil) is a verified-but-uninteresting
	// delivery — a 204, not an error, since retrying would just repeat the
	// same answer.
	inb, err := rcv.Translate(ctx, &ch, req)
	if err != nil {
		logger.Info("webhook: translate failed", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID, "err", err.Error())
		http.Error(w, "undecodable delivery", http.StatusBadRequest)
		return
	}
	if inb == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	payload, err := json.Marshal(channelevents.WebhookInboundPayload{
		ChannelNamespace: ns,
		ChannelName:      chName,
		ChannelKind:      kindName,
		ChannelKey:       inb.ChannelKey,
		MessageText:      inb.MessageText,
		AuthzSubject:     inb.AuthzSubject,
		// body is the same already-read, already-verified bytes Verify ran
		// its HMAC over — never re-read from r.Body, which is drained. inb.Event
		// is the provider event-type header Translate already read for
		// itself. Both flow to channelsd for trigger_delivery.
		RawDelivery:   body,
		DeliveryEvent: inb.Event,
	})
	if err != nil {
		logger.Info("webhook: payload marshal failed", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Step 7: hand off to channelsd. This is the ONE failure worth
	// retrying — the delivery was genuine and verified, and we simply
	// failed to carry it onward, so a 503 tells the provider to try again.
	if err := u.publish(channelevents.WebhookInboundSubject, payload); err != nil {
		logger.Info("webhook: NATS publish failed", "kind", kindName, "ns", ns, "channel", chName, "deliveryID", deliveryID, "err", err.Error())
		http.Error(w, "bus unavailable", http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}
