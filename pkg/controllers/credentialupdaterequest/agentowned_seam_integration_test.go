//go:build integration

// pkg/controllers/credentialupdaterequest/agentowned_seam_integration_test.go
//
// The AGENT-OWNED credential-update path, end to end, across every process
// boundary the design actually splits it across -- over ONE real apiserver and
// ONE real SpiceDB.
//
//	operator   pkg/controllers/agentidentity            writes the #platform tuple
//	operator   pkg/controllers/credentialupdaterequest  decides (phase -> Open)
//	channelsd  pkg/channels/channelsd/pipeline                   broadcasts to monitoring
//	browser    pkg/web/admind/agentcred (the real client)   asserts WHO is asking
//	operator   pkg/web/admind                               CHECKS, then writes
//	operator   pkg/controllers/credentialupdaterequest  Fulfils on the change
//	operator   pkg/controllers/agentsession             unparks the session
//
// # Why this file exists at all
//
// Every prior task in this slice was verified against fake clients and fake
// checkers. That is exactly the evidence this repo has twice found
// insufficient: a feature that could not run in any cluster because a
// ServiceAccount lacked RBAC, and a card silently dropped because a recipient
// field was unset -- both with every suite green, because each component was
// correct in isolation.
//
// So nothing here is faked that can be real. The permission is answered by a
// live SpiceDB against a tuple written by the production reconciler. The write
// is performed by the production admind handler, reached over a real HTTP hop
// through the production agentcred client. The request is Fulfilled by the
// production reconciler observing the Secret the write actually moved. If any
// two of those disagree about which object they mean, this test is where it
// shows.
//
//	go test -tags=integration -count=1 ./pkg/controllers/credentialupdaterequest/
package credentialupdaterequest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	channelsdpipeline "github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	agentidentityctrl "github.com/authzed/openagentprimitives/pkg/controllers/agentidentity"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
	"github.com/authzed/openagentprimitives/test/testspicedb"

	// The monitoring Channel this scenario delivers to is of kind "fake": the
	// ONLY registered kind whose SupportsMonitoring() is true and whose
	// RequiredSecretKeys is empty, so it is a genuine recipient by the same
	// four-question test the relay applies -- no credentials Secret fixture,
	// and no "slack" special-casing.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

const (
	// aoIdentity is the agent's own identity: the shared bot credential a
	// PERSON does not own and cannot fix from their own portal.
	aoIdentity     = "demo-support-bot"
	aoSecret       = "demo-support-bot-creds"
	aoDeadValue    = "tok-dead-and-verified-dead"
	aoFreshValue   = "tok-freshly-pasted-by-an-admin"
	aoAdminToken   = "integration-admind-service-token"
	aoMonitoringCh = "demo-monitoring"
	// aoMonitoringSecret backs the monitoring Channel. The fake kind needs no
	// data keys, but the deliverability pre-check reads the Secret regardless --
	// because the relay's resolve.ForChannel does, unconditionally, and a
	// Channel whose Secret it cannot read is one it silently drops.
	aoMonitoringSecret = "demo-monitoring-creds"

	// aoAdmin holds platform:platform#admin, so update_credential resolves for
	// them through the platform->can_admin arm. aoBystander holds nothing --
	// the negative control that proves the grant is not ambient.
	//
	// Both are bare canonical ids, not emails: a SpiceDB object_id must match
	// ^(([a-zA-Z0-9/_|\-=+]{1,})|\*)$, which an email's "@" and "." do not. In
	// production the canonical form is the base64 of the email reference, which
	// satisfies the same charset.
)

// Canonical ids are struct values now, so they live in a var block rather than
// a const one.
var (
	aoAdmin     = identity.CanonicalFromTrusted("demo-platform-admin", "test fixture")
	aoBystander = identity.CanonicalFromTrusted("demo-bystander", "test fixture")
)

