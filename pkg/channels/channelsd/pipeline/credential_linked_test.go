package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	// Registers the static/oauth/federated credkind.Kinds so
	// credentialFingerprint's credkindregistry.Get(c.Type) dispatch resolves
	// in this package's tests.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

// --- fixtures -----------------------------------------------------------

const (
	linkedTestSubject = "user:" + "alice"
	linkedTestSession = "sess-aaaa1111"
)

// staticCred builds an AgentCredential with type=static referencing the
// named Secret.
func staticCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name,
		Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
		},
	}
}

// oauthCred builds an AgentCredential with type=oauth referencing the
// named Secret.
func oauthCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name,
		Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
		},
	}
}

// fixtureUser returns a UserIdentity for the given canonical subject.
func fixtureUser(name, subject string, creds ...spiceboxv1alpha1.AgentCredential) *spiceboxv1alpha1.UserIdentity {
	return &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     subject,
			Credentials: creds,
		},
	}
}

// fixtureLinkedSession returns an AgentSession whose started-by
// annotation matches subject. createdAt lets a test order multiple
// sessions for "most recent wins" assertions.
func fixtureLinkedSession(name, subject string, createdAt time.Time) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNS,
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: testChannelName,
				spiceboxv1alpha1.LabelChannelKind: testKindName,
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: subject,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: testClass,
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: testChannelName,
				Kind: testKindName,
				Key:  "dm:" + name,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseRunning,
		},
	}
}

// newLinkedWatcher wires a CredentialLinkedWatcher against a fake k8s client
// + a fresh capturingPublisher standing in for NATSPublish, the watcher's only
// delivery path (interaction_applied on .out).
func newLinkedWatcher(t *testing.T, objs ...client.Object) (*CredentialLinkedWatcher, client.Client, *capturingPublisher) {
	t.Helper()
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialLinkedWatcher{
		K8s:         cli,
		NATSPublish: pub.publish,
	}
	return w, cli, pub
}

// interactionApplieds decodes every published KindInteractionApplied
// envelope's payload.
func (p *capturingPublisher) interactionApplieds(t *testing.T) []channelevents.InteractionAppliedPayload {
	t.Helper()
	envs := p.envelopesOfKind(channelevents.KindInteractionApplied)
	out := make([]channelevents.InteractionAppliedPayload, len(envs))
	for i, env := range envs {
		require.NoError(t, json.Unmarshal(env.Payload, &out[i]), "unmarshal InteractionAppliedPayload[%d]", i)
	}
	return out
}

// findInteractionApplied is interactionApplieds asserting there is exactly
// one.
func (p *capturingPublisher) findInteractionApplied(t *testing.T) channelevents.InteractionAppliedPayload {
	t.Helper()
	applied := p.interactionApplieds(t)
	require.Len(t, applied, 1, "expected exactly one KindInteractionApplied published")
	return applied[0]
}

// countInteractionApplieds reports how many KindInteractionApplied
// envelopes have been published — used in place of the old
// resolver.CallCount / sender.sendCount().
func (p *capturingPublisher) countInteractionApplieds() int {
	return len(p.envelopesOfKind(channelevents.KindInteractionApplied))
}

// primeUser inserts an entry in w.observed so that subsequent
// ReconcileOne calls see prior state matching the supplied credentials.
// Bypasses the "first sighting = no emit" priming path.
func primeUser(w *CredentialLinkedWatcher, userName string, creds []spiceboxv1alpha1.AgentCredential) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.observed == nil {
		w.observed = make(map[string]observedCredentials)
	}
	w.observed[userName] = fingerprintCredentials(context.Background(), creds)
}

// --- ADDED ---------------------------------------------------------------

func TestCredentialLinkedWatcher_BrandNewCredentialEmits(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	user := fixtureUser("alice-uid", linkedTestSubject)
	w, _, pub := newLinkedWatcher(t, user, sess)

	// Prime explicitly with empty creds so the user is "known" but had
	// no credentials before this scan.
	primeUser(w, user.Name, nil)

	// Now mutate the in-memory user to include one credential and
	// reconcile.
	user.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{
		staticCred("linear-oauth", "linear-secret"),
	}
	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")

	require.Equal(t, 1, pub.countInteractionApplieds(), "exactly one envelope")
	pl := pub.findInteractionApplied(t)
	assert.Equal(t, categories.CredentialLink, pl.Category, "Category")
	assert.Equal(t, channelevents.OutcomeResolved, pl.Outcome, "Outcome")
	assert.Equal(t, "linear-oauth", pl.RequestRef, "RequestRef = credential name")
	assert.Equal(t, "linear-oauth", pl.OutcomeText, "OutcomeText = credential name")
	assert.Equal(t, testNS, pl.AgentSessionRef.Namespace, "AgentSessionRef.Namespace")
	assert.Equal(t, linkedTestSession, pl.AgentSessionRef.Name, "AgentSessionRef.Name")
}

