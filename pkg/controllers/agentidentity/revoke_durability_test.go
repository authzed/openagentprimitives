// pkg/controllers/agentidentity/revoke_durability_test.go
//
// Controller-level tests for the durable half of the revocation trigger
// state. The publisher unit tests in revoke_publisher_test.go drive Observe
// directly; these drive the whole Reconcile against a fake API server, so
// they also prove the piece Observe cannot prove on its own: that the record
// it stamps actually survives the reconciler's status patch, and is therefore
// there for a successor process to read.
package agentidentity

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// adoptedSecret builds a Secret already carrying the AdoptedLabel, so the
// reconciler's guarded reader is allowed to resolve it.
func adoptedSecret(ns, name string) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"token": []byte("t0ken")},
	}
	adoptguard.WithAdoptedLabel(sec)
	return sec
}

// newDurabilityReconciler builds a reconciler over a fake API server holding
// objs, with a capturing revocation bus.
func newDurabilityReconciler(t *testing.T, objs ...client.Object) (*Reconciler, client.Client, *capBus) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).
		Build()
	bus := &capBus{}
	return &Reconciler{
		Client:          c,
		APIReader:       c,
		SecretReader:    adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
		RevokePublisher: NewRevokePublisher(revocation.NewPublisher(bus)),
	}, c, bus
}

func reconcileAI(t *testing.T, r *Reconciler, ns, name string) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: ns, Name: name},
	})
	return err
}

func TestReconcile_RecordsTheObservedCredentialSetOnStatus(t *testing.T) {
	r, c, bus := newDurabilityReconciler(t,
		adoptedSecret(testNS, "sec-github"),
		aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")),
	)
	require.NoError(t, reconcileAI(t, r, testNS, "ai-alpha"))
	require.Empty(t, bus.snapshot(), "first reconcile primes without emitting")

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "ai-alpha"}, &got))
	assert.Equal(t, []spiceboxv1alpha1.ObservedCredential{{
		Name:        "github-pat",
		Fingerprint: "static:sec-github",
		SecretNames: []string{"sec-github"},
	}}, got.Status.ObservedCredentials,
		"the trigger state must be persisted where a successor process can read it")
}

func TestReconcile_RecordsTheObservedSetEvenWhenTheIdentityIsInvalid(t *testing.T) {
	// The Valid=False path patches status from a different code path than the
	// happy path. An identity whose Secret is missing is exactly the one an
	// admin is about to edit, so if the record does not survive this path the
	// trigger state stops advancing precisely where revocation matters most.
	r, c, _ := newDurabilityReconciler(t,
		aiWithCreds(testNS, "ai-broken", staticCred("github-pat", "sec-gone")),
	)
	require.NoError(t, reconcileAI(t, r, testNS, "ai-broken"))

	var got spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "ai-broken"}, &got))
	require.Equal(t, metav1.ConditionFalse, conditionStatus(t, &got), "precondition: the identity must be Valid=False")
	assert.Equal(t, []spiceboxv1alpha1.ObservedCredential{{
		Name:        "github-pat",
		Fingerprint: "static:sec-gone",
		SecretNames: []string{"sec-gone"},
	}}, got.Status.ObservedCredentials,
		"the Valid=False status patch must carry the revocation record too")
}

// conditionStatus returns the AgentIdentity's Valid condition status.
func conditionStatus(t *testing.T, a *spiceboxv1alpha1.AgentIdentity) metav1.ConditionStatus {
	t.Helper()
	c := meta.FindStatusCondition(a.Status.Conditions, spiceboxv1alpha1.AgentIdentityConditionValid)
	require.NotNil(t, c, "Valid condition must be present")
	return c.Status
}

func TestReconcile_UnchangedSpecLeavesTheRecordByteIdentical(t *testing.T) {
	r, c, _ := newDurabilityReconciler(t,
		adoptedSecret(testNS, "sec-github"),
		aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")),
	)
	require.NoError(t, reconcileAI(t, r, testNS, "ai-alpha"))

	var first spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "ai-alpha"}, &first))

	require.NoError(t, reconcileAI(t, r, testNS, "ai-alpha"))

	var second spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: "ai-alpha"}, &second))
	assert.Equal(t, first.Status.ObservedCredentials, second.Status.ObservedCredentials)
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"an observation is derived from spec, so an unchanged spec must not churn the object")
}

func TestReconcile_FreshOperatorProcessEmitsARemovalCommittedWhileItWasDown(t *testing.T) {
	ctx := context.Background()
	// Operator #1 observes the identity once and records what it saw.
	r1, c, bus1 := newDurabilityReconciler(t,
		adoptedSecret(testNS, "sec-github"),
		aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")),
	)
	require.NoError(t, reconcileAI(t, r1, testNS, "ai-alpha"))
	require.Empty(t, bus1.snapshot(), "first reconcile primes without emitting")

	// The operator goes down. An admin removes the credential.
	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "ai-alpha"}, &ai))
	ai.Spec.Credentials = nil
	require.NoError(t, c.Update(ctx, &ai))

	// Operator #2 comes up against the same API server with an empty cache.
	bus2 := &capBus{}
	r2 := &Reconciler{
		Client:          r1.Client,
		APIReader:       r1.APIReader,
		SecretReader:    r1.SecretReader,
		RevokePublisher: NewRevokePublisher(revocation.NewPublisher(bus2)),
	}
	require.NoError(t, reconcileAI(t, r2, testNS, "ai-alpha"))

	envs := bus2.snapshot()
	require.Len(t, envs, 1, "a restarted operator must still revoke a credential removed while it was down")
	assert.Equal(t, wantKey(testNS, "sec-github"), decodeRevoked(t, envs[0]).Key)

	var after spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "ai-alpha"}, &after))
	assert.Empty(t, after.Status.ObservedCredentials, "the record advances once the revoke published")
}

func TestReconcile_FailedPublishSurfacesAsAReconcileErrorAndIsRetried(t *testing.T) {
	ctx := context.Background()
	r, c, bus := newDurabilityReconciler(t,
		adoptedSecret(testNS, "sec-github"),
		aiWithCreds(testNS, "ai-alpha", staticCred("github-pat", "sec-github")),
	)
	require.NoError(t, reconcileAI(t, r, testNS, "ai-alpha"))

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "ai-alpha"}, &ai))
	ai.Spec.Credentials = nil
	require.NoError(t, c.Update(ctx, &ai))

	bus.setFailure(errors.New("nats: connection closed"))
	err := reconcileAI(t, r, testNS, "ai-alpha")
	require.Error(t, err, "a revoke that did not publish must fail the reconcile so it is requeued")

	var held spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNS, Name: "ai-alpha"}, &held))
	require.Len(t, held.Status.ObservedCredentials, 1,
		"the record must still name the credential whose revoke failed")
	assert.Equal(t, "static:sec-github", held.Status.ObservedCredentials[0].Fingerprint)

	// The requeue lands on a healthy bus.
	bus.setFailure(nil)
	require.NoError(t, reconcileAI(t, r, testNS, "ai-alpha"))
	envs := bus.snapshot()
	require.Len(t, envs, 1, "the retry must re-derive and re-emit the removal")
	assert.Equal(t, wantKey(testNS, "sec-github"), decodeRevoked(t, envs[0]).Key)
}
