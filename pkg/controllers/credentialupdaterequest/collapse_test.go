package credentialupdaterequest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Collapsing is what stops five sessions sharing one dead credential raising
// five cards at the same human. It keys on the CREDENTIAL -- ownership plus the
// backing Secret's (namespace, name, key) -- never on the identity CR a request
// happened to reach it through.
//
// That distinction is the whole reason both shapes are covered here. An
// AgentIdentity is shared by construction: many sessions name the same CR, so
// its coordinates and its credential's coordinates agree, and any keying at all
// collapses them. A SessionUserIdentity is the opposite: it is named after its
// OWN session, so two sessions of the SAME person hold two differently-named
// SUIDs while resolving one master Secret (type=oauth) or per-session copies of
// one (type=static). Keying on the identity CR's name therefore made collapsing
// structurally impossible for every user-owned credential -- and since Determine
// refuses agent-owned oauth outright, that is every oauth card the feature
// raises. The passthrough fixtures at the bottom of this file exist so that
// case is representable at all; the AgentIdentity fixtures above them cover the
// shape where two requests may look alike without being one ask.
const (
	sharedIdentityName = "demo-shared-identity"
	sharedSecretName   = "demo-shared-secret"

	// The SECOND credential on the SAME AgentIdentity. It exists so the
	// different-credential regression guard can hold the identity constant and
	// vary ONLY the credential name -- an implementation that collapsed on
	// "same identity" (or merely on "another Open request exists") would
	// wrongly collapse these two, and that is exactly what that test catches.
	otherCredName   = "demo-other-cred"
	otherMCPName    = "demo-other-mcp"
	otherSecretName = "demo-other-secret"

	sessionAName = "demo-session-a"
	sessionBName = "demo-session-b"
	curAName     = "demo-cur-a"
	curBName     = "demo-cur-b"
)

const (
	sessionAUID types.UID = "demo-session-a-uid"
	sessionBUID types.UID = "demo-session-b-uid"
)

// ownerRefFor is baseObjects' ownerRef() parameterized by session, since this
// file needs several sessions rather than the one fixed fixture session. The
// UID is what resolveIdentity's ownedBySession check compares, so a request
// whose ownerRef UID does not match its spec.sessionRef's session refuses
// before ever resolving a credential -- getting this wrong would make every
// test here pass for the wrong reason.
func ownerRefFor(sessionName string, uid types.UID) metav1.OwnerReference {
	tval := true
	return metav1.OwnerReference{
		APIVersion:         spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
		Kind:               "AgentSession",
		Name:               sessionName,
		UID:                uid,
		Controller:         &tval,
		BlockOwnerDeletion: &tval,
	}
}

// sharedWorld builds the objects every test in this file starts from: one
// identityMode=agent AgentClass, ONE AgentIdentity carrying TWO static
// credentials, one MCPServer per credential, and the backing Secrets. The
// sessions and their requests are added per-test on top.
func sharedWorld() []client.Object {
	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: className, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeAgent},
	}
	ai := &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: sharedIdentityName, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: []spiceboxv1alpha1.AgentCredential{
				{
					Name: credName, Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: sharedSecretName, Key: credName},
					},
				},
				{
					Name: otherCredName, Type: "static",
					Static: &spiceboxv1alpha1.StaticCredentialSource{
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: otherSecretName, Key: otherCredName},
					},
				},
			},
		},
	}
	mcpA := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: mcpName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: credName, Provider: "github-pat"},
		},
	}
	mcpB := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: otherMCPName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: otherCredName, Provider: "github-pat"},
		},
	}
	secA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedSecretName, Namespace: ns},
		Data:       map[string][]byte{credName: []byte("tok-shared")},
	}
	secB := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: otherSecretName, Namespace: ns},
		Data:       map[string][]byte{otherCredName: []byte("tok-other")},
	}
	return []client.Object{class, ai, mcpA, mcpB, secA, secB}
}

// agentModeSession is a session of sharedWorld's class, running as the SHARED
// AgentIdentity. Two of these are what "two sessions, one bot token" means.
func agentModeSession(name string, uid types.UID) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className, AgentIdentity: sharedIdentityName},
	}
}

// requestFor builds a fresh (undecided) CredentialUpdateRequest owned by the
// named session. CreationTimestamp is stamped for the same reason baseObjects
// does it: the fake client does not auto-stamp it, and reconcileOpen's idle-TTL
// math is computed from it, so a zero value makes every Open request look
// already-expired.
func requestFor(name, sessionName string, uid types.UID, mcp string) *spiceboxv1alpha1.CredentialUpdateRequest {
	return &spiceboxv1alpha1.CredentialUpdateRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			OwnerReferences:   []metav1.OwnerReference{ownerRefFor(sessionName, uid)},
			CreationTimestamp: metav1.Now(),
		},
		Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
			SessionRef:  spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sessionName},
			Origin:      "mcpserver/" + mcp,
			ToolName:    "demo_tool",
			RequestedBy: identity.Subject("user:demo-user"),
		},
	}
}

// stampSharedCredential records, on a SEEDED peer's status, everything the
// reconciler itself writes when a request about sharedWorld's shared credential
// opens a card: the resolved identity coordinates AND the backing Secret
// coordinates (namespace, name, key) the collapse election keys on.
//
// Both halves are load-bearing. A fixture stamping only the resolved credential
// describes a request that never opened a card at all, and the election
// correctly skips it -- so a "did not collapse" assertion would pass for a
// reason that has nothing to do with the property under test.
func stampSharedCredential(st *spiceboxv1alpha1.CredentialUpdateRequestStatus) {
	st.ResolvedCredential = &spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: spiceboxv1alpha1.IdentityKindAgentIdentity,
		Namespace:    ns, Name: sharedIdentityName, Credential: credName,
	}
	st.CredentialSecretRef = &spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: sharedSecretName}
	st.CredentialSecretKey = credName
}

