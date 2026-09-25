// pkg/controllers/credentialupdaterequest/unpark_test.go
//
// Covers reconcileOpen: the "unpark: watch the backing Secret" and "expiry:
// idle TTL elapsed" halves of Task 7. Reuses controller_test.go's fixtures
// (baseObjects, staticCredential, staticSecret, newReconciler, reconcileOnce,
// getCUR) -- same package, same file-scoped helpers.
package credentialupdaterequest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
)

// openStaticRequest drives one CredentialUpdateRequest to Open via a static
// credential the fake provider rejects, returning the client + reconciler
// for the caller to then mutate (Secret update, elapsed clock). The
// AgentClass baseObjects builds carries NO Spec.Channels at all -- every
// test in this file therefore already exercises the nil-Channels case for
// the idle-TTL default (Task 7 review, Important 3).
func openStaticRequest(t *testing.T) (client.Client, *credentialupdaterequest.Reconciler) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	cred := staticCredential()
	sess, class, suid, mcp, cur := baseObjects(cred, "github-pat")
	require.Nil(t, class.Spec.Channels, "sanity: this fixture must not set Channels, so the idle-TTL default is exercised unconditionally")
	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "first Reconcile must open the request")
	got := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase)
	require.NotNil(t, got.Status.CredentialSecretRef, "the Open transition must record the backing Secret ref")
	require.NotEmpty(t, got.Status.CredentialSecretObservedHash, "the Open transition must record the baseline hash")
	return c, r
}

// editSecretData patches the backing Secret's DATA (not just metadata),
// simulating a human replacing the credential value.
func editSecretData(t *testing.T, c client.Client, ref *spiceboxv1alpha1.NamespacedRef) {
	t.Helper()
	editSecretKey(t, c, ref, credName)
}

// editSecretKey is editSecretData with the changed KEY as a parameter, so a
// test can rewrite a credential OTHER than the one the request is about --
// impossible to express while every fixture's Secret held exactly one key.
func editSecretKey(t *testing.T, c client.Client, ref *spiceboxv1alpha1.NamespacedRef, key string) {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &sec))
	sec.Data[key] = []byte("brand-new-token-for-" + key)
	require.NoError(t, c.Update(context.Background(), &sec))
}

// otherStaticCredName is a SECOND static credential belonging to the same
// passthrough session. It is never the credential any request here is about;
// it exists only to put a second key in the projected Secret.
const otherStaticCredName = "demo-second-cred"

// openStaticRequestInAMultiKeySecret is openStaticRequest against the Secret
// shape production actually writes: the operator projects EVERY one of a
// userPassthrough session's static credentials into ONE per-session Secret,
// keyed by credential name (agentsession's materializePassthroughCredentials).
// One session with two linked credentials therefore has two keys in one Secret,
// and "the backing Secret changed" stops being the same fact as "this
// credential changed".
//
// Every other fixture in this package projects a single credential, so the
// two facts coincide in all of them and a change-detector that cannot tell them
// apart looks correct everywhere.
func openStaticRequestInAMultiKeySecret(t *testing.T) (client.Client, *credentialupdaterequest.Reconciler) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	sess, class, suid, mcp, cur := baseObjects(staticCredential(), "github-pat")
	suid.Spec.Credentials = append(suid.Spec.Credentials, spiceboxv1alpha1.AgentCredential{
		Name: otherStaticCredName,
		Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "unused-master-secret", Key: otherStaticCredName},
		},
	})
	sec := staticSecret()
	sec.Data[otherStaticCredName] = []byte("unrelated-token")

	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, sec)
	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "first Reconcile must open the request")

	got := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase, "status=%+v", got.Status)
	require.NotNil(t, got.Status.CredentialSecretRef, "the Open transition must record the backing Secret ref")
	require.NotEmpty(t, got.Status.CredentialSecretObservedHash, "the Open transition must record the baseline hash")

	// Control: the fixture really is multi-key, and really does hold BOTH the
	// credential under request and an unrelated one. Without this a green
	// "unrelated edit did not fulfil" could mean the second key was never there.
	var stored corev1.Secret
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{
		Namespace: got.Status.CredentialSecretRef.Namespace,
		Name:      got.Status.CredentialSecretRef.Name,
	}, &stored))
	require.Contains(t, stored.Data, credName, "control: the projected Secret must hold the requested credential")
	require.Contains(t, stored.Data, otherStaticCredName, "control: ...and an unrelated one, in the SAME Secret")
	return c, r
}

