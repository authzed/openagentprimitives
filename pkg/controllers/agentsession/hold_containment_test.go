package agentsession

// SessionHold status is written by TWO controllers. This file pins the
// AgentSession reconciler's half of the contract:
//
//   - it writes Containment and NEVER Determination, and
//   - it does not write at all when the text is unchanged.
//
// Both halves are required and neither is sufficient. Sharing one field made
// each controller ERASE the other's report; skipping the unchanged write is
// what stops them WAKING each other through the same object's status. A test
// that only checked the field split would pass against an implementation that
// still looped.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// countingStatusPatchClient counts status patches so a test can assert that an
// unchanged write never reaches the API server. Counting the WRITE rather than
// the resulting value is the point: a no-op merge patch still bumps
// resourceVersion and still fires the other controller's watch, so a test
// comparing only the stored text would pass against the loop.
type countingStatusPatchClient struct {
	client.Client
	patches *int
}

func (c countingStatusPatchClient) Status() client.SubResourceWriter {
	return countingStatusWriter{SubResourceWriter: c.Client.Status(), patches: c.patches}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	patches *int
}

func (w countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	*w.patches++
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func containmentHold(t *testing.T, containment, determination string) *spiceboxv1alpha1.SessionHold {
	t.Helper()
	now := metav1.Now()
	return &spiceboxv1alpha1.SessionHold{
		ObjectMeta: metav1.ObjectMeta{Name: "hold-1", Namespace: "demo", ResourceVersion: "10"},
		Spec: spiceboxv1alpha1.SessionHoldSpec{
			SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: "demo", Name: "demo-session"},
			Reason:     "12 consecutive out-of-ceiling calls",
			Source:     "tripper/plangate-denial-streak",
		},
		Status: spiceboxv1alpha1.SessionHoldStatus{
			Phase:         spiceboxv1alpha1.SessionHoldPhaseActive,
			TrippedAt:     &now,
			Containment:   containment,
			Determination: determination,
		},
	}
}

func newContainmentReconciler(t *testing.T, hold *spiceboxv1alpha1.SessionHold) (*Reconciler, client.Client, *int) {
	t.Helper()
	patches := 0
	base, _ := newParkTestReconciler(t, hold)
	return &Reconciler{Client: countingStatusPatchClient{Client: base, patches: &patches}}, base, &patches
}

// TestBothNarrativesSurviveTogether is the whole reason for the split.
//
// A workspace snapshot failure and a cascade failure are INDEPENDENT and can
// be true at the same moment. On one shared field each writer erased the
// other, so an operator inspecting a doubly-degraded hold saw only whichever
// controller wrote last — in exactly the state where they need both. This
// asserts the pair coexists.
func TestBothNarrativesSurviveTogether(t *testing.T) {
	// The SessionHold controller has already recorded its cascade failure.
	hold := containmentHold(t, "", "cascade FAILED: label too long -- descendants may still be running unheld")
	r, c, _ := newContainmentReconciler(t, hold)

	r.setHoldContainment(context.Background(), hold,
		snapshotFailedContainment(errors.New("pvc not bound")), log.FromContext(context.Background()))

	var got spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(hold), &got))
	assert.Contains(t, got.Status.Containment, "snapshot FAILED",
		"the session's own containment failure must be recorded")
	assert.Contains(t, got.Status.Determination, "cascade FAILED",
		"and it must not have erased the other controller's report — both are true, and an operator needs both")
}

// TestAnUnchangedContainmentIssuesNoWrite is the anti-loop half.
//
// The snapshot-failure branch runs on EVERY reconcile until the snapshot
// succeeds, and each status write fires the SessionHold controller's watch,
// which re-derives its own answer and writes back. Without this check the two
// controllers wake each other indefinitely over a hold that is not changing.
func TestAnUnchangedContainmentIssuesNoWrite(t *testing.T) {
	want := snapshotFailedContainment(errors.New("pvc not bound"))
	hold := containmentHold(t, want, "")
	r, _, patches := newContainmentReconciler(t, hold)

	r.setHoldContainment(context.Background(), hold, want, log.FromContext(context.Background()))

	assert.Zero(t, *patches,
		"identical text must not be written: the patch is a no-op to the VALUE but still bumps resourceVersion and wakes the other controller")
}

// TestAChangedContainmentIsWritten — the other direction, so the test above
// cannot be satisfied by a helper that simply never writes.
func TestAChangedContainmentIsWritten(t *testing.T) {
	hold := containmentHold(t, "held: 12 consecutive out-of-ceiling calls", "")
	r, c, patches := newContainmentReconciler(t, hold)

	r.setHoldContainment(context.Background(), hold,
		snapshotFailedContainment(errors.New("pvc not bound")), log.FromContext(context.Background()))

	assert.Equal(t, 1, *patches, "a real change must reach the API server exactly once")
	var got spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(hold), &got))
	assert.Contains(t, got.Status.Containment, "snapshot FAILED")
}

// TestTheReconcilerNeverWritesDetermination pins ownership as a property of
// the text this file can produce, not of one code path.
//
// Determination belongs to the SessionHold controller. The failure this
// prevents is subtle: a future edit here that "helpfully" also sets
// Determination would compile, pass every behavioural test, and silently
// restore the erasure the split removed.
func TestTheReconcilerNeverWritesDetermination(t *testing.T) {
	const priorDetermination = "released: cascaded from hold-root"
	hold := containmentHold(t, "", priorDetermination)
	r, c, _ := newContainmentReconciler(t, hold)

	for _, want := range []string{
		heldContainment(hold),
		snapshotFailedContainment(errors.New("pvc not bound")),
	} {
		r.setHoldContainment(context.Background(), hold, want, log.FromContext(context.Background()))
	}

	var got spiceboxv1alpha1.SessionHold
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(hold), &got))
	assert.Equal(t, priorDetermination, got.Status.Determination,
		"this reconciler owns Containment only; Determination must survive untouched")
}

// TestContainmentTextIsAPureFunctionOfItsInputs.
//
// Both messages must be derivable from their inputs alone, with no append onto
// whatever the field already held. An accumulating message can never compare
// equal to itself, which would defeat the equality check above and grow the
// field without bound — the same property recordCascadeFailure documents on
// the SessionHold side.
func TestContainmentTextIsAPureFunctionOfItsInputs(t *testing.T) {
	hold := containmentHold(t, "some earlier text", "")
	assert.Equal(t, heldContainment(hold), heldContainment(hold))

	err := errors.New("pvc not bound")
	assert.Equal(t, snapshotFailedContainment(err), snapshotFailedContainment(err),
		"a repeated failure must produce byte-identical text, or the equality check can never fire")
	assert.NotContains(t, heldContainment(hold), "some earlier text",
		"the message must not append onto what the field already held")
}