// sharedCredHandle is the collapse key sharedWorld's shared credential resolves
// to, built the way the reconciler builds it (the handle type is unexported, so
// its Go type is only nameable by inference).
var sharedCredHandle = credentialupdaterequest.CredentialHandleForTest(
	spiceboxv1alpha1.IdentityKindAgentIdentity,
	spiceboxv1alpha1.CredentialSource{Type: "static", Namespace: ns, Name: sharedSecretName, Key: credName},
)

// rejectingProbe stands up the provider probe every test here shares: a
// definitive 401, which is what drives a request all the way to Open
// (RejectedVerified). The returned counter is the POSITIVE CONTROL these
// tests lean on -- it counts full determinations that actually reached the
// live probe, so "the follower published no card" can be told apart from
// "the follower never ran at all".
func rejectingProbe(t *testing.T) *atomic.Int32 {
	t.Helper()
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)
	return &probes
}

// reconcileNamed reconciles one request and RETURNS the ctrl.Result. The result
// is not incidental: a follower's own RequeueAfter is the only wakeup it is
// guaranteed, so a helper that discarded it made "this request will be looked
// at again" unobservable to every test in the package.
func reconcileNamed(t *testing.T, r *credentialupdaterequest.Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	require.NoError(t, err, "Reconcile %s", name)
	return res
}

func getNamed(t *testing.T, c client.Client, name string) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	var got spiceboxv1alpha1.CredentialUpdateRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &got),
		"Get CredentialUpdateRequest %s", name)
	return &got
}

// TestReconcile_CollapsesOntoAnOpenRequestForTheSameCredential is the slice's
// headline behaviour: two sessions, one shared bot token, ONE card.
//
// The negative claim ("no second card") is the one most at risk of being
// vacuous -- it passes trivially against a setup where the second request
// never resolved anything at all. Three positive controls in this same test
// stop that:
//
//   - the canonical really did reach Open with a card-worthy determination, so
//     the fixture demonstrably produces cards;
//   - the probe counter is >0 after the canonical and UNCHANGED after the
//     follower, so the follower's pipeline ran up to the collapse and stopped
//     THERE rather than never starting;
//   - the follower's own status.resolvedCredential is populated and names the
//     SAME credential, so it genuinely resolved and was collapsed -- it was not
//     refused early (NoCredential, unowned session, ...) for some unrelated
//     reason that would make the whole test prove nothing.
func TestReconcile_CollapsesOntoAnOpenRequestForTheSameCredential(t *testing.T) {
	probes := rejectingProbe(t)

	objs := sharedWorld()
	objs = append(objs,
		agentModeSession(sessionAName, sessionAUID), requestFor(curAName, sessionAName, sessionAUID, mcpName),
		agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, mcpName),
	)
	c, r, _ := newReconciler(t, objs...)

	reconcileNamed(t, r, curAName)
	canonical := getNamed(t, c, curAName)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, canonical.Status.Phase,
		"positive control: the fixture must really produce a card; status=%+v", canonical.Status)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified, canonical.Status.Determination)
	probesAfterCanonical := probes.Load()
	require.Positive(t, probesAfterCanonical,
		"positive control: the canonical's full determination really did run the live probe")

	res := reconcileNamed(t, r, curBName)
	follower := getNamed(t, c, curBName)

	// The collapse itself must schedule the follower's first recheck. Relying on
	// the watch event this very status write generates would leave a follower
	// whose event was dropped waiting on the manager's 10h SyncPeriod -- hours
	// past any wait window, with nothing else able to wake it.
	assert.Positive(t, res.RequeueAfter,
		"collapsing must start the recheck loop from the write that created the follower")

	require.NotNil(t, follower.Status.ResolvedCredential,
		"positive control: the follower must have RESOLVED the credential -- an early refusal here would make "+
			"the no-card claim vacuous; status=%+v", follower.Status)
	assert.Equal(t, credName, follower.Status.ResolvedCredential.Credential)
	assert.Equal(t, spiceboxv1alpha1.IdentityKindAgentIdentity, follower.Status.ResolvedCredential.IdentityKind)
	assert.Equal(t, sharedIdentityName, follower.Status.ResolvedCredential.Name,
		"both sessions resolve the SAME AgentIdentity -- that shared identity is what makes them one ask")

	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
		"the second request must collapse, not open; status=%+v", follower.Status)
	assert.NotEqual(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, follower.Status.Phase,
		"Open is the ONLY phase channelsd publishes on, so 'no second card' is exactly 'never reaches Open'")
	require.NotNil(t, follower.Status.CollapsedInto, "the follower must record WHICH request it is waiting on")
	assert.Equal(t, spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: curAName}, *follower.Status.CollapsedInto)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationCollapsed, follower.Status.Determination)
	assert.NotEmpty(t, follower.Status.Reason, "no-silent-errors: a collapse must still say why nothing was published")

	assert.Nil(t, follower.Status.CredentialSecretRef,
		"a follower records no unpark baseline -- it has no card of its own to unpark")
	assert.Equal(t, probesAfterCanonical, probes.Load(),
		"the follower must collapse BEFORE the live probe: N sessions on one dead token must not mean N probes")

	// The canonical is untouched by the follower's reconcile: one writer per
	// field, follower set derived by listing rather than mirrored onto the
	// canonical (which is why there is no attachedSessions list to check here).
	stillCanonical := getNamed(t, c, curAName)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, stillCanonical.Status.Phase)
	assert.Nil(t, stillCanonical.Status.CollapsedInto, "the canonical must never become a follower of its own follower")
}

