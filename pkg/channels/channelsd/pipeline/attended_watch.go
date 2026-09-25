package pipeline

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// An `attended` delegation child's conversation is with a HUMAN, not with
// the parent that delegated it (spec §4) — its InputChannel/OutputChannel
// are copies of the delegation ROOT's own binding (buildChild, Task 5), and
// the human talks to it exactly as they would any other session. The parent
// is not a party to that thread at all; it only WATCHES, over the relationship
// Task 5 stamped: LabelAttendedParentNamespace/Name, sr's own parent (not
// necessarily the root two hops up).
//
// The watch is the same "see vs. react" split cross-agent Slack participation
// already uses (agentwake.go): Memory.Append is unconditional and free — the
// parent sees every turn of the child's conversation, so the person driving
// the test is never invisible to the session that built the agent being
// tested — while waking the parent is scarce, gated through the exact same
// DecideWake/WakeCredit machinery, and reserved for a turn that actually
// needs the parent's attention.
//
// Scope: this file covers only the child's ORDINARY inbound turns — every
// site where THIS pipeline appends one to the child's own memory. That is
// Deliver's primary existing-session append (the common case) AND
// ResubmitAuthorized's replay append (a sender denied on their first message
// and later approved is still having a turn of the SAME conversation, and
// the parent must see it too — a turn that happened to need an extra
// approval round trip is not a turn the parent is exempt from seeing). Two
// other forced-wake reasons the spec also names, the child's error/failure
// signal and its completion, are not observable from an inbound channel
// event at all: they surface on the child's own terminal SubagentRequest/
// session transition, not on a message arriving through this pipeline. That
// is Task 7's own seam (the "test was stopped" fixed line + forced wake),
// not this one.

// liveAttendedChildOf resolves the session currently watching root as its
// LIVE `attended` delegation child -- attendedParentOf's own inverse: it
// lists sessions carrying LabelAttendedParentNamespace/Name naming root
// (the exact selector attendedParentOf's own doc says a caller may use
// directly), kept only if their phase is non-terminal.
//
// The List is deliberately CLUSTER-WIDE, not scoped to root.Namespace. An
// attended child provisioned inside a builder's WORKSHOP lives in the
// workshop namespace W (subagentrequest controller.go's cross-namespace
// buildChild), which is NOT the root builder session's own namespace B --
// a namespace-scoped List in B would never see it, and the human's turn on
// the builder's channel would never reach the child being tested. The label
// pair is globally unique to exactly one root (an object's namespace+name
// cannot collide), so an unscoped List filtered by both labels returns ONLY
// children watching THIS root, wherever they live (B or W). This is the same
// any-namespace idiom channelsd's own ClusterRole already uses for
// credentialupdaterequests and useridentities ("a session can live in any
// namespace, so list is unscoped") -- channelsd already holds cluster-wide
// `list agentsessions`, so this needs no new authority.
//
// Exactly one live attended child can ever exist for a given watching
// parent -- the SubagentRequest controller's own "one attended child at a
// time" ceiling (controller.go step 6.5) enforces it at creation -- so ok
// is true only when the list resolves to exactly one match. Zero is the
// ordinary "nothing to redirect to" case (no attended child, or its own
// prior one already ended). More than one means that invariant was somehow
// violated elsewhere; redirecting toward an arbitrary pick would be worse
// than not redirecting at all, so this logs and returns as if there were
// none rather than guessing.
func liveAttendedChildOf(ctx context.Context, k8s client.Client, root *spiceboxv1alpha1.AgentSession) (*spiceboxv1alpha1.AgentSession, error) {
	if root == nil {
		return nil, nil
	}
	var candidates spiceboxv1alpha1.AgentSessionList
	if err := k8s.List(ctx, &candidates, client.MatchingLabels{
		spiceboxv1alpha1.LabelAttendedParentNamespace: root.Namespace,
		spiceboxv1alpha1.LabelAttendedParentName:      root.Name,
	}); err != nil {
		return nil, fmt.Errorf("list attended children watching %s/%s: %w", root.Namespace, root.Name, err)
	}
	var live *spiceboxv1alpha1.AgentSession
	for i := range candidates.Items {
		c := &candidates.Items[i]
		if isTerminalAgentSessionPhase(c.Status.Phase) {
			continue
		}
		if live != nil {
			log.FromContext(ctx).Info("attended redirect: more than one live attended child is watching this session; the one-attended-child-at-a-time invariant was violated, so refusing to guess which one to route to",
				"session", root.Namespace+"/"+root.Name)
			return nil, nil
		}
		live = c
	}
	return live, nil
}

// isTerminalAgentSessionPhase reports whether an AgentSession has reached a
// resolved phase. Restated here, rather than imported, for the same reason
// the SubagentRequest controller's own copy is: this package needs the same
// answer about a session it does not itself reconcile
// (pkg/controllers/subagentrequest/controller.go's isTerminalAgentSessionPhase).
func isTerminalAgentSessionPhase(phase string) bool {
	return phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded || phase == spiceboxv1alpha1.AgentSessionPhaseFailed
}

