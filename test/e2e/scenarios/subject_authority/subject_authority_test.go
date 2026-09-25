//go:build e2e

// Package subject_authority_test is the e2e proof that the NATS SUBJECT — not
// Envelope.Session — decides WHICH SESSION an inbound envelope acts on.
//
// A publisher's per-session NATS JWT permits publishing under exactly one
// "ap.session.<ns>.<own-name>.>" tree, and that tree covers ".in." as well as
// ".out.". Envelope.Session is publisher-controlled JSON. So a compromised (or
// merely buggy) session can publish, on its OWN inbound subject, an envelope
// labelled with a DIFFERENT session — and every inbound pipeline handler routes
// on env.Session by design: pkg/channels/channelsd/pipeline/interaction_decision.go
// takes `ns, name := env.Session.Namespace, env.Session.Name` and says outright
// that this "is sound only because the bus wrapper … has already cross-checked
// it". view_message.go, resurface_request.go and interaction_request.go carry
// the same sentence.
//
// The wrapper is therefore load-bearing, and this scenario is what proves it
// fires. Its home is e2e, not a unit test beside the wrapper, because the
// harness IS channelsd for every scenario in this suite: it subscribes the same
// cluster-wide "ap.session.*.*.in.<kind>" wildcards over the same pipeline. A
// gate that lives only in internal/cmd/channelsd is a gate no e2e run exercises, and a
// green e2e run is then not evidence for this class at all.
//
// permission_request is the category under test because its consequence is the
// sharpest: a forged decision that lands grants `interact` in SpiceDB and
// replays a stranger's stashed message into someone else's session.
//
// No real names: alice / guest @example.com per AGENTS.md.
package subject_authority_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// Shares test/e2e/testdata/agent-interaction-roundtrip with the
	// interaction_roundtrips scenario: its authz.session.interactPermission
	// names a SpiceDB group nothing seeds, so any non-owner inbound routes
	// through permission_request. Each scenario package is its own test
	// binary with its own envtest + NATS, so the two never share state.
	fixtureClass = "interaction-roundtrip-agent"
	ownerEmail   = "alice@example.com"
	guestEmail   = "guest@example.com"

	// foreignSession is the session name the forged publish uses as its
	// SUBJECT — standing in for a second session whose JWT authorizes exactly
	// this subtree. It deliberately does NOT exist as an AgentSession CR: the
	// gate refuses on the subject/envelope disagreement alone, before any
	// lookup, and requiring a real second session would test the lookup
	// instead of the gate.
	foreignSession = "sess-elsewhere"

	// settleWindow is how long the forged decision is given to (not) take
	// effect. HandleInteractionDecision publishes Applied synchronously
	// inside its NATS subscription callback and the honest leg below
	// routinely completes in well under a second, so a drop that has not
	// surfaced within this window has not happened.
	settleWindow = 5 * time.Second
)

