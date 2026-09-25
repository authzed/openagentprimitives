//go:build e2e

// Package credential_update_test is the end-to-end scenario for an agent
// asking a human to replace a credential that has stopped authenticating.
//
// The integration test in pkg/controllers/credentialupdaterequest covers the
// operator/channelsd seam over a real apiserver. What only THIS tier can show
// is the runner in the loop: a real LLM turn, a real upstream tool call that
// fails, the real BLOCKING meta tool, and the turn resuming afterwards. Every
// assertion below is about something the agent or the human actually saw --
// the card in the channel, and the tool_result text the model was fed.
//
// Two scenarios, deliberately opposite:
//
//	dead credential -> a human is asked, the credential is replaced, the
//	                   blocked call returns and the turn finishes
//	live credential -> NOBODY is asked, and the agent is told plainly that
//	                   this is a scope problem it must not re-ask about
//
// The second is the one that keeps the feature honest. An implementation that
// simply believes the agent passes the first scenario and fails this one.
package credential_update_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the placeholder human who started the session and owns
	// the credential. The card is addressed to them. Same placeholder every
	// sibling scenario uses -- deliberately NOT a person-shaped full name.
	starterEmail = "alice@example.com"

	className   = "ac-credupdate"
	channelName = "ac-credupdate-fake"
	mcpToolName = "upmcp_list_issues"
	credential  = "cu-upstream-token"

	// signingKey stands in for the shared passthrough-link HMAC key channelsd
	// holds by volume mount. Nothing in this scenario dials the minted link, so
	// its only requirement is length.
	signingKey      = "credential-update-e2e-signing-key"
	externalBaseURL = "https://agent.example.invalid"

	// scenarioTimeout bounds every poll. Generous: the blocked meta tool polls
	// its CR every 2s, and the operator loop below runs on its own tick.
	scenarioTimeout = 60 * time.Second
)

// ---------------------------------------------------------------------------
// Scenario 1: the credential really is dead
// ---------------------------------------------------------------------------

