package registry

import (
	"errors"
	"fmt"
	"sort"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/x/kindregistry"
)

// ErrUnknownKind is returned by lookups that cannot answer for an
// unregistered kind name. A SENTINEL so a caller can tell "this binary does
// not have that kind blank-imported" apart from a real answer: the first is a
// wiring bug, the second is data.
var ErrUnknownKind = errors.New("channelkinds: unknown kind")

// reg is the process-wide channel-kind registry. The exported funcs below are
// thin forwarders so consumers keep the same API while the storage,
// mutexing, sorting, and panic-on-dup live in pkg/x/kindregistry.
var reg = kindregistry.New[channelkinds.Kind]("channelkinds", channelkinds.Kind.Name)

func Register(k channelkinds.Kind) { reg.Register(k) }

func Get(name string) (channelkinds.Kind, bool) { return reg.Get(name) }

func All() []channelkinds.Kind { return reg.All() }

// Names is every registered kind's name, in the order All returns them, which
// is sorted.
//
// It exists because "list what this build actually has" is what every refusal
// for an unknown kind ends with, and three packages had independently written
// the same four-line loop over All. Deriving the list from the registry is the
// property that matters — a refusal must never name a kind this binary did not
// link — and one function is one place for that to stay true.
func Names() []string {
	all := All()
	names := make([]string, 0, len(all))
	for _, k := range all {
		names = append(names, k.Name())
	}
	return names
}

// Reset clears the registry. Test-only helper; do not call from production
// code paths.
func Reset() { reg.Reset() }

// DeliversToHuman reports whether a Channel of the kind registered under
// kindName is read by a person — channelkinds.Kind.DeliversToHuman, asked by
// name. Takes a NAME because callers hold a denormalized
// AgentSession.spec.inputChannel.kind string rather than a Kind, and it is
// shaped as a plain func so it can be passed straight to
// v1alpha1.ResolveHumanDirectedBinding as its predicate.
//
// An unregistered name is an ERROR here, unlike IsBrowserSurface below, which
// reports a fail-safe false. The difference is what the answer decides: a
// missing browser surface costs a link, while this one decides whether a
// permission prompt reaches a human at all. Its caller drops the envelope
// either way, and a drop logged as "unknown kind %q" sends an operator to the
// binary's blank imports, where a drop logged as "no human-readable binding in
// the lineage" would send them to the session tree instead.
func DeliversToHuman(kindName string) (bool, error) {
	k, ok := Get(kindName)
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUnknownKind, kindName)
	}
	return k.DeliversToHuman(), nil
}

// AllowsSessionCounterparty reports whether a Channel of the kind registered
// under kindName may have another AgentSession as its counterparty —
// channelkinds.SessionCounterparty, asked by name. Takes a NAME for the same
// reason DeliversToHuman does: callers hold a denormalized
// AgentSession.spec.inputChannel.kind string, not a Kind. A kind that does not
// implement the interface answers false, which is the interface's own
// documented default.
//
// It is the "is this session talking to another agent rather than to a person"
// question, and every consumer must ask it HERE rather than comparing against
// the one kind that answers yes today (AGENTS.md: registry, not branching).
//
// An unregistered name is an ERROR, following DeliversToHuman rather than
// IsBrowserSurface, because the two consumers want OPPOSITE fail-safe answers
// and a bare bool cannot give both: the runner's subagent_conversation
// capability treats "no session counterparty" as "offer no ask_parent", while
// its channel_interaction capability treats "yes" as "withhold
// respond_to_user". A false returned for a kind nobody registered would be
// fail-safe for the first and fail-OPEN for the second — it would re-open the
// laundering path that withholding exists to close. Returning the error lets
// each caller pick, and both currently refuse.
func AllowsSessionCounterparty(kindName string) (bool, error) {
	k, ok := Get(kindName)
	if !ok {
		return false, fmt.Errorf("%w: %q", ErrUnknownKind, kindName)
	}
	sc, ok := k.(channelkinds.SessionCounterparty)
	return ok && sc.AllowsSessionCounterparty(), nil
}

// BoundKind names one of a session's two channel bindings and the kind it
// carries, so a caller can say WHICH binding decided an answer.
type BoundKind struct {
	// Role is "outbound" or "input".
	Role string
	Kind string
}

