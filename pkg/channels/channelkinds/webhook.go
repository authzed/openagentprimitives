package channelkinds

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
)

// ErrWebhookUnauthenticated is returned by WebhookReceiver.Verify when a
// delivery's signature does not match. The webd route maps it to 401 —
// deliberately NOT a retryable status, because a forged or misconfigured
// delivery must never be replayed.
var ErrWebhookUnauthenticated = errors.New("webhook: delivery failed authentication")

// WebhookRequest is one inbound HTTP delivery, reduced to the two things a
// kind needs. It carries headers and bytes rather than *http.Request
// deliberately: a kind is then unit-testable without a server and
// structurally cannot read the socket or write a response.
type WebhookRequest struct {
	Headers http.Header
	Body    []byte
}

// WebhookSecrets is the resolved credentials Secret for the Channel.
type WebhookSecrets struct {
	Data map[string][]byte
}

// WebhookInbound is a verified, interesting delivery, ready for the pipeline.
type WebhookInbound struct {
	// ChannelKey correlates the delivery to an AgentSession. For github this
	// is "pr:<owner>/<repo>#<number>", so every event on one PR lands in one
	// session and one thread.
	ChannelKey   string
	MessageText  string
	AuthzSubject string

	// Event is the provider's event-type header value (e.g. GitHub's
	// X-GitHub-Event), read here rather than a second time by the caller: a
	// kind's Translate already reads its own provider-shaped header name to
	// decide interestingness, so this is that same read, surfaced rather than
	// discarded. It flows to trigger_delivery via
	// channelkinds.InboundEvent.DeliveryEvent.
	Event string
}

// WebhookReceiver is the seam a channel kind implements to accept inbound HTTP
// deliveries from its upstream provider. Kinds without one return nil from
// Kind.WebhookReceiver and are simply not routable under /webhooks/.
//
// Capability is declared by implementing, never by a list a consumer sweeps —
// the same shape as WebAuthenticator.
type WebhookReceiver interface {
	// Verify authenticates the delivery using the kind's own scheme.
	// MUST be called before Translate. Returns ErrWebhookUnauthenticated on
	// a signature mismatch.
	Verify(ctx context.Context, secrets WebhookSecrets, r WebhookRequest) error

	// Translate maps a verified delivery to an inbound event.
	// (nil, nil) means "this delivery is not interesting" — a 204, not an error.
	Translate(ctx context.Context, ch *spiceboxv1alpha1.Channel, r WebhookRequest) (*WebhookInbound, error)
}

// WebhookURLDriftChecker is an optional Kind capability for a kind whose
// inbound webhook is registered out-of-band with a third-party provider (a
// GitHub App's hook_attributes.url is the case that exists today) — a
// registration that can silently drift from where this cluster actually
// serves. The channel controller discovers this by type-asserting the
// registered Kind from the registry, the same optional-interface shape as
// SessionOwnerProvider / AudienceResolver; it is not part of Kind, and a
// kind without a third-party-registered webhook simply does not implement
// it — there is no "unsupported" answer to return.
//
// Implementations MUST NOT modify the provider's registration. Silently
// repointing a webhook the cluster does not own is an outward-facing change
// to someone else's resource; drift is reported for a human to act on, never
// auto-corrected.
type WebhookURLDriftChecker interface {
	// CheckWebhookURLDrift reads the provider's currently-registered webhook
	// URL and reports whether it matches expectedURL — which the CALLER
	// builds with channelevents.WebhookPathFor against the cluster's actual
	// external base URL, never a literal. secrets is the Channel's resolved
	// credentials Secret. providerAPIBaseURL overrides the provider's API
	// host (tests, or a self-hosted instance of the provider); "" means the
	// implementation's real default.
	//
	// registeredURL is returned alongside drifted so a caller can report or
	// log it even when equal to expectedURL. A non-nil err means the
	// provider could not be reached or read at all — that is NOT the same
	// fact as "no drift", and callers MUST report it distinctly rather than
	// treat an unreachable provider as drift-free.
	CheckWebhookURLDrift(ctx context.Context, ch *spiceboxv1alpha1.Channel, secrets WebhookSecrets, expectedURL, providerAPIBaseURL string) (registeredURL string, drifted bool, err error)
}