// TestCredentialUpdate_DeadCredentialAsksAHumanThenTheTurnResumes drives the
// whole arc in one turn: the upstream call comes back 401, the agent calls
// request_credential_update and BLOCKS, a card reaches the human who started
// the session, the credential is replaced, the blocked call returns, and the
// agent's retry completes the turn.
//
// It polls the memory transcript rather than asserting on ExpectAgentReply
// alone: the session parks MID-TURN (phase AwaitingCredentials) while the tool
// blocks, and the transcript is the record that survives that -- it is also
// where the tool_result the model was actually fed can be read back.
func TestCredentialUpdate_DeadCredentialAsksAHumanThenTheTurnResumes(t *testing.T) {
	s := startScenario(t)
	defer s.cancel()
	h, ctx := s.h, s.ctx

	// The provider says the token is dead. This is the operator's INDEPENDENT
	// evidence -- without it the agent's claim alone is refused, by design.
	redirectVerifyProbe(t, 401, "Bad credentials")

	// The upstream fails once (auth) and succeeds after. The retry's success is
	// scripted: what this test pins is that the blocked call RETURNS and the
	// turn resumes, not that the upstream re-authenticated -- that is the
	// broker's business and is covered elsewhere.
	var upstreamCalls atomic.Int32
	h.MCP.OnTool("list_issues", func(map[string]any) any {
		if upstreamCalls.Add(1) == 1 {
			return map[string]any{"error": "401 Unauthorized: bad credentials"}
		}
		return map[string]any{"issues": []any{"issue-1"}, "status": "ok"}
	})

	s.applyUpstream(t)
	prelinkCredential(t, ctx, h, "dead-token")
	startCredentialUpdateOperator(t, ctx, h)
	startCardPublisher(t, ctx, h)

	h.LLM.OnUserMessage("list the upstream issues").Reply(callUpstream("first attempt"))
	// The 401 is what justifies the ask. Matching on it (rather than
	// AnyResult) means a regression that stops surfacing the auth failure fails
	// loudly here instead of silently asking for a credential update anyway.
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("401")).
		Reply(e2e.ToolUse("request_credential_update", map[string]any{
			"tool": mcpToolName,
			"why":  "the call came back 401 Unauthorized",
		}))
	h.LLM.OnToolResult("request_credential_update", e2e.ResultContains("the credential was updated")).
		Reply(callUpstream("retry after credential replaced"))
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("issue-1")).
		Reply(e2e.RespondToUser("listed the issues once the credential was replaced"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid(className, 30*time.Second)
	h.SendUserMessage("list the upstream issues")

	// --- a human is asked ---------------------------------------------------
	sess := waitForSession(t, ctx, h)
	ensurePlaceholderRunnerPod(t, ctx, h, sess)

	// The session parks while the tool blocks: this is what stops the runner
	// being torn down and losing the in-process blocked call.
	//
	// Asserted BEFORE the card, and with no nudge, so a pass is evidence about
	// the trigger and not just about the end state: the only thing that can
	// enqueue this session between the meta tool creating the
	// CredentialUpdateRequest and this line is SetupWithManager's
	// Watches(&CredentialUpdateRequest{}, mapCredentialUpdateRequestToSession).
	// Waiting until after the card would let the card path's own writes stand
	// in for that watch.
	waitForCredentialUpdateParked(t, ctx, h, sess)

	card := waitForCredentialUpdateCard(t, ctx, h, sess)
	assert.Equal(t, categories.CredentialUpdate, card.Payload.Category)
	assert.Contains(t, card.Payload.Body, "the call came back 401 Unauthorized",
		"the agent's own why reaches the human, attributed")
	require.Len(t, card.Payload.Actions, 1, "one action: the credential-entry link")
	assert.True(t, strings.HasPrefix(card.Payload.Actions[0].URL, externalBaseURL),
		"the card's button points at a link the human can actually open")
	require.NotNil(t, card.Payload.Audience.Requester, "the card is addressed to a person")
	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	gotCanon, err := card.Payload.Audience.Requester.Principal().AllowSynthetic().Canonical()
	require.NoError(t, err)
	assert.Equal(t, starterCanon, gotCanon,
		"the card goes to the human who owns the credential, not to whoever happens to be listening")

	// --- the human replaces the credential ----------------------------------
	// No nudge. A value-only re-link keeps the UserIdentity's credential Spec
	// entry byte-identical (it references the master Secret by deterministic
	// name, not by value), so the only thing that makes this a real write --
	// and therefore fires the AgentSession UserIdentity watch that re-projects
	// the master credential into the per-session Secret -- is PutToken
	// restamping useridentity.RotationFingerprintAnnotation. Nudging here would
	// substitute a test-side poke for exactly the mechanism under test.
	prelinkCredential(t, ctx, h, "freshly-pasted-token")

	// --- the blocked call returns and the turn finishes ---------------------
	h.ExpectAgentReply(e2e.Contains("listed the issues once the credential was replaced"))
	assert.EqualValues(t, 2, upstreamCalls.Load(),
		"the agent retried the upstream call after the credential was replaced")

	// The tool_result the model was actually fed. The reply text above is a
	// scripted constant and proves only that the turn completed; THIS is the
	// evidence the blocked call returned with the platform's own verdict.
	assertToolResultSeenByModel(t, h, "the credential was updated. Retry your original tool call now.")

	// And the transcript -- the record that survives the mid-turn park.
	assertTranscriptHasAssistantText(t, ctx, h, sess, "listed the issues once the credential was replaced")
}

// ---------------------------------------------------------------------------
// Scenario 2: the credential is fine and nobody should be bothered
// ---------------------------------------------------------------------------

// TestCredentialUpdate_LiveCredentialNeverAsksAHuman is the negative that gives
// the feature its value. The agent believes the credential is dead and says so;
// the provider says it still authenticates. No card may be built, and the agent
// must be told -- in the platform's own words, verbatim -- that this is a
// permissions/scope problem it must not re-ask about.
//
// An implementation that took the agent's word for it would pass scenario 1 and
// fail here, which is precisely the point of running both.
func TestCredentialUpdate_LiveCredentialNeverAsksAHuman(t *testing.T) {
	s := startScenario(t)
	defer s.cancel()
	h, ctx := s.h, s.ctx

	// The provider says the credential is FINE. An observed 403 next to a live
	// token is what a scope problem looks like.
	redirectVerifyProbe(t, 200, `{"login":"demo"}`)

	var upstreamCalls atomic.Int32
	h.MCP.OnTool("list_issues", func(map[string]any) any {
		upstreamCalls.Add(1)
		return map[string]any{"error": "403 Forbidden: resource not accessible"}
	})

	s.applyUpstream(t)
	prelinkCredential(t, ctx, h, "perfectly-good-token")
	startCredentialUpdateOperator(t, ctx, h)
	startCardPublisher(t, ctx, h)

	h.LLM.OnUserMessage("list the upstream issues").Reply(callUpstream("first attempt"))
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("403")).
		Reply(e2e.ToolUse("request_credential_update", map[string]any{
			"tool": mcpToolName,
			"why":  "I think the token expired",
		}))
	h.LLM.OnToolResult("request_credential_update", e2e.ResultContains("refused --")).
		Reply(e2e.RespondToUser("the platform refused: the credential still works"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid(className, 30*time.Second)
	h.SendUserMessage("list the upstream issues")
	h.ExpectAgentReply(e2e.Contains("the platform refused"))

	sess := waitForSession(t, ctx, h)

	// The determination, on the CR itself.
	cur := singleRequest(t, ctx, h)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused, cur.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive, cur.Status.Determination)
	assert.Empty(t, cur.Status.InteractionRef, "a refused request never gets a card")

	// The agent's tool_result carries the platform's reason VERBATIM. This is
	// the agent's only signal about why, and the sentence that redirects it away
	// from asking again.
	assertToolResultSeenByModel(t, h, cur.Status.Reason)
	assert.Contains(t, cur.Status.Reason, "permissions or scope problem",
		"the refusal must point the agent at the real problem, not just say no")

	// Nobody was bothered -- and the publisher was given a real chance to prove
	// otherwise. A bare "is the queue empty right now" check would pass merely
	// because the watcher's 5s ticker had not fired yet, so this waits out more
	// than one full interval and fails the instant a card appears.
	assertNoCardWithin(t, h, sess, 2*pipeline.CredentialUpdateWatcherInterval)

	// The session never parked: there was no Open request to park on.
	live := getSession(t, ctx, h, sess)
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, live.Status.Phase,
		"a refused request must not leave the session parked")

	assert.EqualValues(t, 1, upstreamCalls.Load(),
		"the agent was told not to re-ask, so it did not loop on the upstream call")
}

