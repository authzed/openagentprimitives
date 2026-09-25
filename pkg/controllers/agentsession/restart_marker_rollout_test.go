package agentsession_test

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/restartmarker"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// TestReconcileRestart_UnsignedMarkerIsRefusedAsARolloutNotAForgery pins the
// two DIFFERENT facts a failed marker verification can carry, and the two
// different things the user is told about them.
//
// `oap install` rolls the operator and channelsd as independent Deployments with
// no ordering between them, and only the new channelsd carries the marker
// signer. So a marker with no attestation envelope at all is what an OLD
// channelsd writes during every upgrade — and what any marker already sitting on
// an AgentSession at upgrade time looks like. Refusing those with the forgery
// reason and "couldn't be verified as coming from the connector" hands every
// restarting user a security-flavoured refusal on a routine upgrade, and spikes
// an ops signal that means "someone forged a marker".
//
// A marker that carries an attestation which does not verify is the opposite
// fact: nothing a legitimate writer, old or new, ever produces. That half stays
// a hard forgery denial — including the near-miss shape (an envelope with the
// signature bytes emptied), which a forger would otherwise use to elect the
// gentler treatment.
//
// Both halves stay TERMINAL, and neither ever materializes a child: the fix is
// to classify the refusal, not to soften it. Terminality is what makes the
// user's own re-send the retry — nothing re-signs a marker already on the object
// (channelsd's fork triggers are first-writer-wins), and a retained marker
// wedges the thread because every later reply short-circuits on it.
func TestReconcileRestart_UnsignedMarkerIsRefusedAsARolloutNotAForgery(t *testing.T) {
	// A takeover marker: the mode that skips the SpiceDB fork gate entirely, so
	// the marker's attestation is the only thing standing between a named victim
	// and a child stamped with their identity.
	armed := func() *spiceboxv1alpha1.AgentSession {
		return &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "p", Namespace: "ns", UID: "parent-uid",
				Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:starter"},
			},
			Status: spiceboxv1alpha1.AgentSessionStatus{
				Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
				PendingRestart: &spiceboxv1alpha1.PendingRestart{
					Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
					NewUserText:       "carry this on",
					TriggeredBy:       "user:newowner",
					TargetSessionName: "p-tk1",
					InheritHistory:    true,
					RequestedAt:       metav1.NewTime(time.Unix(1, 0)),
				},
			},
		}
	}

	// A signer the operator's registry does not know: a second component, or an
	// attacker who minted their own keypair.
	untrustedSeed := make([]byte, ed25519.SeedSize)
	for i := range untrustedSeed {
		untrustedSeed[i] = 0x77
	}
	untrusted := restartmarker.NewSigner(ed25519.NewKeyFromSeed(untrustedSeed), restartmarker.Publisher)

	cases := []struct {
		name string
		// arm prepares the marker's attestation (or leaves it absent).
		arm func(t *testing.T, sess *spiceboxv1alpha1.AgentSession)
		// wantReason is the RestartDenied reason this class of failure must carry.
		wantReason string
	}{
		{
			name: "marker from a pre-upgrade connector (no attestation envelope): refused as a rollout, retry wording",
			arm:  func(*testing.T, *spiceboxv1alpha1.AgentSession) {},
			// NOT the forgery reason: an upgrade is not an attack, and an
			// alert keyed on the forgery reason must not fire on every rollout.
			wantReason: agentsession.ReasonRestartMarkerUnsigned,
		},
		{
			name: "forged: attestation envelope kept but signature bytes emptied: refused as a forgery",
			arm: func(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
				armSignedRestart(t, sess)
				// No legitimate writer produces this: old channelsd had no
				// envelope at all, new channelsd always fills it. A forger must
				// not reach the gentler rollout wording by emptying one field.
				sess.Status.PendingRestart.Signature.Sig = nil
			},
			wantReason: spiceboxv1alpha1.ReasonRestartMarkerUnverified,
		},
		{
			name: "forged: signed by a key the operator does not trust: refused as a forgery",
			arm: func(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
				require.NoError(t, untrusted.Sign(sess, sess.Status.PendingRestart))
			},
			wantReason: spiceboxv1alpha1.ReasonRestartMarkerUnverified,
		},
		{
			name: "forged: payload rewritten under the connector's signature: refused as a forgery",
			arm: func(t *testing.T, sess *spiceboxv1alpha1.AgentSession) {
				armSignedRestart(t, sess)
				sess.Status.PendingRestart.TriggeredBy = "user:victim"
			},
			wantReason: spiceboxv1alpha1.ReasonRestartMarkerUnverified,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := memorypkg.WithSystemApproval(context.Background(), "test")
			sess := armed()
			tc.arm(t, sess)

			c := fake.NewClientBuilder().
				WithScheme(restartScheme(t)).
				WithObjects(sess).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
				Build()
			r := &agentsession.Reconciler{
				Client:        c,
				RestartMemory: memorypkg.NewLocal(inmem.NewBackend()),
				AuthzGranter:  &fakeGranter{},
				Snapshotter:   &fakeSnapshotter{},
				// allow=true so a refusal can only have come from the marker's
				// attestation, never from the fork gate.
				ForkChecker:       fakeForkChecker{allow: true},
				DeniedLister:      fakeDeniedLister{},
				ForkNoticePublish: func(_ context.Context, _, _, _, _ string) error { return nil },
				PublisherKeys:     testMarkerKeys,
			}

			proceed, res, err := r.ReconcileRestart(ctx, sess)
			require.NoError(t, err, "an unverifiable marker is refused, not propagated as an error")
			assert.False(t, proceed)
			assert.Zero(t, res.RequeueAfter,
				"neither class requeues: nothing re-signs a marker already on the object, and a "+
					"retained marker wedges every later reply in the thread")

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &got))

			// Neither class may fail open, whatever it is called.
			var child spiceboxv1alpha1.AgentSession
			assert.True(t, apierrors.IsNotFound(
				c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p-tk1"}, &child)),
				"an unattested marker must create no child; got one with started-by=%q",
				child.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
			assert.Empty(t, got.Status.SupersededBy, "the parent must not be superseded")
			assert.Nil(t, got.Status.PendingRestart,
				"the marker must be cleared: the user's re-send is the retry, and a retained "+
					"marker short-circuits every later reply")

			cond := meta.FindStatusCondition(got.Status.Conditions,
				spiceboxv1alpha1.AgentSessionConditionRestartDenied)
			require.NotNil(t, cond, "the refusal must be visible: channelsd relays this condition into the thread")
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, tc.wantReason, cond.Reason)
			require.NotEmpty(t, cond.Message, "a refusal the user cannot read is the failure mode this closes")

			if tc.wantReason == agentsession.ReasonRestartMarkerUnsigned {
				assert.Contains(t, strings.ToLower(cond.Message), "again",
					"the rollout refusal must tell the user to re-send, not that they were not believed")
			}
			// The message reaches a human in-thread, so it may not carry any of
			// the vocabulary a refusal like this invites (AGENTS.md: no internal
			// operations vocabulary in user-visible text).
			for _, banned := range []string{"signature", "signed", "unsigned", "pendingRestart", "RestartDenied", "keyID", "publisher"} {
				assert.NotContains(t, strings.ToLower(cond.Message), strings.ToLower(banned),
					"user-facing refusal text must not name %q", banned)
			}
		})
	}
}
