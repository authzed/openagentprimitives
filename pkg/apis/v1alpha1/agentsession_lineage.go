package v1alpha1

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MaxLineageWalk bounds every ancestor walk in the codebase.
//
// Admission DAG-validates delegation rosters, so a cycle reaching this code
// means something upstream already went wrong — but a reconciler that hangs on
// a bad walk takes the whole operator down with it, so every walk terminates
// loudly (an error) rather than looping forever.
const MaxLineageWalk = 16

// ErrLineageCycle is returned when a parent walk exceeds MaxLineageWalk hops,
// which means the chain either loops or is implausibly deep. It is a SENTINEL
// so callers can tell it apart from a transient read failure: a cycle cannot
// clear on its own — it takes a human editing a roster — so a caller that
// retries on it retries forever.
var ErrLineageCycle = errors.New("lineage: cycle or depth exceeded")

// WalkAncestors calls visit for sess and then for each ancestor in turn,
// stopping when visit returns stop=true, when a session has no parent, or when
// MaxLineageWalk hops have been taken (an error).
//
// visit owns the decision to stop; WalkAncestors owns termination and the
// no-silent-errors contract. A failed Get while walking is an error, never
// "the chain ended" — a broken lineage link must surface rather than be
// mistaken for a legitimate root.
func WalkAncestors(
	ctx context.Context,
	r client.Reader,
	sess *AgentSession,
	visit func(*AgentSession) (stop bool, err error),
) error {
	cur := sess
	for i := 0; i < MaxLineageWalk; i++ {
		stop, err := visit(cur)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
		ref, ok := cur.ParentRef()
		if !ok {
			return nil
		}
		var next AgentSession
		if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &next); err != nil {
			return fmt.Errorf("lineage: walk to parent %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		cur = &next
	}
	return fmt.Errorf("%w: over %d hops walking from %s/%s",
		ErrLineageCycle, MaxLineageWalk, sess.Namespace, sess.Name)
}

// HumanDirectedTarget is where a message whose reader must be a person goes:
// the binding to deliver through, and the session that OWNS it.
//
// The owner is not decoration. A card about a delegated child resolves onto an
// ANCESTOR's binding, and a client-hosted host (the `local` TUI, webd's chat
// registry) decides what it may render by asking whether it serves the session
// it was handed. Given the child's identity it refuses a card it is in fact the
// right reader for — the person it serves owns that whole tree. Carrying the
// resolved ancestor means the host is asked about a session it recognizes,
// with no lineage walk of its own and no change to what "owns" means to it.
//
// The walk already had this in hand and threw it away, which is why the
// undeliverable-card bug looked like it needed a new traversal somewhere.
type HumanDirectedTarget struct {
	// Binding always has a kind whose DeliversToHuman is true — the walk's
	// !human arm keeps climbing and never records — so a caller may route to it
	// without re-checking.
	Binding *ChannelBinding
	// Owner is the session Binding was found on: sess itself for a root, an
	// ancestor for a delegated child.
	Owner NamespacedRef

	// Chain is every session the walk passed through to reach Owner, starting
	// at the session asked about and ending at Owner. len(Chain)-1 is the
	// number of delegation hops between them: 0 when the session owns its own
	// binding, 1 for a direct child, 2 for a grandchild.
	//
	// It exists because an approver needs to know HOW the asking session
	// relates to the one they are watching — "this is a grandchild of the
	// session you are in" is material to the decision, and a card that only
	// names the asker leaves them to guess. The walk already visits every hop
	// and kept only the last; collecting them costs nothing extra.
	//
	// A renderer must not try to derive this instead. A client-hosted host
	// knows the asking session and its own, and nothing about the distance
	// between them; finding out means reading lineage on a render path, which
	// is a lookup that host should not be making even where its RBAC allows.
	Chain []NamespacedRef
}

// ResolveHumanDirectedBinding finds the channel binding a message whose reader
// must be a PERSON should be delivered through: the nearest binding, starting
// at sess and climbing the lineage, whose channel kind a human actually reads.
// Any caller that must reach a human about a session resolves delivery through
// this, rather than reading sess.Spec.InputChannel/OutputChannel (or
// OutboundBinding, its single-session counterpart) directly.
//
// Two independent reasons a session's own binding is not the answer:
//
//   - It may have none. A single_turn delegated child is headless by
//     construction — no spec.inputChannel, and a held session cannot gain one —
//     so under forensic hold, without the walk, its release card has nowhere
//     to go and the hold
//     becomes a one-way door whose only exit is deleting the session, which
//     destroys the evidence the hold exists to preserve (spec §2.9(c)).
//   - It may have one that no human reads. A conversational subagent is bound
//     to an `agent` Channel whose far side is its parent SESSION, so routing a
//     permission prompt to the session's own binding would hand a human's
//     decision to another agent. Those bindings are skipped and the walk
//     continues past them.
//
// deliversToHuman answers the second question for a channel-kind name; it is
// REQUIRED (a nil predicate is an error, not an implicit yes) because this
// package cannot import the channel-kind registry that knows the answer, and
// because the fail-open direction here is precisely the defect. Callers inside
// the module pass registry.DeliversToHuman.
//
// sessionhold's publishCard resolves the release card's approver identity
// through this walk, and the outbound relay
// (pkg/channels/channelsd/outbound/relay.go) independently resolves the same
// card's delivery through it — but the walk itself is general-purpose: it
// holds for any session shape a caller constructs, not just a held child.
//
// Three outcomes, kept distinct rather than folded into a single nil:
//   - a resolved binding (found on sess or an ancestor);
//   - (nil, nil) — nothing in the lineage delivers to a human. A kubectl-driven
//     root has no binding at all; a subtree of agent-to-agent channels has
//     bindings that no person reads. Neither is an error, and a caller that
//     needs a human MUST treat it as "there is nobody to ask" rather than
//     falling back to any binding it can find;
//   - (nil, err) — a broken link, cycle, over-deep chain, or a binding whose
//     kind deliversToHuman could not answer for.
func ResolveHumanDirectedBinding(
	ctx context.Context,
	r client.Reader,
	sess *AgentSession,
	deliversToHuman func(kindName string) (bool, error),
) (*HumanDirectedTarget, error) {
	if deliversToHuman == nil {
		return nil, errors.New("lineage: ResolveHumanDirectedBinding requires a deliversToHuman predicate")
	}
	var found *HumanDirectedTarget
	// Every hop the walk takes, recorded as it goes. Accumulated OUTSIDE the
	// found-check below so a session whose binding is skipped (an agent-to-agent
	// surface) still appears: the chain is the delegation path, not the list of
	// sessions that happened to answer.
	var chain []NamespacedRef
	err := WalkAncestors(ctx, r, sess, func(cur *AgentSession) (bool, error) {
		chain = append(chain, NamespacedRef{Namespace: cur.Namespace, Name: cur.Name})
		b := OutboundBinding(cur)
		if b == nil {
			return false, nil
		}
		human, err := deliversToHuman(b.Kind)
		if err != nil {
			return false, fmt.Errorf("lineage: binding %q on %s/%s: %w",
				b.Name, cur.Namespace, cur.Name, err)
		}
		if !human {
			// An agent-to-agent surface. Keep climbing: the person who owns
			// this work is further up, and stopping here would deliver their
			// decision to a machine.
			return false, nil
		}
		found = &HumanDirectedTarget{
			Binding: b,
			Owner:   NamespacedRef{Namespace: cur.Namespace, Name: cur.Name},
			// Copied, not aliased: chain keeps growing if the walk continues,
			// and a caller holding a slice that mutates behind it would read a
			// path that never happened.
			Chain: append([]NamespacedRef(nil), chain...),
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// ResolveRoot returns the topmost ancestor of sess — sess itself when it has no
// parent. It is the identity of the delegation tree: the pooled budget, the
// total-agent ceiling and the closure-aware denial streak are all properties of
// the root rather than of any individual member.
func ResolveRoot(ctx context.Context, r client.Reader, sess *AgentSession) (*AgentSession, error) {
	root := sess
	err := WalkAncestors(ctx, r, sess, func(cur *AgentSession) (bool, error) {
		root = cur
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return root, nil
}

// OwningSubagentRequest returns the SubagentRequest whose reconcile created
// sess, or (nil, nil) when sess was not delegated at all.
//
// It is how a component holding a delegated CHILD reaches the terms of the
// delegation — its mode, and the exchange budget counted down from it. Those
// live on the request rather than on the child on purpose: the child's runner
// can patch its own AgentSession, and a ceiling the constrained party can
// rewrite is not a ceiling. The request carries none of that risk — no runner
// Role grants any write on subagentrequests beyond `create`.
//
// The link is the child's controller OwnerReference, which the SubagentRequest
// controller stamps in the same Create that sets spec.parent — never a name
// derived from a naming convention, which would resolve to whatever happens to
// sit at that name.
//
// Fail-closed, and the two "no request" outcomes are kept apart:
//
//   - spec.parent nil -> (nil, nil). An ordinary session, delegated by nobody.
//     A caller gating on delegation terms has nothing to gate on and proceeds.
//   - spec.parent set but no SubagentRequest owner -> an ERROR. Only that
//     controller sets spec.parent, and it sets both in one Create, so this
//     shape means the link was broken after the fact. Reading it as "not
//     delegated" would drop every term of the delegation silently.
func OwningSubagentRequest(ctx context.Context, r client.Reader, sess *AgentSession) (*SubagentRequest, error) {
	if sess == nil || sess.Spec.Parent == nil {
		return nil, nil
	}
	for _, ref := range sess.OwnerReferences {
		if ref.Kind != "SubagentRequest" || ref.APIVersion != SchemeGroupVersion.String() {
			continue
		}
		var sr SubagentRequest
		if err := r.Get(ctx, types.NamespacedName{Namespace: sess.Namespace, Name: ref.Name}, &sr); err != nil {
			return nil, fmt.Errorf("lineage: get owning SubagentRequest %s/%s of delegated session %s/%s: %w",
				sess.Namespace, ref.Name, sess.Namespace, sess.Name, err)
		}
		return &sr, nil
	}
	return nil, fmt.Errorf("lineage: delegated session %s/%s carries no SubagentRequest owner reference, so the terms of its delegation cannot be read",
		sess.Namespace, sess.Name)
}

// IsDelegatedChild reports whether this session is the CHILD half of a
// conversational delegation: it names a parent, and its own binding reaches
// that parent's session rather than a person.
//
// Both halves are required and neither is sufficient. spec.parent alone also
// describes a single_turn child, which is headless (no binding at all) and was
// never channel-attached to begin with. A session-counterparty binding alone
// would describe any future agent-to-agent surface, including one a session
// holds for reasons that have nothing to do with being delegated to.
//
// The caller decides what to do with the answer; the runner uses it to decide
// that agent_work_complete COMPLETES such a session instead of parking it Idle
// — see runner.Loop.DelegatedChild for why that distinction is load-bearing.
//
// reachesSession answers the kind question, and it is REQUIRED for the same
// reason ResolveHumanDirectedBinding's predicate is: this package cannot
// import the channel-kind registry that knows the answer. Callers inside the
// module pass registry.AllowsSessionCounterparty. An error from it is returned
// rather than swallowed — a kind missing from a binary's blank imports is a
// wiring bug, and guessing either way here silently changes when a delegation
// completes.
func IsDelegatedChild(sess *AgentSession, reachesSession func(kindName string) (bool, error)) (bool, error) {
	if reachesSession == nil {
		return false, errors.New("lineage: IsDelegatedChild requires a reachesSession predicate")
	}
	if sess == nil || sess.Spec.Parent == nil || sess.Spec.InputChannel == nil {
		return false, nil
	}
	toASession, err := reachesSession(sess.Spec.InputChannel.Kind)
	if err != nil {
		return false, fmt.Errorf("lineage: binding %q on %s/%s: %w",
			sess.Spec.InputChannel.Name, sess.Namespace, sess.Name, err)
	}
	return toASession, nil
}