// --- REPLACED ------------------------------------------------------------

func TestCredentialLinkedWatcher_ReplacedCredentialEmits(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	user := fixtureUser("alice-uid", linkedTestSubject,
		staticCred("linear-oauth", "linear-old"),
	)
	w, _, pub := newLinkedWatcher(t, user, sess)

	// Prime with the OLD credential.
	primeUser(w, user.Name, user.Spec.Credentials)

	// Replace the credential's secretRef (different Secret, same name).
	user.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{
		staticCred("linear-oauth", "linear-new"),
	}
	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")

	require.Equal(t, 1, pub.countInteractionApplieds(), "expected one envelope for REPLACED credential")
	pl := pub.findInteractionApplied(t)
	assert.Equal(t, "linear-oauth", pl.RequestRef, "RequestRef = credential name")
}

// TestCredentialLinkedWatcher_TypeChangeEmits verifies that a same-name
// credential migrating between static and oauth (different
// discriminator block) counts as REPLACED.
func TestCredentialLinkedWatcher_TypeChangeEmits(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	user := fixtureUser("alice-uid", linkedTestSubject,
		staticCred("linear-cred", "linear-static"),
	)
	w, _, pub := newLinkedWatcher(t, user, sess)
	primeUser(w, user.Name, user.Spec.Credentials)

	user.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{
		oauthCred("linear-cred", "linear-oauth-secret"),
	}
	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")
	require.Equal(t, 1, pub.countInteractionApplieds(), "type-change emits one envelope")
}

// --- REMOVED -------------------------------------------------------------

func TestCredentialLinkedWatcher_RemovedCredentialDoesNotEmit(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	user := fixtureUser("alice-uid", linkedTestSubject)
	w, _, pub := newLinkedWatcher(t, user, sess)

	// Prime with one credential, then reconcile against an empty list.
	primeUser(w, user.Name, []spiceboxv1alpha1.AgentCredential{
		staticCred("linear-oauth", "linear-secret"),
	})

	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")

	assert.Equal(t, 0, pub.countInteractionApplieds(), "REMOVED credentials must NOT emit (portal owns the revoke UX)")

	// Snapshot updated: a subsequent re-add must emit.
	user.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{
		staticCred("linear-oauth", "linear-secret-2"),
	}
	require.NoError(t, w.ReconcileOne(context.Background(), user), "second ReconcileOne (re-add)")
	require.Equal(t, 1, pub.countInteractionApplieds(), "re-adding the credential emits as ADDED")
}

// --- Unchanged -----------------------------------------------------------

func TestCredentialLinkedWatcher_UnchangedNoEmit(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	user := fixtureUser("alice-uid", linkedTestSubject,
		staticCred("linear-oauth", "linear-secret"),
	)
	w, _, pub := newLinkedWatcher(t, user, sess)

	primeUser(w, user.Name, user.Spec.Credentials)

	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")
	assert.Equal(t, 0, pub.countInteractionApplieds(), "no emit when credentials unchanged")

	// And again — still no emit.
	require.NoError(t, w.ReconcileOne(context.Background(), user), "second ReconcileOne")
	assert.Equal(t, 0, pub.countInteractionApplieds(), "still no emit on idempotent reconcile")
}

// --- First-scan priming --------------------------------------------------

func TestCredentialLinkedWatcher_FirstScanDoesNotEmit(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	user := fixtureUser("alice-uid", linkedTestSubject,
		staticCred("github-cred", "gh-secret"),
		staticCred("linear-cred", "linear-secret"),
		oauthCred("notion-cred", "notion-secret"),
	)
	w, cli, pub := newLinkedWatcher(t, user, sess)

	// primeObserved is the production path the polling loop calls at
	// startup; verify it loads the existing 3 creds.
	require.NoError(t, w.primeObserved(context.Background()), "primeObserved")

	// First reconcile after prime → no diff, no emit.
	var fresh spiceboxv1alpha1.UserIdentity
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Name: user.Name}, &fresh), "Get user")
	require.NoError(t, w.ReconcileOne(context.Background(), &fresh), "ReconcileOne after prime")

	assert.Equal(t, 0, pub.countInteractionApplieds(), "first scan after prime must NOT emit any envelopes")
}