// ---------------------------------------------------------------------------
// Scenario 3: the provider cannot be reached to confirm, but the platform saw
// the failures itself
// ---------------------------------------------------------------------------

// TestCredentialUpdate_UnconfirmedFailureAsksButSaysSo is the corroboration
// arc, and it is the only scenario where a card opens on evidence the PLATFORM
// gathered rather than on a verdict the provider handed back.
//
// The setup is the awkward middle of the verdict table. The provider cannot be
// re-probed (it answers 503, so verification is indeterminate — the same branch
// of Determine an unprobeable provider's "unsupported" lands in, and the only
// one of the two the shipped catalog can currently reach: the sole provider
// declaring an authFailure: block also declares a verify: probe). On the
// agent's word alone that refuses, by design — scenario 2's whole point. What
// tips it is that the runner ITSELF saw the upstream answer HTTP 401, which is
// what this provider declares an authentication failure looks like, and
// recorded that on the session.
//
// So this test is really about three claims a narrower test cannot make
// together:
//
//   - the runner records its own observation from a REAL failing MCP call
//     (an actual HTTP 401 off the wire — not a hand-written status field);
//   - the operator reads it back and opens where it would otherwise refuse;
//   - the human's card SAYS the failure could not be confirmed. That wording
//     is the entire difference between this tier and a verified rejection, and
//     it is what stops an unconfirmed suspicion from being presented to a
//     person as fact.
func TestCredentialUpdate_UnconfirmedFailureAsksButSaysSo(t *testing.T) {
	s := startScenario(t)
	defer s.cancel()
	h, ctx := s.h, s.ctx

	// The provider cannot answer. Not a rejection, not an acceptance — the
	// operator simply cannot confirm anything, which is exactly when
	// corroboration becomes the deciding input.
	redirectVerifyProbe(t, http.StatusServiceUnavailable, "upstream verification is down")

	// The upstream answers a genuine HTTP 401 — the wire-level signal the
	// runner classifies against the provider's declared authFailure: shape.
	// A JSON payload merely CONTAINING "401" would not do: the observation is
	// built from the response status, never from tool output the agent can
	// shape.
	//
	// Held live rather than counted: one logical tool call makes several HTTP
	// attempts (the client re-resolves the credential and retries once on a
	// 401), so the test clears it explicitly when the human's fix lands.
	h.MCP.FailHTTP("list_issues", http.StatusUnauthorized, -1)
	var upstreamCalls atomic.Int32
	h.MCP.OnTool("list_issues", func(map[string]any) any {
		upstreamCalls.Add(1)
		return map[string]any{"issues": []any{"issue-1"}, "status": "ok"}
	})

	s.applyUpstream(t)
	prelinkCredential(t, ctx, h, "possibly-dead-token")
	startCredentialUpdateOperator(t, ctx, h)
	startCardPublisher(t, ctx, h)

	h.LLM.OnUserMessage("list the upstream issues").Reply(callUpstream("first attempt"))
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("401")).
		Reply(e2e.ToolUse("request_credential_update", map[string]any{
			"tool": mcpToolName,
			"why":  "every call is coming back 401 Unauthorized",
		}))
	h.LLM.OnToolResult("request_credential_update", e2e.ResultContains("the credential was updated")).
		Reply(callUpstream("retry after credential replaced"))
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("issue-1")).
		Reply(e2e.RespondToUser("listed the issues once the credential was replaced"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid(className, 30*time.Second)
	h.SendUserMessage("list the upstream issues")

	sess := waitForSession(t, ctx, h)
	ensurePlaceholderRunnerPod(t, ctx, h, sess)

	// --- the platform's own evidence ----------------------------------------
	// Asserted directly, because everything below depends on it and a silent
	// miss here (an origin key that does not match, a status field the CRD
	// drops) would otherwise surface only as an unexplained refusal.
	waitForAuthFailureObservation(t, ctx, h, sess, "mcpserver/cu-upstream")

	// Parked before any card exists, on the CredentialUpdateRequest watch alone
	// -- see the same call in the dead-credential scenario for why the ordering
	// is the assertion.
	waitForCredentialUpdateParked(t, ctx, h, sess)

	// --- a human is asked, and told how strong the evidence is ---------------
	card := waitForCredentialUpdateCard(t, ctx, h, sess)
	assert.Equal(t, categories.CredentialUpdate, card.Payload.Category)
	assert.Contains(t, card.Payload.Lead, "could not confirm",
		"the verdict line must say plainly that the failure was NOT confirmed with the provider; "+
			"presenting an unconfirmed suspicion as a confirmed rejection is how a human gets talked into "+
			"replacing a credential nobody proved was dead")
	assert.Contains(t, card.Payload.Body, "every call is coming back 401 Unauthorized",
		"the agent's own why still reaches the human, attributed")

	cur := singleRequest(t, ctx, h)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, cur.Status.Phase)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedUnverified, cur.Status.Determination,
		"an unconfirmable probe plus the platform's own observation is the UNVERIFIED tier -- "+
			"not a verified rejection, and not a refusal")
	assert.Equal(t, cur.Status.Reason, card.Payload.Lead,
		"the card shows the operator's verdict verbatim, not a paraphrase")

	// --- the human replaces the credential and the turn finishes -------------
	// Clearing the injection is the upstream starting to work again: without
	// it the retry below would 401 forever and the turn could never complete.
	h.MCP.FailHTTP("list_issues", 0, 0)
	// No nudge -- the rotation fingerprint is what re-fires the UserIdentity
	// watch. See the dead-credential scenario's equivalent step.
	prelinkCredential(t, ctx, h, "freshly-pasted-token")

	h.ExpectAgentReply(e2e.Contains("listed the issues once the credential was replaced"))
	assert.EqualValues(t, 1, upstreamCalls.Load(),
		"the handler runs only once the injected 401 is cleared -- the failing attempts never reached it")
}

