//go:build e2e

// Package e2e — end-to-end tests for the interactive identityMode=ask|dynamic
// identity-choice feature (the whole stack: runner SessionStart gate →
// channelsd bridge → operator gate/backstop → re-spawn).
//
// identity_choice is a category on the generic Interaction model
// (pkg/channels/channelevents/interaction.go, pkg/channels/channelinteractions) as of the
// identity-choice/interaction-model migration: the runner's IdentityChoiceGate
// publishes an OUT interaction_request (Category=identity_choice), the
// outbound relay routes it to the fake "interaction" sub-channel sender
// (captured via ExpectIdentityChoicePrompt), the test injects the initiating
// user's decision via IdentityChoice.Choose (IN interaction_decision), the
// channelsd pipeline's category-generic HandleInteractionDecision validates
// standing (DecideRequester — fail-closed unless the decider's canonical
// identity matches the addressee) and publishes an OUT+IN interaction_applied,
// and the runner-side subscribeInteractionApplied delivers it into the gate's
// orchestrator. Slack button rendering itself is covered by Task 9's fakeslack
// tests; at this layer we assert on the request payload (recommendation Body/
// Reason, action labels) and the resulting session state.
//
// TestIdentityChoice_DeciderMustMatchRequester is the founding regression for
// this migration: the requester/decider canonicalization bug (fixed at HEAD
// d5b8fee6, "identity_choice requester must canonicalize to the decider
// subject") made every real decision unresolvable AND would have silently let
// a wrong decider through if the standing check were missing entirely — this
// test proves both the happy path (the addressee's decision resolves) and the
// fail-closed path (anyone else's decision is rejected) against the real
// stack, not a unit-level stub.
//
// No real names — alice@example.com is fictional per AGENTS.md.
package identity_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// identityChoiceOtherUser is a second, distinct verified email used to prove
// the decider must be the exact identity the prompt was addressed to — never
// a real name, per AGENTS.md.
const identityChoiceOtherUser = "mallory@example.com"

const identityChoiceUser = "alice@example.com"

