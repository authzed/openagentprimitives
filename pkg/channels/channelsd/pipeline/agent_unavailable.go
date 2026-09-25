package pipeline

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// unhealthyAgent describes an AgentClass that cannot start a session, and what
// an operator has to fix. It is what both halves of the answer are built from:
// the user's notice and the operators' alert.
type unhealthyAgent struct {
	// namespace/name identify the AgentClass.
	namespace string
	name      string
	// reason/message are its failing condition, verbatim — or the synthetic
	// AgentClassMissing pair when there is no such class at all.
	reason  string
	message string
	// identity is the class's bound AgentIdentity, when it has one. It exists
	// so the operator hint can name the exact thing to re-link rather than a
	// category of thing.
	identity string
}

// ref is the alert dedup key ("AgentClass ns/name"). Built here and only here,
// because the clear-on-recovery path has to reconstruct the same string for a
// class it has NOT diagnosed; two hand-rolled concatenations that drifted apart
// would fail silently — the alert would simply never re-arm, and the next
// outage of that agent would go unreported.
func (u unhealthyAgent) ref() string { return agentRef(u.namespace, u.name) }

func agentRef(namespace, name string) string { return "AgentClass " + namespace + "/" + name }

// agentOutageAlerts remembers which outages have already been reported to the
// monitoring channel, so a day-long outage produces one alert rather than one
// per message that walks into it.
//
// It deliberately does NOT dedup the USER-facing notice. Those are two
// different jobs: every conversation that stalls on a broken agent is owed an
// explanation, while the operators need the alert to stay findable — and an
// alert repeated once per inbound is the fastest way to get a monitoring
// channel muted.
//
// Keyed by (class, reason) so an outage that CHANGES cause re-alerts: the
// second reason is new information, and the remedy for it is usually different.
//
// In-memory, so a channelsd restart re-alerts an ongoing outage. That is the
// right way to be wrong: the durable record is the AgentClass condition itself,
// this is only a reminder, and a duplicate reminder costs a line in a channel
// while a missed one costs an agent nobody notices is down.
type agentOutageAlerts struct {
	mu sync.Mutex
	// alerted maps an AgentClass ref to the reason already reported for it. An
	// entry is removed when that class is next seen healthy.
	alerted map[string]string
}

// shouldAlert records (ref, reason) as reported and returns whether it was new
// — i.e. whether this caller should publish.
func (a *agentOutageAlerts) shouldAlert(ref, reason string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.alerted == nil {
		a.alerted = map[string]string{}
	}
	if prev, ok := a.alerted[ref]; ok && prev == reason {
		return false
	}
	a.alerted[ref] = reason
	return true
}

// resolved forgets ref, re-arming the alert for its next outage.
func (a *agentOutageAlerts) resolved(ref string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.alerted, ref)
}