// waitForAuthFailureObservation polls the AgentSession for the runner's own
// auth-failure observation at origin. It is the corroboration input the
// operator reads, and it is written by a DIFFERENT actor (the runner, from the
// tool hot path) than the one that consumes it, so a test that only asserted
// the final card could not tell "the observation was never recorded" from "it
// was recorded and ignored".
func waitForAuthFailureObservation(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession, origin string) {
	t.Helper()
	var last []spiceboxv1alpha1.CredentialAuthFailure
	e2e.Eventually(t, scenarioTimeout, func() bool {
		var got spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &got); err != nil {
			return false
		}
		last = got.Status.CredentialAuthFailures
		return spiceboxv1alpha1.FindCredentialAuthFailure(last, origin) != nil
	}, "the runner must record its own auth-failure observation for "+origin+
		" -- without it the operator has no independent evidence and must refuse")
	if t.Failed() {
		t.Logf("credentialAuthFailures on the session: %+v", last)
	}
}

// ---------------------------------------------------------------------------
// Scenario plumbing
// ---------------------------------------------------------------------------

// scenario is one booted harness plus the pieces both tests thread around: the
// join context every background goroutine registers against, and the MCPServer
// document held back until the stub URL is known.
type scenario struct {
	h      *e2e.Harness
	ctx    context.Context
	cancel context.CancelFunc
	mcpDoc string
}

