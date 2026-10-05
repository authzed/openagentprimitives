// pkg/controllers/agentsession/memorytoken_test.go
//
// The operator's token registry (pkg/memory/tokens) is process memory, and
// nothing rehydrates it at startup. The per-session Secret holding each memory
// bearer token is durable, and so is every client presenting one — so a
// restarted operator that does not re-register a session's token answers every
// read of that session's memory with a 401, permanently.
//
// These tests pin the restart scenario itself (a Succeeded session, an empty
// registry, one reconcile) and its authorization boundary: the restored token
// must reach exactly what it reached before, must not be minted from nothing,
// and must never come back for a session whose finalizer already revoked it.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// seedMemoryTokenSecret creates the per-session Secret the operator mints on a
// session's first reconcile, carrying tok under the "token" key. data lets a
// case write a Secret with the key missing or empty.
func seedMemoryTokenSecret(t *testing.T, c client.Client, sess *spiceboxv1alpha1.AgentSession, data map[string][]byte) {
	t.Helper()
	require.NoError(t, c.Create(memory.WithSystemApproval(context.Background(), "test"), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sess.Namespace,
			Name:      MemoryTokenSecretName(sess),
		},
		Data: data,
	}), "seed per-session memory-token Secret")
}

// succeededFixture is the exact restart scenario: a Succeeded session whose
// pods the reap tears down, its memory-token Secret still on the cluster, and
// an EMPTY token registry — what a freshly restarted operator sees.
func succeededFixture(t *testing.T, tok string) reapFixture {
	t.Helper()
	f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Minute, -time.Hour)
	seedMemoryTokenSecret(t, f.c, f.sess, map[string][]byte{"token": []byte(tok)})
	return f
}

func sessionKey(f reapFixture) memory.NamespacedName {
	return memory.NamespacedName{Namespace: f.sess.Namespace, Name: f.sess.Name}
}

// TestReconcile_SucceededSession_RestoresMemoryTokenAfterOperatorRestart is the
// regression test for the defect: a terminal session short-circuits at the pod
// reap, which returns long before the reconciler's own Tokens.Set, so an
// operator that restarted after the session finished never re-registered it and
// every read of a permanently-retained transcript got a bare 401.
//
// The registry starts empty ON PURPOSE — that, not the phase, is what makes
// this a restart test.
func TestReconcile_SucceededSession_RestoresMemoryTokenAfterOperatorRestart(t *testing.T) {
	const tok = "restored-bearer-value"
	f := succeededFixture(t, tok)
	key := sessionKey(f)

	require.False(t, f.r.Tokens.Registered(key),
		"precondition: a restarted operator holds no registration for this session")

	f.reconcile(t)

	// The reap ran, so this reconcile really did take the short-circuit the
	// authoritative Tokens.Set sits below. Without this the test could pass on a
	// path that never exercises the defect.
	assert.True(t, f.sandboxGone(t), "the terminal reap must have run: this is the short-circuiting path")
	assert.True(t, f.sessionPresent(t), "the reap keeps the AgentSession itself")

	got, ok := f.r.Tokens.Lookup(tok)
	require.True(t, ok, "the token in the session's Secret must be registered again after the reconcile")
	assert.Equal(t, key, got, "the restored token must resolve to its own session")
	assert.True(t, f.r.Tokens.Authorizes(tok, key), "restored token must read its own session")
	assert.True(t, f.r.Tokens.AuthorizesMutation(tok, key), "restored token keeps the write reach it had before the restart")
}

