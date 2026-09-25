//go:build e2e

// collapse_test.go — TWO agents, ONE dead shared bot credential, ONE admin
// action, both turns resume.
//
// The sibling scenarios in this package drive ONE session through the
// agent-owned credential-update arc. This one is about what happens when the
// SAME dead credential breaks several agents at once, which is the ordinary
// case for a shared bot token: before collapsing, five parked sessions meant
// five credential-entry cards at the same admin, five links, and five chances
// to paste into the wrong one.
//
// # What only this tier can show
//
// The integration test in pkg/controllers/credentialupdaterequest drives the
// same collapse over one apiserver and one SpiceDB, but with no runner: it can
// prove one card was published and both requests settled, and it cannot prove
// that the two BLOCKED AGENTS were released. That is the failure this slice
// most needs closed — a follower that never unblocks reports a timeout to its
// user while a human really did replace the credential — and it is only
// observable where there are real turns to resume. Here both agents call the
// real blocking meta tool, and both turns have to finish.
//
// # The negative and its positive control
//
// "Only one admin was asked" is worthless if the second agent never asked for
// anything. So the single-broadcast window below runs only AFTER the test has
// confirmed that BOTH requests exist, that they resolved to the SAME credential,
// and that the second is Collapsed onto the first — the machinery demonstrably
// ran and stopped exactly where it was supposed to.
package credential_update_agent_owned_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// resumedReply is the scripted text both agents send once their retry
// succeeds. Both sessions run the SAME script — the ScriptedLLM matches on
// message content, and the two conversations are identical after their first
// turn — so "both resumed" is asserted as TWO outbound messages carrying this
// text, alongside the per-session evidence (both requests Fulfilled, both
// sessions unparked, four upstream calls) that no single one of them could
// have produced alone.
const resumedReply = "listed the issues once the shared credential was replaced"

// promptA / promptB are the two humans' opening messages, one per thread. They
// must differ: see the script's own note on why an opening rule is retired by
// consumption rather than by ceasing to match.
const (
	promptA = "list the upstream issues for the first team"
	promptB = "list the upstream issues for the second team"
)

