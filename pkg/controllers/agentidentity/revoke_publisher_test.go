// pkg/controllers/agentidentity/revoke_publisher_test.go
//
// Unit tests for RevokePublisher. Drive the publisher with a
// revocation.Publisher wrapping a capBus that records every emitted
// envelope, and assert on:
//   - First observation primes without emitting (anti-spam on operator restart)
//   - Removed credential → credential revoke ("revoked" reason in the log only)
//   - Replaced credential (same name, different Secret) → credential revoke
//   - Type change (static ↔ oauth, same name) → credential revoke
//   - Added credential → no emit (KindCredentialLinked covers adds)
//   - Unchanged credentials → no emit
//   - Multiple simultaneous diffs in one Observe (order-independent)
//   - Different AgentIdentities track independently
//   - Nil bus tolerated without panic
//
// The unified RevokedPayload carries no reason field — the revoke rides
// ap.revocation, keyed by the backing Secret coords in the AgentIdentity's
// own namespace. The revoke is emitted cluster-wide (Scope:"") because the key
// is globally-unique Secret coords; scoping it would drop the revoke for
// sessions outside the AgentIdentity namespace. So the assertions check:
//
//	RevokedPayload{Kind:"credential", Key:<aiNS>/<secretName>, Scope:""}
package agentidentity

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
)

// capBus captures every revocation.EventPublisher.Publish call so tests can
// decode the unified KindRevoked envelope. Wrapped by a revocation.Publisher
// to match the RevokePublisher.Bus type.
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
// asserting the kind and credential-kind invariants every emitted revoke shares.
func decodeRevoked(t *testing.T, env channelevents.Envelope) channelevents.RevokedPayload {
	t.Helper()
	assert.Equal(t, channelevents.KindRevoked, env.Kind, "must emit unified KindRevoked")
	var p channelevents.RevokedPayload
	require.NoError(t, json.Unmarshal(env.Payload, &p), "decode RevokedPayload")
	assert.Equal(t, "credential", p.Kind, "revoke kind must be credential")
	return p
}

// wantKey is the revoke key for a credential on an AgentIdentity:
// "<aiNamespace>/<secretName>" — the backing Secret lives in the
// AgentIdentity's own namespace.
func wantKey(aiNamespace, secretName string) string {
	return aiNamespace + "/" + secretName
}

// aiWithCreds builds a minimal AgentIdentity with the given namespace, name,
// and credentials.
func aiWithCreds(ns, name string, creds ...spiceboxv1alpha1.AgentCredential) *spiceboxv1alpha1.AgentIdentity {
	return &spiceboxv1alpha1.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentIdentitySpec{
			Credentials: creds,
		},
	}
}

// staticCred builds a type=static AgentCredential.
func staticCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name,
		Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: "token"},
		},
	}
}

// oauthCred builds a type=oauth AgentCredential.
func oauthCred(name, secretName string) spiceboxv1alpha1.AgentCredential {
	return spiceboxv1alpha1.AgentCredential{
		Name: name,
		Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: secretName},
		},
	}
}

const testNS = "agent-ns"

func TestRevokePublisher_FirstObservationPrimesWithoutEmitting(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	ai := aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github"))
	rp.Observe(context.Background(), ai)
	assert.Empty(t, bus.snapshot(), "first observation must prime without emitting")
}

func TestRevokePublisher_RemovedCredentialEmitsRevoke(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	// Prime with one credential.
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")))
	// Next observation: credential is gone.
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha"))

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey(testNS, "sec-github"), p.Key,
		"revoke key must target the removed credential's backing Secret in the AgentIdentity namespace")
	assert.Equal(t, "", p.Scope,
		"credential revokes must be cluster-wide (scope \"\"); the key is "+
			"globally-unique Secret coords, so scoping it would needlessly drop "+
			"the revoke for sessions outside the AgentIdentity namespace")
}

