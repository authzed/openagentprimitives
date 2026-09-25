// pkg/channels/channelsd/pipeline/credential_request_credentialupdate_gate_test.go
//
// CredentialRequestWatcher must not act on a session parked in
// AwaitingCredentials for an Open CredentialUpdateRequest (the
// credential_update park), only on one parked by the passthrough identity gate
// (a missing-credentials SessionUserIdentity). Otherwise the user gets a
// second card stacked on top of the one the other watcher already delivered.
package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
)

// credentialUpdateParkedSession is a fixtureSession additionally marked with
// the AgentSessionConditionCredentialUpdatePending condition the agentsession
// controller stamps while an Open CredentialUpdateRequest exists for it.
func credentialUpdateParkedSession(t *testing.T, mutate ...func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	sess := fixtureSession(t, mutate...)
	conditions.SetTrue(sess, &sess.Status.Conditions,
		spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending, spiceboxv1alpha1.ReasonCredentialUpdateRequested)
	return sess
}

// TestCredentialUpdateParked_TrueForTheMarkerCondition pins the discriminator
// itself: a session with the marker condition True is reported parked for
// credential_update.
func TestCredentialUpdateParked_TrueForTheMarkerCondition(t *testing.T) {
	sess := credentialUpdateParkedSession(t)
	assert.True(t, credentialUpdateParked(sess))
}

func TestCredentialUpdateParked_FalseWithoutTheMarkerCondition(t *testing.T) {
	sess := fixtureSession(t)
	assert.False(t, credentialUpdateParked(sess))
}

// TestCredentialRequestWatcher_ReconcileOne_SkipsCredentialUpdateParkedSession
// deliberately includes a SessionUserIdentity WITH MissingCredentials -- the
// worse case: a passthrough session that ALSO has missing credentials at the
// same moment a credential_update request is Open must still get NO
// credential_link card while credential_update-parked.
//
// That fixture is load-bearing. Without the SUI, ReconcileOne merely hits
// "SessionUserIdentity not found" and no-ops regardless of the gate, so the
// test would stay green with the gate removed. With the SUI present, removing
// the gate publishes an interaction_request and this fails.
func TestCredentialRequestWatcher_ReconcileOne_SkipsCredentialUpdateParkedSession(t *testing.T) {
	sess := credentialUpdateParkedSession(t)
	sui := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: testSession, Namespace: testNS},
		Spec:       spiceboxv1alpha1.SessionUserIdentitySpec{AgentSession: testSession, Subject: testStarter},
		Status: spiceboxv1alpha1.SessionUserIdentityStatus{
			MissingCredentials: []string{"GitHub"},
			Explanation: &spiceboxv1alpha1.CredentialExplanation{
				Items: []spiceboxv1alpha1.CredentialExplanationItem{
					{Credential: "GitHub", Title: "GitHub", Why: "why"},
				},
			},
		},
	}

	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess, sui).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SessionUserIdentity{}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialRequestWatcher{
		K8s:             cli,
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return "https://identityd.example.test" },
		NATSPublish:     pub.publish,
		Now:             fixedFutureNow,
	}

	require.NoError(t, w.ReconcileOne(context.Background(), sess))
	assert.Equal(t, 0, pub.countInteractionRequests(),
		"a credential_update-parked session must never get a credential_link card, even when a SUI with missing credentials genuinely exists for it")
}

// TestCredentialRequestWatcher_ForcePublish_SkipsCredentialUpdateParkedSession
// covers the third entry point. ForcePublish exists to bypass the
// alreadyPublished dedup when a user re-interacts from another device
// (resurface), which is precisely what makes it able to reach the second-card
// case: a session parked on an Open CredentialUpdateRequest that ALSO has
// missing passthrough credentials would otherwise get a "Connect your
// accounts" card posted on top of the credential-update card the other watcher
// already delivered.
//
// The SUI with MissingCredentials is load-bearing for the same reason it is in
// the ReconcileOne test above: without it doPublish's "nothing missing" no-op
// short-circuits and the test would pass with or without the gate.
func TestCredentialRequestWatcher_ForcePublish_SkipsCredentialUpdateParkedSession(t *testing.T) {
	sess := credentialUpdateParkedSession(t)
	sui := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: testSession, Namespace: testNS},
		Spec:       spiceboxv1alpha1.SessionUserIdentitySpec{AgentSession: testSession, Subject: testStarter},
		Status: spiceboxv1alpha1.SessionUserIdentityStatus{
			MissingCredentials: []string{"GitHub"},
			Explanation: &spiceboxv1alpha1.CredentialExplanation{
				Items: []spiceboxv1alpha1.CredentialExplanationItem{
					{Credential: "GitHub", Title: "GitHub", Why: "why"},
				},
			},
		},
	}

	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess, sui).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SessionUserIdentity{}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialRequestWatcher{
		K8s:             cli,
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return "https://identityd.example.test" },
		NATSPublish:     pub.publish,
		Now:             fixedFutureNow,
	}

	require.NoError(t, w.ForcePublish(context.Background(), sess))
	assert.Equal(t, 0, pub.countInteractionRequests(),
		"a re-surface must not stack a credential_link card on top of a credential-update card")
}

