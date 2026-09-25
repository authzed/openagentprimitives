//go:build integration

// pkg/controllers/credentialupdaterequest/collapse_seam_integration_test.go
//
// Collapsing, across every process the design splits it across, over ONE real
// apiserver and ONE real SpiceDB:
//
//	operator   pkg/controllers/credentialupdaterequest  elects a canonical, collapses the rest
//	channelsd  pkg/channels/channelsd/pipeline                   publishes for phase Open and ONLY Open
//	operator   pkg/controllers/agentsession             parks BOTH sessions, canonical and follower
//	browser    pkg/web/admind (the production handler)      one admin action, once
//	operator   pkg/controllers/credentialupdaterequest  Fulfils the canonical, settles the follower
//	operator   pkg/controllers/agentsession             unparks BOTH sessions
//
// # Why this file exists next to agentowned_seam_integration_test.go
//
// That file proves ONE session's card reaches an admin who can act on it. This
// one proves the multiplicity claim, which is a different fact and has a
// different failure mode: five sessions sharing one dead bot credential used to
// raise five cards at the same admin. Nothing in the single-session seam can
// observe that, and nothing in collapse_test.go / propagate_test.go can either
// -- those drive one reconciler against its own fake client, where "channelsd
// published nothing" is an inference from a phase rather than an observation of
// a process that was running, watching, and demonstrably able to publish.
//
// # The two tests are an A/B pair, and that is the whole design
//
// TestSeam_TwoSessionsOneSharedCredentialRaiseOneCardAndBothUnpark and
// TestSeam_TwoSessionsDifferentCredentialsEachRaiseTheirOwnCard run the
// IDENTICAL fixture -- same AgentIdentity, same class, same monitoring surface,
// same admin grant, same two sessions -- and differ in exactly ONE input: which
// MCPServer the second session's request names, and therefore which credential
// it resolves to. One card versus two cards is then attributable to the
// credential and to nothing else, which is precisely what "collapsing must key
// on the credential, not on 'another request exists'" means.
//
// # Every negative here carries a positive control in the same test
//
// "Exactly one card" passes trivially against a setup where the second session
// never produced a request at all, and this repo has twice shipped a designated
// negative that stayed green with the guarded behaviour deleted. So the
// one-card test asserts, before it ever counts cards, that the second request
// EXISTS, RESOLVED, resolved to the SAME credential, and reached Collapsed
// pointing at the canonical -- the machinery ran, and stopped exactly where it
// was supposed to.
package credentialupdaterequest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	channelsdpipeline "github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
)

const (
	// csIdentity is the shared bot identity BOTH sessions run as. An
	// AgentIdentity is the shape where several sessions name ONE identity CR,
	// which is what this seam exercises end to end. It is NOT the only shape
	// that collapses: two passthrough sessions of the same person hold two
	// differently-named SessionUserIdentities while resolving the identical
	// backing Secret, and they collapse too -- collapse keys on the credential's
	// (ownership, Secret namespace, Secret name, key), never on the identity CR
	// a request reached it through. That case is covered in the unit suite
	// (collapse_test.go's passthrough fixtures), which can drive an OAuth
	// dead-refresh verdict without an envtest apiserver.
	csIdentity = "demo-collapse-bot"

	// csPrimarySecret backs itCredName -- the credential BOTH sessions share in
	// the one-card test. csSecondSecret backs csSecondCred, the credential that
	// makes the different-credential test different.
	csPrimarySecret = "demo-collapse-primary-creds"
	csSecondSecret  = "demo-collapse-second-creds"
	csSecondCred    = "demo-collapse-second-token"

	csDeadValue  = "tok-collapse-dead-and-verified-dead"
	csFreshValue = "tok-collapse-freshly-pasted-by-an-admin"

	csClass        = "cs-cls"
	csMCPPrimary   = "cs-mcp-primary"
	csMCPSecond    = "cs-mcp-second"
	csMonitoringCh = "demo-collapse-monitoring"

	csSessA = "cs-sess-a"
	csSessB = "cs-sess-b"
	csCurA  = "cs-cur-a"
	csCurB  = "cs-cur-b"
)

// csOriginPrimary / csOriginSecond are the two origins a request can name. They
// differ ONLY in which MCPServer they point at, and therefore only in which
// credential of the ONE shared AgentIdentity they resolve to.
const (
	csOriginPrimary = "mcpserver/" + csMCPPrimary
	csOriginSecond  = "mcpserver/" + csMCPSecond
)