// aoSpiceDB boots a per-test datastore loaded with the canonical schema. Each
// test gets its own token (its own logical datastore), so one test's platform
// admin cannot leak into another's negative control.
func aoSpiceDB(t *testing.T) *spicedb.Client {
	t.Helper()
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	c, err := spicedb.NewClient(endpoint, token, true /* insecure */)
	require.NoError(t, err, "spicedb.NewClient")
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// aoReconcileAgentIdentity runs the PRODUCTION AgentIdentity reconciler once,
// which is what writes agentidentity:<ns>/<name>#platform@platform:platform.
//
// Calling spdb.EnsureAgentIdentityPlatform directly would be a strictly weaker
// test: it would prove the schema works while leaving "does anything in
// production actually write the tuple" unasserted -- which is the exact shape
// of this slice's stated failure mode.
func aoReconcileAgentIdentity(t *testing.T, env *testenv.Env, spdb *spicedb.Client, name string) {
	t.Helper()
	sr := adoptguard.NewSecretReader(env.Client, env.Client, adoptguard.Warn, func(types.NamespacedName) bool { return false })
	r := &agentidentityctrl.Reconciler{
		Client:         env.Client,
		APIReader:      env.Client,
		SecretReader:   sr,
		PlatformLinker: spdb,
	}
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: itNS, Name: name}})
	require.NoError(t, err, "AgentIdentity reconcile must succeed")
}

// aoStartOperatorAdmind serves the REAL pkg/web/admind handler -- the operator's
// half of the write path -- over a real HTTP listener, and returns the
// production agentcred client pointed at it.
//
// The client is the one webd uses, so the body this test sends is the body
// production sends: a session ref, a credential name, and a value. No identity
// ref, no Secret ref, no "already authorized" flag. Everything else is the
// operator's to determine, and this test is what proves it determines it.
func aoStartOperatorAdmind(t *testing.T, env *testenv.Env, spdb *spicedb.Client) *agentcred.Client {
	t.Helper()
	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     env.Client,
		Checker: spdb, // the LIVE SpiceDB, not a fake that always says yes
		Token:   aoAdminToken,
		Logger:  testr.New(t),
	})
	require.NoError(t, err, "admind.New")
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return agentcred.New(srv.URL, aoAdminToken)
}

// aoStartChannelsd is itStartChannelsd plus the Authz seam, which the
// agent-owned route needs and the user-owned route does not.
func aoStartChannelsd(t *testing.T, env *testenv.Env, spdb *spicedb.Client) *itCapturingPublisher {
	t.Helper()
	pub := itNewPublisher()
	w := &channelsdpipeline.CredentialUpdateWatcher{
		K8s:             env.Client,
		LinkSigner:      passthroughlink.New([]byte("integration-test-signing-key-not-for-production")),
		ExternalBaseURL: func() string { return "https://agent.example.invalid" },
		NATSPublish:     pub.publish,
		Authz:           spdb,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go w.Run(ctx)
	return pub
}

// monitoringEvents decodes every MonitoringEvent captured so far. The
// agent-owned card reaches a platform admin as a monitoring broadcast, not as
// an interaction_request: an interaction_request is routed by the outbound
// relay to the SESSION's own channel, which is the wrong audience entirely.
func (p *itCapturingPublisher) monitoringEvents(t *testing.T) []channelevents.MonitoringEvent {
	t.Helper()
	<-p.mu
	defer func() { p.mu <- struct{}{} }()
	var out []channelevents.MonitoringEvent
	for _, m := range p.messages {
		if m.subject != channelevents.MonitoringEventSubject {
			continue
		}
		var ev channelevents.MonitoringEvent
		require.NoError(t, json.Unmarshal(m.data, &ev), "decode monitoring event")
		out = append(out, ev)
	}
	return out
}

// aoMonitoringChannel is a role=monitoring Channel of a kind that can really
// deliver one. Deliverability, not existence, is what channelsd requires --
// see monitoringRecipientExists.
//
// name is parameterized because the shared envtest apiserver outlives each
// test in this file, so two tests wanting their own monitoring surface need two
// objects.
func aoMonitoringChannel(name string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: itNS},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: aoMonitoringSecret},
		},
	}
}

// aoEnsureMonitoringSecret creates the monitoring Channel's credentials Secret
// if some earlier test in this shared-apiserver package has not already.
//
// The fake kind declares no required keys, so the DATA is irrelevant; what
// matters is that the Secret EXISTS. The relay reads it unconditionally
// (resolve.ForChannel) before it can build any sender, so a monitoring Channel
// pointing at a missing Secret is one the relay drops -- and the pre-check must
// agree, or channelsd records a delivery nobody received.
func aoEnsureMonitoringSecret(t *testing.T, ctx context.Context, env *testenv.Env) {
	t.Helper()
	err := env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: aoMonitoringSecret, Namespace: itNS},
	})
	if apierrors.IsAlreadyExists(err) {
		return
	}
	require.NoError(t, err, "create the monitoring Channel's credentials Secret")
}