// TestReconcile_DifferentCredentialStillPublishesItsOwnCard is THE regression
// guard the brief calls out by name: collapsing must key on the CREDENTIAL,
// never merely on "another request is already open".
//
// The two requests here are as close as two requests can get without being the
// same ask: same namespace, same AgentClass, same shared AgentIdentity, same
// provider, both reaching the same verdict. They differ in exactly one field of
// ResolvedCredentialRef -- Credential. An implementation that collapsed on
// "any Open peer", or on the identity coordinates without the credential name,
// leaves the second agent's genuinely-different dead token with no card at all
// and no human ever asked about it.
func TestReconcile_DifferentCredentialStillPublishesItsOwnCard(t *testing.T) {
	probes := rejectingProbe(t)

	objs := sharedWorld()
	objs = append(objs,
		agentModeSession(sessionAName, sessionAUID), requestFor(curAName, sessionAName, sessionAUID, mcpName),
		// Session B's failing tool is the OTHER MCPServer, which resolves to the
		// other credential on the SAME identity.
		agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, otherMCPName),
	)
	c, r, _ := newReconciler(t, objs...)

	reconcileNamed(t, r, curAName)
	first := getNamed(t, c, curAName)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, first.Status.Phase,
		"positive control: an Open peer MUST exist when the second request reconciles, or this proves nothing "+
			"about keying on the credential; status=%+v", first.Status)
	require.NotNil(t, first.Status.ResolvedCredential)

	reconcileNamed(t, r, curBName)
	second := getNamed(t, c, curBName)

	require.NotNil(t, second.Status.ResolvedCredential, "status=%+v", second.Status)
	// The discriminating fact: identical identity coordinates, different
	// credential name. Collapsing on anything coarser than the credential
	// would have swallowed this card.
	assert.Equal(t, first.Status.ResolvedCredential.IdentityKind, second.Status.ResolvedCredential.IdentityKind)
	assert.Equal(t, first.Status.ResolvedCredential.Namespace, second.Status.ResolvedCredential.Namespace)
	assert.Equal(t, first.Status.ResolvedCredential.Name, second.Status.ResolvedCredential.Name,
		"same shared AgentIdentity -- only the credential differs")
	assert.Equal(t, otherCredName, second.Status.ResolvedCredential.Credential)
	assert.NotEqual(t, first.Status.ResolvedCredential.Credential, second.Status.ResolvedCredential.Credential)

	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, second.Status.Phase,
		"a different credential is a different ask and gets its OWN card; status=%+v", second.Status)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified, second.Status.Determination)
	assert.Nil(t, second.Status.CollapsedInto, "nothing to collapse onto: no open card exists for THIS credential")
	require.NotNil(t, second.Status.CredentialSecretRef,
		"its own Open must record its own unpark baseline")
	assert.Equal(t, otherSecretName, second.Status.CredentialSecretRef.Name)

	assert.Equal(t, int32(2), probes.Load(),
		"both requests must have run their own full determination -- two credentials, two probes, two cards")
}

// credentialKey projects a ResolvedCredentialRef onto the four fields that
// IDENTIFY a credential -- exactly the four the reconciler's own
// sameCredential compares. ProviderID is deliberately excluded: it is display
// metadata (the card's title and icon), so folding it into the key would make
// a provider-catalog edit look like a different credential and silently stop
// two genuinely-shared asks from collapsing.
func credentialKey(ref *spiceboxv1alpha1.ResolvedCredentialRef) spiceboxv1alpha1.ResolvedCredentialRef {
	return spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: ref.IdentityKind,
		Namespace:    ref.Namespace,
		Name:         ref.Name,
		Credential:   ref.Credential,
	}
}

// TestReconcile_PeerPhaseDecidesCollapse pins that only a LIVE card absorbs
// another ask. Every row shares one fixture and varies exactly one thing: the
// phase of a pre-existing peer request that resolved to the IDENTICAL
// credential.
//
// The Open row is the positive control for the other three: it proves the
// fixture's peer really does match on credential, so a terminal row that
// publishes normally is doing so BECAUSE of the phase and not because the
// peer was never a candidate in the first place.
func TestReconcile_PeerPhaseDecidesCollapse(t *testing.T) {
	sharedRef := spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: spiceboxv1alpha1.IdentityKindAgentIdentity,
		Namespace:    ns,
		Name:         sharedIdentityName,
		Credential:   credName,
	}

	cases := []struct {
		name          string
		peerPhase     string
		wantCollapsed bool
	}{
		{
			name:          "peer Open: a live card absorbs the second ask (control -- proves the peer really matches)",
			peerPhase:     spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			wantCollapsed: true,
		},
		{
			name:          "peer Fulfilled: a settled request is not a live card, so this one publishes normally",
			peerPhase:     spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled,
			wantCollapsed: false,
		},
		{
			name:          "peer Refused: a settled request is not a live card, so this one publishes normally",
			peerPhase:     spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused,
			wantCollapsed: false,
		},
		{
			name:          "peer Expired: a settled request is not a live card, so this one publishes normally",
			peerPhase:     spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired,
			wantCollapsed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probes := rejectingProbe(t)

			peer := requestFor(curAName, sessionAName, sessionAUID, mcpName)
			peer.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
				Phase:         tc.peerPhase,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
				Reason:        "seeded",
			}
			stampSharedCredential(&peer.Status)

			objs := sharedWorld()
			objs = append(objs,
				agentModeSession(sessionAName, sessionAUID), peer,
				agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, mcpName),
			)
			c, r, _ := newReconciler(t, objs...)

			// Positive control, asserted for EVERY row: the seeded peer resolved
			// the same credential this request is about to resolve. Without this,
			// a "publishes normally" row could be green because the peer never
			// matched at all -- the vacuous shape this file exists to avoid.
			seeded := getNamed(t, c, curAName)
			require.NotNil(t, seeded.Status.ResolvedCredential)
			assert.Equal(t, sharedRef, credentialKey(seeded.Status.ResolvedCredential),
				"control: the peer must be a genuine candidate on credential, so only its PHASE can decide")

			reconcileNamed(t, r, curBName)
			got := getNamed(t, c, curBName)

			require.NotNil(t, got.Status.ResolvedCredential, "status=%+v", got.Status)
			assert.Equal(t, sharedRef, credentialKey(got.Status.ResolvedCredential),
				"control: this request resolved the very credential the peer holds")

			if tc.wantCollapsed {
				assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, got.Status.Phase,
					"status=%+v", got.Status)
				require.NotNil(t, got.Status.CollapsedInto)
				assert.Equal(t, curAName, got.Status.CollapsedInto.Name)
				assert.Zero(t, probes.Load(), "a collapse must never reach the live probe")
				return
			}
			assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
				"a settled peer is not a live card; status=%+v", got.Status)
			assert.Nil(t, got.Status.CollapsedInto)
			assert.Positive(t, probes.Load(),
				"publishing normally means the full determination ran, probe included")
		})
	}
}

