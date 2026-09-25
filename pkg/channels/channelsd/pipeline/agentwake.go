package pipeline

import (
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// Cross-agent participation in a human thread needs two controls, and the spec
// is explicit that the first alone is the attack:
//
//   - MENTION-GATING supplies intent — a bot message wakes another agent only
//     if it @-mentions it. On its own it is defeated by an injected "always
//     @codebot when you reply", which turns the gate into the loop.
//   - CREDIT supplies the structural bound — each HUMAN turn refills a small
//     budget of agent→agent wakes in that thread, and past it the loop cannot
//     continue without a person.
//
// Credit is spent on the WAKE, never on the append. Seeing is free and always
// happens; the scarce thing is being driven. That split is what lets srebot
// read codebot's reply in full while declining to answer it, so a human
// watching the thread sees a conversation rather than a silence.

// WakeCredit is the remaining agent→agent wakes a session may accept before a
// human speaks again.
type WakeCredit struct {
	// Remaining never goes below zero. A negative counter would read as a
	// large deficit on the next comparison and could take several human turns
	// to climb back, silently extending the pause past the one turn intended.
	Remaining int
}

// RefillOnHumanTurn sets the budget. ONLY a human turn calls this — that is
// the entire structural bound, and refilling on an agent turn would make the
// budget decorative.
//
// It SETS rather than adds: a chatty thread must not bank credit for a later
// loop. Ten human messages do not buy ten budgets' worth of agent ping-pong
// afterwards.
//
// A budget of zero — or a negative one, which is a configuration mistake —
// means agent→agent wakes are OFF. The unsafe reading here is "no ceiling",
// which is exactly the loop this exists to bound, so the clamp goes the other
// way.
func RefillOnHumanTurn(budget int) WakeCredit {
	if budget < 0 {
		return WakeCredit{}
	}
	return WakeCredit{Remaining: budget}
}

// SpendForAgentWake consumes one wake, reporting whether there was one to
// consume.
//
// The bool is what the caller acts on: false means append but do not annotate.
// Returning the credit unchanged at zero keeps the counter truthful, so a
// later human refill restores exactly the declared budget.
func SpendForAgentWake(c WakeCredit) (WakeCredit, bool) {
	if c.Remaining <= 0 {
		return WakeCredit{}, false
	}
	return WakeCredit{Remaining: c.Remaining - 1}, true
}

// WakeDecision is what to do with an inbound message, once its origin is known.
type WakeDecision struct {
	// Append is always true for an admitted message. Seeing is free.
	Append bool
	// Wake stamps the wake annotation. False for an agent-driven message with
	// no credit left — the session sees it and answers when a person speaks.
	Wake bool
	// Credit is the value to persist after this decision.
	Credit WakeCredit
}

// DecideWake applies the see/react split to one inbound message.
//
// A HUMAN turn always wakes and refills. An AGENT-driven turn always appends
// and wakes only while credit remains. Those two sentences are the whole of
// track 3b's runtime behaviour, and keeping them in one function is what stops
// a later edit from refilling on the wrong turn — the mistake that would leave
// every spending test passing while the budget did nothing.
func DecideWake(fromAgent bool, budget int, current WakeCredit) WakeDecision {
	if !fromAgent {
		// A person spoke: wake, and reset the budget. SET, not add — a chatty
		// thread must not bank credit for a later loop.
		return WakeDecision{Append: true, Wake: true, Credit: RefillOnHumanTurn(budget)}
	}
	next, ok := SpendForAgentWake(current)
	return WakeDecision{Append: true, Wake: ok, Credit: next}
}

// withPreTurnAnnotations folds the kind's pre-turn annotations into the map a
// NEW session is created with.
//
// The new-session path needs its own route to the same destination because it
// wakes nothing: the operator starts the runner when it observes the created
// object, so there is no post-create moment guaranteed to precede the turn.
// Writing them at construction means no version of the object without them is
// ever visible — the strongest form of the ordering stampPreTurnAnnotations
// achieves by sequencing.
//
// This is where the first fix fell short. The stamp went onto the
// existing-session branch only, so a session's FIRST message — the one that
// creates it — still raced, and that is precisely what a fresh DM is. It
// passed one gate run and failed the next, which is what a narrowed race looks
// like from the outside.
//
// base may be nil; the returned map is always safe to assign.
func withPreTurnAnnotations(base map[string]string, ev channelkinds.InboundEvent, canonical string) map[string]string {
	out := base
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range ev.PreTurnAnnotations {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	// Empty is a real state (a kubectl-driven session has no canonical), and
	// writing "" would be a present-but-meaningless annotation rather than an
	// absent one.
	if ev.RequesterCanonicalIDAnnotation != "" && canonical != "" {
		out[ev.RequesterCanonicalIDAnnotation] = canonical
	}
	return out
}

// stampPreTurnAnnotations writes the annotations the turn's own output will
// need, BEFORE anything wakes.
//
// The ordering is the whole point. These used to be written by the kind after
// Deliver RETURNED — which is after the append and after the wake — so the
// runner could produce a reply and the sender could read the session before
// the write landed. Slack's anchor missing that race posted the agent's reply
// top-level instead of under the user's message: a wrong result, in the user's
// DM, from a write that was merely late.
//
// Reported as a bool rather than an error: a failure here must not fail the
// inbound, which is already appended and worth delivering. The kind's own
// post-Deliver stamp remains the fallback, and false is what tells it to run.
func (p *Pipeline) stampPreTurnAnnotations(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ev channelkinds.InboundEvent, canonical string) bool {
	ann := map[string]string{}
	for k, v := range ev.PreTurnAnnotations {
		if k != "" && v != "" {
			ann[k] = v
		}
	}
	// An empty canonical is a real state — kubectl-driven sessions have none —
	// and writing "" would overwrite a good value from an earlier inbound with
	// nothing.
	if ev.RequesterCanonicalIDAnnotation != "" && canonical != "" {
		ann[ev.RequesterCanonicalIDAnnotation] = canonical
	}
	if len(ann) == 0 {
		return false
	}

	// Skip the write when every value is already what we would write. The
	// common case in a busy thread is the same requester sending again under
	// the same anchor, and a merge patch that changes nothing still bumps
	// resourceVersion and wakes every watcher of this object.
	same := true
	for k, v := range ann {
		if sess.Annotations[k] != v {
			same = false
			break
		}
	}
	if same {
		return true
	}

	patch := map[string]any{"metadata": map[string]any{"annotations": ann}}
	b, err := json.Marshal(patch)
	if err != nil {
		log.FromContext(ctx).Info("pre-turn annotations: marshal failed; the kind's post-Deliver stamp remains the only writer",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return false
	}
	target := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sess.Name, Namespace: sess.Namespace},
	}
	if err := p.K8s.Patch(ctx, target, client.RawPatch(types.MergePatchType, b)); err != nil {
		log.FromContext(ctx).Info("pre-turn annotations: patch failed; falling back to the kind's post-Deliver stamp, which races the turn",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return false
	}
	// Keep the in-memory copy in step: the wake decision and everything after
	// it read this object, and a stale snapshot here would reintroduce the
	// race we just closed, one layer up.
	if sess.Annotations == nil {
		sess.Annotations = map[string]string{}
	}
	for k, v := range ann {
		sess.Annotations[k] = v
	}
	return true
}

// applyWake drives the session for this inbound — or declines to, when the
// message came from another agent and the budget is spent.
//
// Both wake mechanisms are here, behind one decision, on purpose. The
// annotation respawns a runner whose pod has exited; the NATS wakeup nudges
// one that is still alive. Either alone starts a turn, so a bound that gated
// only one of them would do nothing for running sessions — exactly the ones
// with an open loop to sustain.
//
// The append already happened before this is called and is never conditional.
// Declining here means the session SEES the message and answers when a person
// next speaks; it does not mean the message was dropped.
func (p *Pipeline) applyWake(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ev channelkinds.InboundEvent) error {
	if !p.decideAndRecordWake(ctx, sess, ev) {
		return nil
	}
	if err := p.annotateWake(ctx, sess); err != nil {
		return err
	}
	// Best-effort NATS publish (the runner reads memory regardless), but
	// logged on failure — a missing wakeup means the runner only notices the
	// new turn on its next poll, which can look like a stuck session in
	// production.
	besteffort.Log(log.FromContext(ctx).Info, "publishWakeup", p.publishWakeup(sess.Namespace, sess.Name),
		"session", sess.Namespace+"/"+sess.Name)
	return nil
}

// decideAndRecordWake applies the credit rule to one inbound and persists the
// result, reporting whether this message may also WAKE the session.
//
// The credit is written BEFORE the caller annotates. That order is the
// fail-closed one: a spend recorded without the wake that follows costs one
// agent turn of responsiveness, which the next human message restores, while a
// wake stamped before a spend that then fails is an unbounded loop — the
// failure this whole track exists to prevent. A HUMAN turn is the exception in
// the other direction: its refill is bookkeeping, and a person must never be
// left unanswered because a status write failed, so that path logs and wakes
// anyway.
//
// It writes only when the value actually changes. A session that is not in a
// cross-agent thread computes the value it already has and issues no write at
// all, so the ordinary path costs one comparison.
func (p *Pipeline) decideAndRecordWake(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, ev channelkinds.InboundEvent) bool {
	current := WakeCredit{}
	if sess.Status.AgentWakeCredit != nil {
		current.Remaining = *sess.Status.AgentWakeCredit
	}
	d := DecideWake(ev.FromAgent, ev.WakeBudget, current)

	// Nil and a written zero are the same state — "no cross-agent wake has
	// been considered" and "the budget is spent" both refuse a wake — so
	// writing zero over nil would be status churn on every human turn of every
	// session in the cluster, recording nothing.
	unchanged := (sess.Status.AgentWakeCredit == nil && d.Credit.Remaining == 0) ||
		(sess.Status.AgentWakeCredit != nil && *sess.Status.AgentWakeCredit == d.Credit.Remaining)
	if unchanged {
		return d.Wake
	}

	if err := p.writeWakeCredit(ctx, sess, d.Credit.Remaining); err != nil {
		logger := log.FromContext(ctx)
		if !ev.FromAgent {
			// A person spoke. Their turn is not held hostage to a bookkeeping
			// write: log and wake. The cost of proceeding is that the budget
			// keeps its old value, which is the CONSERVATIVE direction — the
			// refill is the half that would have widened it.
			logger.Info("agent-wake credit refill failed; delivering the human turn anyway on the previous budget",
				"session", sess.Namespace+"/"+sess.Name, "budget", ev.WakeBudget, "err", err.Error())
			return d.Wake
		}
		// An agent spoke and the spend did not record. Refuse the wake: the
		// message is already appended, so the session still SEES it and
		// answers when a person next speaks. Waking here is how a persistent
		// write failure turns into the unbounded loop.
		logger.Info("agent-wake credit spend failed; appending without waking so an unrecordable spend cannot become a loop",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
		return false
	}
	return d.Wake
}

// writeWakeCredit records the new credit on status, re-reading under
// RetryOnConflict for the same reason annotateWake does: the operator writes
// this object's status concurrently, so a stale-base optimistic-lock patch
// loses the race.
func (p *Pipeline) writeWakeCredit(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, remaining int) error {
	key := types.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur spiceboxv1alpha1.AgentSession
		if err := p.K8s.Get(ctx, key, &cur); err != nil {
			return err
		}
		base := cur.DeepCopy()
		cur.Status.AgentWakeCredit = &remaining
		return p.K8s.Status().Patch(ctx, &cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}