// TestCredentialLinkedWatcher_NewUserPostStartupDoesNotEmit verifies the
// "first sighting = no emit" path for a UserIdentity that appears AFTER
// channelsd's prime has run. Otherwise every new identityd-created
// UserIdentity would emit on its first scan (the user's initial
// credential was added by identityd between two ticks; the watcher
// should treat the whole identity as a baseline, not a delta).
func TestCredentialLinkedWatcher_NewUserPostStartupDoesNotEmit(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	w, _, pub := newLinkedWatcher(t, sess)

	// Prime with zero users — startup state.
	require.NoError(t, w.primeObserved(context.Background()), "primeObserved")

	// Now a user appears (identityd just created it with an initial cred).
	user := fixtureUser("alice-uid", linkedTestSubject,
		staticCred("linear-oauth", "linear-secret"),
	)
	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")

	assert.Equal(t, 0, pub.countInteractionApplieds(), "first sighting of a new UserIdentity must NOT emit")

	// A subsequent ADD against the same user emits normally.
	user.Spec.Credentials = append(user.Spec.Credentials,
		staticCred("github-oauth", "gh-secret"))
	require.NoError(t, w.ReconcileOne(context.Background(), user), "second ReconcileOne")
	require.Equal(t, 1, pub.countInteractionApplieds(), "subsequent ADD on a known user emits")
	pl := pub.findInteractionApplied(t)
	assert.Equal(t, "github-oauth", pl.RequestRef, "second envelope is for the new credential")
}

// --- No recent session ---------------------------------------------------

func TestCredentialLinkedWatcher_NoRecentSessionLogsAndSkips(t *testing.T) {
	// User exists with a credential, but NO AgentSession matches its
	// started-by canonical.
	user := fixtureUser("alice-uid", linkedTestSubject,
		staticCred("linear-oauth", "linear-secret"),
	)
	w, _, pub := newLinkedWatcher(t, user)
	// Prime as empty so the credential reads as ADDED.
	primeUser(w, user.Name, nil)

	err := w.ReconcileOne(context.Background(), user)
	require.NoError(t, err, "missing session is a log + skip, not an error")

	assert.Equal(t, 0, pub.countInteractionApplieds(), "no envelope emitted when no session matches")
}

// --- Multiple users in parallel -----------------------------------------

func TestCredentialLinkedWatcher_MultipleUsersScanEmitsPerUser(t *testing.T) {
	const bobSubject = "user:bob"
	bobSession := "sess-bbbb2222"

	aliceSess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	bobSess := fixtureLinkedSession(bobSession, bobSubject, time.Now())

	alice := fixtureUser("alice-uid", linkedTestSubject,
		staticCred("linear-oauth", "linear-alice"),
	)
	bob := fixtureUser("bob-uid", bobSubject,
		staticCred("github-oauth", "gh-bob"),
	)
	w, _, pub := newLinkedWatcher(t, alice, bob, aliceSess, bobSess)

	// Both users are KNOWN (primed empty) — the credentials they
	// already carry should ALL read as ADDED.
	primeUser(w, alice.Name, nil)
	primeUser(w, bob.Name, nil)

	// Drive the full scan (one tick).
	w.scan(context.Background(), discardLogger(t))

	require.Equal(t, 2, pub.countInteractionApplieds(), "one envelope per user")

	// Build a set keyed by (session, credentialName) so order is
	// independent of scan's internal iteration.
	type key struct{ session, cred string }
	got := make(map[key]bool, 2)
	for _, pl := range pub.interactionApplieds(t) {
		got[key{pl.AgentSessionRef.Name, pl.RequestRef}] = true
	}
	assert.True(t, got[key{linkedTestSession, "linear-oauth"}], "Alice's envelope")
	assert.True(t, got[key{bobSession, "github-oauth"}], "Bob's envelope")
}

