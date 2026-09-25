//go:build e2e

// Package interaction_roundtrips_test is Task 13's NEW e2e coverage for the
// generic Interaction model's wire contract (pkg/channels/channelevents/interaction.go,
// pkg/channels/channelinteractions) on two categories that already have HEAVIER,
// narrative e2e scenarios elsewhere:
//
//   - passthrough_portal exercises portal_access through a full identityd
//     web flow (mint link → GET /my/accounts → link a credential → OOB
//     confirmation).
//   - sessioninteract_grant exercises permission_request through a full
//     multi-turn conversation, cold-runner respawn, and per-turn-authorship
//     dive.
//
// These tests are deliberately lighter: they assert directly on the
// interaction_request / interaction_decision / interaction_applied envelope
// shapes and the resulting SpiceDB state, using a minimal fixture with no
// MCP tools and no identityd server, so the wire contract itself — not the
// surrounding subsystems — is what's under test.
//
//   - TestE2E_PortalAccess_PublishesLinkInteraction proves the portal
//     trigger phrase publishes exactly the interaction_request(portal_access)
//     shape the identity-portal React app depends on: one ActionKindLink
//     action, addressed to the requester (AudienceRequester).
//
//   - TestE2E_PermissionRequest_ApproveGrantsInteract / _DenyBlocksInteract
//     prove the session-join round trip at the wire level: a non-owner's
//     message fails CheckInteract and produces an
//     interaction_request(permission_request); the owner's decision
//     publishes interaction_decision on `.in.interaction_decision`
//     (mirroring IdentityChoice.Choose — see test/e2e/identity_choice.go);
//     decidePermission (bound in
//     pkg/channels/channelsd/pipeline/permission_interaction.go) grants/denies
//     `interact` in SpiceDB and publishes interaction_applied with the
//     matching Outcome.
//
// Both share the interaction-roundtrip-agent fixture
// (test/e2e/testdata/agent-interaction-roundtrip): its
// authz.session.interactPermission targets a SpiceDB group no test seeds a
// tuple for, so any non-owner inbound routes through permission_request —
// the same technique
// test/e2e/scenarios/centerdot/sessioninteract_grant's agentClassOverride
// uses, scoped to this fixture's own group name so the two can never
// collide.
//
// No real names: alice / guest @example.com per AGENTS.md.
package interaction_roundtrips_test

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
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	fixtureClass = "interaction-roundtrip-agent"
	ownerEmail   = "alice@example.com"
	guestEmail   = "guest@example.com"

	// signingKey only needs to be a well-formed HMAC key — TestE2E_PortalAccess
	// never verifies the minted link (no identityd server in this scenario),
	// so its content is arbitrary. 32 bytes to mirror the production/
	// passthrough_portal convention.
	signingKey = "interaction-roundtrip-test-key3" // 32 bytes
)

