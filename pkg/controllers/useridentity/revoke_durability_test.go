// pkg/controllers/useridentity/revoke_durability_test.go
//
// Controller-level tests for the durable half of the revocation trigger
// state. The publisher unit tests in revoke_publisher_test.go drive Observe
// directly; these drive the whole Reconcile against a fake API server, so
// they also prove the piece Observe cannot prove on its own: that the record
// it stamps actually survives the reconciler's status patch, and is therefore
// there for a successor process to read.
package useridentity

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
)

// adoptedIdentitySecret builds a Secret in IdentitiesNamespace already
// carrying the AdoptedLabel, so the reconciler's guarded reader may read it.
func adoptedIdentitySecret(name string) *corev1.Secret {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: spiceboxv1alpha1.IdentitiesNamespace},
		Data:       map[string][]byte{"token": []byte("t0ken")},
	}
	adoptguard.WithAdoptedLabel(sec)
	return sec
}

// reconcilerOver builds a reconciler over c with a capturing revocation bus.
func reconcilerOver(c client.Client, bus *capBus) *Reconciler {
	return &Reconciler{
		Client:          c,
		APIReader:       c,
		SecretReader:    adoptguard.NewSecretReader(c, c, adoptguard.Warn, func(types.NamespacedName) bool { return false }),
		RevokePublisher: NewRevokePublisher(revocation.NewPublisher(bus)),
	}
}

func reconcileUI(t *testing.T, r *Reconciler, name string) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
	return err
}

func getUI(t *testing.T, c client.Client, name string) spiceboxv1alpha1.UserIdentity {
	t.Helper()
	var got spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &got))
	return got
}

func TestReconcile_RecordsTheObservedCredentialSetOnStatus(t *testing.T) {
	c := buildClient(t,
		adoptedIdentitySecret("u-alice-gh"),
		uiWithCreds("u-alice", "user:alice", patCred("github-pat", "u-alice-gh")),
	)
	bus := &capBus{}
	require.NoError(t, reconcileUI(t, reconcilerOver(c, bus), "u-alice"))
	require.Empty(t, bus.snapshot(), "first reconcile primes without emitting")

	got := getUI(t, c, "u-alice")
	assert.Equal(t, []spiceboxv1alpha1.ObservedCredential{{
		Name:        "github-pat",
		Fingerprint: "static:u-alice-gh",
	}}, got.Status.ObservedCredentials,
		"the trigger state must be persisted where a successor process can read it")
}

func TestReconcile_RecordsTheObservedSetEvenWhenTheIdentityIsInvalid(t *testing.T) {
	// The Valid=False path patches status from a different code path than the
	// happy path. An identity whose Secret is missing is exactly the one an
	// admin is about to edit, so if the record does not survive this path the
	// trigger state stops advancing precisely where revocation matters most.
	c := buildClient(t, uiWithCreds("u-broken", "user:broken", patCred("github-pat", "u-broken-gone")))
	require.NoError(t, reconcileUI(t, reconcilerOver(c, &capBus{}), "u-broken"))

	got := getUI(t, c, "u-broken")
	cond := meta.FindStatusCondition(got.Status.Conditions, spiceboxv1alpha1.UserIdentityConditionValid)
	require.NotNil(t, cond, "Valid condition must be present")
	require.Equal(t, metav1.ConditionFalse, cond.Status, "precondition: the identity must be Valid=False")
	assert.Equal(t, []spiceboxv1alpha1.ObservedCredential{{
		Name:        "github-pat",
		Fingerprint: "static:u-broken-gone",
	}}, got.Status.ObservedCredentials,
		"the Valid=False status patch must carry the revocation record too")
}

func TestReconcile_UnchangedSpecLeavesTheRecordByteIdentical(t *testing.T) {
	c := buildClient(t,
		adoptedIdentitySecret("u-alice-gh"),
		uiWithCreds("u-alice", "user:alice", patCred("github-pat", "u-alice-gh")),
	)
	r := reconcilerOver(c, &capBus{})
	require.NoError(t, reconcileUI(t, r, "u-alice"))
	first := getUI(t, c, "u-alice")

	require.NoError(t, reconcileUI(t, r, "u-alice"))
	second := getUI(t, c, "u-alice")

	assert.Equal(t, first.Status.ObservedCredentials, second.Status.ObservedCredentials)
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"an observation is derived from spec, so an unchanged spec must not churn the object")
}

func TestReconcile_FreshOperatorProcessEmitsAnUnlinkCommittedWhileItWasDown(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t,
		adoptedIdentitySecret("u-alice-gh"),
		uiWithCreds("u-alice", "user:alice", patCred("github-pat", "u-alice-gh")),
	)
	// Operator #1 observes the identity once and records what it saw.
	bus1 := &capBus{}
	require.NoError(t, reconcileUI(t, reconcilerOver(c, bus1), "u-alice"))
	require.Empty(t, bus1.snapshot(), "first reconcile primes without emitting")

	// The operator goes down. An admin unlinks the credential.
	ui := getUI(t, c, "u-alice")
	ui.Spec.Credentials = nil
	require.NoError(t, c.Update(ctx, &ui))

	// Operator #2 comes up against the same API server with an empty cache.
	bus2 := &capBus{}
	require.NoError(t, reconcileUI(t, reconcilerOver(c, bus2), "u-alice"))

	envs := bus2.snapshot()
	require.Len(t, envs, 1, "a restarted operator must still revoke a credential unlinked while it was down")
	assert.Equal(t, wantKey("u-alice", "github-pat"), decodeRevoked(t, envs[0]).Key)
	assert.Empty(t, getUI(t, c, "u-alice").Status.ObservedCredentials,
		"the record advances once the revoke published")
}

func TestReconcile_FailedPublishSurfacesAsAReconcileErrorAndIsRetried(t *testing.T) {
	ctx := context.Background()
	c := buildClient(t,
		adoptedIdentitySecret("u-alice-gh"),
		uiWithCreds("u-alice", "user:alice", patCred("github-pat", "u-alice-gh")),
	)
	bus := &capBus{}
	r := reconcilerOver(c, bus)
	require.NoError(t, reconcileUI(t, r, "u-alice"))

	ui := getUI(t, c, "u-alice")
	ui.Spec.Credentials = nil
	require.NoError(t, c.Update(ctx, &ui))

	bus.setFailure(errors.New("nats: connection closed"))
	require.Error(t, reconcileUI(t, r, "u-alice"),
		"a revoke that did not publish must fail the reconcile so it is requeued")

	held := getUI(t, c, "u-alice")
	require.Len(t, held.Status.ObservedCredentials, 1,
		"the record must still name the credential whose revoke failed")
	assert.Equal(t, "static:u-alice-gh", held.Status.ObservedCredentials[0].Fingerprint)

	// The requeue lands on a healthy bus.
	bus.setFailure(nil)
	require.NoError(t, reconcileUI(t, r, "u-alice"))
	envs := bus.snapshot()
	require.Len(t, envs, 1, "the retry must re-derive and re-emit the removal")
	assert.Equal(t, wantKey("u-alice", "github-pat"), decodeRevoked(t, envs[0]).Key)
}