// aoSession is itSession with a started-by canonical the AUTHORIZATION layer
// can actually answer for.
//
// itSession stamps "user:agent-owner@example.invalid", which is fine for every
// user-owned test in this package: none of them asks SpiceDB about it. An
// agent-owned request does. spec.requestedBy is filled from the started-by
// annotation and fed straight into CheckAgentIdentityUpdateCredential, and an
// email's "@" and "." are outside SpiceDB's object_id charset (see aoAdmin).
// The check then returns InvalidArgument, turnAuthorMayUpdateCredential takes
// its ERROR branch, and THE PERMISSION IS NEVER EVALUATED -- so an "and no
// in-thread card appeared" assertion would hold no matter how badly the
// permission were over-granted.
//
// Stamping a charset-valid canonical is what makes the check actually run,
// which is what makes both the refusal below and its positive control mean
// anything at all.
func aoSession(name, class string, starter identity.CanonicalUserID) *spiceboxv1alpha1.AgentSession {
	sess := itSession(name, class, true)
	sess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] = starter.Subject().String()
	return sess
}

// aoAgentIdentity is the shared bot identity whose one static credential is
// present, non-empty, and dead. Present matters: the identity stays Valid, so
// nothing in this scenario parks for a MISSING credential and the park under
// test cannot pass for the wrong reason.
func aoAgentIdentity(name, secretName, credential string) *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: itNS},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credential, Type: "static",
				Static: &spiceboxv1alpha1.StaticCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: credential},
				},
			}},
		},
	}
}

// aoClass is itClass's agent-mode twin: the same MCPServer wiring, but the
// credential belongs to the AGENT rather than to whoever started the session.
// That single difference is what makes ResolvedCredential.IdentityKind
// "AgentIdentity" and routes everything downstream.
func aoClass(name, identityName string, mcpNames ...string) *spiceboxv1alpha1.AgentClass {
	ac := itClass(name, mcpNames...)
	ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeAgent
	ac.Spec.AgentIdentity = identityName
	return ac
}

func aoSecretValue(t *testing.T, c client.Client, name string) string {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: itNS, Name: name}, &sec),
		"get Secret %s", name)
	return string(sec.Data[itCredName])
}

// ---------------------------------------------------------------------------
// The full agent-owned path
// ---------------------------------------------------------------------------