// TestAgentOwnedCredentialUpdate_TwoSessionsShareOneCardAndOneAdminActionResumesBoth
// is the slice's end-to-end multiplicity proof.
func TestAgentOwnedCredentialUpdate_TwoSessionsShareOneCardAndOneAdminActionResumesBoth(t *testing.T) {
	s := startScenario(t)
	defer s.cancel()
	h, ctx := s.h, s.ctx

	redirectVerifyProbeByToken(t, map[string]int{deadValue: 401, freshValue: 200})

	// The upstream answers on the credential that is ACTUALLY configured, read
	// fresh from the cluster on every call.
	//
	// The sibling scenarios switch on a call COUNT ("the first call 401s"),
	// which is only correct for one session: with two, calls 1 and 2 are the two
	// agents' first attempts and a count-based switch would hand the second
	// agent a success it never earned — and the collapse under test would never
	// happen at all. Keying on the Secret is both deterministic and a faithful
	// model of a provider that rejects a dead token.
	var upstreamCalls atomic.Int32
	h.MCP.OnTool("list_issues", func(map[string]any) any {
		upstreamCalls.Add(1)
		if botSecretValueOrEmpty(ctx, h) != freshValue {
			return map[string]any{"error": "401 Unauthorized: bad credentials"}
		}
		return map[string]any{"issues": []any{"issue-1"}, "status": "ok"}
	})
	s.applyUpstream(t)

	grantPlatformAdmin(t, ctx, h, canonicalFor(t, adminEmail))
	linkAgentIdentityToPlatform(t, ctx, h, "default", agentIdentityName)

	operatorURL := startOperatorAdmind(t, h)
	idBaseURL := startIdentityd(t, h, s.identitydSigner, operatorURL)
	startCredentialUpdateOperator(t, ctx, h)
	monitoring := startMonitoringSink(t, ctx, h)
	startCardPublisher(t, ctx, h, s.channelsdSigner, idBaseURL)

	// One script, two sessions.
	//
	// The two opening rules are DISTINCT and NON-repeating, and both properties
	// are load bearing. matchUserText walks BACK past tool-result-only messages
	// to the human-typed text, so an opening rule matches every later turn of
	// its own conversation too -- it is consumption, not the matcher, that
	// retires it. A single `.Repeating()` opener therefore preempts every
	// tool-result rule below and the agent calls the upstream forever (observed:
	// 50 turns, budget exhausted, no request ever raised). Two distinct prompts,
	// each consumed by its own session's first turn, is the shape that works.
	h.LLM.OnUserMessage(promptA).Reply(callUpstream("first attempt for the first team"))
	h.LLM.OnUserMessage(promptB).Reply(callUpstream("first attempt for the second team"))
	// Everything after the opening IS identical between the two conversations,
	// so these rules must repeat: one match each would serve one session and
	// leave the other with no rule, which ScriptedLLM fails the test on.
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("401")).
		Reply(e2e.ToolUse("request_credential_update", map[string]any{
			"tool": mcpToolName,
			"why":  "the shared bot token came back 401 Unauthorized",
		})).Repeating()
	h.LLM.OnToolResult("request_credential_update", e2e.ResultContains("the credential was updated")).
		Reply(callUpstream("retry after the shared credential was replaced")).Repeating()
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("issue-1")).
		Reply(e2e.RespondToUser(resumedReply)).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid(className, 30*time.Second)

	// Two threads on the one Channel: the pipeline's channelKey -> sessionName
	// index is what makes these two SESSIONS rather than two turns of one.
	h.SendUserMessage(promptA, e2e.InThread("collapse-thread-a"))
	h.SendUserMessage(promptB, e2e.InThread("collapse-thread-b"))

	sessions := waitForSessions(t, ctx, h, 2)
	for i := range sessions {
		ensurePlaceholderRunnerPod(t, ctx, h, &sessions[i])
	}

	// --- POSITIVE CONTROL: both agents really asked, and about ONE credential -
	//
	// Everything below this line is a claim about ONE card and ONE action. All
	// of it passes trivially if the second agent never got as far as raising a
	// request, so this is asserted first and asserted from the live objects.
	canonical, follower := waitForCollapsedPair(t, ctx, h)
	require.NotNil(t, canonical.Status.ResolvedCredential)
	require.NotNil(t, follower.Status.ResolvedCredential,
		"the follower must have RESOLVED a credential, or 'no second card' says nothing about collapsing")
	assert.Equal(t, spiceboxv1alpha1.IdentityKindAgentIdentity, follower.Status.ResolvedCredential.IdentityKind)
	assert.Equal(t, canonical.Status.ResolvedCredential.Name, follower.Status.ResolvedCredential.Name)
	assert.Equal(t, canonical.Status.ResolvedCredential.Credential, follower.Status.ResolvedCredential.Credential,
		"the two agents must genuinely be blocked on ONE credential")
	assert.NotEqual(t, canonical.Spec.SessionRef.Name, follower.Spec.SessionRef.Name,
		"two DIFFERENT sessions -- the ask budget is charged per session, so one session asking twice "+
			"would prove nothing about collapsing across sessions")
	assert.Empty(t, follower.Status.InteractionRef,
		"a follower is never delivered a card of its own, and must never claim it was")

	// --- ONE admin asked --------------------------------------------------
	ev := monitoring.await(t)
	assert.Contains(t, ev.Summary, agentIdentityName,
		"an admin must see WHICH agent identity is broken -- that is the blast radius")
	assertNoSecondCredentialBroadcast(t, monitoring, 2*pipeline.CredentialUpdateWatcherInterval)

	// --- both sessions park -------------------------------------------------
	//
	// The follower's session parks exactly as hard as the canonical's: its agent
	// is blocked in the same meta-tool call on the same credential. Asserting it
	// for both is what makes the unpark below a real transition.
	for i := range sessions {
		waitForCredentialUpdateParked(t, ctx, h, &sessions[i])
	}
	require.Equal(t, deadValue, botSecretValue(t, ctx, h), "precondition: the shared credential is still dead")

	// --- one admin action, on the one link the one card carried -------------
	resp := submitReplacement(t, idBaseURL, s.identitydSigner, linkFromHint(t, ev.Hint), adminEmail, freshValue)
	require.Equal(t, http.StatusFound, resp.StatusCode,
		"a platform admin's paste must be accepted; body=%s", bodyOf(t, resp))
	assert.Equal(t, freshValue, botSecretValue(t, ctx, h), "the shared bot credential is replaced")

	// --- both requests settle, from the one answer --------------------------
	waitForNamedRequestPhase(t, ctx, h, canonical.Name, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)
	waitForNamedRequestPhase(t, ctx, h, follower.Name, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)
	settledFollower := namedRequest(t, ctx, h, follower.Name)
	assert.Contains(t, settledFollower.Status.Reason, "another session",
		"a settled follower must say whose ask it is answering; the canonical's own wording is written from the "+
			"point of view of a request that HAD a card")
	assert.Empty(t, settledFollower.Status.InteractionRef,
		"settling must not retroactively make the follower look delivered")

	// --- and BOTH turns resume ----------------------------------------------
	//
	// This is the claim no other tier can make. A follower that settled in the
	// API but never released its agent looks identical to this one right up to
	// here, and its user is told the request timed out.
	waitForRepliesContaining(t, h, resumedReply, 2)
	assert.EqualValues(t, 4, upstreamCalls.Load(),
		"each agent called the upstream twice: once to fail, once to retry after the ONE replacement")
	assertToolResultSeenByModel(t, h, "the credential was updated. Retry your original tool call now.")

	// Both sessions leave the park.
	for i := range sessions {
		waitForCredentialUpdateUnparked(t, ctx, h, sessions[i].Namespace, sessions[i].Name)
	}
}