// FirstSessionCounterparty asks AllowsSessionCounterparty of a session's two
// binding kinds and reports the first that reaches another AgentSession rather
// than a person, or an unregistered kind that leaves the question unanswerable.
//
// It is THE derivation of "this session talks to an agent, not to a person",
// and it exists as one function because two decisions depend on that single
// fact and must not come to disagree about it:
//
//   - whether respond_to_user is offered at all (the channel_interaction
//     capability withholds it, so the reply cannot be laundered into another
//     session's transcript uninspected), and
//   - whether the `artifact-delivered` completion requirement can be answered
//     at all, since the call that answers it is the one being withheld.
//
// The OUTBOUND binding is asked first because that is the one delivery
// follows; the input binding is asked only when it differs, so a session with
// no separate output channel costs exactly one lookup. Either one reaching a
// session is enough — fail-closed is the only direction that cannot re-open
// the laundering path.
//
// Returns (binding, reaches, err). On an unregistered kind, binding names the
// one that could not be resolved and err is non-nil; every caller REFUSES on
// that — each in its own currency — because a false read as "not a session
// counterparty" is fail-open for the tool-withholding half. When neither
// binding reaches a session, binding is the zero value and reaches is false.
func FirstSessionCounterparty(inputKind, outboundKind string) (binding BoundKind, reaches bool, err error) {
	// One entry per DISTINCT binding; when the two coincide exactly one
	// question is asked.
	asked := []BoundKind{{Role: "outbound", Kind: outboundKind}}
	if inputKind != outboundKind {
		asked = append(asked, BoundKind{Role: "input", Kind: inputKind})
	}
	for _, b := range asked {
		toASession, err := AllowsSessionCounterparty(b.Kind)
		if err != nil {
			return b, false, err
		}
		if toASession {
			return b, true, nil
		}
	}
	return BoundKind{}, false, nil
}

// IsBrowserSurface reports whether the kind registered under kindName is a
// channelkinds.BrowserSurface. Takes a NAME because callers hold a
// denormalized AgentSession.spec.inputChannel.kind string rather than a Kind.
//
// An unregistered name reports false — "not a browser surface" — which is the
// same fail-safe answer a kind that simply does not implement the interface
// gives. It is deliberately not an error: the caller's question has a useful
// answer either way, and a kind missing from a binary's blank imports is a
// wiring bug that surfaces far more loudly elsewhere (the channel controller's
// `unknown kind %q`).
func IsBrowserSurface(kindName string) bool {
	k, ok := Get(kindName)
	if !ok {
		return false
	}
	bs, ok := k.(channelkinds.BrowserSurface)
	return ok && bs.IsBrowserSurface()
}

// NeedsWebhook reports whether the kind registered under kindName receives its
// inbound over HTTP — i.e. whether it answers Kind.WebhookReceiver with a
// receiver rather than nil.
//
// It takes a NAME for the same reason IsBrowserSurface does: the callers hold a
// denormalized kind string (a bundle's requires.channels declaration, a
// Channel's spec.kind) rather than a Kind. And it is HERE rather than in each
// caller because the answer decides something none of them should decide by
// name: a kind that receives webhooks needs this cluster to have a public
// address, and a new webhook kind must inherit that without any consumer being
// edited to name it.
//
// An unregistered name reports false. The caller's own path already refuses an
// unknown kind in words that name it (`oap agent install`'s wireOne, the
// channel controller's `unknown kind %q`); answering "no webhook" here does not
// hide that, and it keeps this from being a second place an unlinked kind
// produces a different-shaped failure.
//
// Deps is zero-valued because the question is about the KIND, not about any one
// channel: WebhookReceiver's contract is that a kind either has a webhook
// surface or does not, and every registered kind answers it without touching
// deps (pinned by TestEveryRegisteredKindAnswersWebhookReceiver).
func NeedsWebhook(kindName string) bool {
	k, ok := Get(kindName)
	if !ok {
		return false
	}
	return k.WebhookReceiver(channelkinds.Deps{}) != nil
}

// IsUserlessInput reports whether ch delivers inbound that carries no human:
// its role can receive (input, both, or the CRD's `both` default) AND its
// kind's Kind.UserAttributable() is false — github, whose payload names a PR
// author with no AP identity who consented to nothing, and bento, a cron tick
// nobody sent, today.
//
// It takes the whole Channel rather than a kind name because the answer needs
// BOTH halves: the same github kind in role=output produces no inbound at all,
// so there is nothing to attribute and the answer is false. role=monitoring is
// a framework-event sink bound to no agent — likewise nothing inbound.
//
// It is HERE rather than open-coded at each site because the answer decides
// several rules that do not look related: the class must declare an interact
// permission and the Channel a service subject (there is no user to attribute a
// session to), and a role=output Channel bound to that class must carry its own
// destination (there is no inbound message to reply to). One predicate is what
// keeps the next such rule from becoming another private copy of this walk,
// free to disagree with the rest.
//
// An unregistered kind reports false, matching IsBrowserSurface/NeedsWebhook:
// the caller's own path already refuses an unknown kind in words that name it
// (the channel controller's `unknown kind %q`), and answering "not userless"
// here does not hide that.
func IsUserlessInput(ch *spiceboxv1alpha1.Channel) bool {
	if ch == nil {
		return false
	}
	switch ch.Spec.Role {
	case spiceboxv1alpha1.ChannelRoleOutput, spiceboxv1alpha1.ChannelRoleMonitoring:
		return false
	}
	k, ok := Get(ch.Spec.Kind)
	if !ok {
		return false
	}
	return !k.UserAttributable()
}