// TestIdentityChoice_AskChooseAgent is the ask happy path (state-table rows 1,
// 2): a channel-attached ask session parks AwaitingIdentityChoice with the
// runner up and a plain 3-way request published (no recommendation); injecting
// "agent" resolves EffectiveIdentityMode=agent and the session runs a turn
// under the agent identity to completion.
func TestIdentityChoice_AskChooseAgent(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-identity-ask"),
		DefaultUser:    identityChoiceUser,
		DefaultTimeout: 45 * time.Second,
	})

	// The turn only runs AFTER the choice resolves to agent; script the reply.
	h.LLM.OnUserMessage("hi there").Reply(e2e.RespondToUser("ran as the agent"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ask-agent", 45*time.Second)
	h.SendUserMessage("hi there")

	// Row 1: park in AwaitingIdentityChoice with a runner up and the plain-ask
	// request published (no recommendation — no IdentityRecommender is wired
	// for this scenario, so identitygate.go's Step 4 leaves Body/Reason empty).
	choice := h.ExpectIdentityChoicePrompt()
	p := choice.Prompt()
	assert.Empty(t, p.Body, "a plain ask carries NO recommendation Body")
	assert.Empty(t, p.Reason, "a plain ask carries no recommendation reason")
	assert.Equal(t, "Run as Ask Bot", p.ActionLabel(e2e.IdentityChoiceActionAgent),
		`"Run as <agent>" uses the class displayName`)

	ns, name := h.SingleSession("AskChooseAgent")
	e2e.WaitForSessionPhase(t, h, ns, name, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice)
	assert.True(t, h.RunnerFactory().IsRunning(ns, name),
		"the runner MUST be up in AwaitingIdentityChoice (unlike AwaitingCredentials) — it drives the choice prompt")

	// Row 2: choose agent → EffectiveIdentityMode=agent, the turn runs + replies.
	choice.Choose(e2e.IdentityChoiceActionAgent)
	h.ExpectAgentReply(e2e.Contains("ran as the agent"))
	waitForEffectiveMode(t, h, ns, name, spiceboxv1alpha1.IdentityModeAgent)

	h.AssertAllRulesConsumed()
}

// TestIdentityChoice_DynamicChoosePassthrough is the dynamic → userPassthrough
// path (state-table rows 3, 4): a dynamic session's request carries the
// recommender's suggestion; choosing "Run as me" hands off, the operator parks
// AwaitingCredentials and reaps the runner; linking the credential un-parks and
// re-spawns a turn under passthrough.
func TestIdentityChoice_DynamicChoosePassthrough(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-identity-dynamic"),
		DefaultUser:    identityChoiceUser,
		DefaultTimeout: 90 * time.Second,
	})
	// The isolated advisory recommender: deterministic {userPassthrough, reason}.
	h.SetIdentityRecommender(identityadvisor.Fake{
		Rec: identityadvisor.Recommendation{Mode: "userPassthrough", Reason: "solo fresh context"},
	})
	// MCP backing so the AgentClass converges + the (provisional-agent) first
	// boot builds the MCP tool.
	h.MCP.OnTool("echo", func(args map[string]any) any {
		return map[string]any{"echo": args["message"]}
	})
	// The turn runs only on the re-spawn (after the choice + credential link).
	h.LLM.OnUserMessage("do the thing").Reply(e2e.RespondToUser("ran as the user")).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid("dyn-agent", 60*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// PutToken (below) writes the master Secret into the identities namespace,
	// which envtest doesn't auto-create.
	createIdentityChoiceIdentitiesNamespace(t, ctx, h.K8s)

	h.SendUserMessage("do the thing")

	// Row 3 (request side): a dynamic request carries the recommendation. The
	// generic Interaction payload has no explicit Mode field — the wire-level
	// signal is a non-empty Body (WithRecommendation), naming the recommended
	// (trusted) action label; the advisory Reason routes through Excerpt
	// (untrusted) per identitygate.go's Step 4.
	choice := h.ExpectIdentityChoicePrompt(e2e.WithRecommendation())
	p := choice.Prompt()
	assert.Contains(t, p.Body, "Run as me",
		"Body names the recommender's suggested (trusted) label")
	assert.Equal(t, "solo fresh context", p.Reason,
		"the request carries the recommender's reason verbatim")

	ns, name := h.SingleSession("DynamicChoosePassthrough")
	e2e.WaitForSessionPhase(t, h, ns, name, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice)

	// Row 3 (decision) → handoff. Operator parks AwaitingCredentials + reaps the
	// runner. EffectiveIdentityMode projects to userPassthrough from the folded
	// IdentityChoiceResolved event.
	choice.Choose(e2e.IdentityChoiceActionUserPassthrough)
	e2e.WaitForSessionPhase(t, h, ns, name, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials)
	waitForEffectiveMode(t, h, ns, name, spiceboxv1alpha1.IdentityModeUserPassthrough)
	waitForRunnerStopped(t, h, ns, name)

	// Link the credential the MCP server requires (dyn-mcp-bearer) for the
	// initiating user. PutToken writes the master Secret + UserIdentity exactly
	// as identityd's /link/submit does; the operator's UserIdentity watch then
	// un-parks the session.
	starterCanon, err := identity.EmailReference(identityChoiceUser).Canonical()
	require.NoError(t, err, "canonicalize starter email")
	require.NoError(t, useridentity.PutToken(ctx, h.K8s, useridentity.PutTokenRequest{
		Subject:        starterCanon.Subject(),
		CredentialName: "dyn-mcp-bearer",
		Token:          "pat-e2e-identity-choice-passthrough-9f8e7d6c",
	}), "link the passthrough credential")

	// Row 4: un-park → re-spawn → a turn runs under passthrough. The reply proves
	// the re-spawned runner (EffectiveIdentityMode=userPassthrough, so it skips
	// the gate) resolved credentials from the linked UserIdentity and completed.
	h.ExpectAgentReply(e2e.Contains("ran as the user"))
	waitForCredentialsReady(t, h, ns, name)
}