// WebhookURLRepointer is an optional Kind capability for a kind that can WRITE
// its inbound webhook's registration back to the provider — the other half of
// WebhookURLDriftChecker, kept as its own interface because a kind can
// perfectly well be able to read a registration it has no API to change.
// Discovered the same way, by type-asserting the registered Kind.
//
// Implementing this grants no authority by itself. The channel controller
// calls it only for a Channel carrying AnnotationAppProvisionedBy — the
// marker a wizard stamps when its own exchange registered the upstream
// application. An application a human registered by hand is someone else's
// resource: it gets a drift finding and is never written to. That check lives
// in the controller, once, rather than in each implementation, so a new kind
// cannot forget it.
type WebhookURLRepointer interface {
	// RepointWebhookURL sets the provider's registered webhook URL for this
	// Channel to url — which the CALLER builds with
	// channelevents.WebhookPathFor against the address this cluster is
	// actually reachable at, never a literal. secrets is the Channel's
	// resolved credentials Secret. providerAPIBaseURL overrides the
	// provider's API host (tests, or a self-hosted instance of the
	// provider); "" means the implementation's real default.
	//
	// Implementations MUST touch only the webhook URL: this is a repoint, not
	// a re-provision, and rewriting any neighbouring field (a webhook secret,
	// a permission set) would break deliveries this call exists to keep
	// working. A returned error means the registration is unchanged and the
	// caller must not record the write as done.
	RepointWebhookURL(ctx context.Context, ch *spiceboxv1alpha1.Channel, secrets WebhookSecrets, url, providerAPIBaseURL string) error
}

// TriggerFact is one observation derived from a verified delivery: the objects
// the payload is about, and what it asserts of them.
//
// Subjects and Facts travel in ONE struct because they are co-derived from a
// single signed payload. A shape that let a caller supply them separately would
// permit a fact about one object to be filed against another, which is the
// attack the mechanism exists to prevent.
type TriggerFact struct {
	Subjects []factcontent.Subject
	Facts    map[string]any
}

// TriggerFactProvider is an optional Kind capability: a webhook-receiving kind
// that can derive facts from a VERIFIED delivery.
//
// Discovered by type-asserting the value the kind's WebhookReceiver(Deps)
// returns (github's receiver{}) — NOT the registered Kind value itself. This
// is the opposite shape from WebhookURLDriftChecker and SessionOwnerProvider,
// which genuinely ARE discovered by type-asserting the registered Kind
// directly (see the callers in pkg/controllers/channel/controller.go). The
// two shapes look alike enough to confuse: a caller that type-asserts the
// Kind here gets an assertion that is always false and silently derives no
// facts, ever, for any kind — no compile error, no panic, just permanent
// silence. Capability is still declared by implementing, never by a list a
// consumer sweeps; it is implemented one level down from where
// WebhookURLDriftChecker/SessionOwnerProvider are.
//
// It takes (ch, event, body) rather than a WebhookRequest because the only
// caller that may WRITE these facts is channelsd's session-open path, which
// holds the delivery as it crossed NATS — a body and an event string, no
// headers. That is deliberate: deriving at the writer means no
// security-relevant derived value travels between components as data.
//
// Implementations must return facts ONLY from a verified delivery, and only
// from values the implementation derived structurally. Provider-authored text
// (a title, a submitter-chosen repository name) is untrusted and must not
// become a fact.
type TriggerFactProvider interface {
	TriggerFacts(ch *spiceboxv1alpha1.Channel, event string, body []byte) ([]TriggerFact, error)
}

// TriggerOwnerProvider is an optional Kind capability: a webhook-receiving kind
// that can name, from a VERIFIED delivery, the external account the triggering
// object belongs to — as a subject-set reference (github's
// "github_user:<numeric-id>#user") suitable for an agentsession owner tuple.
// The session the delivery opens then belongs to that account's holder, who
// resolves to a platform user only through the attested identity edge minted
// from a verified credential — an unlinked account is an empty subject-set and
// the tuple grants nobody anything.
//
// Discovered exactly like TriggerFactProvider — by type-asserting the value the
// kind's WebhookReceiver(Deps) returns, NOT the registered Kind — and with the
// same (ch, event, body) shape, for the same reason: the deriving caller holds
// the delivery as it crossed NATS, and deriving at the writer means no
// security-relevant derived value travels between components as data.
//
// Implementations MUST derive the subject only from provider-STRUCTURAL fields
// of a verified delivery (a numeric account id GitHub itself stamped), never
// from submitter-authored text, and MUST prefer an immutable key over a
// display name — a login is renameable, and a released login claimed by a new
// account would silently transfer whatever the subject grants. Return
// ok=false, not a zero-valued subject, when the delivery names no account.
type TriggerOwnerProvider interface {
	TriggerOwnerSubject(ch *spiceboxv1alpha1.Channel, event string, body []byte) (subject string, ok bool, err error)
}