// ---------------------------------------------------------------------------
// The shared fixture both tests run
// ---------------------------------------------------------------------------

// csWorld is everything the two tests share, built identically for both so the
// only variable between them is the origin the second request names.
type csWorld struct {
	spdb *spicedb.Client
	pub  *itCapturingPublisher
	// sessA / sessB are re-read after creation, so their apiserver-assigned
	// UIDs are real: resolveIdentity's ownedBySession compares exactly that UID
	// and refuses a request whose ownerRef names a fabricated one.
	sessA, sessB *spiceboxv1alpha1.AgentSession
}

// csStart builds the world and starts every actor: the AgentSession reconciler
// (so the parks are real), the CredentialUpdateRequest reconciler in a real
// manager (so its watches and its canonical->followers enqueue are real), and
// channelsd's CredentialUpdateWatcher on its own goroutine with its own client
// (so "one card" is an observation of a process that was genuinely watching).
func csStart(t *testing.T, ctx context.Context, env *testenv.Env) *csWorld {
	t.Helper()
	spdb := aoSpiceDB(t)
	itRejectingProvider(t)

	// Two backing Secrets, both dead. The second one exists in BOTH tests --
	// the one-card test simply never asks about it -- so the two fixtures are
	// byte-identical up to the origin each request names.
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: csPrimarySecret, Namespace: itNS},
		Data:       map[string][]byte{itCredName: []byte(csDeadValue)},
	}), "create the shared bot's primary Secret")
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: csSecondSecret, Namespace: itNS},
		Data:       map[string][]byte{csSecondCred: []byte(csDeadValue)},
	}), "create the shared bot's second Secret")

	// ONE AgentIdentity, TWO credentials. Holding the identity constant is what
	// makes the different-credential test a real regression guard: an
	// implementation keyed on the identity coordinates without the credential
	// name would collapse two genuinely different dead tokens onto one card.
	require.NoError(t, env.Client.Create(ctx, &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: csIdentity, Namespace: itNS},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: itCredName, Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: csPrimarySecret, Key: itCredName},
					},
				},
				{
					Name: csSecondCred, Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: csSecondSecret, Key: csSecondCred},
					},
				},
			},
		},
	}), "create the shared AgentIdentity")

	aoEnsureMonitoringSecret(t, ctx, env)
	require.NoError(t, env.Client.Create(ctx, aoMonitoringChannel(csMonitoringCh)),
		"create the monitoring Channel: without a deliverable one there is no admin surface at all")

	require.NoError(t, env.Client.Create(ctx, itMCPServer(csMCPPrimary)), "create the primary MCPServer")
	second := itMCPServer(csMCPSecond)
	second.Spec.Auth.Credential = csSecondCred
	require.NoError(t, env.Client.Create(ctx, second), "create the second MCPServer")

	ac := aoClass(csClass, csIdentity, csMCPPrimary, csMCPSecond)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)

	// Both grants run in BOTH tests, so the one-card test's admin click and the
	// two-card test's fixture differ in nothing but the origin. TouchPlatformAdmin
	// makes update_credential satisfiable by aoAdmin; aoReconcileAgentIdentity is
	// the PRODUCTION reconciler writing the #platform tuple that makes it
	// satisfiable at all.
	require.NoError(t, spdb.TouchPlatformAdmin(ctx, aoAdmin), "grant platform admin")
	aoReconcileAgentIdentity(t, env, spdb, csIdentity)

	// Both sessions are started by the BYSTANDER, who holds nothing. That keeps
	// "exactly one card" unambiguous: an agent-owned card reaches an admin as a
	// monitoring broadcast, and the in-thread twin is published only when the
	// turn's author personally holds update_credential. A starter who does would
	// add a second, correctly-routed surface per Open request and blur the count
	// this test is about.
	csCreateSession(t, ctx, env, csSessA, "thread:C0DEMO:1700000000.000100")
	csCreateSession(t, ctx, env, csSessB, "thread:C0DEMO:1700000000.000200")

	mem := itStartSessionOperator(t, env)
	w := &csWorld{spdb: spdb}
	w.sessA = itGetSession(t, env.Client, csSessA)
	w.sessB = itGetSession(t, env.Client, csSessB)
	// A live, turning session is the state the park actually interrupts, and
	// seeding it is what makes the UNPARK observable -- see
	// itStartSessionOperator's LifecycleMemory note.
	itSeedRunning(t, mem, csSessA, string(w.sessA.UID))
	itSeedRunning(t, mem, csSessB, string(w.sessB.UID))

	itStartCredentialUpdateOperator(t, env, 10*time.Minute)
	w.pub = aoStartChannelsd(t, env, spdb)
	return w
}