// TestReconcileOpen_AnUnrelatedCredentialInTheSameSecretDoesNotFulfill is the
// regression guard for "an unrelated credential closes an open request".
//
// A human linking some OTHER credential rewrites the one per-session projected
// Secret every static credential shares. A change-detector that digests the
// whole Secret reads that as "the credential was updated" and marks the open
// request Fulfilled -- so the agent is told to retry a credential nobody
// touched, retries into the identical failure having burned one of its two
// asks, and the human still looking at the real card is never told it closed.
func TestReconcileOpen_AnUnrelatedCredentialInTheSameSecretDoesNotFulfill(t *testing.T) {
	c, r := openStaticRequestInAMultiKeySecret(t)
	cur := getCUR(t, c)

	editSecretKey(t, c, cur.Status.CredentialSecretRef, otherStaticCredName)

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "second Reconcile (post unrelated-credential edit)")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"a different credential's value changing in the shared Secret is not this credential being replaced; "+
			"status=%+v", got.Status)
	assert.NotEqual(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, got.Status.Phase,
		"Fulfilled tells the agent 'the credential was updated, retry your call' -- a claim nothing here supports")
}

// TestReconcileOpen_TheRequestedCredentialInAMultiKeySecretStillFulfills is the
// positive control for the test above, against the SAME multi-key fixture:
// scoping change-detection to one key must not cost the detector its actual
// job. Without this pair, a detector that never fulfils anything would pass.
func TestReconcileOpen_TheRequestedCredentialInAMultiKeySecretStillFulfills(t *testing.T) {
	c, r := openStaticRequestInAMultiKeySecret(t)
	cur := getCUR(t, c)

	editSecretKey(t, c, cur.Status.CredentialSecretRef, credName)

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "second Reconcile (post requested-credential edit)")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, got.Status.Phase,
		"the credential this request is ABOUT changed in the shared Secret; status=%+v", got.Status)
}

// TestReconcileOpen_SecretChangeMarksFulfilled pins the unpark half: once a
// human edits the backing Secret's DATA (not just unrelated metadata), the
// next reconcile of the Open request must mark it Fulfilled. Reverting
// secretChangedSinceOpen to always return false (or comparing resourceVersion
// instead of a content hash) makes this fail.
func TestReconcileOpen_SecretChangeMarksFulfilled(t *testing.T) {
	c, r := openStaticRequest(t)
	cur := getCUR(t, c)

	editSecretData(t, c, cur.Status.CredentialSecretRef)

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "second Reconcile (post Secret edit)")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, got.Status.Phase,
		"a real content change to the backing Secret must mark the request Fulfilled")
}

// TestReconcileOpen_UnrelatedSecretMetadataEditIsNotFulfilled proves the
// dedup is on CONTENT, not resourceVersion: a label/annotation edit that
// leaves Data untouched must NOT be mistaken for a human replacing the
// credential.
func TestReconcileOpen_UnrelatedSecretMetadataEditIsNotFulfilled(t *testing.T) {
	c, r := openStaticRequest(t)
	cur := getCUR(t, c)

	sec := staticSecret()
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: cur.Status.CredentialSecretRef.Namespace, Name: cur.Status.CredentialSecretRef.Name}, sec))
	if sec.Labels == nil {
		sec.Labels = map[string]string{}
	}
	sec.Labels["unrelated"] = "touched"
	require.NoError(t, c.Update(context.Background(), sec))

	_, err := reconcileOnce(t, r)
	require.NoError(t, err)

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"an unrelated metadata edit must not be mistaken for the human replacing the credential")
}