func TestRevokePublisher_ReplacedCredentialEmitsRevoke(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-v1")))
	// Same name, different Secret ref → replaced.
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-v2")))

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey(testNS, "sec-v2"), p.Key)
	assert.Equal(t, "", p.Scope, "credential revokes must be cluster-wide (scope \"\")")
}

func TestRevokePublisher_TypeChangeEmitsRevoke(t *testing.T) {
	// static → oauth same name: different fingerprint → REPLACED.
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-static")))
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", oauthCred("github-pat", "sec-oauth")))

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey(testNS, "sec-oauth"), p.Key)
}

func TestRevokePublisher_AddedCredentialDoesNotEmit(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")))
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha",
		staticCred("github-pat", "sec-github"),
		staticCred("linear-pat", "sec-linear"),
	))
	assert.Empty(t, bus.snapshot(),
		"added credentials must not emit; KindCredentialLinked covers that direction")
}

func TestRevokePublisher_UnchangedCredentialsDoNotEmit(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")))
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")))
	assert.Empty(t, bus.snapshot(), "no diff → no emit")
}

func TestRevokePublisher_MultipleSimultaneousDiffs(t *testing.T) {
	// One removed + one replaced + one unchanged → 2 emits in any order.
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha",
		staticCred("github-pat", "sec-github"), // will stay unchanged
		staticCred("linear-pat", "sec-lin-v1"), // will be replaced
		staticCred("stripe-pat", "sec-stripe"), // will be removed
	))
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha",
		staticCred("github-pat", "sec-github"), // unchanged
		staticCred("linear-pat", "sec-lin-v2"), // replaced
		// stripe-pat removed
	))

	envs := bus.snapshot()
	require.Len(t, envs, 2)

	// Order-independent: collect the revoked keys.
	got := map[string]bool{}
	for _, env := range envs {
		p := decodeRevoked(t, env)
		got[p.Key] = true
	}
	assert.Equal(t, map[string]bool{
		wantKey(testNS, "sec-lin-v2"): true, // replaced → new secret coords
		wantKey(testNS, "sec-stripe"): true, // removed → old secret coords
	}, got)
}

func TestRevokePublisher_OAuthRemovedEmitsRevoke(t *testing.T) {
	// OAuth credential removal should revoke the OAuth Secret.
	rp, bus := newCapturingPublisher(t)
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", oauthCred("github-oauth", "sec-oauth")))
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha"))

	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey(testNS, "sec-oauth"), p.Key)
	assert.Equal(t, "", p.Scope, "credential revokes must be cluster-wide (scope \"\")")
}

func TestRevokePublisher_DifferentAgentIdentitiesAreIndependent(t *testing.T) {
	rp, bus := newCapturingPublisher(t)
	// Prime both AgentIdentities.
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-alpha")))
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-beta", staticCred("github-pat", "sec-beta")))
	assert.Empty(t, bus.snapshot(), "priming both AIs must not emit")

	// Revoke from ai-alpha only.
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-alpha"))
	envs := bus.snapshot()
	require.Len(t, envs, 1)
	p := decodeRevoked(t, envs[0])
	assert.Equal(t, wantKey(testNS, "sec-alpha"), p.Key,
		"revoke must target ai-alpha's credential, not ai-beta's")
}

func TestRevokePublisher_SameNameAIsInDifferentNamespacesRevokeTheirOwnSecret(t *testing.T) {
	// AgentIdentity is namespaced, so two identities may legitimately share a
	// name. Each must carry its own trigger state: a removal in ns-a must
	// revoke ns-a's Secret, never ns-b's.
	const nsA, nsB = "ns-a", "ns-b"
	ctx := context.Background()
	rp, bus := newCapturingPublisher(t)
	rp.Observe(ctx, aiWithCreds(nsA, "ai-shared", staticCred("github-pat", "sec-a")))
	rp.Observe(ctx, aiWithCreds(nsB, "ai-shared", staticCred("github-pat", "sec-b")))
	require.Empty(t, bus.snapshot(), "both prime observations must not emit")

	rp.Observe(ctx, aiWithCreds(nsA, "ai-shared"))

	envs := bus.snapshot()
	require.Len(t, envs, 1, "removing ns-a's credential must emit exactly one revoke")
	assert.Equal(t, wantKey(nsA, "sec-a"), decodeRevoked(t, envs[0]).Key,
		"the revoke must name ns-a's own Secret; ns-b's identity shares only the name")
}