// TestCredentialLinkedWatcher_MultipleCredentialsSameScan verifies that a
// user gaining two credentials in the gap between two scans gets one
// envelope per added credential.
func TestCredentialLinkedWatcher_MultipleCredentialsSameScan(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())

	user := fixtureUser("alice-uid", linkedTestSubject)
	w, _, pub := newLinkedWatcher(t, user, sess)
	primeUser(w, user.Name, nil)

	user.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{
		staticCred("github-oauth", "gh-secret"),
		staticCred("linear-oauth", "linear-secret"),
	}
	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")

	applied := pub.interactionApplieds(t)
	require.Len(t, applied, 2, "one envelope per ADDED credential in a single scan")

	// Iteration order is deterministic (sorted by name) — github first.
	assert.Equal(t, "github-oauth", applied[0].RequestRef, "first envelope (sorted)")
	assert.Equal(t, "linear-oauth", applied[1].RequestRef, "second envelope (sorted)")
}

// TestCredentialLinkedWatcher_MostRecentSessionWins verifies that when a
// user has multiple AgentSessions, the envelope routes to the one with
// the latest CreationTimestamp.
func TestCredentialLinkedWatcher_MostRecentSessionWins(t *testing.T) {
	earlier := fixtureLinkedSession("sess-old", linkedTestSubject,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	later := fixtureLinkedSession("sess-new", linkedTestSubject,
		time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))

	user := fixtureUser("alice-uid", linkedTestSubject)
	w, _, pub := newLinkedWatcher(t, user, earlier, later)
	primeUser(w, user.Name, nil)

	user.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{
		staticCred("linear-oauth", "linear-secret"),
	}
	require.NoError(t, w.ReconcileOne(context.Background(), user), "ReconcileOne")
	require.Equal(t, 1, pub.countInteractionApplieds(), "exactly one envelope")

	pl := pub.findInteractionApplied(t)
	assert.Equal(t, "sess-new", pl.AgentSessionRef.Name,
		"envelope routes via the most-recent session")
}

// TestCredentialLinkedWatcher_NilNATSPublishFailsLoud pins the wiring-bug
// guard: emit requires NATSPublish, its only delivery path, so a nil publisher
// must return a loud error rather than silently do nothing — AND still advance
// the snapshot, so the same credential change isn't re-tried (and re-erroring)
// on every tick.
func TestCredentialLinkedWatcher_NilNATSPublishFailsLoud(t *testing.T) {
	sess := fixtureLinkedSession(linkedTestSession, linkedTestSubject, time.Now())
	user := fixtureUser("alice-uid", linkedTestSubject)
	w, _, _ := newLinkedWatcher(t, user, sess)
	w.NATSPublish = nil // simulate the wiring bug directly
	primeUser(w, user.Name, nil)

	user.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{
		staticCred("linear-oauth", "linear-secret"),
	}
	err := w.ReconcileOne(context.Background(), user)
	require.Error(t, err, "ReconcileOne must error when NATSPublish is nil")
	assert.Contains(t, err.Error(), "NATSPublish not configured", "error names the wiring gap")

	// Snapshot advanced despite the error — a second reconcile with the
	// same credentials is a no-op (matches reconcileUser's existing
	// trade-off: advance-after-decision favors at-most-one attempt over a
	// wedged retry loop).
	err = w.ReconcileOne(context.Background(), user)
	require.NoError(t, err, "unchanged credentials after the failed attempt is a no-op, not a retry")
}

// --- credentialFingerprint registry dispatch ------------------------------

// TestCredentialFingerprint_UnregisteredTypeLogsAndDegrades is the R15-shaped
// case for the `switch c.Type { case "static": ...; case "oauth": ... }` →
// registry-dispatch migration: for a known type, both the old switch and the
// new registry.Get(...).SecretRef(...) dispatch land on the exact same
// "<type>:<secretRefName>" string, so no test built only from static/oauth
// fixtures can tell old and new code apart (a revert would pass identically).
// An UNREGISTERED type is where they diverge: the fingerprint STRING stays
// "<type>:" in both (deliberately — see credentialFingerprint's doc), but the
// NEW code additionally logs, which the old switch's default (no case, no
// log) never did.
func TestCredentialFingerprint_UnregisteredTypeLogsAndDegrades(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})
	ctx := log.IntoContext(context.Background(), capLogger)

	got := fingerprintCredentials(ctx, []spiceboxv1alpha1.AgentCredential{
		{Name: "mystery", Type: "nosuch"},
	})
	assert.Equal(t, "nosuch:", got["mystery"],
		"an unrecognized type still fingerprints as \"<type>:\" so a later valid version compares unequal")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "an unregistered credential type must be logged, not silently folded into the fallback")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "mystery", "log must name the credential")
	assert.Contains(t, joined, "nosuch", "log must name the unrecognized type")
}