// TestReconcile_SucceededSession_RestoresAuditVerifyKeyAfterOperatorRestart is
// the sibling regression to the memory-token restore above, for the OTHER half
// of the per-session registration a restart wipes: the Ed25519 audit VERIFY key
// the facade resolves on every append-only write.
//
// Both live in the same process-memory registry. The token was restored on
// every short-circuit path (reregisterMemoryToken, at the top of Reconcile) but
// the verify key was registered only at step 4, ~900 lines and ~30 early
// returns below. So a terminal session — whose reap returns long before step 4,
// forever — got its token back but not its key, and every append-only write
// (and every provenance-verifying read) then failed with
// `403 ... no usable key <keyID> for session:...`, exactly the error a
// production operator OOMKill produced.
//
// The registry starts empty ON PURPOSE — that, not the phase, is the restart.
func TestReconcile_SucceededSession_RestoresAuditVerifyKeyAfterOperatorRestart(t *testing.T) {
	const tok = "restored-bearer-value"
	ctx := memory.WithSystemApproval(context.Background(), "test")
	f := succeededFixture(t, tok)

	// The session's durable audit identity, as it stands on the CR after the
	// full reconcile that minted it — BEFORE the restart. generateAuditKeypair
	// is the operator's own mint, so status carries a real (publicKey, keyID).
	_, pubB64, keyID, err := generateAuditKeypair()
	require.NoError(t, err, "mint the session's audit keypair")
	f.sess.Status.AuditPublicKey = pubB64
	f.sess.Status.AuditKeyID = keyID
	require.NoError(t, f.c.Status().Update(ctx, f.sess),
		"persist the audit key on status (the K8s-witnessed trust root the operator registers from)")

	publisher := provenance.SessionPublisher(f.sess.Namespace, f.sess.Name)
	_, had := f.r.Tokens.PublisherKey(publisher, keyID)
	require.False(t, had, "precondition: a restarted operator holds no verify key for this session")

	f.reconcile(t)

	// The reap ran, so this reconcile really did take the short-circuit that
	// step 4's key registration sits below — the same gate the token test pins.
	assert.True(t, f.sandboxGone(t), "the terminal reap must have run: this is the short-circuiting path")

	gotPub, ok := f.r.Tokens.PublisherKey(publisher, keyID)
	require.True(t, ok,
		"the session's audit verify key must be re-registered after the reconcile, or append-only writes 403 with 'no usable key'")
	wantPub, decErr := provenance.DecodePubKey(pubB64)
	require.NoError(t, decErr, "decode the status public key")
	assert.Equal(t, wantPub, gotPub, "the restored verify key must be the one anchored on status")
}

// TestReconcile_RestoredTokenScopes pins that the restoration reaches exactly
// what the registration it replaces reached — the session plus the bundle
// SpiceboxSessions recorded on status, for READS only — and nothing else.
func TestReconcile_RestoredTokenScopes(t *testing.T) {
	const tok = "scoped-bearer-value"
	f := succeededFixture(t, tok)
	f.reconcile(t)

	for _, name := range f.bundles {
		bundleKey := memory.NamespacedName{Namespace: f.sess.Namespace, Name: name}
		assert.True(t, f.r.Tokens.Authorizes(tok, bundleKey),
			"a bundle scope on status.bundleSessions is a READ scope of the restored token: %s", name)
		assert.False(t, f.r.Tokens.AuthorizesMutation(tok, bundleKey),
			"a bundle scope must NOT become writable through the restoration: %s", name)
	}
	stranger := memory.NamespacedName{Namespace: f.sess.Namespace, Name: "some-other-session"}
	assert.False(t, f.r.Tokens.Authorizes(tok, stranger),
		"restoration must not widen the token to a session it was never registered for")
}

// TestReconcile_RevokedTokenIsNotResurrected is the authorization half of the
// fix. Deleting the session is the ONLY kill switch for a memory bearer token
// (the memory API has no liveness check on the session), so a restoration that
// ran for a deleting session would turn a durability fix into a permanent
// credential leak.
func TestReconcile_RevokedTokenIsNotResurrected(t *testing.T) {
	const tok = "revoked-bearer-value"
	ctx := memory.WithSystemApproval(context.Background(), "test")
	f := succeededFixture(t, tok)
	key := sessionKey(f)
	f.r.RunnerFactory = noopRunnerFactory{}

	// The session is registered, exactly as it would be while alive.
	f.r.Tokens.Set(key, tok, "")
	require.True(t, f.r.Tokens.Registered(key), "precondition: the live session holds a registration")

	// Delete it. The finalizer is present, so the object survives with a
	// deletionTimestamp — the state finalize reconciles.
	require.NoError(t, f.c.Delete(ctx, f.sess), "delete the AgentSession")
	var deleting spiceboxv1alpha1.AgentSession
	require.NoError(t, f.c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "s1"}, &deleting), "get the deleting session")
	require.False(t, deleting.DeletionTimestamp.IsZero(), "precondition: the fake client stamped a deletionTimestamp")

	f.reconcile(t)

	_, stillThere := f.r.Tokens.Lookup(tok)
	require.False(t, stillThere, "finalize must revoke the token")
	require.False(t, f.r.Tokens.Registered(key), "the session must hold no registration after finalize")

	// Every later reconcile of that key must leave it revoked. finalize removed
	// the last finalizer, so the object is gone and these passes load nothing —
	// which is the point: no reconcile after a delete has anything to restore
	// from. The husk that CAN still be loaded is the test below.
	for i := 0; i < 3; i++ {
		_, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
		require.NoError(t, err, "post-finalize reconcile %d", i)
	}
	_, back := f.r.Tokens.Lookup(tok)
	assert.False(t, back, "a revoked token must never be restored by a later reconcile")
	assert.False(t, f.r.Tokens.Registered(key), "and the session must hold no registration")
	assert.False(t, f.r.Tokens.Authorizes(tok, key), "nor authorize anything")
}

