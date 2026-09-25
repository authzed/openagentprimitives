// Whether the metaagent bot needs to be — and is — a participant in a
// channel's conversation, as answered by the channel kind rather than by a
// kind-name compare in the operator.
package channelkinds

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// MetaagentMembership is a channel kind's answer about the metaagent bot's
// presence in one Channel's conversation. The AgentSession reconciler copies
// it verbatim onto Channel.status's MetaagentChannelMembership condition, so
// every field must be one the API server will accept:
//
//   - Status MUST be ConditionTrue or ConditionFalse. Unknown/empty is not a
//     legal condition status here — the metaagent either can reach the
//     conversation or it cannot, and an empty string fails CRD validation on
//     every reconcile.
//   - Reason MUST be non-empty and a legal k8s condition reason. Prefer the
//     ChannelReasonMetaagent* constants in pkg/apis/v1alpha1 over a new
//     string; `oap` surfaces render off the reason.
//   - Message MUST be non-empty and say what an operator should DO. It is the
//     only place the cause is written down.
type MetaagentMembership struct {
	Status  metav1.ConditionStatus
	Reason  string
	Message string
}

// MetaagentMembershipReporter is the OPTIONAL interface a Kind implements when
// its transport has a notion of the metaagent bot being a *member* of a
// conversation — a bot identity that must be present in the room for a scope
// approval to be renderable at all.
//
// Returning ok=false means "not applicable to this transport": the reconciler
// leaves the condition ABSENT rather than writing True or False, so a reader
// can tell "this kind has no such concept" apart from "the bot is missing".
// Kinds whose surface has no bot roster (a TUI, a fake, an in-process web
// chat) simply do not implement this interface.
//
// The answer must be a pure function of ch and of process configuration — it
// is recomputed on every reconcile of every session bound to the channel, and
// a value that moves on its own would rewrite the condition forever.
//
// Optional so out-of-package test doubles need not implement it, NOT as a
// per-kind escape hatch: any kind that DOES report is held to the
// well-formedness contract above by
// TestEveryRegisteredKindReportsWellFormedMetaagentMembership, which sweeps
// the registry rather than a name list.
type MetaagentMembershipReporter interface {
	MetaagentMembership(ch *spiceboxv1alpha1.Channel) (MetaagentMembership, bool)
}

// MetaagentMembershipFor returns k's membership answer for ch, or ok=false
// when k is nil (an unregistered channel-kind name) or does not implement
// MetaagentMembershipReporter.
//
// The "not applicable" fallback lives HERE, not at the call site, so no
// consumer needs to know a kind name to decide whether the condition applies.
func MetaagentMembershipFor(k Kind, ch *spiceboxv1alpha1.Channel) (MetaagentMembership, bool) {
	r, ok := k.(MetaagentMembershipReporter)
	if !ok || r == nil {
		return MetaagentMembership{}, false
	}
	return r.MetaagentMembership(ch)
}