// TestIdentityChoice_Cancel is state-table row 6: cancelling the choice fails
// the session with reason IdentityChoiceCancelled and no turn ever runs (no LLM
// rules are registered, so a turn attempt would fatal in ScriptedLLM).
func TestIdentityChoice_Cancel(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-identity-ask"),
		DefaultUser:    identityChoiceUser,
		DefaultTimeout: 45 * time.Second,
	})
	h.WaitForAgentClassValid("ask-agent", 45*time.Second)
	h.SendUserMessage("hi there")

	choice := h.ExpectIdentityChoicePrompt()
	ns, name := h.SingleSession("Cancel")
	e2e.WaitForSessionPhase(t, h, ns, name, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice)

	choice.Choose(e2e.IdentityChoiceActionCancel)
	waitForSessionFailed(t, h, ns, name, spiceboxv1alpha1.ReasonIdentityChoiceCancelled)
}

// TestIdentityChoice_DeciderMustMatchRequester is the founding regression for
// the identity_choice/interaction-model migration (see the package doc
// comment). It proves the FULL stack round-trip end to end:
//
//  1. The session-creation path (SendUserMessage → the real channelsd
//     pipeline's inbound handler → backfill.go's inheritedAnnotations) stamps
//     AnnotationStartedByEmail with the initiating user's verified email —
//     the SAME code path production Slack/webui sessions take.
//  2. The runner's IdentityChoiceGate requester() reads that annotation back
//     (LastInboundExternalID is never assigned by internal/cmd/runner today — see
//     identitygate.go's requester() doc comment) and stamps it onto the
//     interaction_request's Audience.Requester.
//  3. channelsd's HandleInteractionDecision canonicalizes the decider's email
//     against that cached Requester (DecideRequester, WITHOUT AllowSynthetic)
//     and REJECTS fail-closed — silently, no Applied published, the session
//     stays parked — when a different verified email tries to decide.
//  4. The SAME request, decided by the addressee (matching email), resolves
//     normally and the turn completes.
//
// A regression in requester()'s email-stamping (the bug fixed at d5b8fee6)
// would make step 4 hang until DefaultTimeout and fatal loudly; a regression
// that dropped the standing check entirely would let the wrong-user Choose in
// step 3 wrongly resolve the request (as "cancel" — a DIFFERENT action than
// step 4's "agent" — chosen precisely so a wrongly-accepted decision is
// distinguishable from the correct one), failing the session before step 4
// ever runs.
func TestIdentityChoice_DeciderMustMatchRequester(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-identity-ask"),
		DefaultUser:    identityChoiceUser,
		DefaultTimeout: 45 * time.Second,
	})
	h.LLM.OnUserMessage("hi there").Reply(e2e.RespondToUser("ran as the agent"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ask-agent", 45*time.Second)
	h.SendUserMessage("hi there") // starter = identityChoiceUser (alice@example.com)

	choice := h.ExpectIdentityChoicePrompt()
	ns, name := h.SingleSession("DeciderMustMatchRequester")
	e2e.WaitForSessionPhase(t, h, ns, name, spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice)

	// A verified identity that is NOT the addressee tries to decide — chosen as
	// "cancel" (a different action than the real requester will choose below)
	// so a wrongly-accepted decision is unambiguously distinguishable from the
	// correct one. HandleInteractionDecision's DecideRequester standing check
	// must reject it fail-closed: no Applied is published, the session stays
	// parked, and the runner keeps waiting.
	choice.Choose(e2e.IdentityChoiceActionCancel, e2e.AsUser(identityChoiceOtherUser))
	assertIdentityChoiceStaysParked(t, h, ns, name)

	// The real requester's decision resolves the SAME still-pending request
	// normally: EffectiveIdentityMode=agent and the turn completes.
	choice.Choose(e2e.IdentityChoiceActionAgent)
	h.ExpectAgentReply(e2e.Contains("ran as the agent"))
	waitForEffectiveMode(t, h, ns, name, spiceboxv1alpha1.IdentityModeAgent)

	h.AssertAllRulesConsumed()
}

// TestIdentityChoice_Timeout is state-table row 7: with no decision injected,
// the choice TTL (spec.identityChoiceTimeout=5s) elapses and the session fails
// with reason IdentityChoiceTimeout. No turn ever runs.
func TestIdentityChoice_Timeout(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-identity-ask-timeout"),
		DefaultUser:    identityChoiceUser,
		DefaultTimeout: 60 * time.Second,
	})
	h.WaitForAgentClassValid("asktimeout-agent", 45*time.Second)
	h.SendUserMessage("hi there")

	// Prove the gate published (the runner is up + parked), then let it time out.
	h.ExpectIdentityChoicePrompt()
	ns, name := h.SingleSession("Timeout")

	// No Choose — the 5s TTL fires (runner gate + operator backstop both write
	// IdentityChoiceTimeout).
	waitForSessionFailed(t, h, ns, name, spiceboxv1alpha1.ReasonIdentityChoiceTimeout)
}