// startScenario boots the harness with this package's manifests. The MCPServer
// document is deliberately NOT applied here -- see applyUpstream.
func startScenario(t *testing.T) *scenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	mcpDoc, rest := splitMCPServerDoc(string(raw))
	require.NotEmpty(t, mcpDoc, "manifests.yaml must contain an MCPServer document")

	h := e2e.Start(t, e2e.Options{
		DefaultUser:    starterEmail,
		ExtraManifests: []string{rest},
		DefaultTimeout: scenarioTimeout,
	})
	ctx, cancel := context.WithCancel(context.Background())
	return &scenario{h: h, ctx: ctx, cancel: cancel, mcpDoc: mcpDoc}
}

// applyUpstream applies the MCPServer with the live stub URL substituted in.
// Must run AFTER h.MCP.OnTool: the MCPServer controller's tools/list probe runs
// during validation, and the AgentClass binding-coverage check fails if the
// declared tool is not advertised.
func (s *scenario) applyUpstream(t *testing.T) {
	t.Helper()
	s.h.ApplyManifest(strings.ReplaceAll(s.mcpDoc, "{{MCP_URL}}", s.h.MCP.URL()))
}

// splitMCPServerDoc separates the MCPServer document from the rest, so the
// former can be applied later with the live stub URL. Same shape as the
// credentials and webchat_credential scenarios.
func splitMCPServerDoc(all string) (mcpDoc, rest string) {
	var keep []string
	for _, doc := range strings.Split(all, "\n---\n") {
		if strings.Contains(doc, "kind: MCPServer") {
			mcpDoc = doc
			continue
		}
		keep = append(keep, doc)
	}
	return mcpDoc, strings.Join(keep, "\n---\n")
}

// prelinkCredential writes (or replaces) the starter's master credential.
// Called once before the first message -- the premise is a credential that is
// PRESENT and dead, not one that was never linked -- and again to stand in for
// the human pasting a replacement.
func prelinkCredential(t *testing.T, ctx context.Context, h *e2e.Harness, token string) {
	t.Helper()
	ensureIdentitiesNamespace(t, ctx, h.K8s)
	canon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err, "canonicalize starter email")
	require.NoError(t, useridentity.PutToken(ctx, h.K8s, useridentity.PutTokenRequest{
		Subject:        canon.Subject(),
		CredentialName: credential,
		Token:          token,
	}), "put credential %q", credential)
}

func ensureIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create identities namespace")
	}
}

