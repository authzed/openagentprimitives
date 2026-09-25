// pkg/controllers/useridentity/revoke_publisher_test.go
//
// Unit tests for RevokePublisher. Drive the publisher with a
// revocation.Publisher wrapping a capBus that records every emitted
// envelope, and assert on:
//   - First observation primes without emitting (anti-spam on operator restart)
//   - Removed credential → credential revoke ("revoked" reason in the log only)
//   - Replaced credential (same name, different fingerprint) → credential revoke
//   - Type change (static ↔ oauth, same name) → credential revoke
//   - Added credential → no emit (Slice 2.5 ι3's KindCredentialLinked covers adds)
//   - Unchanged credentials → no emit
//   - Multiple simultaneous diffs in one Observe
//   - Different UserIdentities track independently
//   - Nil bus tolerated without panic
//
// The unified RevokedPayload carries no reason field — the migration moved
// the revoke onto ap.revocation, keyed by the backing master Secret. So the
// assertions check RevokedPayload{Kind:"credential", Key:<ns>/<masterSecret>,
// Scope:""} rather than the old (Subject, CredentialName, Reason) tuple.
// Scope is "" (cluster-wide) because the key is globally-unique Secret coords;
// scoping it would drop the revoke for sessions outside IdentitiesNamespace.
package useridentity

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	iduseridentity "github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

// capBus captures every revocation.EventPublisher.Publish call so the test
// can decode the unified KindRevoked envelope. Wrapped by a
// revocation.Publisher to match the new RevokePublisher.Bus type.
//
// setFailure makes every subsequent Publish record nothing and return the given
// error — the "NATS refused the envelope" case the trigger state must survive.
type capBus struct {
	mu        sync.Mutex
	published []channelevents.Envelope
	failErr   error
}

func (c *capBus) Publish(_ context.Context, env channelevents.Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failErr != nil {
		return c.failErr
	}
	c.published = append(c.published, env)
	return nil
}

// setFailure switches the bus between failing and healthy. nil restores it.
func (c *capBus) setFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failErr = err
}

func (c *capBus) snapshot() []channelevents.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]channelevents.Envelope, len(c.published))
	copy(out, c.published)
	return out
}

// newCapturingPublisher returns a RevokePublisher whose bus records every
// emitted envelope, plus the capBus for assertions.
func newCapturingPublisher(t *testing.T) (*RevokePublisher, *capBus) {
	t.Helper()
	bus := &capBus{}
	return NewRevokePublisher(revocation.NewPublisher(bus)), bus
}

// decodeRevoked decodes one envelope as a KindRevoked + RevokedPayload,
// asserting the kind and credential-kind invariants every emitted revoke
// shares.
func decodeRevoked(t *testing.T, env channelevents.Envelope) channelevents.RevokedPayload {
	t.Helper()
	assert.Equal(t, channelevents.KindRevoked, env.Kind, "must emit unified KindRevoked")
	var p channelevents.RevokedPayload
	require.NoError(t, json.Unmarshal(env.Payload, &p), "decode RevokedPayload")
	assert.Equal(t, "credential", p.Kind, "revoke kind must be credential")
	assert.Equal(t, "", p.Scope,
		"credential revokes must be cluster-wide (scope \"\"); the key is "+
			"globally-unique Secret coords, and a scope gate would drop the "+
			"revoke for sessions outside IdentitiesNamespace")
	return p
}

// wantKey is the revoke key for a credential on uiName: the backing master
// Secret coords "<IdentitiesNamespace>/<MasterSecretName(uiName,cred)>".
func wantKey(uiName, cred string) string {
	return spiceboxv1alpha1.IdentitiesNamespace + "/" + iduseridentity.MasterSecretName(uiName, cred)
}

func uiWithCreds(name, subject string, creds ...spiceboxv1alpha1.AgentCredential) *spiceboxv1alpha1.UserIdentity {
	return &spiceboxv1alpha1.UserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     subject,
			Credentials: creds,
		},
	}
}

func patCred(name, secret string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name,
		Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secret, Key: "token"},
		},
	}
}

func oauthCred(name, secret string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name,
		Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: secret},
		},
	}
}

func TestRevokePublisher_FirstObservationPrimesWithoutEmitting(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	ui := uiWithCreds("u-alice", "user:alice", patCred("github-pat", "u-alice-github-pat"))
	rp.Observe(context.Background(), ui)
	assert.Empty(t, bus.snapshot(), "first observation must prime without emitting")
}

func TestRevokePublisher_RemovedCredentialEmitsRevoke(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	// Prime with one credential.
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec1")))
	// Next observation: credential is gone.
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice"))

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey("u-alice", "github-pat"), p.Key,
		"revoke key targets the removed credential's master Secret")
}

func TestRevokePublisher_ReplacedCredentialEmitsRevoke(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec1")))
	// Same name, different Secret → replaced.
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec2")))

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey("u-alice", "github-pat"), p.Key)
}