func TestRevokePublisher_FailedEmitLeavesTheRemovalPendingForTheNextReconcile(t *testing.T) {
	ctx := context.Background()
	rp, bus := newCapturingPublisher(t)
	rp.Observe(ctx, aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")))

	// The credential is removed, but the bus refuses the envelope.
	bus.setFailure(errors.New("nats: connection closed"))
	rp.Observe(ctx, aiWithCreds(testNS, "ai-alpha"))
	require.Empty(t, bus.snapshot(), "precondition: the failing bus recorded nothing")

	// The bus recovers. The next reconcile sees the same spec it already
	// diffed, so the only thing that can produce a second attempt is the
	// trigger state NOT having advanced past the failed emit.
	bus.setFailure(nil)
	rp.Observe(ctx, aiWithCreds(testNS, "ai-alpha"))

	envs := bus.snapshot()
	require.Len(t, envs, 1, "a failed emit must leave the removal pending, not roll the trigger state forward")
	assert.Equal(t, wantKey(testNS, "sec-github"), decodeRevoked(t, envs[0]).Key)
}

func TestRevokePublisher_FailedEmitLeavesTheReplacementPendingForTheNextReconcile(t *testing.T) {
	ctx := context.Background()
	rp, bus := newCapturingPublisher(t)
	rp.Observe(ctx, aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-v1")))

	bus.setFailure(errors.New("nats: connection closed"))
	rp.Observe(ctx, aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-v2")))
	require.Empty(t, bus.snapshot(), "precondition: the failing bus recorded nothing")

	bus.setFailure(nil)
	rp.Observe(ctx, aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-v2")))

	envs := bus.snapshot()
	require.Len(t, envs, 1, "a failed emit must leave the replacement pending, not roll the trigger state forward")
	assert.Equal(t, wantKey(testNS, "sec-v2"), decodeRevoked(t, envs[0]).Key)
}

func TestRevokePublisher_FreshProcessReDerivesARemovalCommittedWhileItWasDown(t *testing.T) {
	ctx := context.Background()
	ai := aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github"))

	// Operator #1 observes the identity and records what it saw.
	rp1, bus1 := newCapturingPublisher(t)
	rp1.Observe(ctx, ai)
	require.Empty(t, bus1.snapshot(), "first observation primes without emitting")

	// The operator restarts. While it was down, an admin removed the credential.
	ai.Spec.Credentials = nil

	// Operator #2 is a fresh process: its in-memory map is empty, so the only
	// thing that can tell it a credential disappeared is what operator #1
	// recorded durably on the object.
	rp2, bus2 := newCapturingPublisher(t)
	rp2.Observe(ctx, ai)

	envs := bus2.snapshot()
	require.Len(t, envs, 1, "a fresh process must still emit a revoke for a credential removed while it was down")
	assert.Equal(t, wantKey(testNS, "sec-github"), decodeRevoked(t, envs[0]).Key)
}

func TestRevokePublisher_NilPublisher_Tolerates(t *testing.T) {
	// Constructed with nil bus → Observe doesn't panic; just doesn't publish.
	rp := NewRevokePublisher(nil)
	// First observe primes.
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-x", staticCred("a", "sec-a")))
	// Diff observe (would emit if bus were set).
	rp.Observe(context.Background(), aiWithCreds(testNS, "ai-x"))
	// No panic, no emit (and nothing to assert beyond not crashing).
}