// OwnerGroupRefForChannel asks ch's kind for the subject-set that represents
// the channel's membership (e.g. "slack_channel:C123#member"), for use as an
// ownerless session's owner. Returns "" when the kind has no such concept or
// the channel is not configured for one.
//
// Deliberately PURE — no client, no lookups. Resolving WHICH Channel is the
// output one needs an API read and stays with the caller; asking that Channel's
// kind for its group ref does not. Splitting there is what lets both the
// operator (which writes the owner tuple) and the inbound pipeline (which has
// to describe the ownership to humans) share this without dragging a
// controller-runtime client into the registry.
func OwnerGroupRefForChannel(ch *spiceboxv1alpha1.Channel) string {
	if ch == nil {
		return ""
	}
	k, ok := Get(ch.Spec.Kind)
	if !ok {
		return ""
	}
	og, ok := k.(channelkinds.OwnerGroupProvider)
	if !ok {
		return ""
	}
	ref, ok := og.OwnerGroupRef(ch)
	if !ok {
		return ""
	}
	return ref
}

// TriggerStatusReporterFor resolves the kind registered under kindName to its
// channelkinds.TriggerStatusReporter, when it has one.
//
// THE lookup. Every consumer of the trigger-status seam — the capability
// deciding whether to offer the tools, and each tool resolving its surface at
// call time — routes through this one function, so they cannot disagree about
// which kinds report and cannot grow a private `if kind == "github"` between
// them. A kind gains the capability by implementing the interface; nothing here
// or in any consumer is edited for it.
//
// It takes a NAME for the same reason IsBrowserSurface and NeedsWebhook do: the
// callers hold a denormalized AgentSession.spec.inputChannel.kind string, not a
// Kind. An unregistered name reports (nil, false) — "this build has no reporter
// for that" — matching the rest of this file; a kind missing from a binary's
// blank imports is a wiring bug that surfaces far more loudly at the channel
// controller's `unknown kind %q`.
//
// The bool, not just a nil interface, is what callers branch on: a kind that
// does not implement the interface is a normal, silent state, and conflating it
// with a nil reporter would invite the typed-nil trap this repo has already
// been bitten by.
func TriggerStatusReporterFor(kindName string) (channelkinds.TriggerStatusReporter, bool) {
	k, ok := Get(kindName)
	if !ok {
		return nil, false
	}
	r, ok := k.(channelkinds.TriggerStatusReporter)
	if !ok {
		return nil, false
	}
	return r, true
}

// TriggerDescriberFor resolves the kind registered under kindName to its
// channelkinds.TriggerDescriber, when it has one.
//
// THE lookup the opening_summary capability gates on: "does this input kind
// post an opening line of its own" is exactly "does it implement
// TriggerDescriber", and this is the one place that question is asked. A kind
// gains the capability by implementing the interface; nothing here or in any
// consumer is edited for it, matching TriggerStatusReporterFor above.
//
// It takes a NAME for the same reason TriggerStatusReporterFor does: the
// caller holds a denormalized AgentSession.spec.inputChannel.kind /
// ChannelBinding.Kind string, not a Kind. An unregistered name reports
// (nil, false) — "this build has no describer for that" — rather than an
// error: a kind missing from a binary's blank imports is a wiring bug that
// surfaces far more loudly at the channel controller's `unknown kind %q`.
//
// The bool, not just a nil interface, is what callers branch on: a kind that
// does not implement the interface is a normal, silent state (every
// human-typed kind, Slack included), and conflating it with a nil describer
// would invite the typed-nil trap this repo has already been bitten by.
func TriggerDescriberFor(kindName string) (channelkinds.TriggerDescriber, bool) {
	k, ok := Get(kindName)
	if !ok {
		return nil, false
	}
	d, ok := k.(channelkinds.TriggerDescriber)
	if !ok {
		return nil, false
	}
	return d, true
}

// SessionRelationLinks is the union of every registered kind's
// channelkinds.SessionRelationLinker declaration — the subject-set link types
// ("slack_channel#member") this build's channel kinds add to the
// subject-bearing agentsession relations (owner, participant, denied).
//
// THE lookup. The guardian composer unions these into the live SpiceDB schema,
// and every gate that must admit exactly what that schema admits derives its
// answer from this same call, so the two cannot disagree about which subject
// types are legitimate. A kind gains a link type by implementing the interface;
// nothing here or in any consumer is edited for it. The alternative — a literal
// list of types kept beside each gate — is how one controller came to refuse
// slack_channel#member as an agentsession participant while the schema it was
// guarding admitted it.
//
// Sorted and de-duplicated: the composed schema text must be byte-stable across
// reconciles (a churning schema write re-reconciles everything watching it), and
// two kinds may legitimately name the same link type.
//
// A build that links no such kind gets an empty slice, which is the honest
// answer for it: the schema that build would compose carries no channel link
// types either.
func SessionRelationLinks() []string {
	var links []string
	seen := map[string]bool{}
	for _, k := range All() {
		rl, ok := k.(channelkinds.SessionRelationLinker)
		if !ok {
			continue
		}
		for _, l := range rl.SessionRelationLinks() {
			if l == "" || seen[l] {
				continue
			}
			seen[l] = true
			links = append(links, l)
		}
	}
	sort.Strings(links)
	return links
}