// TestReconcileOpen_IdleTTLElapsedMarksExpired pins the expiry half: an Open
// request older than the resolved idle TTL must transition to Expired, a
// DISTINCT terminal phase from Fulfilled -- per the design, a later
// out-of-band credential update must never auto-resume a session that has
// already been abandoned this way.
func TestReconcileOpen_IdleTTLElapsedMarksExpired(t *testing.T) {
	c, r := openStaticRequest(t)
	r.IdleTTL = time.Minute
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) } // well past any created-at

	_, err := reconcileOnce(t, r)
	require.NoError(t, err)

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, got.Status.Phase)
}

// TestReconcileOpen_NoChangeNoExpiryIsANoOp proves reconcileOpen does not
// spuriously transition a request that is simply still waiting.
func TestReconcileOpen_NoChangeNoExpiryIsANoOp(t *testing.T) {
	c, r := openStaticRequest(t)
	before := getCUR(t, c)

	_, err := reconcileOnce(t, r)
	require.NoError(t, err)

	after := getCUR(t, c)
	assert.Equal(t, before.Status, after.Status, "nothing changed -- status must be byte-identical")
}

// TestReconcileOpen_SecretChangeWinsOverSimultaneousExpiry pins the Task 7
// review's reordering minor: when the backing Secret changed AND the idle
// deadline has ALSO elapsed by the same reconcile, Fulfilled must win, not
// Expired -- a human who fixes the credential right at the buzzer must not
// have their fix recorded as a timeout. Reverting reconcileOpen's check
// order (expiry before secret-change) makes this fail.
func TestReconcileOpen_SecretChangeWinsOverSimultaneousExpiry(t *testing.T) {
	c, r := openStaticRequest(t)
	cur := getCUR(t, c)

	editSecretData(t, c, cur.Status.CredentialSecretRef)
	r.IdleTTL = time.Minute
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) } // idle TTL has ALSO elapsed

	_, err := reconcileOnce(t, r)
	require.NoError(t, err)

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, got.Status.Phase,
		"a credential fix must win over a simultaneously-elapsed idle deadline")
}

// TestReconcileOpen_NeverDeliveredExpiryReasonSaysSo pins Task 7 review's
// New-Important-1: determination (this reconciler) and publication
// (channelsd, a separate process) are split, and publication has silent skip
// paths of its own -- so a request can sit Open for its entire idle TTL
// having NEVER been shown to a human (InteractionRef stays empty; that is
// exactly openStaticRequest's fixture, which channelsd never touches). The
// Expired Reason must say so plainly rather than "nobody updated the
// credential", which implies a human was asked and declined to act.
func TestReconcileOpen_NeverDeliveredExpiryReasonSaysSo(t *testing.T) {
	c, r := openStaticRequest(t)
	cur := getCUR(t, c)
	require.Empty(t, cur.Status.InteractionRef, "sanity: openStaticRequest's fixture never simulates a channelsd publish")

	r.IdleTTL = time.Minute
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	_, err := reconcileOnce(t, r)
	require.NoError(t, err)

	got := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, got.Status.Phase)
	assert.Contains(t, got.Status.Reason, "never delivered",
		"an undelivered request's Expired reason must say so, not imply a human was asked and did nothing")
}

// TestReconcileOpen_DeliveredThenExpiredReasonDiffers pins the other half of
// New-Important-1: once channelsd HAS published (InteractionRef stamped
// non-empty, exactly as CredentialUpdateWatcher.ReconcileOne does in the same
// patch as CardDelivered=True), an eventual expiry must keep the ORIGINAL
// "nobody updated it in time" wording -- a human genuinely had the chance and
// the window still elapsed, which is a materially different fact from never
// having been asked.
func TestReconcileOpen_DeliveredThenExpiredReasonDiffers(t *testing.T) {
	c, r := openStaticRequest(t)
	cur := getCUR(t, c)

	prior := cur.DeepCopy()
	cur.Status.InteractionRef = "req-fake-delivered"
	require.NoError(t, c.Status().Patch(context.Background(), cur, client.MergeFrom(prior)),
		"simulate channelsd having already published this request's card")

	r.IdleTTL = time.Minute
	r.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	_, err := reconcileOnce(t, r)
	require.NoError(t, err)

	got := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, got.Status.Phase)
	assert.Equal(t, "Nobody updated the credential before the wait window elapsed.", got.Status.Reason,
		"a DELIVERED request's expiry must keep the original wording -- a human genuinely had the chance")
}