func csCreateSession(t *testing.T, ctx context.Context, env *testenv.Env, name, threadKey string) {
	t.Helper()
	sess := aoSession(name, csClass, aoBystander)
	// A distinct channel key per session. Nothing in this file reads it, but two
	// sessions sharing one thread key is a shape production never produces, and
	// a fixture that models two conversations should look like two.
	sess.Spec.InputChannel.Key = threadKey
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession %s", name)
}

// csOpenAndDelivered drives one request all the way to "a human has been asked":
// the operator Opens it, and channelsd -- in its own goroutine, off its own
// client -- publishes and stamps the delivery back onto the CR.
//
// Both tests create the SECOND request only after this returns for the first.
// That is deliberate and is not weakening the test: collapsing is defined
// against an ALREADY-COMMITTED Open peer (canonicalOpenRequestFor property (1)),
// and the simultaneous-arrival election has its own coverage against a fake
// client where the interleaving can be controlled exactly
// (TestReconcile_SimultaneousRequestsElectExactlyOneCanonical). Racing two
// creations here would test the manager's cache-propagation latency, not the
// collapse.
func csOpenAndDelivered(t *testing.T, env *testenv.Env,
	name string, sess *spiceboxv1alpha1.AgentSession, origin string) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	require.NoError(t, env.Client.Create(context.Background(), itRequest(name, sess, origin)),
		"create request %s", name)
	itPhaseReaches(t, env.Client, name, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
	testfixtures.Eventually(t, itPollTimeout, func() bool {
		return itGetRequest(t, env.Client, name).Status.InteractionRef != ""
	})
	opened := itGetRequest(t, env.Client, name)
	require.NotNil(t, opened.Status.ResolvedCredential, "an Open request must record what the origin resolved to")
	require.Equal(t, spiceboxv1alpha1.IdentityKindAgentIdentity, opened.Status.ResolvedCredential.IdentityKind,
		"both sessions run in agent mode; everything downstream routes on this")
	return opened
}

// csAwaitDecided waits until the named request has been through the
// determination pipeline at all -- any phase other than the undecided ""/Pending
// pair, with a resolved credential recorded.
//
// It is deliberately phase-AGNOSTIC. itPhaseReaches names one expected phase, so
// a test that used it here would, under a revert of collapsing, fatal on a
// phase-timeout message and never run the card-count assertion that is the
// actual subject. This helper lets a reverted build proceed to that assertion.
func csAwaitDecided(t *testing.T, env *testenv.Env, name string) {
	t.Helper()
	var last string
	deadline := time.Now().Add(itPollTimeout)
	for time.Now().Before(deadline) {
		var got spiceboxv1alpha1.CredentialUpdateRequest
		if err := env.Client.Get(context.Background(),
			types.NamespacedName{Namespace: itNS, Name: name}, &got); err == nil {
			last = got.Status.Phase
			decided := last != "" && last != spiceboxv1alpha1.CredentialUpdateRequestPhasePending
			if decided && got.Status.ResolvedCredential != nil {
				return
			}
		}
		time.Sleep(testfixtures.PollInterval)
	}
	t.Fatalf("CredentialUpdateRequest %s never reached a determination within %v; last phase was %q",
		name, itPollTimeout, last)
}

// csAwaitParked waits for every named session to be parked on a
// credential-update request, and asserts the park is the one under test (the
// phase override, not merely the condition).
func csAwaitParked(t *testing.T, env *testenv.Env, names ...string) {
	t.Helper()
	for _, n := range names {
		itAwaitCredentialUpdatePending(t, env, n, metav1.ConditionTrue)
		parked := itGetSession(t, env.Client, n)
		assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, parked.Status.Phase,
			"session %s must be parked: its runner is blocked in-process inside the meta tool, "+
				"whether it holds the card or is riding on somebody else's", n)
	}
}

func csAwaitUnparked(t *testing.T, env *testenv.Env, names ...string) {
	t.Helper()
	for _, n := range names {
		itAwaitCredentialUpdatePending(t, env, n, metav1.ConditionFalse)
		unparked := itGetSession(t, env.Client, n)
		assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, unparked.Status.Phase,
			"session %s must leave the credential-update park once its request is no longer waiting on a human", n)
		cond := apimeta.FindStatusCondition(unparked.Status.Conditions,
			spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending)
		require.NotNil(t, cond, "session %s", n)
		assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateResolved, cond.Reason, "session %s", n)
	}
}