// TestSeam_AgentOwnedCredentialReachesAnAdminWhoWritesItAndTheSessionUnparks is
// this slice's end-to-end proof, and it is deliberately ONE test: the negative
// (a bystander is refused) is only meaningful against the same live tuple, the
// same live handler, and the same live Secret that the positive then moves. Run
// as two tests with two fixtures, a bystander refusal would also pass against a
// cluster where the permission is unsatisfiable for EVERYONE -- which is
// precisely the failure this slice must not ship.
//
// Ordering is therefore load-bearing: bystander first, admin second. If the
// bystander's 403 came from a broken permission rather than a working one, the
// admin's 200 two lines later could not happen.
func TestSeam_AgentOwnedCredentialReachesAnAdminWhoWritesItAndTheSessionUnparks(t *testing.T) {
	env := testenv.Shared(t)
	spdb := aoSpiceDB(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")
	itRejectingProvider(t)

	const (
		sessName  = "ao-sess"
		className = "ao-cls"
		mcpName   = "ao-mcp"
		curName   = "ao-cur"
	)

	// --- the world -------------------------------------------------------
	require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: aoSecret, Namespace: itNS},
		Data:       map[string][]byte{itCredName: []byte(aoDeadValue)},
	}), "create the bot's backing Secret")
	require.NoError(t, env.Client.Create(ctx, aoAgentIdentity(aoIdentity, aoSecret, itCredName)), "create AgentIdentity")
	aoEnsureMonitoringSecret(t, ctx, env)
	require.NoError(t, env.Client.Create(ctx, aoMonitoringChannel(aoMonitoringCh)), "create the monitoring Channel")
	require.NoError(t, env.Client.Create(ctx, itMCPServer(mcpName)), "create MCPServer")
	ac := aoClass(className, aoIdentity, mcpName)
	require.NoError(t, env.Client.Create(ctx, ac), "create AgentClass")
	itMarkClassValid(t, ctx, env.Client, ac)

	// The grant that makes an admin an admin, and the reconcile that makes the
	// permission satisfiable at all. Both are production paths.
	require.NoError(t, spdb.TouchPlatformAdmin(ctx, aoAdmin), "grant platform admin")
	aoReconcileAgentIdentity(t, env, spdb, aoIdentity)

	// Started by the BYSTANDER: an ordinary human who does not hold
	// update_credential. That is what makes the "no in-thread card" assertion
	// below a real refusal -- the check runs and answers no -- rather than an
	// InvalidArgument the code counts as a refusal without ever asking.
	sess := aoSession(sessName, className, aoBystander)
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession")

	mem := itStartSessionOperator(t, env)
	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &live), "re-read session for UID")
	itSeedRunning(t, mem, sessName, string(live.UID))

	itStartCredentialUpdateOperator(t, env, 10*time.Minute)
	pub := aoStartChannelsd(t, env, spdb)
	operator := aoStartOperatorAdmind(t, env, spdb)

	// --- the agent's ask -------------------------------------------------
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &live), "re-read session before request")
	require.NoError(t, env.Client.Create(ctx, itRequest(curName, &live, "mcpserver/"+mcpName)), "create request")

	// --- the operator decides --------------------------------------------
	itPhaseReaches(t, env.Client, curName, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen)
	opened := itGetRequest(t, env.Client, curName)
	require.NotNil(t, opened.Status.ResolvedCredential, "an Open request must record what the origin resolved to")
	assert.Equal(t, spiceboxv1alpha1.IdentityKindAgentIdentity, opened.Status.ResolvedCredential.IdentityKind,
		"an agent-mode session's credential belongs to the AGENT; routing everything downstream depends on this")
	assert.Equal(t, aoIdentity, opened.Status.ResolvedCredential.Name)
	require.NotNil(t, opened.Status.CredentialSecretRef, "the Open transition must record the backing Secret")
	assert.Equal(t, aoSecret, opened.Status.CredentialSecretRef.Name,
		"the recorded Secret is the one the reconciler will watch for the fix; a different one means Fulfilled never fires")

	// --- channelsd broadcasts, in its own process ------------------------
	testfixtures.Eventually(t, itPollTimeout, func() bool {
		return itGetRequest(t, env.Client, curName).Status.InteractionRef != ""
	})
	delivered := itGetRequest(t, env.Client, curName)
	cardCond := apimeta.FindStatusCondition(delivered.Status.Conditions,
		spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered)
	require.NotNil(t, cardCond, "CardDelivered must be stamped alongside InteractionRef")
	assert.Equal(t, metav1.ConditionTrue, cardCond.Status)

	events := pub.monitoringEvents(t)
	require.Len(t, events, 1, "exactly one monitoring broadcast, published once")
	ev := events[0]
	assert.Equal(t, channelevents.MonitoringLevelError, ev.Level)
	assert.Equal(t, spiceboxv1alpha1.IdentityKindAgentIdentity, ev.Source.Kind,
		"the broadcast must name the AGENT IDENTITY as its source, not the session")
	assert.Equal(t, aoIdentity, ev.Source.Name)
	assert.Contains(t, ev.Summary, aoIdentity,
		"an admin must see WHICH agent identity is broken before clicking -- that is the blast radius")
	assert.Contains(t, ev.Summary, itCredName, "and which of its credentials")
	assert.Contains(t, ev.Hint, "https://agent.example.invalid",
		"and must be given the link that actually replaces it")

	// The starter is a signed-in human who simply does not hold
	// update_credential, so the LIVE check answers no and the in-thread card is
	// suppressed; the monitoring broadcast is the only surface. Asserting this
	// pins the routing split itself: an agent-owned credential that DMs the
	// session's starter is the misroute where a human pastes a live token into
	// their own personal identity and walks away believing they fixed it.
	//
	// Meaningful ONLY because aoSession stamps a charset-valid canonical -- see
	// its doc -- and because
	// TestSeam_InThreadAdminCardFollowsTheLiveUpdateCredentialCheck shows the
	// same code path DOES publish for a starter who holds the permission.
	assert.Empty(t, pub.interactionRequests(t),
		"an agent-owned credential must NOT be DM'd to a starter who cannot replace it")

	// --- the session parks ------------------------------------------------
	itAwaitCredentialUpdatePending(t, env, sessName, metav1.ConditionTrue)
	parked := itGetSession(t, env.Client, sessName)
	assert.Equal(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, parked.Status.Phase,
		"an Open request parks the session")

	// --- a bystander clicks: refused, and nothing moves -------------------
	//
	// The request body is IDENTICAL to the admin's below. The only difference
	// is the subject webd asserts -- which is exactly the assertion a
	// compromised or buggy webd controls, and exactly why the operator must not
	// take the caller's word for authorization.
	_, err := operator.Replace(ctx, identity.Subject("user:"+aoBystander.String()), agentcred.Request{
		SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: itNS, Name: sessName},
		Credential: itCredName,
		Token:      "tok-pasted-by-somebody-who-may-not",
	})
	require.Error(t, err, "a subject without agentidentity#update_credential must be refused")
	assert.ErrorIs(t, err, agentcred.ErrRefused, "and refused as a PERMISSION denial, not a fault")
	assert.Equal(t, aoDeadValue, aoSecretValue(t, env.Client, aoSecret),
		"a refused click must move nothing")
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
		itGetRequest(t, env.Client, curName).Status.Phase,
		"and must not be mistaken for a fix")

	// --- the admin clicks: authorized, and the write lands ----------------
	resp, err := operator.Replace(ctx, identity.Subject("user:"+aoAdmin.String()), agentcred.Request{
		SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: itNS, Name: sessName},
		Credential: itCredName,
		Token:      aoFreshValue,
	})
	require.NoError(t, err, "a platform admin holds agentidentity#update_credential and must be allowed; "+
		"if this is red while the bystander above was refused, the permission is unsatisfiable by ANYONE "+
		"and every credential-update card in the cluster is a dead button")
	assert.Equal(t, aoSecret, resp.SecretRef.Name,
		"the operator reports the destination IT chose, from the request's own status -- not one the caller named")
	assert.Equal(t, aoFreshValue, aoSecretValue(t, env.Client, aoSecret), "the shared bot credential is replaced")

	// --- the request Fulfils, and the session unparks ---------------------
	itPhaseReaches(t, env.Client, curName, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)
	fulfilled := itGetRequest(t, env.Client, curName)
	assert.Equal(t, opened.Status.Determination, fulfilled.Status.Determination,
		"the ORIGINAL verdict that justified the card is preserved through the fulfilment")

	itAwaitCredentialUpdatePending(t, env, sessName, metav1.ConditionFalse)
	unparked := itGetSession(t, env.Client, sessName)
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, unparked.Status.Phase,
		"with no Open request left, the session must leave the credential-update park")
	cond := apimeta.FindStatusCondition(unparked.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending)
	require.NotNil(t, cond)
	assert.Equal(t, spiceboxv1alpha1.ReasonCredentialUpdateResolved, cond.Reason)

	// Still exactly one broadcast: a fulfilled request must not be re-published.
	assert.Len(t, pub.monitoringEvents(t), 1, "no second broadcast for a request that already had one")
}