// TestReconcile_FollowerNeverLooksDelivered guards failure mode 2. A follower
// has no card, so it must carry none of delivery's marks: slice 1's expiry
// branch reads status.interactionRef to say "never delivered to a human"
// instead of "nobody acted", and the CardDelivered condition is the same
// signal in condition form. A follower that stamped either would make both
// messages lie about a human who was never asked.
//
// The canonical here is pre-stamped with BOTH marks -- which is exactly what
// channelsd does once it publishes -- and that is this test's positive
// control: it proves the assertions below can actually SEE those marks on a
// CredentialUpdateRequest, so finding neither on the follower is a fact about
// the follower and not about a finder that never works.
func TestReconcile_FollowerNeverLooksDelivered(t *testing.T) {
	rejectingProbe(t)

	const canonicalRef = "cur-interaction-ref-123"
	canonical := requestFor(curAName, sessionAName, sessionAUID, mcpName)
	canonical.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase:          spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
		Determination:  spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
		Reason:         "seeded",
		InteractionRef: canonicalRef,
		Conditions: []metav1.Condition{{
			Type:               spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered,
			Status:             metav1.ConditionTrue,
			Reason:             spiceboxv1alpha1.ReasonCredentialUpdateCardDelivered,
			LastTransitionTime: metav1.Now(),
		}},
	}
	stampSharedCredential(&canonical.Status)

	objs := sharedWorld()
	objs = append(objs,
		agentModeSession(sessionAName, sessionAUID), canonical,
		agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, mcpName),
	)
	c, r, _ := newReconciler(t, objs...)

	// Positive control: a delivered request in this fixture really does carry
	// both marks, and both are visible through the very accessors used below.
	seeded := getNamed(t, c, curAName)
	require.Equal(t, canonicalRef, seeded.Status.InteractionRef, "control: delivery marks are visible on a delivered request")
	require.NotNil(t, meta.FindStatusCondition(seeded.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered),
		"control: the condition finder really does find a CardDelivered condition when one is present")

	reconcileNamed(t, r, curBName)
	follower := getNamed(t, c, curBName)

	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
		"control: this request really did take the collapse path; status=%+v", follower.Status)
	require.NotNil(t, follower.Status.CollapsedInto)

	assert.Empty(t, follower.Status.InteractionRef,
		"a follower has no card: stamping interactionRef makes the expiry branch claim a human was asked")
	assert.Nil(t, meta.FindStatusCondition(follower.Status.Conditions, spiceboxv1alpha1.CredentialUpdateRequestConditionCardDelivered),
		"a follower must carry no delivered condition either -- the two signals must never disagree")
}

// TestReconcile_SimultaneousRequestsElectExactlyOneCanonical is the property
// the brief singles out as easiest to get subtly wrong: two requests landing in
// the same reconcile window must not each conclude the OTHER is canonical.
// That outcome -- two followers, no card, both agents blocked until their wait
// windows elapse -- is strictly worse than the duplicate cards this slice sets
// out to remove, because at least a duplicate card gets somebody asked.
//
// Both orderings are exercised because canonical selection must be stable
// under either, and each row reconciles a SECOND time to pin that the election
// does not oscillate: the same request stays canonical, the same one stays a
// follower, and no additional determination runs.
func TestReconcile_SimultaneousRequestsElectExactlyOneCanonical(t *testing.T) {
	cases := []struct {
		name  string
		order []string
	}{
		{name: "A reconciles first: exactly one canonical, one follower", order: []string{curAName, curBName}},
		{name: "B reconciles first: exactly one canonical, one follower", order: []string{curBName, curAName}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probes := rejectingProbe(t)

			objs := sharedWorld()
			objs = append(objs,
				agentModeSession(sessionAName, sessionAUID), requestFor(curAName, sessionAName, sessionAUID, mcpName),
				agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, mcpName),
			)
			c, r, _ := newReconciler(t, objs...)

			for _, name := range tc.order {
				reconcileNamed(t, r, name)
			}

			a, b := getNamed(t, c, curAName), getNamed(t, c, curBName)
			canonical, follower := electedPair(t, a, b)

			assert.Equal(t, tc.order[0], canonical.Name,
				"whichever reconciles first holds the card; the other follows")
			assert.Nil(t, canonical.Status.CollapsedInto, "the canonical must not be a follower of anything")
			require.NotNil(t, follower.Status.CollapsedInto)
			assert.Equal(t, canonical.Name, follower.Status.CollapsedInto.Name,
				"the follower must point at the canonical, not at itself or at nothing")
			assert.NotEqual(t, follower.Name, follower.Status.CollapsedInto.Name, "no request may collapse onto itself")

			// Positive control: exactly ONE full determination ran. Two would
			// mean both opened; zero would mean neither request was ever
			// processed and the whole assertion set above is vacuous.
			assert.Equal(t, int32(1), probes.Load(),
				"exactly one full determination -- one card, at one human")

			// Stability: re-reconciling both must not re-elect, re-probe, or flip
			// the pair. A follower re-reading a still-Open canonical is a no-op.
			for _, name := range tc.order {
				reconcileNamed(t, r, name)
			}
			a2, b2 := getNamed(t, c, curAName), getNamed(t, c, curBName)
			canonical2, follower2 := electedPair(t, a2, b2)
			assert.Equal(t, canonical.Name, canonical2.Name, "the election must be stable across reconciles")
			assert.Equal(t, follower.Name, follower2.Name)
			assert.Equal(t, follower.Status, follower2.Status, "a follower's status must be idempotent")
			assert.Equal(t, int32(1), probes.Load(), "a second pass must not re-run the determination")
		})
	}
}