// ---------------------------------------------------------------------------
// Multi-session helpers
// ---------------------------------------------------------------------------
//
// The single-session helpers in credential_update_agent_owned_test.go
// deliberately fatal on more than one session/request -- that guard is load
// bearing for those scenarios, which infer their target. These are their
// explicit-target twins rather than a relaxation of them.

// waitForSessions blocks until exactly want AgentSessions exist and returns
// them. Exactly, not at least: an extra session would mean the two inbounds did
// not land on the two threads this test intends, and the pair the rest of the
// test reasons about would be arbitrary.
func waitForSessions(t *testing.T, ctx context.Context, h *e2e.Harness, want int) []spiceboxv1alpha1.AgentSession {
	t.Helper()
	var found []spiceboxv1alpha1.AgentSession
	e2e.Eventually(t, scenarioTimeout, func() bool {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(ctx, &list); err != nil || len(list.Items) != want {
			return false
		}
		found = list.Items
		return true
	}, "two inbound messages on two threads must produce two AgentSessions")
	return found
}

// waitForCollapsedPair blocks until exactly two CredentialUpdateRequests exist,
// one Open and one Collapsed onto it, and returns them in that order.
//
// It is phase-explicit on purpose: the shape it waits for IS the claim. A
// looser wait ("two requests exist") would let the test proceed with two Open
// requests and then fail somewhere downstream on a symptom.
func waitForCollapsedPair(t *testing.T, ctx context.Context, h *e2e.Harness) (
	canonical, follower *spiceboxv1alpha1.CredentialUpdateRequest) {
	t.Helper()
	var last string
	e2e.Eventually(t, scenarioTimeout, func() bool {
		canonical, follower = nil, nil
		var list spiceboxv1alpha1.CredentialUpdateRequestList
		if err := h.K8s.List(ctx, &list); err != nil || len(list.Items) != 2 {
			return false
		}
		var phases []string
		for i := range list.Items {
			it := &list.Items[i]
			phases = append(phases, it.Name+"="+it.Status.Phase)
			switch it.Status.Phase {
			case spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen:
				canonical = it
			case spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed:
				follower = it
			}
		}
		last = strings.Join(phases, " ")
		return canonical != nil && follower != nil &&
			follower.Status.CollapsedInto != nil &&
			follower.Status.CollapsedInto.Name == canonical.Name
	}, "two agents blocked on ONE shared credential must produce ONE card and ONE follower collapsed onto it; "+
		"two Open requests here means two admins were asked about the same dead token")
	if t.Failed() {
		t.Logf("last observed request phases: %s", last)
	}
	return canonical, follower
}