// TestE2E_PortalAccess_PublishesLinkInteraction proves the portal trigger
// phrase ("manage my accounts") is consumed by channelsd's
// PortalAccessTriggerer and produces an interaction_request envelope with
// Category=portal_access, exactly one ActionKindLink action carrying a
// non-empty URL, and Audience.Scope=AudienceRequester addressed to the
// starter. This is the wire contract the identity-portal React app's
// "Manage your linked accounts" button depends on — passthrough_portal
// additionally proves the LinkURL round-trips through a real identityd
// server; this test isolates the publish step alone.
func TestE2E_PortalAccess_PublishesLinkInteraction(t *testing.T) {
	var triggerer *pipeline.PortalAccessTriggerer
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "../../testdata/agent-interaction-roundtrip",
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    ownerEmail,
		// PipelineExtender wires the PortalAccessTriggerer onto the harness's
		// in-process channelsd pipeline — the default harness pipeline leaves
		// PortalAccess nil (see passthrough_portal_test.go for the same
		// wiring). NATS: pl.NATS is REQUIRED: TryHandle publishes the
		// interaction_request via NATS.Publish (Task 6's flip off a direct
		// sub-channel send), and a nil NATS makes it silently consume the
		// trigger with no publish.
		PipelineExtender: func(pl *pipeline.Pipeline, _ e2e.ExtenderView) {
			triggerer = &pipeline.PortalAccessTriggerer{
				LinkSigner:      passthroughlink.New([]byte(signingKey)),
				ExternalBaseURL: func() string { return "https://identityd.invalid" },
				NATS:            pl.NATS,
			}
			pl.PortalAccess = triggerer
		},
	})
	require.NotNil(t, triggerer, "PipelineExtender must have run during e2e.Start")

	h.WaitForAgentClassValid(fixtureClass, 30*time.Second)

	h.LLM.OnUserMessage("hi").Reply(e2e.RespondToUser("hi there"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Prime the session — the triggerer only fires on inbound routed to an
	// existing active session (Pending/Running/Idle).
	h.SendUserMessage("hi")
	h.ExpectAgentReply(e2e.Contains("hi there"))

	// The portal trigger phrase is consumed by the triggerer and never
	// reaches the agent — no LLM rule is registered for it, and
	// AssertAllRulesConsumed below would fail if it leaked through.
	h.SendUserMessage("manage my accounts")

	ch := singleChannel(t, h.K8s)
	var prompt fakekind.InteractionPrompt
	require.Eventually(t, func() bool {
		drv := fakekind.DriverFor(ch.Namespace, ch.Name)
		if drv == nil {
			return false
		}
		for _, p := range drv.InteractionPrompts() {
			if p.Payload.Category == categories.PortalAccess {
				prompt = p
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond,
		"interaction_request(portal_access) should be published within 10s")

	require.Len(t, prompt.Payload.Actions, 1,
		"portal_access request should carry exactly one action")
	action := prompt.Payload.Actions[0]
	assert.Equal(t, channelevents.ActionKindLink, action.Kind,
		"the portal_access action should be a link action")
	assert.NotEmpty(t, action.URL, "the link action must carry a non-empty URL")
	assert.Equal(t, channelevents.AudienceRequester, prompt.Payload.Audience.Scope,
		"portal_access addresses the requester directly, not an approver set")
	require.NotNil(t, prompt.Payload.Audience.Requester,
		"portal_access request should carry the addressed requester's identity")
	assert.NotEmpty(t, prompt.Payload.Audience.Requester.ExternalID,
		"the addressed requester's canonical subject must be set")

	h.AssertAllRulesConsumed()
}

// TestE2E_PermissionRequest_ApproveGrantsInteract drives the session-join
// round trip's approve leg: a guest's message fails CheckInteract and
// produces an interaction_request(permission_request); the owner approves
// via the generic interaction_decision path (SessionJoinApproval.Approve,
// mirroring a channel surface's Approve click); decidePermission grants
// `interact` in SpiceDB, publishes interaction_applied(Approved), and
// replays the guest's stashed message so the agent responds without the
// guest retyping.
func TestE2E_PermissionRequest_ApproveGrantsInteract(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: "../../testdata/agent-interaction-roundtrip",
		// Mirrors sessioninteract_grant's DefaultTimeout: the guest's
		// resubmitted message can wait on a cold runner respawn once the
		// owner's first turn goes Idle.
		DefaultTimeout: 60 * time.Second,
		DefaultUser:    ownerEmail,
	})
	h.WaitForAgentClassValid(fixtureClass, 30*time.Second)

	h.LLM.OnUserMessage("hi from owner").Reply(e2e.RespondToUser("hello owner"))
	h.LLM.OnUserMessage("let me in").Reply(e2e.RespondToUser("hello guest"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	// Turn 1: the owner starts the session. As started_by she passes
	// CheckInteract on her own session trivially.
	h.SendUserMessage("hi from owner", e2e.AsUser(ownerEmail))
	h.ExpectAgentReply(e2e.Contains("hello owner"))

	// Turn 2: a guest (not started_by, not in the fixture's interactPermission
	// group) tries to join. CheckInteract fails, so channelsd stashes the
	// message on PendingRequesters and publishes
	// interaction_request(permission_request) instead of routing to the agent.
	h.SendUserMessage("let me in", e2e.AsUser(guestEmail))
	join := h.ExpectSessionJoinPrompt(
		e2e.JoinFromRequester(guestEmail),
		e2e.JoinToStartedBy(ownerEmail),
	)
	requestRef := join.Prompt().RequestRef
	require.NotEmpty(t, requestRef, "the captured join prompt must carry a RequestRef")

	// The owner approves. Publishes interaction_decision on
	// `.in.interaction_decision`; HandleInteractionDecision validates the
	// owner's standing, then decidePermission grants interact, clears the
	// PendingRequester, and replays the guest's stashed message.
	join.Approve(e2e.AsUser(ownerEmail))

	// The replay produces "hello guest" — proves the resubmit-on-approve
	// step (pkg/channels/channelsd/pipeline/permission_interaction.go) actually fired,
	// not just that SpiceDB was granted.
	h.ExpectAgentReply(e2e.Contains("hello guest"))

	_, resourceRef := singleSessionRef(t, h.K8s)
	guestSubjectTyped, err := identity.FromExternal("fake", "", guestEmail, guestEmail).Subject()
	require.NoError(t, err)
	guestSubject := guestSubjectTyped
	h.AssertSpiceDB(resourceRef, "interact", guestSubject, true)

	ch := singleChannel(t, h.K8s)
	applied := waitForInteractionApplied(t, ch, categories.PermissionRequest, requestRef)
	assert.Equal(t, channelevents.OutcomeApproved, applied.Payload.Outcome,
		"interaction_applied(permission_request) outcome should be approved")
	assert.Equal(t, requestRef, applied.Payload.RequestRef)

	h.AssertAllRulesConsumed()
}

// TestE2E_PermissionRequest_DenyBlocksInteract drives the deny leg: the
// owner denies the guest's join. decidePermission must write the SpiceDB
// denied relation (NOT grant interact), publish
// interaction_applied(Denied), and must NOT replay the guest's stashed
// message — no LLM rule is registered for "let me in", so a leaked replay
// would fatal the scripted LLM.
func TestE2E_PermissionRequest_DenyBlocksInteract(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "../../testdata/agent-interaction-roundtrip",
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    ownerEmail,
	})
	h.WaitForAgentClassValid(fixtureClass, 30*time.Second)

	h.LLM.OnUserMessage("hi from owner").Reply(e2e.RespondToUser("hello owner"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.SendUserMessage("hi from owner", e2e.AsUser(ownerEmail))
	h.ExpectAgentReply(e2e.Contains("hello owner"))

	h.SendUserMessage("let me in", e2e.AsUser(guestEmail))
	join := h.ExpectSessionJoinPrompt(
		e2e.JoinFromRequester(guestEmail),
		e2e.JoinToStartedBy(ownerEmail),
	)
	requestRef := join.Prompt().RequestRef
	require.NotEmpty(t, requestRef, "the captured join prompt must carry a RequestRef")

	join.Deny(e2e.AsUser(ownerEmail))

	_, resourceRef := singleSessionRef(t, h.K8s)
	guestSubjectTyped, err := identity.FromExternal("fake", "", guestEmail, guestEmail).Subject()
	require.NoError(t, err)
	guestSubject := guestSubjectTyped

	ch := singleChannel(t, h.K8s)
	applied := waitForInteractionApplied(t, ch, categories.PermissionRequest, requestRef)
	assert.Equal(t, channelevents.OutcomeDenied, applied.Payload.Outcome,
		"interaction_applied(permission_request) outcome should be denied")

	h.AssertSpiceDB(resourceRef, "interact", guestSubject, false)
	h.AssertSpiceDB(resourceRef, "is_denied", guestSubject, true)

	// Only the owner's greeting rule may have fired; the guest's message
	// must never have been replayed (no rule was registered for it).
	h.AssertAllRulesConsumed()
}

// ----- helpers -------------------------------------------------------------

// singleChannel returns the single Channel CR in the harness namespace.
// Mirrors the identically-named helper duplicated across other e2e
// scenarios (e.g. passthrough_portal_test.go) — each scenario package is
// self-contained rather than sharing a common test-helper package.
func singleChannel(t *testing.T, c client.Client) *spiceboxv1alpha1.Channel {
	t.Helper()
	var channels spiceboxv1alpha1.ChannelList
	require.NoError(t, c.List(context.Background(), &channels), "list Channels")
	require.Len(t, channels.Items, 1, "scenario assumes one Channel CR")
	return &channels.Items[0]
}

// singleSessionRef returns the single AgentSession CR's (namespace, name)
// and its SpiceDB resource ref ("agentsession:<ns>/<name>").
func singleSessionRef(t *testing.T, c client.Client) (spiceboxv1alpha1.AgentSession, string) {
	t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &sessions), "list AgentSessions")
	require.Len(t, sessions.Items, 1, "scenario assumes one AgentSession")
	sess := sessions.Items[0]
	return sess, "agentsession:" + sess.Namespace + "/" + sess.Name
}

// waitForInteractionApplied polls the Channel's fake Driver for an
// interaction_applied envelope matching category + requestRef, or fatals
// after 10s. HandleInteractionDecision publishes Applied synchronously
// within its NATS subscription callback, so this is normally a fast poll.
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