// ---------------------------------------------------------------------------
// The relationship write, against a live SpiceDB
// ---------------------------------------------------------------------------

// TestAgentOwnedWrite_IsAuthorizedByTheReconcilersOwnRelationshipWrite isolates
// the claim the test above depends on but cannot separate out: that the
// AgentIdentity reconciler's tuple is what makes the write authorizable.
//
// Same admin, same live SpiceDB, same production handler, two identities that
// differ in exactly ONE thing -- whether Reconcile ran. The unreconciled one
// must refuse the very subject the reconciled one accepts. Without that pair, a
// green happy-path proves only that SOMETHING granted the permission; it cannot
// distinguish the reconciler's write from an ambient over-grant.
func TestAgentOwnedWrite_IsAuthorizedByTheReconcilersOwnRelationshipWrite(t *testing.T) {
	env := testenv.Shared(t)
	spdb := aoSpiceDB(t)
	ctx := context.Background()

	const (
		linkedID     = "linked-bot"
		linkedSecret = "linked-bot-creds"
		unlinkedID   = "unlinked-bot"
		unlinkedSec  = "unlinked-bot-creds"
	)

	require.NoError(t, spdb.TouchPlatformAdmin(ctx, aoAdmin), "grant platform admin")

	// Two identities, two sessions, two requests -- identical but for the
	// reconcile.
	for _, f := range []struct{ id, secret, sess, cur string }{
		{linkedID, linkedSecret, "linked-sess", "linked-cur"},
		{unlinkedID, unlinkedSec, "unlinked-sess", "unlinked-cur"},
	} {
		require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: f.secret, Namespace: itNS},
			Data:       map[string][]byte{itCredName: []byte(aoDeadValue)},
		}), "create Secret %s", f.secret)
		require.NoError(t, env.Client.Create(ctx, aoAgentIdentity(f.id, f.secret, itCredName)),
			"create AgentIdentity %s", f.id)
		aoOpenAgentRequest(t, ctx, env, itSession(f.sess, "unused-class", false), f.cur, f.id, f.secret)
	}

	// ONLY the linked one is reconciled.
	aoReconcileAgentIdentity(t, env, spdb, linkedID)

	operator := aoStartOperatorAdmind(t, env, spdb)

	_, err := operator.Replace(ctx, identity.Subject("user:"+aoAdmin.String()), agentcred.Request{
		SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: itNS, Name: "linked-sess"},
		Credential: itCredName,
		Token:      aoFreshValue,
	})
	require.NoError(t, err, "after Reconcile, a platform admin MUST be able to replace this identity's credential.\n"+
		"  needed tuple: agentidentity:%s/%s#platform@platform:platform\n"+
		"If this is red, nothing in production writes the platform link and EVERY agent-owned\n"+
		"credential-update card is unactionable -- silently: schema compiles, card publishes,\n"+
		"button renders, click refused.", itNS, linkedID)
	assert.Equal(t, aoFreshValue, aoSecretValue(t, env.Client, linkedSecret))

	_, err = operator.Replace(ctx, identity.Subject("user:"+aoAdmin.String()), agentcred.Request{
		SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: itNS, Name: "unlinked-sess"},
		Credential: itCredName,
		Token:      aoFreshValue,
	})
	require.Error(t, err, "an AgentIdentity that was never reconciled has no platform link, so the SAME admin "+
		"must be refused -- this is what proves the reconciler's write is load-bearing, not incidental")
	assert.ErrorIs(t, err, agentcred.ErrRefused)
	assert.Equal(t, aoDeadValue, aoSecretValue(t, env.Client, unlinkedSec),
		"and a refusal must leave the Secret alone")
}