// TestReconcile_IdleTTLDefaultsWhenReconcilerFieldUnset pins Important 3: a
// Reconciler wired with NO explicit IdleTTL (the Go zero value, exactly what
// happens if a future wiring change forgets to set it) must still expire an
// abandoned Open request using DefaultCredentialUpdateIdleTTL, rather than
// silently disabling expiry the way the old AgentClass.Spec.Channels.IdleTTL
// coupling did when Spec.Channels was nil. openStaticRequest's fixture
// AgentClass already has nil Channels, so this proves the decoupling.
func TestReconcile_IdleTTLDefaultsWhenReconcilerFieldUnset(t *testing.T) {
	c, r := openStaticRequest(t)
	require.Zero(t, r.IdleTTL, "sanity: this Reconciler must not have IdleTTL explicitly set")

	// Well past DefaultCredentialUpdateIdleTTL (30m), proving the default
	// applies rather than a 0 duration (which would mean "never expires").
	r.Now = func() time.Time {
		return time.Now().Add(credentialupdaterequest.DefaultCredentialUpdateIdleTTL + time.Hour)
	}

	_, err := reconcileOnce(t, r)
	require.NoError(t, err)

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseExpired, got.Status.Phase,
		"an unset IdleTTL must still expire via DefaultCredentialUpdateIdleTTL, not disable expiry")
}

// TestReconcileOpen_TheWaitWindowRunsFromWhenTheCardOpened pins the deadline to
// the observation it is actually about.
//
// A request is created by the meta tool and determined by this reconciler, and
// those are not the same instant: an operator restart, or twenty minutes of
// probe backoff against an unreachable provider, can sit between them. Measuring
// the wait window from CreationTimestamp then makes the deadline elapse BEFORE
// the card exists -- determination writes Open, channelsd publishes, and the very
// next reconcile expires it. The card is dead on arrival, and its expiry reason
// blames a human who had no window at all.
func TestReconcileOpen_TheWaitWindowRunsFromWhenTheCardOpened(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	installRedirectClient(t, srv)

	sess, class, suid, mcp, cur := baseObjects(staticCredential(), "github-pat")
	cur.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	c, r, _ := newReconciler(t, sess, class, suid, mcp, cur, staticSecret())
	r.IdleTTL = 30 * time.Minute

	_, err := reconcileOnce(t, r)
	require.NoError(t, err, "first Reconcile must open the request")

	opened := getCUR(t, c)
	require.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, opened.Status.Phase,
		"control: the determination must still open the card -- the pipeline has no deadline of its own; status=%+v",
		opened.Status)
	require.Greater(t, time.Since(opened.CreationTimestamp.Time), r.IdleTTL,
		"control: the request must really be older than the whole wait window, or this proves nothing")
	require.NotNil(t, opened.Status.OpenedAt, "the Open transition must record WHEN the card opened")

	// The very next reconcile -- exactly the one the status write above enqueues.
	_, err = reconcileOnce(t, r)
	require.NoError(t, err, "second Reconcile")

	got := getCUR(t, c)
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen, got.Status.Phase,
		"a card must not expire on the reconcile after it opened: nobody has had any window at all; status=%+v",
		got.Status)
	assert.Equal(t, opened.Status.OpenedAt, got.Status.OpenedAt,
		"openedAt is an observation set once when the card opened -- rewriting it every reconcile would push the "+
			"deadline forward forever and the request would never expire")
}