// ---------------------------------------------------------------------------
// 1: one shared credential -> ONE card, and one admin action releases both
// ---------------------------------------------------------------------------

// TestSeam_TwoSessionsOneSharedCredentialRaiseOneCardAndBothUnpark is the
// slice's headline claim at the tier that can actually observe it.
//
// The order of assertions is load-bearing. Before this test counts cards it
// proves the machinery ran: the follower EXISTS, RESOLVED a credential, and
// resolved to the SAME one -- so "no second card" is a statement about the
// collapse and not about a request that was refused early, never reconciled, or
// never created. Only then does it assert that channelsd -- running, watching,
// and demonstrably able to publish, since it published the canonical's -- issued
// exactly one broadcast.
func TestSeam_TwoSessionsOneSharedCredentialRaiseOneCardAndBothUnpark(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	w := csStart(t, ctx, env)

	// --- session A asks, and a human is asked ------------------------------
	canonical := csOpenAndDelivered(t, env, csCurA, w.sessA, csOriginPrimary)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified, canonical.Status.Determination,
		"a provider that answers 401 is a definitive rejection")
	assert.Equal(t, itCredName, canonical.Status.ResolvedCredential.Credential)
	require.NotNil(t, canonical.Status.CredentialSecretRef,
		"the Open transition must record the backing Secret it will watch for the fix")

	firstBroadcast := w.pub.monitoringEvents(t)
	require.Len(t, firstBroadcast, 1, "positive control: the fixture really does produce a card")

	// --- session B asks about the SAME credential --------------------------
	require.NoError(t, env.Client.Create(ctx, itRequest(csCurB, w.sessB, csOriginPrimary)),
		"create the second session's request")

	// Waits for the second request to be DECIDED AT ALL, deliberately not for
	// phase Collapsed.
	//
	// This ordering is the difference between a mutation report that names the
	// damage and one that names a symptom. Waiting on Collapsed here makes the
	// test fatal, under a revert, on a 90s phase timeout -- and the card-count
	// assertion below, which is the whole point of this test, never executes at
	// all, so nothing ever proves IT works. Waiting only for "decided" lets a
	// reverted build walk straight into the two-cards check, which is where the
	// harm actually is.
	csAwaitDecided(t, env, csCurB)
	follower := itGetRequest(t, env.Client, csCurB)

	// POSITIVE CONTROLS for every negative below. Each one rules out a way this
	// test could stay green with collapsing deleted. None of them reads the
	// phase, so all of them still hold under a revert -- which is exactly what
	// makes the card count that follows attributable to the collapse.
	require.NotNil(t, follower.Status.ResolvedCredential,
		"the follower must have RESOLVED a credential -- an early refusal (NoCredential, unowned session, "+
			"unknown identity kind) would make every no-second-card claim below vacuous; status=%+v",
		follower.Status)
	assert.Equal(t, canonical.Status.ResolvedCredential.IdentityKind, follower.Status.ResolvedCredential.IdentityKind)
	assert.Equal(t, canonical.Status.ResolvedCredential.Namespace, follower.Status.ResolvedCredential.Namespace)
	assert.Equal(t, canonical.Status.ResolvedCredential.Name, follower.Status.ResolvedCredential.Name)
	assert.Equal(t, canonical.Status.ResolvedCredential.Credential, follower.Status.ResolvedCredential.Credential,
		"the two sessions must genuinely be asking about ONE credential, or collapsing them would be a bug")
	assert.NotEqual(t, w.sessA.Name, follower.Spec.SessionRef.Name,
		"the two requests are raised by two DIFFERENT sessions -- the budget is charged per session, "+
			"so a follower spending the canonical's budget instead of its own would be invisible if they were one")

	// --- THE claim: still exactly one card ---------------------------------
	//
	// Two full watcher ticks: the second publish a revert would produce is
	// driven by channelsd's own loop, so a window shorter than its interval
	// could miss the pass entirely -- and would keep reporting "one card" after
	// collapsing was reverted, which is the one thing it must never do. Taken
	// from the watcher's own constant rather than restated, so a future cadence
	// change widens this window instead of silently letting the negative go
	// vacuous.
	csAssertStillOneBroadcast(t, w.pub, 2*channelsdpipeline.CredentialUpdateWatcherInterval)
	assert.Empty(t, w.pub.interactionRequests(t),
		"an agent-owned credential is never DM'd to a starter who cannot replace it, for either session")

	// --- and the shape that produced it -------------------------------------
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
		"Open is the ONLY phase channelsd publishes on, so 'no second card' is exactly 'never reaches Open'; "+
			"status=%+v", follower.Status)
	require.NotNil(t, follower.Status.CollapsedInto, "the follower must record WHICH request it is waiting on")
	assert.Equal(t, spiceboxv1alpha1.NamespacedRef{Namespace: itNS, Name: csCurA}, *follower.Status.CollapsedInto)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationCollapsed, follower.Status.Determination)
	assert.NotEmpty(t, follower.Status.Reason, "no-silent-errors: a collapse must still say why nothing was published")

	// A follower must never LOOK delivered: slice 1's expiry branch and the meta
	// tool's timeout message both read exactly these fields to tell "a human was
	// asked" from "nobody was asked".
	assert.Empty(t, follower.Status.InteractionRef,
		"no card was published for the follower, so nothing may claim one was")
	assert.Nil(t, apimeta.FindStatusCondition(follower.Status.Conditions,
		spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered),
		"CardDelivered is stamped by channelsd, and only for a card a human was really shown")
	assert.Nil(t, follower.Status.CredentialSecretRef,
		"a follower records no unpark baseline -- it has no card of its own to unpark")

	// --- both sessions park -------------------------------------------------
	//
	// The follower's session parks exactly as hard as the canonical's: its agent
	// is blocked in the same meta-tool call on the same credential. A park that
	// only saw phase Open would leave this session reading Running while its
	// runner was stuck, and would make "the follower's session unparks" a
	// statement about a park that never happened.
	csAwaitParked(t, env, csSessA, csSessB)

	// --- ONE admin action ---------------------------------------------------
	operator := aoStartOperatorAdmind(t, env, w.spdb)
	resp, err := operator.Replace(ctx, identity.Subject("user:"+aoAdmin.String()), agentcred.Request{
		SessionRef: canonical.Spec.SessionRef,
		Credential: itCredName,
		Token:      csFreshValue,
	})
	require.NoError(t, err, "a platform admin holds agentidentity#update_credential and must be allowed")
	assert.Equal(t, csPrimarySecret, resp.SecretRef.Name,
		"the operator writes the destination IT resolved from the request's own status")
	assert.Equal(t, csFreshValue, aoSecretValue(t, env.Client, csPrimarySecret),
		"the shared bot credential is replaced, once")

	// --- both requests settle -----------------------------------------------
	itPhaseReaches(t, env.Client, csCurA, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)
	itPhaseReaches(t, env.Client, csCurB, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)
	settledCanonical := itGetRequest(t, env.Client, csCurA)
	settledFollower := itGetRequest(t, env.Client, csCurB)

	assert.Equal(t, settledCanonical.Status.Determination, settledFollower.Status.Determination,
		"the two requests are about the identical credential, so the canonical's verdict IS the follower's")
	require.NotNil(t, settledFollower.Status.CollapsedInto,
		"the pointer stays set after settling: it is the only durable record of whose answer this outcome came from")
	assert.NotEqual(t, settledCanonical.Status.Reason, settledFollower.Status.Reason,
		"the REASON is the one thing propagation may not copy: the canonical's is written from the point of view "+
			"of a request that HAD a card")
	assert.Contains(t, settledFollower.Status.Reason, "another session",
		"a settled follower must say whose ask it is answering")
	assert.Empty(t, settledFollower.Status.InteractionRef,
		"settling must not retroactively make the follower look delivered")

	// --- and both sessions unpark -------------------------------------------
	csAwaitUnparked(t, env, csSessA, csSessB)

	// One card for the whole scenario, start to finish.
	assert.Len(t, w.pub.monitoringEvents(t), 1,
		"one dead shared credential, two blocked sessions, ONE admin asked, once")
}