// startCredentialUpdateOperator runs the operator's determination loop. The
// harness's controller manager is not extensible from a scenario, so this
// drives Reconcile directly on a tick -- equivalent for everything this test
// asserts, since the reconciler re-reads the backing Secret on every Open pass
// and does not depend on its own Secret watch to notice a change.
func startCredentialUpdateOperator(t *testing.T, ctx context.Context, h *e2e.Harness) {
	t.Helper()
	r := &credentialupdaterequest.Reconciler{
		Client: h.K8s,
		Broker: inproc.New(h.K8s),
		// Long: this scenario never exercises the expiry path, and a short TTL
		// would race the human's replacement.
		IdleTTL: 10 * time.Minute,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				var list spiceboxv1alpha1.CredentialUpdateRequestList
				if err := h.K8s.List(ctx, &list); err != nil {
					if ctx.Err() == nil {
						t.Logf("credential-update operator: list requests: %v", err)
					}
					continue
				}
				for i := range list.Items {
					it := &list.Items[i]
					if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
						Namespace: it.Namespace, Name: it.Name,
					}}); err != nil && ctx.Err() == nil {
						t.Logf("credential-update operator: reconcile %s/%s: %v", it.Namespace, it.Name, err)
					}
				}
			}
		}
	}()
	t.Cleanup(func() { <-done })
}

// startCardPublisher runs channelsd's real CredentialUpdateWatcher against the
// harness's apiserver and NATS, exactly as the channelsd binary does. It is the
// only thing that can turn an Open request into something a human sees.
func startCardPublisher(t *testing.T, ctx context.Context, h *e2e.Harness) {
	t.Helper()
	conn := dialHarnessNATS(t, h.NATSURL)
	w := &pipeline.CredentialUpdateWatcher{
		K8s:             h.K8s,
		LinkSigner:      passthroughlink.New([]byte(signingKey)),
		ExternalBaseURL: func() string { return externalBaseURL },
		NATSPublish:     conn.Publish,
	}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Logf("credential-update card publisher did not exit within 5s")
		}
	})
}

// credentialUpdatePrompts is the delivered-card view a test asserts on. The
// cards land in the fake channel driver -- the same path a real Slack send
// takes -- so this reads from there rather than intercepting NATS, which would
// only prove the watcher published, not that anything was deliverable.
func credentialUpdatePrompts(h *e2e.Harness, sess *spiceboxv1alpha1.AgentSession) []fakekind.InteractionPrompt {
	drv := fakekind.DriverFor(sess.Namespace, channelName)
	if drv == nil {
		return nil
	}
	var out []fakekind.InteractionPrompt
	for _, p := range drv.InteractionPrompts() {
		if p.Payload.Category != categories.CredentialUpdate {
			continue
		}
		if p.Payload.AgentSessionRef.Namespace != sess.Namespace || p.Payload.AgentSessionRef.Name != sess.Name {
			continue
		}
		out = append(out, p)
	}
	return out
}

func dialHarnessNATS(t *testing.T, natsURL string) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.Name("e2e-credential-update"))
	require.NoError(t, err, "dial harness NATS")
	t.Cleanup(func() {
		if err := conn.Drain(); err != nil {
			t.Logf("credential-update: nats drain on cleanup: %v", err)
		}
	})
	return conn
}

// redirectVerifyProbe points the provider-catalog verify probe at a local
// server answering `status`. This is the operator's INDEPENDENT evidence about
// the credential -- the single input that decides whether a human is asked at
// all -- so each scenario sets it explicitly rather than inheriting a default.
//
// It mutates package-level state in pkg/platform/identity/setup/builtins, so tests using
// it must not run in parallel with each other.
func redirectVerifyProbe(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse probe server URL")
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: &redirectTransport{target: u}}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}

// redirectTransport rewrites every outbound request at the real catalog
// provider's hardcoded endpoint to the local server instead. The provider
// catalog is embedded and has no injectable test entry, so this is how a test
// drives a REAL provider's declarative verify without touching the network.
type redirectTransport struct{ target *url.URL }

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = t.target.Scheme
	r2.URL.Host = t.target.Host
	r2.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// ---------------------------------------------------------------------------