// TestReconcile_DeletingSession_TokenIsNotRestoredFromItsSecret is the
// ORDERING half of the revocation claim, and the one a placement mistake
// actually breaks.
//
// Within a single reconcile of a deleting session, restoring before finalize
// would net out to nothing — finalize revokes on the way past. The hazard is
// the reconcile finalize does NOT revoke on: its first line returns as soon as
// the operator's own finalizer is gone, and an object can outlive that under
// somebody else's finalizer. A restoration placed above the deletionTimestamp
// check would register the token on that pass and nothing would ever take it
// away again — the memory API has no liveness check on the session, so deleting
// the session is the only kill switch there is.
//
// So: deleting, our finalizer already removed, a foreign one holding the object
// on the cluster, an empty registry (the operator also restarted). Nothing may
// be registered.
func TestReconcile_DeletingSession_TokenIsNotRestoredFromItsSecret(t *testing.T) {
	const tok = "must-stay-revoked"
	ctx := memory.WithSystemApproval(context.Background(), "test")
	deletedAt := metav1.NewTime(time.Date(2026, 6, 28, 11, 0, 0, 0, time.UTC))
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "s1", Namespace: "default", UID: "uid-1",
			DeletionTimestamp: &deletedAt,
			// NOT the operator's finalizer: this session is past finalize, and
			// only a foreign finalizer is keeping the husk on the cluster.
			Finalizers: []string{"example.test/keepalive"},
		},
		Spec:   spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded},
	}
	c := buildFakeClient(t, sess)
	seedMemoryTokenSecret(t, c, sess, map[string][]byte{"token": []byte(tok)})
	r := &Reconciler{
		Client:        c,
		APIReader:     c,
		Tokens:        tokens.NewRegistry(),
		RunnerFactory: noopRunnerFactory{},
	}
	key := memory.NamespacedName{Namespace: sess.Namespace, Name: sess.Name}
	require.False(t, r.Tokens.Registered(key), "precondition: the restarted operator holds nothing for this session")

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
	require.NoError(t, err, "Reconcile of a deleting session")

	assert.False(t, r.Tokens.Registered(key), "a deleting session must never be registered from its Secret")
	_, ok := r.Tokens.Lookup(tok)
	assert.False(t, ok, "the revoked token must not be resolvable")
	assert.False(t, r.Tokens.Authorizes(tok, key), "nor authorize the session it was revoked from")
}

// TestReregisterMemoryToken_Refusals pins what the restoration will NOT do. It
// never mints, so a session whose Secret is gone or carries no usable token
// gets nothing; and it never displaces a registration this process already
// holds, so it cannot overwrite the authoritative one made when the token was
// minted.
func TestReregisterMemoryToken_Refusals(t *testing.T) {
	const tok = "durable-bearer-value"
	cases := []struct {
		name      string
		secret    map[string][]byte // nil = do not create the Secret at all
		preset    string            // non-empty = already registered under this token
		wantToken string            // "" = nothing registered for the session
	}{
		{
			name:      "Secret absent (deleted, or never minted): nothing is registered — restoration never mints",
			secret:    nil,
			wantToken: "",
		},
		{
			name:      "Secret present but carries no token key: nothing is registered",
			secret:    map[string][]byte{"args-hash-key": []byte("unrelated")},
			wantToken: "",
		},
		{
			name:      "Secret present with an empty token: nothing is registered",
			secret:    map[string][]byte{"token": []byte("")},
			wantToken: "",
		},
		{
			name:      "Secret present with a token: it is registered",
			secret:    map[string][]byte{"token": []byte(tok)},
			wantToken: tok,
		},
		{
			name:      "already registered: the live registration is left alone, not replaced from the Secret",
			secret:    map[string][]byte{"token": []byte(tok)},
			preset:    "registration-this-process-already-holds",
			wantToken: "registration-this-process-already-holds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memory.WithSystemApproval(context.Background(), "test")
			f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Minute, -time.Hour)
			if tc.secret != nil {
				seedMemoryTokenSecret(t, f.c, f.sess, tc.secret)
			}
			key := sessionKey(f)
			if tc.preset != "" {
				f.r.Tokens.Set(key, tc.preset, "")
			}

			f.r.reregisterMemoryToken(ctx, f.sess)

			if tc.wantToken == "" {
				assert.False(t, f.r.Tokens.Registered(key), "no registration expected")
				return
			}
			got, ok := f.r.Tokens.Lookup(tc.wantToken)
			require.True(t, ok, "expected %q to be the registered token", tc.wantToken)
			assert.Equal(t, key, got)
		})
	}
}