// aoOpenAgentRequest creates sess and a CredentialUpdateRequest already at Open
// with an agent-owned resolved credential, and returns the live request.
//
// The admind route resolves its target from that status, so the tests here need
// a request in exactly the state the reconciler leaves one in -- without running
// the whole determination path again for a claim that is purely about
// authorization. sess is passed in rather than built here because the two
// callers need different sessions: one channel-less, one channel-attached with a
// chosen starter.
func aoOpenAgentRequest(t *testing.T, ctx context.Context, env *testenv.Env,
	sess *spiceboxv1alpha1.AgentSession, curName, identityName, secretName string,
) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	sess.Spec.AgentIdentity = identityName
	require.NoError(t, env.Client.Create(ctx, sess), "create AgentSession %s", sess.Name)

	var live spiceboxv1alpha1.AgentSession
	require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(sess), &live), "re-read session %s", sess.Name)

	cur := itRequest(curName, &live, "mcpserver/unused")
	require.NoError(t, env.Client.Create(ctx, cur), "create request %s", curName)
	cur.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen
	cur.Status.Determination = spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified
	cur.Status.Reason = "the provider rejected the current value"
	cur.Status.ResolvedCredential = &spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: spiceboxv1alpha1.IdentityKindAgentIdentity,
		Namespace:    itNS, Name: identityName, Credential: itCredName,
	}
	cur.Status.CredentialSecretRef = &spiceboxv1alpha1.NamespacedRef{Namespace: itNS, Name: secretName}
	require.NoError(t, env.Client.Status().Update(ctx, cur), "stamp request %s Open", curName)
	return cur
}

// ---------------------------------------------------------------------------
// The in-thread card follows the LIVE permission, in both directions
// ---------------------------------------------------------------------------