// LLM script helpers
// ---------------------------------------------------------------------------

// callUpstream is the failing tool call: the one whose 401 the agent reacts to,
// and whose Origin() ("mcpserver/cu-upstream") is what the operator resolves
// back to a credential.
//
// operation_id is a literal rather than one minted by new_operation. This class
// declares no tool bundles, so the runner leaves SessionContext.Operations nil
// and MCP dispatch skips the is-it-registered check -- it still requires the
// envelope fields to be PRESENT, which is what the constant satisfies. Calling
// new_operation here would fail outright ("SessionContext.Operations is nil")
// and has nothing to do with what this scenario tests.
func callUpstream(query string) e2e.ReplyPart {
	return e2e.ToolUse(mcpToolName, map[string]any{
		"operation_id": "op-credential-update-e2e",
		"_reason":      "read the upstream issue list",
		"args":         map[string]any{"query": query},
	})
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

func waitForSession(t *testing.T, ctx context.Context, h *e2e.Harness) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var found *spiceboxv1alpha1.AgentSession
	e2e.Eventually(t, scenarioTimeout, func() bool {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(ctx, &list); err != nil || len(list.Items) == 0 {
			return false
		}
		found = &list.Items[0]
		return true
	}, "an AgentSession must be created for the inbound message")
	return found
}

func getSession(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &got), "re-read AgentSession")
	return &got
}

// waitForCredentialUpdateCard polls the fake channel driver for the card. The
// driver is where an outbound interaction lands after travelling the same
// pipeline a real channel send would.
func waitForCredentialUpdateCard(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession) fakekind.InteractionPrompt {
	t.Helper()
	var got fakekind.InteractionPrompt
	deadline := time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) {
		if prompts := credentialUpdatePrompts(h, sess); len(prompts) > 0 {
			return prompts[0]
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitForCredentialUpdateCard: cancelled before a card for %s/%s arrived",
				sess.Namespace, sess.Name)
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("no credential_update card reached the channel for %s/%s within %v -- THE HUMAN WAS NEVER ASKED",
		sess.Namespace, sess.Name, scenarioTimeout)
	return got
}

// waitForCredentialUpdateParked waits for the session to park on the open
// request. The park is what keeps the runner alive while the meta tool blocks
// in-process; tearing it down would destroy the blocked call and make
// "credential fixed -> retry" impossible.
//
// It does NOT nudge. Callers must have created the placeholder runner Pod
// first (see ensurePlaceholderRunnerPod); given that, the CredentialUpdate-
// Request watch registered in agentsession.SetupWithManager is the production
// trigger and is sufficient, and a timeout here means that watch stopped
// re-enqueueing the session -- exactly the wedge a nudge would hide.
func waitForCredentialUpdateParked(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	parked := false
	deadline := time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && !parked {
		var got spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &got); err == nil {
			for _, c := range got.Status.Conditions {
				if c.Type == spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending &&
					c.Status == metav1.ConditionTrue {
					parked = true
				}
			}
		}
		if !parked {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if !parked {
		var got spiceboxv1alpha1.AgentSession
		_ = h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &got)
		var list spiceboxv1alpha1.CredentialUpdateRequestList
		_ = h.K8s.List(ctx, &list)
		reqs := make([]string, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, list.Items[i].Name+"="+list.Items[i].Status.Phase)
		}
		t.Fatalf("the session never parked on the open credential-update request within %v; "+
			"session phase=%q conditions=%+v requests=%v",
			scenarioTimeout, got.Status.Phase, got.Status.Conditions, reqs)
	}
}