// csAssertStillOneBroadcast fails the moment a SECOND credential broadcast
// appears, and otherwise keeps watching for d.
//
// A negative like this cannot be one read: the publish it must catch is driven
// by channelsd's own ticker, so a window shorter than its interval could miss
// the pass entirely -- and would keep reporting "one card" after collapsing was
// reverted, which is the one thing it must never do.
func csAssertStillOneBroadcast(t *testing.T, pub *itCapturingPublisher, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := pub.monitoringEvents(t); len(got) != 1 {
			t.Fatalf("two sessions sharing ONE dead credential raised %d cards, not 1 -- a second human was "+
				"asked to replace a credential somebody had already been asked about: %+v", len(got), got)
		}
		time.Sleep(testfixtures.PollInterval)
	}
}

// ---------------------------------------------------------------------------
// 2: two different credentials -> TWO cards (the regression guard)
// ---------------------------------------------------------------------------

// TestSeam_TwoSessionsDifferentCredentialsEachRaiseTheirOwnCard is the negative
// the one-card test is meaningless without: collapsing must key on the
// CREDENTIAL, never merely on "another request is already open".
//
// The fixture is byte-identical to the one-card test's -- same shared
// AgentIdentity, same class, same monitoring surface, same two sessions, same
// admin grant -- and one input differs: session B's request names the OTHER
// MCPServer, so it resolves to the other credential ON THE SAME IDENTITY. An
// implementation that collapsed on "any Open peer", or on the identity
// coordinates without the credential name, leaves the second agent's
// genuinely-different dead token with no card and no human ever asked about it,
// while every phase in the one-card test still looks right.
//
// Its own positive control is the first request: session A is asserted Open AND
// DELIVERED before session B's request is created, so the second card is
// published in the presence of a live peer card rather than in an empty world.
func TestSeam_TwoSessionsDifferentCredentialsEachRaiseTheirOwnCard(t *testing.T) {
	env := testenv.Shared(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	w := csStart(t, ctx, env)

	// --- session A holds a live card for the primary credential -------------
	first := csOpenAndDelivered(t, env, csCurA, w.sessA, csOriginPrimary)
	assert.Equal(t, itCredName, first.Status.ResolvedCredential.Credential)
	require.Len(t, w.pub.monitoringEvents(t), 1,
		"positive control: an Open, DELIVERED peer must exist when the second request reconciles, "+
			"or nothing below proves anything about keying on the credential")

	// --- session B asks about a DIFFERENT credential ------------------------
	second := csOpenAndDelivered(t, env, csCurB, w.sessB, csOriginSecond)

	// The discriminating fact: identical identity coordinates, different
	// credential name. Anything coarser than the credential swallows this card.
	assert.Equal(t, first.Status.ResolvedCredential.IdentityKind, second.Status.ResolvedCredential.IdentityKind)
	assert.Equal(t, first.Status.ResolvedCredential.Namespace, second.Status.ResolvedCredential.Namespace)
	assert.Equal(t, first.Status.ResolvedCredential.Name, second.Status.ResolvedCredential.Name,
		"the SAME shared AgentIdentity -- only the credential differs")
	assert.Equal(t, csSecondCred, second.Status.ResolvedCredential.Credential)
	assert.NotEqual(t, first.Status.ResolvedCredential.Credential, second.Status.ResolvedCredential.Credential)

	assert.Nil(t, second.Status.CollapsedInto,
		"nothing to collapse onto: no open card exists for THIS credential")
	require.NotNil(t, second.Status.CredentialSecretRef,
		"its own Open must record its own unpark baseline, or its fix can never be observed")
	assert.Equal(t, csSecondSecret, second.Status.CredentialSecretRef.Name)
	assert.NotEmpty(t, second.Status.InteractionRef,
		"a second, genuinely-different dead token must reach a human")

	// --- TWO cards, and they are about the two different credentials --------
	events := w.pub.monitoringEvents(t)
	require.Len(t, events, 2,
		"two different dead credentials are two different asks, and each needs its own card")
	summaries := []string{events[0].Summary, events[1].Summary}
	assert.True(t,
		csMentions(summaries, itCredName) && csMentions(summaries, csSecondCred),
		"the two broadcasts must name the two DIFFERENT credentials -- two cards about the SAME one would "+
			"mean the collapse simply failed to fire, not that it correctly declined to; summaries=%v", summaries)
	for _, ev := range events {
		assert.Equal(t, csIdentity, ev.Source.Name,
			"both broadcasts name the agent identity as their source -- that is the admin's blast radius")
	}

	// Both sessions park, for two genuinely separate reasons.
	csAwaitParked(t, env, csSessA, csSessB)
}

// csMentions reports whether any of the summaries contains want. Used rather
// than indexing events[0]/events[1] because the two broadcasts are published by
// a ticker and their order is not a fact this test should depend on.
func csMentions(summaries []string, want string) bool {
	for _, s := range summaries {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