// TestIdentityChoice_NonInteractive is the fail-closed guard: a kubectl-style
// ask session with NO input channel has no way to ask a human, so the runner
// gate refuses to run rather than silently defaulting to the agent identity. It
// fails with IdentityChoiceFailed and never runs a turn.
func TestIdentityChoice_NonInteractive(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-identity-ask"),
		DefaultUser:    identityChoiceUser,
		DefaultTimeout: 45 * time.Second,
	})
	h.WaitForAgentClassValid("ask-agent", 45*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// A channel-less (kubectl-style) session: no spec.inputChannel. The operator
	// spawns the runner, but with no channel to ask on the factory leaves
	// IdentityChoicePublish nil, so the SessionStart gate fails closed rather
	// than silently defaulting to the agent identity.
	const sessName = "ask-kubectl-session"
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessName, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "ask-agent",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "hi there"},
		},
	}
	require.NoError(t, h.K8s.Create(ctx, sess), "create channel-less AgentSession")

	// Fails closed with IdentityChoiceFailed and never runs a turn: no LLM rules
	// are registered, so any turn attempt would fatal in ScriptedLLM, and the
	// terminal reason (IdentityChoiceFailed, not Succeeded/ScopeReviewFailed)
	// confirms the gate refused before the turn. The failure message is
	// "session start halted: identity_choice_unavailable".
	waitForSessionFailed(t, h, "default", sessName, spiceboxv1alpha1.ReasonIdentityChoiceFailed)
}

// ---- poll helpers -------------------------------------------------------
//
// HARNESS NOTE — where these transitions' reconcile triggers come from:
//
// The operator projects a session's phase (and folds runner-emitted lifecycle
// events like IdentityChoicePending / IdentityChoiceResolved) by re-reading the
// signed lifecycle log, so a runner-appended event reaches status only once
// something makes the operator reconcile again. In production the runner POD is
// that something: it carries an AgentSession ownerReference (podspec.go), so
// every kubelet status write is an Owns(&corev1.Pod{}) event — including the
// Succeeded a userPassthrough handoff produces when the runner returns nil
// "for operator re-drive" (loop.go) without writing CR status.
//
// The in-process harness has no kubelet, so it reproduces that trigger rather
// than faking one: the placeholder runner Pod carries the SAME ownerReference
// the real one does, and the runner goroutine's exit is written onto it as
// Phase=Succeeded (inprocess_runner_factory.go — ensureRunnerPodPresent +
// markRunnerPodExited). These helpers therefore only poll.
//
// They used to poke the AgentSession every second to force a reconcile, and
// exactly one assertion in this file depended on it: the placeholder Pod had no
// ownerReference, so nothing it did ever enqueued the session, and the
// handoff's AwaitingCredentials park was unreachable without the poke
// (measured: 90s timeout, against ~150ms once the ownerReference is there).
// Every other transition asserted here already landed on its own — in
// milliseconds, or, for the operator's choice-deadline backstop, at the
// configured TTL. The poke was masking one missing harness trigger and paying
// a reconcile per second everywhere else for the privilege.

