package sessionhold_test

// The mirror of pkg/controllers/agentsession/hold_containment_test.go. Two
// controllers write this object's status; each owns one field and each must
// skip the write when its own text is unchanged.
//
// This side's equality check already existed, inside recordCascadeFailure, and
// was NOT covered by any test — which is how the half of a two-part fix goes
// quietly missing. A cascade error is returned so controller-runtime requeues
// with backoff, so this path runs again and again while the cascade keeps
// failing; without the check each pass writes status, and every status write
// fires the AgentSession reconciler's watch on the same object.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	memorypkg "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// countingStatusClient counts status writes. Counting the WRITE and not the
// resulting value is the point: a patch that changes nothing still bumps
// resourceVersion and still wakes the other controller, so a test that only
// compared the stored text would pass against the loop.
type countingStatusClient struct {
	client.Client
	writes *int
}

func (c countingStatusClient) Status() client.SubResourceWriter {
	return countingStatusWriter{SubResourceWriter: c.Client.Status(), writes: c.writes}
}

type countingStatusWriter struct {
	client.SubResourceWriter
	writes *int
}

func (w countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	*w.writes++
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (w countingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	*w.writes++
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

// TestARepeatedCascadeFailureStopsWritingStatus.
//
// The first pass records the failure; every pass after it must be silent while
// the failure text is unchanged. Reconcile is driven twice against the same
// permanently-failing cascade, and the write count is taken only over the
// second — the pass that, without the check, is the loop.
func TestARepeatedCascadeFailureStopsWritingStatus(t *testing.T) {
	root := treeSession("dup-root", "", "")
	root.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "slack", Key: "thread:C1:1"}
	root.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
		spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
	}
	// A child naming a parent that does not exist is the established way to
	// make the descendant walk fail without a real API server.
	ghost := treeSession("dup-ghost", "missing-parent", "dup-root")
	rootHold := holdOn("dup-root-hold", "dup-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, ghost, rootHold)

	_, err := reconcileNamed(t, r, "dup-root-hold")
	require.Error(t, err, "precondition: the cascade fails")
	first := getHoldNamed(t, c, "dup-root-hold")
	require.Contains(t, first.Status.Determination, "cascade FAILED",
		"precondition: the first pass recorded the failure")

	// Second pass, same unchanged failure. Count writes only from here.
	writes := 0
	r.Client = countingStatusClient{Client: c, writes: &writes}
	_, err = reconcileNamed(t, r, "dup-root-hold")
	require.Error(t, err, "the cascade still fails, so the error still surfaces for the requeue")

	assert.Zero(t, writes,
		"an unchanged cascade failure must not re-write status: the value would be identical, but the write still wakes the AgentSession reconciler, which writes back")
}

// TestTheCascadeFailureNeverWritesContainment pins the other half of the
// ownership split from this side.
//
// Containment belongs to the AgentSession reconciler and carries the session's
// own freeze narrative — including a failed workspace snapshot. A cascade
// failure overwriting it would erase a report about evidence that is already
// lost, in exactly the doubly-degraded state where an operator needs both
// sentences.
func TestTheCascadeFailureNeverWritesContainment(t *testing.T) {
	const priorContainment = "held; workspace snapshot FAILED: pvc not bound"

	root := treeSession("own-root", "", "")
	root.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "slack", Key: "thread:C1:1"}
	root.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: "U-OWNER",
		spiceboxv1alpha1.AnnotationStartedByEmail:      "owner@example.com",
	}
	ghost := treeSession("own-ghost", "missing-parent", "own-root")

	rootHold := holdOn("own-root-hold", "own-root", "manual", spiceboxv1alpha1.SessionHoldPhaseActive)
	// The AgentSession reconciler already recorded a snapshot failure here.
	rootHold.Status.Containment = priorContainment

	mem := memorypkg.NewLocal(inmem.NewBackend())
	c, r, _ := newFixture(t, mem, root, ghost, rootHold)

	_, err := reconcileNamed(t, r, "own-root-hold")
	require.Error(t, err, "precondition: the cascade fails")

	got := getHoldNamed(t, c, "own-root-hold")
	assert.Contains(t, got.Status.Determination, "cascade FAILED",
		"this controller records its own failure on the field it owns")
	assert.Equal(t, priorContainment, got.Status.Containment,
		"and leaves the AgentSession reconciler's narrative untouched — both failures are true, and an operator needs both")
}