// TestReconcile_CanonicalElectionAmongSeveralOpenPeers covers the two election
// rules nothing else can reach, because the ordinary path never produces more
// than one Open peer for a credential -- the first request to reconcile Opens
// and every later one collapses.
//
// Both rules exist for the degraded case they leave behind: two cards opened
// before either observed the other (a redeploy overlap, or a future
// MaxConcurrentReconciles > 1). What must not happen then is followers
// SPLITTING across two canonicals, because the follower set is derived by
// listing who points at whom -- a split set means a canonical settles and only
// some of its followers are ever woken.
//
// Each row seeds two Open peers resolving the IDENTICAL credential and asserts
// which one a third request attaches to. The names are chosen so the correct
// answer is NOT the one a reversed comparison or a deleted skip would pick.
func TestReconcile_CanonicalElectionAmongSeveralOpenPeers(t *testing.T) {
	sharedRef := spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: spiceboxv1alpha1.IdentityKindAgentIdentity,
		Namespace:    ns, Name: sharedIdentityName, Credential: credName,
	}
	// Deliberately ordered so lowest-name and highest-name give DIFFERENT
	// answers: a reversed tie-break picks curHighName instead of curLowName.
	const curLowName, curHighName = "demo-cur-aaa", "demo-cur-zzz"

	openPeer := func(name string, collapsedInto *spiceboxv1alpha1.NamespacedRef) *spiceboxv1alpha1.CredentialUpdateRequest {
		peer := requestFor(name, sessionAName, sessionAUID, mcpName)
		peer.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
			Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
			Reason:        "seeded",
			CollapsedInto: collapsedInto,
		}
		stampSharedCredential(&peer.Status)
		return peer
	}

	cases := []struct {
		name          string
		peers         []*spiceboxv1alpha1.CredentialUpdateRequest
		wantCanonical string
	}{
		{
			// Reversing `item.Name < canonical.Name` reddens exactly this row.
			name:          "two clean Open peers: every follower picks the lexicographically smallest, so none can split",
			peers:         []*spiceboxv1alpha1.CredentialUpdateRequest{openPeer(curLowName, nil), openPeer(curHighName, nil)},
			wantCanonical: curLowName,
		},
		{
			// Deleting the `CollapsedInto != nil` skip reddens exactly this row:
			// the bad peer sorts FIRST, so the tie-break would hand it the
			// election and start a chain -- a follower of a follower, whose
			// eventual settlement propagates to nobody.
			name: "an Open peer that is itself a follower is skipped, even though it sorts first",
			peers: []*spiceboxv1alpha1.CredentialUpdateRequest{
				openPeer(curLowName, &spiceboxv1alpha1.NamespacedRef{Namespace: ns, Name: "demo-cur-somebody-else"}),
				openPeer(curHighName, nil),
			},
			wantCanonical: curHighName,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probes := rejectingProbe(t)

			objs := sharedWorld()
			objs = append(objs, agentModeSession(sessionAName, sessionAUID))
			for _, p := range tc.peers {
				objs = append(objs, p)
			}
			objs = append(objs,
				agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, mcpName))
			c, r, _ := newReconciler(t, objs...)

			// Positive control for EVERY row: both seeded peers really are Open
			// and really resolve the credential this request is about to resolve.
			// Without it a row could pass because a peer never matched at all.
			for _, p := range tc.peers {
				seeded := getNamed(t, c, p.Name)
				require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, seeded.Status.Phase,
					"control: %s must be a live card", p.Name)
				require.NotNil(t, seeded.Status.ResolvedCredential)
				require.Equal(t, sharedRef, credentialKey(seeded.Status.ResolvedCredential),
					"control: %s must be a genuine candidate on credential", p.Name)
			}

			reconcileNamed(t, r, curBName)
			follower := getNamed(t, c, curBName)

			require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
				"control: with a live card for this credential the request must collapse; status=%+v", follower.Status)
			require.NotNil(t, follower.Status.CollapsedInto)
			assert.Equal(t, tc.wantCanonical, follower.Status.CollapsedInto.Name,
				"every follower must compute the SAME canonical, or the follower set splits and a settled card "+
					"wakes only some of the sessions riding on it")
			assert.Zero(t, probes.Load(), "a collapse must never reach the live probe")
		})
	}
}

// TestCanonicalOpenRequestFor_TieBreakIsLowestNameRegardlessOfListOrder pins
// property (4) against the one input the reconciler can never hand it: peers in
// an order other than sorted-by-name.
//
// Every route into the election from a reconcile goes through a List, and the
// fake client returns List results NAME-SORTED. Under that ordering "the first
// matching peer" and "the lowest-named matching peer" are the same request, so
// deleting the tie-break entirely leaves every end-to-end test green -- only
// REVERSING the comparison ever reddens one. A cache-backed List in production
// makes no ordering promise at all, and the tie-break is what stops two
// followers of one credential attaching to two DIFFERENT canonicals (after
// which a settled card wakes only half the sessions riding on it).
//
// So the peers here are fed in DESCENDING name order, which is exactly the
// ordering under which first-match-wins and lowest-name-wins disagree.
func TestCanonicalOpenRequestFor_TieBreakIsLowestNameRegardlessOfListOrder(t *testing.T) {
	openPeer := func(name string) spiceboxv1alpha1.CredentialUpdateRequest {
		peer := requestFor(name, sessionAName, sessionAUID, mcpName)
		peer.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
			Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
			Reason:        "seeded",
		}
		stampSharedCredential(&peer.Status)
		return *peer
	}

	// Descending, so peers[0] is the WRONG answer under a tie-break that has
	// been deleted rather than reversed.
	peers := []spiceboxv1alpha1.CredentialUpdateRequest{
		openPeer("demo-cur-zzz"), openPeer("demo-cur-mmm"), openPeer("demo-cur-aaa"),
	}
	require.Greater(t, peers[0].Name, peers[len(peers)-1].Name,
		"control: the peers must really be fed in descending order, or first-match and lowest-name agree "+
			"and this test proves nothing")

	got := credentialupdaterequest.CanonicalOpenRequestForTest(peers, sharedCredHandle)

	require.NotNil(t, got, "three Open peers on the credential must elect one of them")
	assert.Equal(t, "demo-cur-aaa", got.Name,
		"every follower must compute the SAME canonical from the same set whatever order it is listed in; "+
			"taking the first match instead splits the follower set across two canonicals")
}