// TriggerSlotInstance is one resource id a verified webhook delivery names,
// paired with the resource type a class slot's fillFrom must match it
// against. A slot binds it only when the slot's own resourceType agrees —
// this struct does not itself decide which slot, if any, wants it.
type TriggerSlotInstance struct {
	ResourceType string
	ResourceID   string
}

// TriggerSlotProvider is an optional Kind capability: a webhook-receiving
// kind that can name, from a VERIFIED delivery, the instances a class's
// slots may bind at session mint (the "trigger" fillFrom).
//
// Discovered exactly like TriggerFactProvider and TriggerOwnerProvider — by
// type-asserting the value the kind's WebhookReceiver(Deps) returns, NOT the
// registered Kind. The two shapes look alike enough to confuse: a caller
// that type-asserts the Kind here gets an assertion that is always false and
// silently derives no instances, ever, for any kind — no compile error, no
// panic, just permanent silence.
//
// It takes (ch, event, body) rather than a WebhookRequest for the same
// reason TriggerFactProvider does: the only caller that may bind these
// instances is channelsd's session-open path, which holds the delivery as it
// crossed NATS — a body and an event string, no headers. Deriving at the
// writer means no security-relevant derived value travels between
// components as data.
//
// Implementations must return instances ONLY from a verified delivery, and
// only from values the implementation derived structurally — the same rule
// TriggerFactProvider follows: a gate input (what a slot binds) is a worse
// home for submitter-authored text than a prompt is.
type TriggerSlotProvider interface {
	TriggerSlotInstances(ch *spiceboxv1alpha1.Channel, event string, body []byte) ([]TriggerSlotInstance, error)
}

// UnreachableFromInternet says why host cannot be a webhook target a provider
// could ever deliver to, or "" when nothing here rules it out.
//
// It lives beside the webhook seam rather than inside one kind because three
// callers ask the same question and must not disagree: a kind's wizard, before
// it registers an address with a provider; a kind's RepointWebhookURL, at the
// outward-facing write itself; and the channel controller, before it derives an
// expected URL to compare a registration against. A second spelling is how one
// of them refuses an address another has already written.
//
// IT DOES NOT RESOLVE DNS, deliberately. A name is left alone: a lookup would
// put a network dependency on a check that has to work offline (`oap agent
// lint`, a scripted install, a reconcile), and an answer derived from what a
// name resolves to TODAY would be wrong the moment the record changes. Only a
// literal address — which is what a local cluster actually carries — is judged.
//
// The literals are parsed and tested with net's own predicates rather than
// matched as strings, so `127.9.9.9`, `10.x`, `[::1]` and an IPv6 ULA are all
// caught by the rule that covers them instead of by a prefix that happens to
// spell one of them. "localhost" is the one name here, and it is not a
// pattern-match on a range: RFC 6761 reserves it and its subdomains to mean the
// resolving machine itself, whatever they resolve to — which for a webhook is
// the provider's machine, not this one.
func UnreachableFromInternet(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	switch {
	case h == "":
		return "it names no host"
	case h == "localhost" || strings.HasSuffix(h, ".localhost"):
		return `"localhost" always means whichever machine is resolving it, which for a webhook is the provider's`
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return ""
	}
	switch {
	case ip.IsLoopback():
		return fmt.Sprintf("%s is a loopback address, reachable only from the machine it is on", ip)
	case ip.IsPrivate():
		return fmt.Sprintf("%s is a private address, reachable only from inside your own network", ip)
	case ip.IsLinkLocalUnicast():
		return fmt.Sprintf("%s is a link-local address, reachable only on the local network segment", ip)
	case ip.IsUnspecified():
		return fmt.Sprintf("%s is a wildcard, not an address anything can connect to", ip)
	default:
		return ""
	}
}

// UnreachableWebhookURL says why raw cannot be a webhook target, or "" when
// nothing rules it out. It is UnreachableFromInternet over a whole URL, for the
// callers that hold one rather than a bare host.
//
// A value that does not parse as an absolute URL is refused here too: "expected"
// would then be a string no provider-registered URL can ever equal, which is a
// permanent false drift finding rather than a comparison.
func UnreachableWebhookURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "it is not an absolute URL with a scheme and host"
	}
	return UnreachableFromInternet(u.Hostname())
}
