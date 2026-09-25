// pkg/controllers/agentsession/owner_write_ordering_test.go
//
// Reconcile-level (fake-client) tests pinning WHEN agentsession#owner is
// written relative to the bundle/detector provisioning gates.
//
// The bug these guard against: ResolveAndWriteOwners used to run at step 3 of
// Reconcile, *after* the step-2 bundle-readiness block, which returns early
// (RequeueAfter) on every pass while a bundle is still provisioning — and
// returns terminally via markBootFailed when provisioning fails. A session
// whose bundles never reached Ready therefore never reached step 3, so
// agentsession#owner was NEVER written for it.
//
// The blast radius is every gate that resolves through #owner:
//
//	permission fork        = owner   ← thread continuation / restart-from-here
//	permission interact    = owner + participant - denied
//	permission manage_scope = owner
//	permission approve     = owner
//
// Observed in production: a Slack thread whose session died during bundle
// provisioning was acked with "…I'm picking it up in a new session…", then the
// operator's SessionFork gate denied the session's OWN starter with
// ForkNotAuthorized, because SpiceDB held only a started_by tuple and no owner.
// The owner write is cheap and idempotent (TOUCH), so it belongs ahead of every
// provisioning gate, not behind them.
package agentsession_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// ownerRecordingGranter implements authz.Granter and records every TouchOwner
// subject in call order. Only TouchOwner is interesting here; the rest satisfy
// the interface. failOwnerSubject, when non-empty, makes TouchOwner error for
// exactly that subject WITHOUT recording it — the shape of SpiceDB refusing a
// subject type the live schema does not yet admit.
type ownerRecordingGranter struct {
	owners           []string
	failOwnerSubject string
}

func (g *ownerRecordingGranter) TouchStartedBy(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	return nil
}

func (g *ownerRecordingGranter) TouchOwner(_ context.Context, _, _, subjectRef string) error {
	if g.failOwnerSubject != "" && subjectRef == g.failOwnerSubject {
		return errors.New("subject type not allowed on relation owner (schema not yet composed)")
	}
	g.owners = append(g.owners, subjectRef)
	return nil
}

func (g *ownerRecordingGranter) TouchInteractParticipant(_ context.Context, _, _, _ string) error {
	return nil
}

func (g *ownerRecordingGranter) TouchInteractParticipantUser(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	return nil
}

func (g *ownerRecordingGranter) TouchDeniedUser(_ context.Context, _, _ string, _ identity.CanonicalUserID) error {
	return nil
}

func (g *ownerRecordingGranter) TouchInteractor(_ context.Context, _, _, _ string) error {
	return nil
}

// crashedBundle returns a bundle SpiceboxSession carrying a true Failed
// condition — the shape a PodCrashed bundle presents, which drives the
// retry-once branch and then the terminal BundleFailed branch. Both of those
// return early from Reconcile, so neither ever reached the owner write.
func crashedBundle(sessName, bundleName string, created time.Time) *spiceboxv1alpha1.SpiceboxSession {
	b := notReadyBundle(sessName, bundleName, created)
	b.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.SpiceboxSessionConditionFailed,
		Status:             metav1.ConditionTrue,
		Reason:             "PodCrashed",
		Message:            "Pod entered Failed phase",
		LastTransitionTime: metav1.NewTime(created),
	}}
	return b
}

// TestReconcileWritesOwnerBeforeBundleGates asserts the owner tuple is written
// for a session whose bundle never becomes Ready, across every way the bundle
// gate can short-circuit Reconcile before the owner write:
//
//   - still provisioning        → RequeueAfter early-return
//   - past the ready deadline   → markBootFailed
//   - crashed (PodCrashed)      → retry-once early-return, then terminal
//
// The third case is the one observed in production. In all three the session is
// one a user can still reply to in-thread, so #owner must exist for the fork
// gate to allow the continuation.
func TestReconcileWritesOwnerBeforeBundleGates(t *testing.T) {
	const starter = "user:YWJjQGV4YW1wbGUuY29t"
	nowT := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return nowT }

	cases := []struct {
		name   string
		bundle func() *spiceboxv1alpha1.SpiceboxSession
	}{
		{
			name: "bundle still provisioning (reconcile requeues): owner written anyway",
			bundle: func() *spiceboxv1alpha1.SpiceboxSession {
				return notReadyBundle("s1", "code", nowT.Add(-10*time.Second))
			},
		},
		{
			name: "bundle provisioning past deadline (session fails): owner written anyway",
			bundle: func() *spiceboxv1alpha1.SpiceboxSession {
				// > bundleReadyDeadline (8m)
				return notReadyBundle("s1", "code", nowT.Add(-9*time.Minute))
			},
		},
		{
			name: "bundle crashed, retry-once early-return: owner written anyway",
			bundle: func() *spiceboxv1alpha1.SpiceboxSession {
				return crashedBundle("s1", "code", nowT.Add(-30*time.Second))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := classWithBundle("ac1", spiceboxv1alpha1.ToolBundle{Name: "code", Class: "toolbelt"})
			sess := sessionCreatedAt("s1", "ac1", nowT.Add(-12*time.Minute))
			sess.Annotations = map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: starter,
			}

			r, _ := fakeReconciler(t, clock, ac, sess, tc.bundle())
			granter := &ownerRecordingGranter{}
			r.AuthzGranter = granter

			runReconciles(t, r, "s1", 5)

			require.NotEmpty(t, granter.owners,
				"agentsession#owner must be written even though the bundle never became Ready — "+
					"otherwise fork/interact/approve all deny the session's own starter")
			assert.Equal(t, starter, granter.owners[0],
				"owner subject resolves from the started-by annotation")
		})
	}
}