// TestReconcile_SameCredentialNameUnderADifferentIdentityKindDoesNotCollapse
// pins the IdentityKind clause of the collapse key, whose hazard is easy to
// miss because every other AgentIdentity fixture in this file uses one identity
// kind and so cannot tell the clause from a no-op.
//
// The peer here names the IDENTICAL backing Secret coordinates -- same
// namespace, same Secret, same key -- and differs only in whose credential it
// is. Drop the ownership clause and the two become "one ask". The consequences
// run both ways: one human is asked to replace a credential that is not theirs,
// and the session that really has a dead token gets no card at all and is told
// to keep waiting on an ask about something else entirely. Ownership is also
// what decides which gate authorizes the click and which object the replacement
// is written to, so it can never be inferred from the Secret alone.
func TestReconcile_SameCredentialNameUnderADifferentIdentityKindDoesNotCollapse(t *testing.T) {
	probes := rejectingProbe(t)

	// An Open peer identical to what session B will resolve in every coordinate
	// EXCEPT ownership: same Secret namespace, same Secret name, same key.
	peer := requestFor(curAName, sessionAName, sessionAUID, mcpName)
	peer.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
		Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
		Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
		Reason:        "seeded",
	}
	stampSharedCredential(&peer.Status)
	peer.Status.ResolvedCredential.IdentityKind = spiceboxv1alpha1.IdentityKindSessionUserIdentity

	objs := sharedWorld()
	objs = append(objs,
		agentModeSession(sessionAName, sessionAUID), peer,
		agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, mcpName),
	)
	c, r, _ := newReconciler(t, objs...)

	reconcileNamed(t, r, curBName)
	got := getNamed(t, c, curBName)
	seeded := getNamed(t, c, curAName)

	// Positive controls: the peer really is Open (a live card, so phase is not
	// what turned it away), and the two differ in ownership and ONLY in
	// ownership -- every Secret coordinate the election keys on is shared.
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, seeded.Status.Phase,
		"control: a live card must exist, or 'did not collapse' proves nothing; status=%+v", seeded.Status)
	require.NotNil(t, got.Status.ResolvedCredential, "status=%+v", got.Status)
	require.NotNil(t, seeded.Status.ResolvedCredential)
	require.NotNil(t, got.Status.CredentialSecretRef, "status=%+v", got.Status)
	require.NotNil(t, seeded.Status.CredentialSecretRef)
	require.Equal(t, spiceboxv1alpha1.IdentityKindAgentIdentity, got.Status.ResolvedCredential.IdentityKind)
	require.NotEqual(t, seeded.Status.ResolvedCredential.IdentityKind, got.Status.ResolvedCredential.IdentityKind,
		"control: the two must differ in ownership -- that is the field under test")
	require.Equal(t, *seeded.Status.CredentialSecretRef, *got.Status.CredentialSecretRef,
		"control: the two must name the SAME backing Secret, so ownership is the ONLY discriminator left")
	require.Equal(t, seeded.Status.CredentialSecretKey, got.Status.CredentialSecretKey,
		"control: ...and the same key within it")

	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"a credential belonging to somebody else is a different ask and gets its own card; status=%+v", got.Status)
	assert.Nil(t, got.Status.CollapsedInto,
		"collapsing here would ask one human to replace somebody else's credential, and leave this session "+
			"waiting on an answer about a different one")
	assert.Positive(t, probes.Load(),
		"publishing normally means the full determination ran, probe included")
}