// TestCredentialRequestWatcher_ForcePublish_StillPublishesForAPassthroughPark
// is the other half: the gate above must be selective, not a blanket refusal
// to re-surface. A session parked by the passthrough identity gate still gets
// its card re-posted.
func TestCredentialRequestWatcher_ForcePublish_StillPublishesForAPassthroughPark(t *testing.T) {
	sess := fixtureSession(t)
	sui := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: testSession, Namespace: testNS},
		Spec:       spiceboxv1alpha1.SessionUserIdentitySpec{AgentSession: testSession, Subject: testStarter},
		Status: spiceboxv1alpha1.SessionUserIdentityStatus{
			MissingCredentials: []string{"GitHub"},
			Explanation: &spiceboxv1alpha1.CredentialExplanation{
				Items: []spiceboxv1alpha1.CredentialExplanationItem{
					{Credential: "GitHub", Title: "GitHub", Why: "why"},
				},
			},
		},
	}

	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess, sui).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SessionUserIdentity{}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialRequestWatcher{
		K8s:             cli,
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return "https://identityd.example.test" },
		NATSPublish:     pub.publish,
		Now:             fixedFutureNow,
	}

	require.NoError(t, w.ForcePublish(context.Background(), sess))
	assert.Equal(t, 1, pub.countInteractionRequests(),
		"a passthrough-gate park must still re-surface its credential_link card")
}

// TestCredentialRequestWatcher_ReconcileAll_SkipsCredentialUpdateParkedSession
// covers the same gate at the reconcileAll (List + filter) level, alongside a
// genuinely passthrough-gate-parked session to prove the filter is
// selective, not a blanket skip.
func TestCredentialRequestWatcher_ReconcileAll_SkipsCredentialUpdateParkedSession(t *testing.T) {
	credUpdateParked := credentialUpdateParkedSession(t, func(s *spiceboxv1alpha1.AgentSession) {
		s.Name = "sess-credupdate"
	})
	passthroughParked := fixtureSession(t, func(s *spiceboxv1alpha1.AgentSession) {
		s.Name = "sess-passthrough"
	})
	sui := &spiceboxv1alpha1.SessionUserIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-passthrough", Namespace: testNS},
		Spec:       spiceboxv1alpha1.SessionUserIdentitySpec{AgentSession: "sess-passthrough", Subject: testStarter},
		Status: spiceboxv1alpha1.SessionUserIdentityStatus{
			MissingCredentials: []string{"GitHub"},
			Explanation: &spiceboxv1alpha1.CredentialExplanation{
				Items: []spiceboxv1alpha1.CredentialExplanationItem{
					{Credential: "GitHub", Title: "GitHub", Why: "why"},
				},
			},
		},
	}

	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(credUpdateParked, passthroughParked, sui).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}, &spiceboxv1alpha1.SessionUserIdentity{}).
		Build()
	pub := &capturingPublisher{}
	w := &CredentialRequestWatcher{
		K8s:             cli,
		LinkSigner:      newSigner(t),
		ExternalBaseURL: func() string { return "https://identityd.example.test" },
		NATSPublish:     pub.publish,
		Now:             fixedFutureNow,
	}

	w.reconcileAll(context.Background(), discardLogger(t))

	require.Equal(t, 1, pub.countInteractionRequests(),
		"exactly one card -- the passthrough-gate session, never the credential_update-parked one")
	pl := pub.findInteractionRequest(t)
	assert.Equal(t, "sess-passthrough", pl.AgentSessionRef.Name)
}
