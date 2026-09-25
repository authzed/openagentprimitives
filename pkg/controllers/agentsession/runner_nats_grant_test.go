package agentsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	apnats "github.com/authzed/openagentprimitives/pkg/platform/nats"
	"github.com/authzed/openagentprimitives/pkg/platform/nats/natstest"
)

const (
	grantNS   = "default"
	grantName = "messaging-bot-channel-8cf75632"
	grantPfx  = "ap.session." + grantNS + "." + grantName
)

// The per-session runner NATS grant must let the runner reach its own session
// subject tree AND subscribe to the cluster-wide revocation bus. Revocation is
// subscribe-only: a missing SubAllow entry is exactly what produced the
// production "Permissions Violation for Subscription to ap.revocation" and
// silently disabled revocation delivery, so this guards both directions.
//
// Every assertion runs against a real embedded nats-server (natstest) rather
// than against the allow-list strings. A string assertion cannot express what
// this grant is for: `assert.NotContains(g.PubAllow, "…in.interaction_decision")`
// passes trivially against a grant of `<prefix>.>`, which permits that subject.
func TestRunnerNATSUserGrant(t *testing.T) {
	g, err := runnerNATSUserGrant(grantNS, grantName)
	require.NoError(t, err, "runnerNATSUserGrant")
	h := natstest.New(t)

	// The grant's own reply inbox, settled by the server. This used to be
	// `assert.Equal("runner-"+grantNS+"-"+grantName, g.Name)`, which pinned the
	// hyphen-joined name as intent — and that join was the defect: '-' is legal
	// in both a namespace and a name, so it mapped distinct sessions onto one
	// inbox. What the name is FOR is asserted here and, for the distinctness the
	// string shape cannot express, in runner_nats_grant_injective_test.go.
	assert.True(t, h.SubscribeAllowed(t, g, apnats.InboxPrefixFor(g.Name)+".nuidnuidnuidnuidnuidnu.1"),
		"subscribe: the session's own reply inbox, or every request/reply it makes is refused")

	assert.True(t, h.SubscribeAllowed(t, g, grantPfx+".in.user_message"),
		"subscribe: own session subtree (the await_user_message wake-up)")
	assert.True(t, h.SubscribeAllowed(t, g, grantPfx+".out.interaction_applied"),
		"subscribe: own session subtree (the applied bridge that resumes an approval)")

	// Revocation bus: subscribe granted, publish denied (subscribe-only).
	assert.True(t, h.SubscribeAllowed(t, g, revocation.Subject),
		"subscribe: revocation bus must be allowed or ap.revocation subscription is denied")
	assert.False(t, h.PublishAllowed(t, g, revocation.Subject),
		"publish: runner consumes revokes, must never be able to emit them")
}