// TestReconcile_CollapseReasonOnlyClaimsAHumanWasAskedWhenTheCanonicalWasDelivered
// pins the honesty gate on the reason a FOLLOWER is written with.
//
// Determination and publication are split across two processes: this reconciler
// writes Open, and channelsd puts the card in front of somebody -- with its own
// silent skip paths. So a canonical sitting Open is NOT evidence anybody has
// been asked; only its interactionRef is. Writing "a human has already been
// asked" onto a follower of an undelivered canonical is the same false claim
// slice 1 built the interactionRef discriminator to remove, one indirection
// further out.
//
// The two rows differ in exactly one field of the seeded canonical, and each
// asserts the OTHER row's wording is absent -- either half alone would pass
// against a gate wired to a constant.
func TestReconcile_CollapseReasonOnlyClaimsAHumanWasAskedWhenTheCanonicalWasDelivered(t *testing.T) {
	sharedRef := spiceboxv1alpha1.ResolvedCredentialRef{
		IdentityKind: spiceboxv1alpha1.IdentityKindAgentIdentity,
		Namespace:    ns, Name: sharedIdentityName, Credential: credName,
	}

	cases := []struct {
		name string
		// delivered stamps the canonical's interactionRef, exactly as channelsd
		// does in the same patch as CardDelivered=True.
		delivered    bool
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "canonical WAS delivered: the follower may say a human has already been asked",
			delivered:    true,
			wantContains: []string{"A human has already been asked to replace this exact credential"},
			wantAbsent:   []string{"not been confirmed delivered"},
		},
		{
			name:      "canonical is Open but undelivered: nobody has been asked, so the claim must be weaker",
			delivered: false,
			wantContains: []string{
				"Another session has already raised a request to replace this exact credential",
				"not been confirmed delivered to a human",
			},
			wantAbsent: []string{"A human has already been asked"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probes := rejectingProbe(t)

			canonical := requestFor(curAName, sessionAName, sessionAUID, mcpName)
			canonical.Status = spiceboxv1alpha1.CredentialUpdateRequestStatus{
				Phase:         spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationRejectedVerified,
				Reason:        "seeded",
			}
			stampSharedCredential(&canonical.Status)
			if tc.delivered {
				canonical.Status.InteractionRef = "cur-interaction-ref-123"
			}

			objs := sharedWorld()
			objs = append(objs,
				agentModeSession(sessionAName, sessionAUID), canonical,
				agentModeSession(sessionBName, sessionBUID), requestFor(curBName, sessionBName, sessionBUID, mcpName),
			)
			c, r, _ := newReconciler(t, objs...)

			// Positive control: the seeded canonical is a genuine candidate on
			// credential AND carries exactly the delivery mark this row set --
			// so the reason below varies with delivery and nothing else.
			seeded := getNamed(t, c, curAName)
			require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, seeded.Status.Phase)
			require.NotNil(t, seeded.Status.ResolvedCredential)
			require.Equal(t, sharedRef, credentialKey(seeded.Status.ResolvedCredential))
			require.Equal(t, tc.delivered, seeded.Status.InteractionRef != "",
				"control: the canonical's delivery mark must be exactly what this row set")

			reconcileNamed(t, r, curBName)
			follower := getNamed(t, c, curBName)

			require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
				"control: this request must really have collapsed, or the reason under test was never written; "+
					"status=%+v", follower.Status)
			assert.Empty(t, follower.Status.InteractionRef,
				"whatever the canonical's delivery state, the follower still has no card of its own")
			assert.Zero(t, probes.Load(), "a collapse must never reach the live probe")

			for _, want := range tc.wantContains {
				assert.Contains(t, follower.Status.Reason, want, "reason=%q", follower.Status.Reason)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, follower.Status.Reason, absent,
					"this row's reason must not borrow the other row's claim; reason=%q", follower.Status.Reason)
			}
		})
	}
}

// electedPair asserts that exactly one of the two requests is Open and exactly
// one is Collapsed, and returns them in that order. Doing this as one helper
// (rather than two independent phase assertions) is what makes "two followers"
// and "two canonicals" BOTH failures rather than one of them slipping through
// as an unasserted combination.
func electedPair(t *testing.T, a, b *spiceboxv1alpha1.CredentialUpdateRequest) (canonical, follower *spiceboxv1alpha1.CredentialUpdateRequest) {
	t.Helper()
	switch {
	case a.Status.Phase == spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen &&
		b.Status.Phase == spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed:
		return a, b
	case b.Status.Phase == spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen &&
		a.Status.Phase == spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed:
		return b, a
	}
	t.Fatalf("expected exactly one Open canonical and one Collapsed follower, got %s=%q and %s=%q",
		a.Name, a.Status.Phase, b.Name, b.Status.Phase)
	return nil, nil
}

// --- user-owned credentials -------------------------------------------------
//
// Everything above resolves an AgentIdentity, which many sessions name by
// construction. A SessionUserIdentity is the opposite shape and the one this
// section exists to make representable: it is named after its OWN session, so
// two sessions of the SAME person hold two differently-named identity CRs while
// resolving one credential. Keying collapse on the identity CR therefore could
// never see them as one ask, and since Determine refuses agent-owned oauth
// outright, that covered every OAuth card the feature raises.

const (
	passClassName    = "demo-pass-class"
	passMCPName      = "demo-pass-mcp"
	passSessionAName = "demo-pass-session-a"
	passSessionBName = "demo-pass-session-b"
	passCurAName     = "demo-pass-cur-a"
	passCurBName     = "demo-pass-cur-b"
	// One person's connection, one master Secret in the identities namespace.
	passSharedMaster = "demo-user-oauth-master"
	// A DIFFERENT person's connection to the same provider.
	passOtherMaster = "demo-other-user-oauth-master"
)

const (
	passSessionAUID types.UID = "demo-pass-session-a-uid"
	passSessionBUID types.UID = "demo-pass-session-b-uid"
)

// passthroughSession is a userPassthrough-mode session. Its SessionUserIdentity
// is named after it (BuildSessionUserIdentity), which is the coordinate collapse
// must NOT key on.
func passthroughSession(name string, uid types.UID) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: uid},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: passClassName},
	}
}

// passthroughSUID is the per-session projection of a person's catalog, carrying
// one type=oauth credential backed by masterSecret. Two sessions of the same
// person get two of these -- different CR names, identical credential.
func passthroughSUID(sessionName, subject, masterSecret string) *spiceboxv1alpha1.SessionUserIdentity {
	return &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: sessionName, Namespace: ns},
		Spec: spiceboxv1alpha1.SessionUserIdentitySpec{
			AgentSession: sessionName,
			UserIdentity: subject,
			Subject:      "user:" + subject,
			Credentials: []spiceboxv1alpha1.AgentCredential{{
				Name: credName, Type: "oauth",
				OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
					SecretRef: spiceboxv1alpha1.SecretRef{Name: masterSecret},
				},
			}},
		},
	}
}