// assertNoSecondCredentialBroadcast fails the moment a SECOND credential
// broadcast reaches the admin surface, and otherwise keeps watching for d.
//
// A negative like this cannot be a single read: the publish it must catch is
// driven by channelsd's own ticker, so a window shorter than its interval could
// miss the pass entirely -- and would keep reporting "one admin asked" after
// collapsing was reverted, which is the one thing it must never do.
func assertNoSecondCredentialBroadcast(t *testing.T, sink *monitoringSink, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev := <-sink.events:
			if ev.Category == "credential" {
				t.Fatalf("a SECOND admin was asked to replace the SAME dead shared credential: %+v\n"+
					"one dead bot token breaking N agents must cost a human ONE card, not N", ev)
			}
		case <-deadline:
			return
		}
	}
}

// botSecretValueOrEmpty reads the shared bot credential without any testify
// call.
//
// botSecretValue is unusable from the MCP stub's handler: it runs on the stub
// server's goroutine, and require.NoError there calls t.FailNow off the test
// goroutine, which is undefined behaviour. An unreadable Secret returns "" and
// is treated as "not yet replaced", which is the fail-closed direction -- the
// upstream keeps rejecting rather than handing an agent a success the cluster
// never authorized.
func botSecretValueOrEmpty(ctx context.Context, h *e2e.Harness) string {
	var sec corev1.Secret
	if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: botSecretName}, &sec); err != nil {
		return ""
	}
	return string(sec.Data[credential])
}

func namedRequest(t *testing.T, ctx context.Context, h *e2e.Harness, name string) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	var got spiceboxv1alpha1.CredentialUpdateRequest
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &got),
		"get CredentialUpdateRequest %s", name)
	return &got
}

func waitForNamedRequestPhase(t *testing.T, ctx context.Context, h *e2e.Harness, name, want string) {
	t.Helper()
	var last string
	e2e.Eventually(t, scenarioTimeout, func() bool {
		var got spiceboxv1alpha1.CredentialUpdateRequest
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: name}, &got); err != nil {
			return false
		}
		last = got.Status.Phase
		return last == want
	}, "CredentialUpdateRequest "+name+" must reach phase "+want)
	if t.Failed() {
		t.Logf("%s last observed phase: %q", name, last)
	}
}

// waitForCredentialUpdateUnparked is waitForCredentialUpdateParked's mirror: the
// session must leave the park once its request stops waiting on a human.
//
// Like the park, it does NOT nudge: the request's own transition out of
// Open/Collapsed is a CredentialUpdateRequest status write, and
// SetupWithManager's Watches(&CredentialUpdateRequest{}) re-enqueues the
// session named by spec.sessionRef on it. A timeout here means that watch
// stopped firing -- a wedge, and the whole point of the assertion.
func waitForCredentialUpdateUnparked(t *testing.T, ctx context.Context, h *e2e.Harness, ns, name string) {
	t.Helper()
	var last string
	e2e.Eventually(t, scenarioTimeout, func() bool {
		var got spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending {
				last = string(c.Status) + "/" + c.Reason
				if c.Status == metav1.ConditionFalse {
					return true
				}
			}
		}
		return false
	}, "session "+ns+"/"+name+" must leave the credential-update park once its request settles")
	if t.Failed() {
		t.Logf("%s/%s last observed CredentialUpdatePending: %q", ns, name, last)
	}
}

// waitForRepliesContaining blocks until exactly want outbound user messages
// carrying `text` have reached the session channel, then asserts there are no
// more than that.
//
// Both sessions run the same script and share the one Channel, so counting is
// how "both turns resumed" is observed here. It is not the only evidence the
// test relies on -- both requests reaching Fulfilled, both sessions unparking,
// and four upstream calls are per-session facts that one session replying twice
// could not produce.
func waitForRepliesContaining(t *testing.T, h *e2e.Harness, text string, want int) {
	t.Helper()
	e2e.Eventually(t, scenarioTimeout, func() bool {
		return countRepliesContaining(h, text) >= want
	}, "both blocked agents must finish their turn after the ONE credential replacement; "+
		"a follower released only in the API, with its agent still waiting, reports a timeout to its user "+
		"while a human really did act")
	assert.Equal(t, want, countRepliesContaining(h, text),
		"exactly %d turns resumed, not more: a duplicate reply would mean one session ran the arc twice", want)
}

func countRepliesContaining(h *e2e.Harness, text string) int {
	drv := fakekind.DriverFor("default", channelName)
	if drv == nil {
		return 0
	}
	n := 0
	for _, m := range drv.Sent() {
		if strings.Contains(m.Text, text) {
			n++
		}
	}
	return n
}