// attendedParentOf reports the watching parent named on sess's own
// LabelAttendedParentNamespace/Name pair, and whether sess carries one at
// all. Both labels are required — a bare index miss on either is "not
// watched", never a partially-resolved parent — mirroring the resolver
// pattern buildChild's own doc describes for reading this pair.
func attendedParentOf(sess *spiceboxv1alpha1.AgentSession) (ns, name string, ok bool) {
	if sess == nil {
		return "", "", false
	}
	ns = sess.Labels[spiceboxv1alpha1.LabelAttendedParentNamespace]
	name = sess.Labels[spiceboxv1alpha1.LabelAttendedParentName]
	return ns, name, ns != "" && name != ""
}

// mentionsAttendedParent reports whether text explicitly calls out the
// watching parent by name, using the same "@name" convention Slack's own
// mentionsUser applies to a bot user id — generalized here because the
// parent is never a member of the child's own channel (it watches from
// outside the thread entirely) and so has no channel-native mention markup
// of its own for a person to invoke.
func mentionsAttendedParent(text, parentName string) bool {
	if parentName == "" || text == "" {
		return false
	}
	return strings.Contains(text, "@"+parentName)
}

// mirrorToAttendedParent appends this turn to the watching parent's own
// inbox unconditionally, then wakes the parent only when the turn
// @-mentions it. It is a no-op for any session that is not a live attended
// child (attendedParentOf returns ok=false) and best-effort end to end: a
// mirror failure must never fail the caller's own inbound, which has
// already appended the turn to the CHILD's own memory and is proceeding to
// wake it regardless of whatever happens here.
//
// Called from both places this pipeline appends a turn to an active
// session's own memory: Deliver's primary existing-session path, and
// ResubmitAuthorized's replay of a message that was denied and then
// approved. Same content, same call — a turn that needed an extra approval
// round trip is still a turn of the child's conversation, and the parent's
// "sees every turn" guarantee does not carve out an exception for it.
func (p *Pipeline) mirrorToAttendedParent(ctx context.Context, active *spiceboxv1alpha1.AgentSession, ev channelkinds.InboundEvent, content []MemContent) {
	parentNS, parentName, ok := attendedParentOf(active)
	if !ok {
		return
	}
	logger := log.FromContext(ctx)
	child := active.Namespace + "/" + active.Name
	parentRef := parentNS + "/" + parentName

	var parent spiceboxv1alpha1.AgentSession
	if err := p.K8s.Get(ctx, client.ObjectKey{Namespace: parentNS, Name: parentName}, &parent); err != nil {
		logger.Info("attended-parent watch: could not resolve the watching parent; the child's turn was not mirrored",
			"child", child, "parent", parentRef, "err", err.Error())
		return
	}

	// See: unconditional, exactly like the child's own append the caller just
	// made. A mirror-append failure is logged and dropped rather than failing
	// the inbound — the child's own turn already landed, and losing
	// visibility for the parent is not a reason to lose the child's message
	// too.
	if err := p.Memory.Append(ctx, parent.Namespace, parent.Name, MemTurn{
		Role: "user", Author: authorSubject(ev), Via: ev.Via, Content: content,
	}); err != nil {
		logger.Info("attended-parent watch: memory append to the parent failed; the parent will not see this turn",
			"child", child, "parent", parentRef, "err", err.Error())
		return
	}

	// React: gated, and only attempted at all when the turn actually names
	// the parent. An ordinary turn of the child's own conversation never
	// wakes the parent, credit or no credit — the parent answers when a
	// person next speaks to IT, same as any cross-agent turn it only sees.
	if !mentionsAttendedParent(ev.MessageText, parent.Name) {
		return
	}

	// The mention is the trigger; DecideWake/WakeCredit (agentwake.go) is
	// still the gate — exactly as it is for a cross-agent Slack mention — so
	// a compromised or manipulated child cannot force unbounded parent wakes
	// merely by repeating the mention in every turn. FromAgent is true
	// because, from the PARENT's point of view, this turn was produced by
	// another agent (the watched child), which is the same "spend, don't
	// refill" branch of DecideWake a cross-agent bot message takes. WakeBudget
	// is left zero: DecideWake reads it only on the fromAgent=false (human)
	// branch, to seed RefillOnHumanTurn, so it is inert on this path rather
	// than a missing config — the parent's own credit stays human-anchored,
	// refilled only when a human turn arrives on the PARENT's own session,
	// unchanged by anything in this file.
	if err := p.applyWake(ctx, &parent, channelkinds.InboundEvent{FromAgent: true}); err != nil {
		logger.Info("attended-parent watch: waking the parent on mention failed",
			"child", child, "parent", parentRef, "err", err.Error())
	}
}