// TestSeam_InThreadAdminCardFollowsTheLiveUpdateCredentialCheck is the pair the
// seam test's `assert.Empty(interactionRequests)` needs to be worth anything.
//
// A lone negative cannot distinguish "the check ran and said no" from "the
// check never ran": turnAuthorMayUpdateCredential is fail-closed in FOUR
// branches, three of which suppress the card without ever asking SpiceDB. Two
// rows differing only in whether the starter holds platform admin -- against
// one live SpiceDB, one live #platform tuple written by the production
// reconciler, and one live watcher -- pin that the CHECK is what decides.
//
// Mutation sensitivity: over-grant update_credential (drop the platform arm's
// requirement, or make the check return true) and the "does not hold it" row
// goes red. Suppress the in-thread surface entirely and the "holds it" row goes
// red. Regress the starter's canonical to an email-shaped subject -- the bug
// this test was added for -- and the "holds it" row goes red too, because the
// InvalidArgument that used to masquerade as a refusal now has to face a case
// where a card is REQUIRED.
func TestSeam_InThreadAdminCardFollowsTheLiveUpdateCredentialCheck(t *testing.T) {
	cases := []struct {
		name       string
		starter    identity.CanonicalUserID
		grantAdmin bool
		wantCard   bool
	}{
		{
			name:       "starter holds update_credential: the in-thread card IS published to them",
			starter:    aoAdmin,
			grantAdmin: true,
			wantCard:   true,
		},
		{
			name:       "starter does not: no in-thread card, and the check is what refused it",
			starter:    aoBystander,
			grantAdmin: false,
			wantCard:   false,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.Shared(t)
			spdb := aoSpiceDB(t) // its own datastore: one row's grant cannot leak into the other
			ctx := context.Background()

			// Distinct names per row: the apiserver is shared across this
			// package, so a reused name would collide with the sibling row.
			suffix := fmt.Sprintf("-%d", i)
			identityName := "inthread-bot" + suffix
			secretName := "inthread-bot-creds" + suffix

			if tc.grantAdmin {
				require.NoError(t, spdb.TouchPlatformAdmin(ctx, tc.starter), "grant platform admin")
			}

			require.NoError(t, env.Client.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: itNS},
				Data:       map[string][]byte{itCredName: []byte(aoDeadValue)},
			}), "create the bot's backing Secret")
			require.NoError(t, env.Client.Create(ctx, aoAgentIdentity(identityName, secretName, itCredName)),
				"create AgentIdentity")

			// The production reconciler writes the tuple, so a green "holds it"
			// row proves the WHOLE chain -- reconciler write, live schema, live
			// check -- not just that a fake said yes.
			aoReconcileAgentIdentity(t, env, spdb, identityName)

			// A deliverable monitoring surface, so the monitoring broadcast
			// always lands and the ONLY thing varying between rows is the
			// in-thread card. Without it a refused row would take the
			// no-recipient error path and stop being a clean comparison.
			aoEnsureMonitoringSecret(t, ctx, env)
			require.NoError(t, env.Client.Create(ctx, aoMonitoringChannel("inthread-monitor"+suffix)),
				"create the monitoring Channel")

			cur := aoOpenAgentRequest(t, ctx, env,
				aoSession("inthread-sess"+suffix, "unused-class", tc.starter),
				"inthread-cur"+suffix, identityName, secretName)

			pub := itNewPublisher()
			w := &channelsdpipeline.CredentialUpdateWatcher{
				K8s:             env.Client,
				LinkSigner:      passthroughlink.New([]byte("integration-test-signing-key-not-for-production")),
				ExternalBaseURL: func() string { return "https://agent.example.invalid" },
				NATSPublish:     pub.publish,
				Authz:           spdb, // the LIVE SpiceDB, not a fake that always answers the same way
			}
			require.NoError(t, w.ReconcileOne(ctx, cur), "the monitoring surface is deliverable, so this must succeed")

			require.Len(t, pub.monitoringEvents(t), 1,
				"the monitoring broadcast is the constant in both rows; if it is missing, the rows are not comparable")

			cards := pub.interactionRequests(t)
			if !tc.wantCard {
				assert.Empty(t, cards,
					"the starter does not hold agentidentity#update_credential, so the in-thread card must be suppressed")
				return
			}
			require.Len(t, cards, 1,
				"a starter who DOES hold agentidentity#update_credential must be given the in-thread card; "+
					"if this is empty the check is refusing everyone and the negative row above proves nothing")
			assert.Equal(t, categories.CredentialUpdate, cards[0].Category)
			require.NotNil(t, cards[0].Audience.Requester, "the card must be addressed to the author, not broadcast")
			assert.Equal(t, identity.RawExternalID(itExtID), cards[0].Audience.Requester.ExternalID,
				"addressed to the starter through the channel identity the session recorded")
		})
	}
}