// waitForSessionFailed asserts the AUTHORITATIVE fail-closed signal: the
// runner-written Failed=True condition + status.FailureReason == reason. (Phase
// is the operator's fold-projected view and can lag; the Failed condition +
// FailureReason are what the runner sets directly, and are the contract for
// "fails closed with a clear reason".)
func waitForSessionFailed(t *testing.T, h *e2e.Harness, ns, name, reason string) {
	t.Helper()
	e2e.PollSession(t, h, ns, name, "Failed=True/"+reason, func(s *spiceboxv1alpha1.AgentSession) bool {
		failed := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed)
		return failed != nil && failed.Status == metav1.ConditionTrue && s.Status.FailureReason == reason
	})
}

// assertIdentityChoiceStaysParked asserts a rejected (non-addressee) decision
// left the session untouched: still AwaitingIdentityChoice, not Failed, and
// the runner still up waiting on the real requester. Polls for a bounded
// window (rather than a single point-in-time check) so a decision that IS
// wrongly accepted — which resolves near-instantly over the harness's
// in-process NATS, no real network latency — is reliably caught rather than
// raced. No nudge: unlike the positive-state pollers above, there is no
// runner-appended lifecycle event to fold here (a rejected decision writes
// nothing), so an operator reconcile wouldn't change what this observes.
func assertIdentityChoiceStaysParked(t *testing.T, h *e2e.Harness, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var sess spiceboxv1alpha1.AgentSession
		require.NoError(t, h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &sess),
			"get session while asserting it stays parked")
		if failed := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionFailed); failed != nil && failed.Status == metav1.ConditionTrue {
			t.Fatalf("session failed (reason=%s) after a decision from a non-requester identity — "+
				"the fail-closed DecideRequester standing check did not reject it", sess.Status.FailureReason)
		}
		if sess.Status.Phase != spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice {
			t.Fatalf("session left AwaitingIdentityChoice (phase=%s) after a decision from a non-requester identity — "+
				"the fail-closed DecideRequester standing check did not reject it", sess.Status.Phase)
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.True(t, h.RunnerFactory().IsRunning(ns, name),
		"the runner must still be up, waiting on the real requester")
}

func waitForEffectiveMode(t *testing.T, h *e2e.Harness, ns, name, mode string) {
	t.Helper()
	e2e.PollSession(t, h, ns, name, "EffectiveIdentityMode="+mode, func(s *spiceboxv1alpha1.AgentSession) bool {
		return s.Status.EffectiveIdentityMode == mode
	})
}

func waitForRunnerStopped(t *testing.T, h *e2e.Harness, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(h.DefaultTimeout())
	for time.Now().Before(deadline) {
		if !h.RunnerFactory().IsRunning(ns, name) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("waitForRunnerStopped: runner for %s/%s still running after %s (AwaitingCredentials must have no runner)\n%s",
		ns, name, h.DefaultTimeout(), h.DumpState())
}

func waitForCredentialsReady(t *testing.T, h *e2e.Harness, ns, name string) {
	t.Helper()
	e2e.PollSession(t, h, ns, name, "CredentialsReady=True", func(s *spiceboxv1alpha1.AgentSession) bool {
		c := meta.FindStatusCondition(s.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionCredentialsReady)
		return c != nil && c.Status == metav1.ConditionTrue
	})
}

func createIdentityChoiceIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s namespace: %v", spiceboxv1alpha1.IdentitiesNamespace, err)
	}
}