// unhealthyAgentFor reports whether the named AgentClass is in a state the
// AgentSession controller will refuse to start a runner for.
//
// It mirrors that controller's gate EXACTLY — the class must exist and be
// Valid=True — because the whole point is to predict what it will do. Nothing
// about the delivery CHANNEL is consulted: a Channel can be Valid=False for
// reasons that never block a session (an owner policy that does not resolve, an
// unresolvable reply target), and telling a user their agent is down while it
// answers them would be worse than saying nothing.
//
// It takes a CLASS NAME rather than the Channel, so that every caller has to
// say whose health it is asking about. The Channel's own spec.agentClass is the
// right answer only when that Channel is the target session's binding: an
// `agent` Channel is ONE conversation shared by a pair and carries the CHILD's
// class, so reading it on a child -> parent delivery described the child. Both
// callers ask about the session the message is being delivered into.
//
// A class with no Valid condition yet is treated as healthy. That is the
// ordinary cold-start window between a class being created and its controller
// first reconciling it; calling it an outage would make every fresh install's
// first message report one.
//
// This costs one apiserver round-trip per inbound that resolves a session
// (channelsd's client is uncached), on a path that already does several and
// runs at human message rates. It is deliberately not cached: a stale hit would
// tell a user their agent is broken after someone had just fixed it.
func (p *Pipeline) unhealthyAgentFor(
	ctx context.Context,
	namespace, className string,
) (unhealthyAgent, bool) {
	var zero unhealthyAgent
	if className == "" {
		return zero, false
	}
	var ac spiceboxv1alpha1.AgentClass
	if err := p.K8s.Get(ctx, client.ObjectKey{
		Namespace: namespace, Name: className,
	}, &ac); err != nil {
		if errors.IsNotFound(err) {
			// The controller will park the session at
			// ClassResolved=False/AgentClassMissing, and nothing watches a
			// class that does not exist — so this one never self-heals and is
			// the most important of all to say out loud.
			return unhealthyAgent{
				namespace: namespace,
				name:      className,
				reason:    spiceboxv1alpha1.ReasonAgentClassMissing,
				message:   "AgentClass " + className + " not found",
			}, true
		}
		// A transient API error must not become an outage report. Logged rather
		// than dropped: a persistent failure here silently disables this whole
		// surface.
		log.FromContext(ctx).V(1).Info("agent-health check: AgentClass lookup failed; assuming healthy",
			"namespace", namespace, "agentClass", className, "err", err.Error())
		return zero, false
	}
	c := meta.FindStatusCondition(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	if c == nil || c.Status != metav1.ConditionFalse {
		return zero, false
	}
	return unhealthyAgent{
		namespace: ac.Namespace,
		name:      ac.Name,
		reason:    c.Reason,
		message:   c.Message,
		identity:  ac.Spec.AgentIdentity,
	}, true
}

// surfaceUnhealthyAgent explains a stall that would otherwise be silent: the
// message has been accepted and a session exists, but its agent is unhealthy,
// so the AgentSession controller parks it at ClassResolved=False and no runner
// picks it up.
//
// It does NOT refuse the message. Parking is the designed recovery path — the
// session resumes and answers the ORIGINAL message once the class goes
// Valid=True again — and refusing would both throw the message away and stop a
// mid-session credential revocation from staying surgical (an agent that loses
// one tool must keep talking). The gap this closes is purely that nobody was
// TOLD: the user watched a thread do nothing, and the operators had no signal
// that anyone was blocked.
//
// Callers must skip it when a live runner will answer — see liveRunner.
func (p *Pipeline) surfaceUnhealthyAgent(
	ctx context.Context,
	ev channelkinds.InboundEvent,
	agent unhealthyAgent,
	sess channelevents.SessionRef,
	requester identity.CanonicalUserID,
) {
	logger := log.FromContext(ctx)
	logger.Info("inbound parked: bound AgentClass cannot start a runner",
		"channel", channelRefForLog(ev.Channel), "agentClass", agent.ref(),
		"session", sess.Namespace+"/"+sess.Name,
		"reason", agent.reason, "message", agent.message)

	// A fresh requestRef per notice: the interaction contract requires one, and
	// a shared constant would let a surface treat the second stalled
	// conversation's explanation as a duplicate of the first and drop it.
	if err := agentUnavailableNotice(agent.reason).Publish(p.NATS.Publish, sess, mintRequestID()); err != nil {
		// The user is now waiting on a thread that will not move until an
		// admin acts, with nothing on screen saying so — exactly the silence
		// this exists to end. Loud, not V(1).
		logger.Info("agent-unavailable notice publish failed",
			"session", sess.Namespace+"/"+sess.Name, "err", err.Error())
	}
	// The user is told on EVERY stalled conversation; the operators are alerted
	// once per outage. Both halves matter — see agentOutageAlerts.
	if p.outages.shouldAlert(agent.ref(), agent.reason) {
		p.publishUnhealthyAgentMonitoring(ctx, ev, agent, requester)
	}
}

// liveRunner reports whether sess has a runner that will pick this message up
// regardless of the class's health.
//
// A session already Running has its pod up, and the AgentSession controller's
// class gate does not reap it — that is what keeps a mid-session credential
// revocation surgical: the agent loses the revoked tool and keeps answering.
// Telling that user "this agent isn't available" while it visibly replies would
// be false, so the surfacing is skipped for them.
//
// Every other phase either has no pod yet or needs a respawn the class gate
// blocks, so those genuinely stall.
func liveRunner(sess *spiceboxv1alpha1.AgentSession) bool {
	return sess != nil && sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseRunning
}

// publishUnhealthyAgentMonitoring tells the operators that a person is now
// blocked on the unhealthy agent.
//
// The AgentClass going invalid already emits its own transition event from the
// operator's monitoring watcher, so this is deliberately not a duplicate of
// that: the watcher can report that an agent is unhealthy, but only channelsd
// knows whether anyone is actually waiting on it. An agent nobody talks to
// being invalid is a backlog item; the same condition with a person stalled
// behind it is an interruption, and the two must be distinguishable in the
// monitoring channel.
func (p *Pipeline) publishUnhealthyAgentMonitoring(
	ctx context.Context,
	ev channelkinds.InboundEvent,
	agent unhealthyAgent,
	requester identity.CanonicalUserID,
) {
	if p.NATS == nil {
		return
	}
	mev := channelevents.MonitoringEvent{
		// Error, where the class's own transition event is a warning: this one
		// reports a person waiting, not a reconcile result.
		Level:      channelevents.MonitoringLevelError,
		Category:   "session",
		Transition: channelevents.MonitoringTransitionFailed,
		Source: channelevents.MonitoringSourceRef{
			Kind:      "AgentClass",
			Namespace: agent.namespace,
			Name:      agent.name,
		},
		Condition: spiceboxv1alpha1.AgentClassConditionValid,
		Reason:    agent.reason,
		// Identify the requester by canonical SpiceDB subject rather than the
		// channel-native id, matching every other monitoring summary: it is the
		// value an admin can actually look up. SubjectRef rather than Subject
		// because an inbound's principal is not always a human — a cron firing
		// acts as its Channel's "service:<id>", already qualified.
		Summary: fmt.Sprintf("%s messaged agent %q on channel %q and is waiting: the agent cannot start (%s). The message is parked and will be answered once it is healthy.",
			requester.SubjectRef(), agent.name, ev.Channel.Name, agent.message),
		Hint:      unhealthyAgentHint(agent),
		Timestamp: p.Now(),
	}
	if err := channelevents.PublishMonitoring(p.NATS.Publish, mev); err != nil {
		log.FromContext(ctx).Info("agent-unavailable monitoring publish failed",
			"agentClass", agent.ref(), "err", err.Error())
	}
}

// unhealthyAgentHint is the operator-facing remedy. Unlike the user-facing
// notice this one names internal objects and commands on purpose — the people
// reading a monitoring channel are the people who can run them.
//
// When the class is invalid because its bound identity is, the identity name is
// on the class spec, so the exact command is given rather than a category of
// thing to go looking for.
func unhealthyAgentHint(agent unhealthyAgent) string {
	if agent.reason == spiceboxv1alpha1.ReasonAgentIdentityInvalid && agent.identity != "" {
		return fmt.Sprintf("re-link the agent's credentials with `oap identity setup %s`; "+
			"parked sessions resume on their own once the AgentClass is Valid=True again", agent.identity)
	}
	if agent.reason == spiceboxv1alpha1.ReasonAgentClassMissing {
		// No object to watch means no self-healing: this one stays parked until
		// a human acts, however long that is.
		return "no AgentClass by that name exists in the namespace; the Channel's spec.agentClass " +
			"names one that was never created or has been deleted — parked sessions here will NOT resume on their own"
	}
	return "sessions for this agent are parked until its Valid condition clears; " +
		"the AgentClass conditions name the failing reference"
}