func TestRevokePublisher_TypeChangeEmitsRevoke(t *testing.T) {
	// PAT → OAuth (same name, different fingerprint).
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec1")))
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", oauthCred("github-pat", "sec1-oauth")))

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey("u-alice", "github-pat"), p.Key)
}

func TestRevokePublisher_AddedCredentialDoesNotEmit(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec1")))
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice",
		patCred("github-pat", "sec1"),
		patCred("linear-pat", "sec2"),
	))
	assert.Empty(t, bus.snapshot(),
		"adds are KindCredentialLinked's responsibility, not the revoke path's")
}

func TestRevokePublisher_UnchangedCredentialsDoNotEmit(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec1")))
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec1")))
	assert.Empty(t, bus.snapshot(), "no diff → no emit")
}

func TestRevokePublisher_MultipleSimultaneousDiffs(t *testing.T) {
	// One removed + one replaced + one unchanged → 2 emits in any order.
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice",
		patCred("github-pat", "sec1"),
		patCred("linear-pat", "sec2"),
		patCred("stripe-pat", "sec3"),
	))
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice",
		patCred("github-pat", "sec1"),    // unchanged
		patCred("linear-pat", "sec2new"), // replaced
		// stripe-pat removed
	))
	envs := bus.snapshot()
	require.Len(t, envs, 2)

	// Order-independent check: collect the revoked keys.
	got := map[string]bool{}
	for _, env := range envs {
		p := decodeRevoked(t, env)
		got[p.Key] = true
	}
	assert.Equal(t, map[string]bool{
		wantKey("u-alice", "linear-pat"): true, // replaced
		wantKey("u-alice", "stripe-pat"): true, // removed
	}, got)
}

func TestRevokePublisher_DifferentUserIdentitiesAreIndependent(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice", patCred("github-pat", "sec1")))
	rp.Observe(context.Background(), uiWithCreds("u-bob", "user:bob", patCred("github-pat", "sec1")))
	// Both primed; no emits.
	assert.Empty(t, bus.snapshot())
	// Now revoke from Alice.
	rp.Observe(context.Background(), uiWithCreds("u-alice", "user:alice"))
	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey("u-alice", "github-pat"), p.Key,
		"revoke must target Alice's credential, not Bob's")
}

func TestRevokePublisher_FailedEmitLeavesTheRemovalPendingForTheNextReconcile(t *testing.T) {
	ctx := context.Background()
	rp, bus := newCapturingPublisher(t)
	rp.Observe(ctx, uiWithCreds("u-alice", "user:alice", patCred("github-pat", "u-alice-github-pat")))

	// The credential is removed, but the bus refuses the envelope.
	bus.setFailure(errors.New("nats: connection closed"))
	rp.Observe(ctx, uiWithCreds("u-alice", "user:alice"))
	require.Empty(t, bus.snapshot(), "precondition: the failing bus recorded nothing")

	// The bus recovers. The next reconcile sees the same spec it already
	// diffed, so the only thing that can produce a second attempt is the
	// trigger state NOT having advanced past the failed emit.
	bus.setFailure(nil)
	rp.Observe(ctx, uiWithCreds("u-alice", "user:alice"))

	envs := bus.snapshot()
	require.Len(t, envs, 1, "a failed emit must leave the removal pending, not roll the trigger state forward")
	assert.Equal(t, wantKey("u-alice", "github-pat"), decodeRevoked(t, envs[0]).Key)
}

func TestRevokePublisher_FreshProcessReDerivesARemovalCommittedWhileItWasDown(t *testing.T) {
	ctx := context.Background()
	ui := uiWithCreds("u-alice", "user:alice", patCred("github-pat", "u-alice-github-pat"))

	// Operator #1 observes the identity and records what it saw.
	rp1, bus1 := newCapturingPublisher(t)
	rp1.Observe(ctx, ui)
	require.Empty(t, bus1.snapshot(), "first observation primes without emitting")

	// The operator restarts. While it was down, an admin unlinked the credential.
	ui.Spec.Credentials = nil

	// Operator #2 is a fresh process: its in-memory map is empty, so the only
	// thing that can tell it a credential disappeared is what operator #1
	// recorded durably on the object.
	rp2, bus2 := newCapturingPublisher(t)
	rp2.Observe(ctx, ui)

	envs := bus2.snapshot()
	require.Len(t, envs, 1, "a fresh process must still emit a revoke for a credential removed while it was down")
	assert.Equal(t, wantKey("u-alice", "github-pat"), decodeRevoked(t, envs[0]).Key)
}

func TestRevokePublisher_NilPublisher_Tolerates(t *testing.T) {
	// Constructed with nil bus → Observe doesn't panic; just doesn't publish.
	rp := NewRevokePublisher(nil)
	// First observe primes.
	rp.Observe(context.Background(), uiWithCreds("u-x", "user:x", patCred("a", "s1")))
	// Diff observe (would emit if bus were set).
	rp.Observe(context.Background(), uiWithCreds("u-x", "user:x"))
	// No panic, no emit (and nothing to assert beyond not crashing).
}