// passthroughWorld builds two userPassthrough sessions and their requests. The
// two master Secret names decide whether the same person is behind both.
func passthroughWorld(t *testing.T, masterA, masterB string) []client.Object {
	t.Helper()
	tokenEndpoint := installDeadRefreshTokenEndpoint(t)

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: passClassName, Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{IdentityMode: spiceboxv1alpha1.IdentityModeUserPassthrough},
	}
	mcp := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: passMCPName, Namespace: ns},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Auth: spiceboxv1alpha1.MCPServerAuth{Credential: credName},
		},
	}
	objs := []client.Object{class, mcp,
		passthroughSession(passSessionAName, passSessionAUID),
		passthroughSUID(passSessionAName, "demo-user-one", masterA),
		requestFor(passCurAName, passSessionAName, passSessionAUID, passMCPName),
		passthroughSession(passSessionBName, passSessionBUID),
		passthroughSUID(passSessionBName, "demo-user-two", masterB),
		requestFor(passCurBName, passSessionBName, passSessionBUID, passMCPName),
		deadOAuthMasterSecret(masterA, tokenEndpoint),
	}
	if masterB != masterA {
		objs = append(objs, deadOAuthMasterSecret(masterB, tokenEndpoint))
	}
	return objs
}

// TestReconcile_TwoPassthroughSessionsOnOneConnectionCollapse is the case
// collapsing was structurally unable to reach.
//
// Both sessions run userPassthrough and both resolve the SAME master Secret --
// the same person's dead connection. Their identity CRs are named after their
// own sessions, so their identity coordinates DIFFER, which is asserted below
// rather than assumed: that difference is precisely what a collapse keyed on
// the identity CR saw, and why it published a second card and DM'd the same
// person twice about one connection.
func TestReconcile_TwoPassthroughSessionsOnOneConnectionCollapse(t *testing.T) {
	objs := passthroughWorld(t, passSharedMaster, passSharedMaster)
	c, r, _ := newReconciler(t, objs...)

	reconcileNamed(t, r, passCurAName)
	canonical := getNamed(t, c, passCurAName)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, canonical.Status.Phase,
		"positive control: the fixture must really produce a card; status=%+v", canonical.Status)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateDeterminationRejectedRefreshDead, canonical.Status.Determination,
		"control: the dead-refresh verdict is what opens a user-owned OAuth card")

	res := reconcileNamed(t, r, passCurBName)
	follower := getNamed(t, c, passCurBName)

	require.NotNil(t, follower.Status.ResolvedCredential,
		"positive control: the follower must have RESOLVED the credential -- an early refusal would make the "+
			"no-second-card claim vacuous; status=%+v", follower.Status)
	require.NotNil(t, canonical.Status.ResolvedCredential)

	// The discriminating fact: the two requests are about ONE credential while
	// their identity coordinates disagree.
	assert.Equal(t, spiceboxv1alpha1.IdentityKindSessionUserIdentity, follower.Status.ResolvedCredential.IdentityKind)
	assert.NotEqual(t, canonical.Status.ResolvedCredential.Name, follower.Status.ResolvedCredential.Name,
		"control: each SessionUserIdentity is named after its own session, so the identity coordinates MUST "+
			"differ -- keying collapse on them is what made this case unreachable")
	require.NotNil(t, canonical.Status.CredentialSecretRef)
	assert.Equal(t, spiceboxv1alpha1.IdentitiesNamespace, canonical.Status.CredentialSecretRef.Namespace)
	assert.Equal(t, passSharedMaster, canonical.Status.CredentialSecretRef.Name,
		"control: ...while the backing Secret is the same one, because it is the same connection")

	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed, follower.Status.Phase,
		"one person, one dead connection, one card; status=%+v", follower.Status)
	require.NotNil(t, follower.Status.CollapsedInto, "the follower must record WHICH request it is waiting on")
	assert.Equal(t, passCurAName, follower.Status.CollapsedInto.Name)
	assert.Positive(t, res.RequeueAfter, "collapsing must start the follower's recheck loop")
}

// TestReconcile_TwoPassthroughSessionsOnDIFFERENTConnectionsDoNotCollapse is
// the test above's indispensable counterweight: keying on the backing Secret
// must not become "every passthrough OAuth request is one ask".
//
// Two people, two connections to the same provider, two master Secrets. Each
// person can only fix their own, so each must get their own card -- collapsing
// here would leave one person's session waiting on a card shown to somebody
// else, about a credential they cannot replace.
func TestReconcile_TwoPassthroughSessionsOnDIFFERENTConnectionsDoNotCollapse(t *testing.T) {
	objs := passthroughWorld(t, passSharedMaster, passOtherMaster)
	c, r, _ := newReconciler(t, objs...)

	reconcileNamed(t, r, passCurAName)
	first := getNamed(t, c, passCurAName)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, first.Status.Phase,
		"positive control: an Open peer MUST exist when the second request reconciles, or this proves nothing; "+
			"status=%+v", first.Status)

	reconcileNamed(t, r, passCurBName)
	second := getNamed(t, c, passCurBName)

	require.NotNil(t, first.Status.CredentialSecretRef)
	require.NotNil(t, second.Status.CredentialSecretRef, "status=%+v", second.Status)
	require.NotNil(t, first.Status.ResolvedCredential)
	require.NotNil(t, second.Status.ResolvedCredential)
	// Control: identical in every coordinate collapse could confuse -- same
	// ownership, same Secret namespace, same credential name -- and differing
	// only in the Secret that actually holds the token.
	require.Equal(t, first.Status.ResolvedCredential.IdentityKind, second.Status.ResolvedCredential.IdentityKind)
	require.Equal(t, first.Status.CredentialSecretRef.Namespace, second.Status.CredentialSecretRef.Namespace)
	require.Equal(t, first.Status.ResolvedCredential.Credential, second.Status.ResolvedCredential.Credential)
	require.NotEqual(t, first.Status.CredentialSecretRef.Name, second.Status.CredentialSecretRef.Name,
		"control: two people's connections are two Secrets -- that is the field under test")

	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, second.Status.Phase,
		"a different person's connection is a different ask and gets its own card; status=%+v", second.Status)
	assert.Nil(t, second.Status.CollapsedInto,
		"collapsing here would leave this session waiting on a card shown to somebody who cannot fix its credential")
}