// TestRunnerNATSUserGrantPublishSurface pins the runner's publish rights to the
// subjects internal/cmd/runner and pkg/agent actually reach, and proves the rest of its
// own session subtree is refused by the server.
//
// The grant used to be `PubAllow: <prefix>.>`, the whole session subtree. That
// includes `<prefix>.in.interaction_decision`, whose channelsd handler takes
// the acting principal from `pl.Decider` — an unauthenticated payload field
// (pkg/channels/channelsd/pipeline/interaction_decision.go). The runner knows the
// owner's external identity, so it could publish a decision naming the owner as
// Decider and approve its own request.
//
// What that is worth, precisely: every runner pod already carries the
// operator's unscoped SpiceDB preshared token and a raw write client, so
// forging a decision buys the runner no authorization it does not already have
// today. This is defense in depth that becomes load-bearing when that token
// problem is closed. It is not, today, a live approval bypass.
//
// The IN list is deliberately three explicit leaves rather than a wildcard:
// those are the only inbound subjects the runner publishes, and the whole point
// is that a fourth one — interaction_decision — must be server-denied.
func TestRunnerNATSUserGrantPublishSurface(t *testing.T) {
	g, err := runnerNATSUserGrant(grantNS, grantName)
	require.NoError(t, err, "runnerNATSUserGrant")
	h := natstest.New(t)

	permitted := map[string]string{
		grantPfx + ".out.user_message": "meta/respond.go respond_to_user",
		grantPfx + ".out.notification": "Loop.Notify / update_status",
		// Not covered by `out.*`: the kind itself contains dots, so the
		// subject is out.assistant.stream.delta — four tokens after the
		// prefix. This is why the grant says out.> and not out.*.
		grantPfx + ".out.assistant.stream.delta": "buildLoopOnStreamEvent per-token stream delta",
		grantPfx + ".out.interaction_request":    "identity-choice gate + notice.Publish",
		grantPfx + ".out.interaction_applied":    "identity-choice applied + timeout applied (OUT leg)",
		grantPfx + ".out.interrupt_applied":      "subscribeInterruptRequest outcome",
		grantPfx + ".out.tool_session_delta":     "sandbox InteractiveHooks.OnOutput",
		grantPfx + ".history.request":            "read_thread_history request/reply",
		grantPfx + ".channel_history.request":    "read_channel_history request/reply",
		grantPfx + ".in.metaagent_request":       "ColdStartRequestPublish",
		grantPfx + ".in.interaction_request":     "InteractionRequestPublish (tool approval, leakage, content inspection)",
		grantPfx + ".in.interaction_applied":     "TimeoutAppliedPublish (IN leg)",
		// reply_to_subagent, on the SENDER's own subject. Without it a
		// delegating parent cannot answer a child that asked it a question:
		// the publish takes a permissions violation, which arrives
		// asynchronously on the connection, so the reply simply never lands
		// and the child parks until the operator's parent-reply bound ends
		// the delegation. This grant is built at RUNTIME and is invisible to
		// gen:api, TestInstallYAMLMatchesKustomize and TestRBACSufficiency
		// alike, which is exactly why it is pinned here.
		grantPfx + ".in.agent_message_send": "reply_to_subagent: a delegating parent answering its child",
		// The one cluster-scoped subject in the grant. Without it a runner
		// that catches its own agent's definition failing at evaluation — the
		// only place that fault is observable — takes a NATS permissions
		// violation instead of publishing, which arrives asynchronously on the
		// connection and so presents as the report simply never appearing.
		channelevents.MonitoringEventSubject: "reportDefinitionError: an unevaluatable agent definition, to the operators who can fix it",
	}
	for subject, why := range permitted {
		assert.True(t, h.PublishAllowed(t, g, subject),
			"runner must be able to publish %s — %s", subject, why)
	}

	denied := map[string]string{
		grantPfx + ".in.interaction_decision": "the approval gate the runner is constrained BY; channelsd trusts the payload's Decider field",
		grantPfx + ".in.user_message":         "channelsd is the sole inbound writer; a self-published user turn would forge a user",
		grantPfx + ".in.view_message":         "a view surface's subject; the runner has no view",
		grantPfx + ".in.interrupt_request":    "channel-initiated; the runner is the subscriber, never the publisher",
		grantPfx + ".in.app_tool_call":        "webd-initiated; the runner is the responder",
		grantPfx + ".in.tool_session_input":   "channel-initiated tool-session input",
		grantPfx + ".in.resurface_request":    "a view surface's subject",
		// Cross-session containment: the runner's grant is per session.
		"ap.session." + grantNS + ".other-session.out.user_message": "another session's out-subject",
		"ap.session." + grantNS + ".other-session.in.user_message":  "another session's in-subject",
		// The containment the whole agent_message_send direction exists to
		// respect. A session that wants to reach another one publishes on its
		// OWN subject and names the destination in the payload, precisely so
		// this stays denied: a runner able to publish onto another session's
		// inbound subtree could inject inbound traffic — including a forged
		// agent_message claiming any sender — into any session in the cluster.
		"ap.session." + grantNS + ".other-session.in.agent_message":      "another session's inbound: a forged message from a claimed sender",
		"ap.session." + grantNS + ".other-session.in.agent_message_send": "another session's send subject: publishing AS that session",
	}
	for subject, why := range denied {
		assert.False(t, h.PublishAllowed(t, g, subject),
			"runner must NOT be able to publish %s — %s", subject, why)
	}
}