// TestE2E_ForgedSubjectCannotResolveAnotherSessionsApproval publishes an
// interaction_decision that is valid in every respect EXCEPT its subject —
// right category, right RequestRef, right env.Session, and a decider who
// genuinely holds approver standing — on another session's inbound subject.
//
// It must be dropped: no interaction_applied, no `interact` grant, no replay of
// the guest's stashed message. The honest Approve that follows must still work,
// which is the positive control: it proves the guard refuses the forgery rather
// than merely breaking the decision path.
func TestE2E_ForgedSubjectCannotResolveAnotherSessionsApproval(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../testdata/agent-interaction-roundtrip",
		// Mirrors interaction_roundtrips' approve leg: the guest's replayed
		// message can wait on a cold runner respawn once the owner's first
		// turn goes Idle.
		DefaultTimeout: 60 * time.Second,
		DefaultUser:    ownerEmail,
	})
	h.WaitForAgentClassValid(fixtureClass, 30*time.Second)

	h.LLM.OnUserMessage("hi from owner").Reply(e2e.RespondToUser("hello owner"))
	h.LLM.OnUserMessage("let me in").Reply(e2e.RespondToUser("hello guest"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Turn 1: the owner starts the session, so she is the approver the
	// permission_request category's DeciderPolicy will accept.
	h.SendUserMessage("hi from owner", e2e.AsUser(ownerEmail))
	h.ExpectAgentReply(e2e.Contains("hello owner"))

	// Turn 2: the guest fails CheckInteract; channelsd stashes the message and
	// parks a permission_request instead of routing it to the agent.
	h.SendUserMessage("let me in", e2e.AsUser(guestEmail))
	join := h.ExpectSessionJoinPrompt(
		e2e.JoinFromRequester(guestEmail),
		e2e.JoinToStartedBy(ownerEmail),
	)
	requestRef := join.Prompt().RequestRef
	require.NotEmpty(t, requestRef, "the captured join prompt must carry a RequestRef")

	sess, resourceRef := singleSessionRef(t, h.K8s)
	require.NotEqual(t, foreignSession, sess.Name,
		"the forged subject must name a session other than the victim")
	guestSubject, err := identity.FromExternal("fake", "", guestEmail, guestEmail).Subject()
	require.NoError(t, err)
	ch := singleChannel(t, h.K8s)

	// THE ATTACK: the victim's own approve decision, on someone else's inbound
	// subject.
	join.DecideFromForeignSubject(foreignSession, "approve", e2e.AsUser(ownerEmail))

	assert.Never(t, func() bool {
		return interactionAppliedSeen(ch, categories.PermissionRequest, requestRef)
	}, settleWindow, 100*time.Millisecond,
		"a decision published on another session's inbound subject must never resolve this session's parked approval")

	h.AssertSpiceDB(resourceRef, "interact", guestSubject, false)

	// The honest click, on the subject the publisher is actually authorized
	// for, must still land — otherwise this test would pass just as well
	// against a harness that dropped every decision.
	join.Approve(e2e.AsUser(ownerEmail))
	h.ExpectAgentReply(e2e.Contains("hello guest"))
	h.AssertSpiceDB(resourceRef, "interact", guestSubject, true)

	applied := waitForInteractionApplied(t, ch, categories.PermissionRequest, requestRef)
	assert.Equal(t, channelevents.OutcomeApproved, applied.Payload.Outcome,
		"the honest decision should resolve the request as approved")

	h.AssertAllRulesConsumed()
}

// ----- helpers -------------------------------------------------------------

// interactionAppliedSeen reports whether the Channel's fake Driver has recorded
// an interaction_applied matching category + requestRef.
func interactionAppliedSeen(ch *spiceboxv1alpha1.Channel, category, requestRef string) bool {
	drv := fakekind.DriverFor(ch.Namespace, ch.Name)
	if drv == nil {
		return false
	}
	for _, a := range drv.InteractionApplieds() {
		if a.Payload.Category == category && a.Payload.RequestRef == requestRef {
			return true
		}
	}
	return false
}

// waitForInteractionApplied polls for the applied envelope, or fatals after
// 10s. Mirrors the identically-named helper in the interaction_roundtrips
// scenario — each scenario package is self-contained rather than sharing a
// common test-helper package.
func waitForInteractionApplied(t *testing.T, ch *spiceboxv1alpha1.Channel, category, requestRef string) fakekind.InteractionApplied {
	t.Helper()
	var applied fakekind.InteractionApplied
	require.Eventually(t, func() bool {
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv == nil {
			return false
		}
		for _, a := range drv.InteractionApplieds() {
			if a.Payload.Category == category && a.Payload.RequestRef == requestRef {
				applied = a
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond,
		"interaction_applied(%s, requestRef=%s) should be published within 10s", category, requestRef)
	return applied
}

// singleChannel returns the single Channel CR in the harness namespace.
func singleChannel(t *testing.T, c client.Client) *spiceboxv1alpha1.Channel {
	t.Helper()
	var channels spiceboxv1alpha1.ChannelList
	require.NoError(t, c.List(context.Background(), &channels), "list Channels")
	require.Len(t, channels.Items, 1, "scenario assumes one Channel CR")
	return &channels.Items[0]
}

// singleSessionRef returns the single AgentSession CR and its SpiceDB resource
// ref ("agentsession:<ns>/<name>").
func singleSessionRef(t *testing.T, c client.Client) (spiceboxv1alpha1.AgentSession, string) {
	t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions), "list AgentSessions")
	require.Len(t, sessions.Items, 1, "scenario assumes one AgentSession")
	sess := sessions.Items[0]
	return sess, "agentsession:" + sess.Namespace + "/" + sess.Name
}
