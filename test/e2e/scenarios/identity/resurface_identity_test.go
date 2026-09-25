//go:build e2e

// Package e2e — end-to-end coverage for the AwaitingIdentityChoice re-surface
// leg (Task 10). A session parked at the runner's SessionStart identity gate
// gets its identity-choice prompt RE-POSTED to the device a re-interacting user
// is on, driven by pipeline.resurfacePending reading the shared
// resurface.Registry the outbound relay populated (harness Part A wiring).
//
// The tool-approval (AwaitingDecision) re-surface leg is deliberately NOT
// covered here: that phase is not projected in the pod-less in-process harness.
// It is covered by the unit TestResurfacePendingMatrix + the Slack driver unit
// test.
//
// No real names — alice@example.com (identityChoiceUser) is fictional per
// AGENTS.md.
package identity_test

import (
	"encoding/json"
	"github.com/authzed/openagentprimitives/test/e2e"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestResurfaceIdentityChoicePromptOnReinteraction drives an ask session to
// AwaitingIdentityChoice, then delivers a SECOND inbound from the same user
// while still parked and asserts the identity-choice prompt is re-published on
// the OUT subject. The re-surfaced envelope MUST NOT carry a
// ResurfaceInterruptRequestID: identity choice is a hard, non-interruptible
// gate (unlike a mid-turn tool approval), so no "Interrupt & Send Now" button
// is stamped.
//
// identity_choice is a category on the generic Interaction model — the
// request/resurface envelope kind is KindInteractionRequest (Category ==
// identity_choice), not a bespoke identity_choice_request kind; see
// pkg/channels/channelsd/pipeline/resurface.go's "generic interaction leg", which
// republishes the PendingPrompts-cached KindInteractionRequest verbatim for
// any category parked at the session's current phase (identity_choice's
// Resurface policy is ResurfaceCached — pkg/channels/channelinteractions/categories).
func TestResurfaceIdentityChoicePromptOnReinteraction(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-identity-ask"),
		DefaultUser:    identityChoiceUser,
		DefaultTimeout: 45 * time.Second,
	})

	// No Choose is ever injected and no LLM rules are registered: the session
	// stays parked in AwaitingIdentityChoice for the whole test (a turn would
	// only run after the choice resolves, which never happens here).
	h.WaitForAgentClassValid("ask-agent", 45*time.Second)
	h.SendUserMessage("hi there")

	// First inbound → the runner's IdentityChoiceGate publishes an
	// interaction_request (OUT, Category=identity_choice); the outbound relay
	// routes it to the fake "interaction" sub-channel sender AND Notes it into
	// the shared resurface.Registry (Part A wiring). Block until it is
	// captured so we know the session exists.
	h.ExpectIdentityChoicePrompt()

	ns, name := h.SingleSession("ResurfaceIdentityChoice")
	// The pipeline reads active.Status.Phase in resurfacePending; wait until the
	// operator has fold-projected AwaitingIdentityChoice so the second inbound
	// actually takes the identity leg.
	e2e.WaitForSessionPhase(t, h, ns, name, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice)

	// Subscribe to the OUT interaction_request subject BEFORE the second
	// inbound. NATS core does not replay, so the first request (already
	// published) is not queued — the next message is the re-surfaced republish
	// that the second inbound triggers.
	subj := channelevents.SubjectOut(
		channelevents.SubjectPrefix(ns, name), channelevents.KindInteractionRequest)
	sub, err := h.NATS().SubscribeSync(subj)
	require.NoError(t, err, "subscribe OUT interaction_request")
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// Second inbound from the same user (same default thread → same session)
	// while still AwaitingIdentityChoice: resurfacePending Gets the cached
	// prompt and republishes it verbatim.
	h.SendUserMessage("are you still there")

	msg, err := sub.NextMsg(h.DefaultTimeout())
	require.NoError(t, err,
		"the identity_choice interaction_request must be re-surfaced (republished) on the second inbound")

	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(msg.Data, &env), "decode re-surfaced envelope")
	assert.Equal(t, channelevents.KindInteractionRequest, env.Kind,
		"the re-surfaced OUT envelope is an interaction_request")
	assert.Empty(t, env.ResurfaceInterruptRequestID,
		"identity choice is a non-interruptible gate — the re-surfaced envelope carries NO interrupt request id")

	var pl channelevents.InteractionRequestPayload
	require.NoError(t, json.Unmarshal(env.Payload, &pl), "decode re-surfaced payload")
	assert.Equal(t, categories.IdentityChoice, pl.Category,
		"the re-surfaced request is still the identity_choice category")
}
