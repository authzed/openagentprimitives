package outbound

import (
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// humanDirectedKinds is the set of envelope kinds whose reader must be a
// PERSON: each one asks for, records, or refuses a decision that only a human
// has the standing to make. The relay resolves these through the lineage to a
// channel a human actually reads (see handle), instead of through the
// session's own binding.
//
// An EXPLICIT list, never a default branch. The two buckets are "a person
// decides this" and "an agent may read this", and nothing about a new envelope
// kind's Go type says which it is. A default would silently sort the next
// sub-channel someone adds into whichever bucket the switch happened to fall
// through to, which is the same defect this map exists to fix — so a new kind
// is added here by someone who thought about the question, or it routes
// conversationally.
//
// The membership today is the interaction family, and it is the whole
// escalation surface: since the interaction rewrite, every prompt category —
// tool approval, credential request, identity choice, plan gate, session-hold
// release — rides KindInteractionRequest with a channelinteractions.Category
// discriminator, rather than a kind of its own.
//
//   - KindInteractionRequest is the card itself.
//   - KindInteractionApplied is its resolution, rendered in place of the card
//     to the same audience. Delivering it anywhere but where the card went
//     leaves a resolved prompt showing its buttons forever.
//   - KindInteractionDecisionRejected goes back to a person who clicked and
//     lacked standing. Its whole content is "you may not decide this", which
//     is meaningless to any reader but the human who clicked.
//
// Deliberately NOT here: the view/UI offers (live_view_offer,
// session_view_offer, agent_ui_offer), thread_title, user_echo, and the
// queued_messages pair. Each is human-facing in intent, but each is routed
// through SubChannelSenderFor, and the `agent` kind returns nil for every
// sub-channel name — so those already fail closed on a conversational child
// (dropped and logged, never delivered to a session's context). Moving them
// here would not close a hole; it would change where a headless child's
// offers land, which is a delivery feature and not this guard's business.
var humanDirectedKinds = map[channelevents.Kind]struct{}{
	channelevents.KindInteractionRequest:          {},
	channelevents.KindInteractionApplied:          {},
	channelevents.KindInteractionDecisionRejected: {},
}

// isHumanDirected reports whether env.Kind is one the relay must route to a
// human-readable channel.
func isHumanDirected(k channelevents.Kind) bool {
	_, ok := humanDirectedKinds[k]
	return ok
}