// assertNoCardWithin fails if any credential_update card reaches the channel
// for sess during d. The window matters: the publisher polls on a ticker, so an
// instantaneous empty-queue check proves nothing about whether it WOULD have
// published. The same wiring demonstrably publishes in this package's other
// scenario, so an empty queue here is a real negative.
func assertNoCardWithin(t *testing.T, h *e2e.Harness, sess *spiceboxv1alpha1.AgentSession, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if prompts := credentialUpdatePrompts(h, sess); len(prompts) > 0 {
			t.Fatalf("a card was put in front of a human for a credential the provider says is LIVE: %+v",
				prompts[0].Payload)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ensurePlaceholderRunnerPod creates the inert Pod the AgentSession reconciler
// expects to find under RunnerPodName.
//
// In production a runner Pod always exists by this point. In the in-process
// harness the runner is a goroutine, and the factory only materializes a
// placeholder Pod for identity-choice sessions (deliberately: doing it for
// every session measurably slowed the suite -- see
// InProcessRunnerFactory.ensureRunnerPodPresent's comment). Without one,
// AgentSession.Reconcile stops at the runner-spawn step -- stamping
// RunnerReady=False/RunnerCreating and returning -- several hundred lines
// before the credential-update park, so the park could never be observed here
// at all. The Pod stays Pending, so nothing else about the session's lifecycle
// changes; it exists only to let Reconcile get far enough to reach the park.
//
// CALL THIS BEFORE THE REQUEST EXISTS, i.e. right after the session appears.
// The Pod is created out-of-band with no ownerReference, so unlike the Pod the
// PodRunnerFactory creates in production it fires no Owns(&corev1.Pod{}) event.
// Create it after the CredentialUpdateRequest is already Open and the watch
// event that would have driven the park has been and gone: the session then
// sits with no pending event and the park lands ~25s later, off some unrelated
// write, which is what the old nudge in waitForCredentialUpdateParked was
// papering over. Created first, the request's own watch event does the work,
// and the park lands in single-digit milliseconds.
func ensurePlaceholderRunnerPod(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentsession.RunnerPodName(sess),
			Namespace: sess.Namespace,
			Labels:    map[string]string{"agentprimitives.authzed.com/agentsession": sess.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers:    []corev1.Container{{Name: "runner", Image: "e2e-credential-update-placeholder"}},
		},
	}
	if err := h.K8s.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create placeholder runner pod")
	}
}

func singleRequest(t *testing.T, ctx context.Context, h *e2e.Harness) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, h.K8s.List(ctx, &list), "list CredentialUpdateRequests")
	require.Len(t, list.Items, 1, "exactly one credential-update request expected")
	return &list.Items[0]
}

// assertToolResultSeenByModel searches every request the runner sent to the LLM
// for a tool_result containing want.
//
// This is the load-bearing assertion in both scenarios, and it is deliberately
// NOT an assertion on the agent's reply text: the reply is a scripted constant
// and would stay green even if the tool returned something entirely different.
// Substring, not equality, because the runner wraps every tool result in an
// untrusted-output envelope before it reaches the model.
func assertToolResultSeenByModel(t *testing.T, h *e2e.Harness, want string) {
	t.Helper()
	var seen []string
	for _, req := range h.LLM.Requests() {
		for _, m := range req.Messages {
			for _, block := range m.Content {
				if block.ToolResult == nil {
					continue
				}
				if strings.Contains(block.ToolResult.Content, want) {
					return
				}
				seen = append(seen, block.ToolResult.Content)
			}
		}
	}
	t.Fatalf("no tool_result fed to the model contained %q; tool results seen:\n%s",
		want, strings.Join(seen, "\n---\n"))
}

// assertTranscriptHasAssistantText reads the durable memory transcript -- the
// record that survives the mid-turn park -- for the agent's final text.
func assertTranscriptHasAssistantText(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession, want string) {
	t.Helper()
	scope := pkgmemory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
	readCtx := pkgmemory.WithSystemApproval(ctx, "e2e-test")
	var dump []pkgmemory.Turn
	e2e.Eventually(t, scenarioTimeout, func() bool {
		turns, err := turn.ReadAll(readCtx, h.Memory(), scope)
		if err != nil {
			return false
		}
		dump = turns
		for _, tn := range turns {
			if tn.Role == "assistant" && strings.Contains(e2e.FirstText(tn.Content), want) {
				return true
			}
		}
		return false
	}, "the transcript must record the agent's post-resume reply")
	if t.Failed() {
		t.Logf("transcript:\n%s", e2e.DumpTurns(dump))
	}
}
