package pipeline

// annotateWake must wrap its Get+Patch in RetryOnConflict. A single Patch
// against the stale session object Deliver classified loses the optimistic-lock
// race roughly 1-in-3 times, because the operator writes the same session's
// status concurrently (Idle sessions are reconciled on every loop). A
// propagated Conflict silently drops the wake: the inbound is already in memory
// but no runner is ever respawned, so the user's reply strands with no signal.
//
// These tests pin:
//  1. A Conflict on the first Patch is retried and the annotation is eventually
//     written.
//  2. If the session raced to a terminal phase between classification and the
//     Get inside the retry, the annotation is NOT written and nil is returned —
//     the wake-a-terminal protection survives the retry path.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// conflictOnFirstPatchClient wraps a real fake client and injects a Conflict
// error on the very first Patch call, then delegates subsequent calls to the
// underlying client. This replicates the race where annotateWake reads a stale
// session whose resourceVersion was already bumped by a concurrent operator
// status write.
type conflictOnFirstPatchClient struct {
	client.Client
	patchCalls int
}

func (c *conflictOnFirstPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patchCalls++
	if c.patchCalls == 1 {
		return apierrors.NewConflict(
			spiceboxv1alpha1.Resource("agentsessions"),
			obj.GetName(),
			fmt.Errorf("the object has been modified; please apply your changes to the latest version"),
		)
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// newWakeTestScheme builds a minimal Scheme for annotateWake tests.
func newWakeTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

// TestAnnotateWake_RetriesOnConflictAndWritesAnnotation is the primary
// regression pin: a Conflict on the first Patch must not drop the wake. The
// retry must re-read the session, re-check respawnability, and succeed on the
// second Patch, leaving the wake-requested-at annotation present.
func TestAnnotateWake_RetriesOnConflictAndWritesAnnotation(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "idle-sess", Namespace: "default",
			ResourceVersion: "10",
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
		},
	}
	realFake := fake.NewClientBuilder().
		WithScheme(newWakeTestScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(sess).
		Build()
	wrapper := &conflictOnFirstPatchClient{Client: realFake}

	fixedNow := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	p := &Pipeline{K8s: wrapper, Now: func() time.Time { return fixedNow }}

	err := p.annotateWake(ctx, sess)
	require.NoError(t, err, "annotateWake must succeed after the conflict retry")
	assert.Equal(t, 2, wrapper.patchCalls,
		"must retry exactly once: 1 Conflict + 1 success")

	// Confirm the annotation was written to the stored object.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, realFake.Get(ctx, client.ObjectKey{Namespace: "default", Name: "idle-sess"}, &got))
	assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"wake annotation must be written after the successful retry")
}

// TestAnnotateWake_TerminalRaceSkipsAnnotation tests the terminal-race
// protection: when the session transitions to a non-respawnable phase
// (Succeeded, Failed) between classification and the Get inside the retry
// loop, annotateWake must return nil without writing the annotation. Waking a
// terminal session would never respawn a runner — the terminal-continuation
// (new inheriting session) path handles the user's next message instead.
func TestAnnotateWake_TerminalRaceSkipsAnnotation(t *testing.T) {
	ctx := context.Background()
	// The session is already Succeeded in the store — simulating the race
	// where the operator archived it between our classify and our annotate.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "raced-sess", Namespace: "default",
			ResourceVersion: "5",
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded,
		},
	}
	realFake := fake.NewClientBuilder().
		WithScheme(newWakeTestScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(sess).
		Build()

	fixedNow := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	p := &Pipeline{K8s: realFake, Now: func() time.Time { return fixedNow }}

	// Pass a stale Idle copy — as if we classified it before the race.
	stale := sess.DeepCopy()
	stale.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle

	err := p.annotateWake(ctx, stale)
	require.NoError(t, err, "terminal race must return nil, not an error")

	// The annotation must NOT be present: annotateWake re-reads the session,
	// sees Succeeded (not respawn-eligible), logs, and returns nil.
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, realFake.Get(ctx, client.ObjectKey{Namespace: "default", Name: "raced-sess"}, &got))
	assert.Empty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"wake annotation must NOT be written when session raced to a terminal phase")
}

// The OPPOSITE race to TestAnnotateWake_TerminalRaceSkipsAnnotation, and the
// one that strands a user's message: we classified the session as Running (pod
// alive, so the inbound would ride a NATS wakeup) and by the time we act it has
// gone Idle and its pod has exited. Nothing will respawn it unless the wake
// annotation is written, and the inbound is already in memory — so a miss here
// is a turn that sits forever with no reply and no error.
//
// annotateWake must decide from the LIVE object, never from the caller's stale
// copy. This is the contract that lets the caller drop its own respawnOnWake
// pre-check: keeping that check meant the stale phase decided whether the
// re-read ever happened, which defeated the re-read in exactly this direction.
func TestAnnotateWake_StaleRunningButNowIdleStillAnnotates(t *testing.T) {
	ctx := context.Background()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "went-idle-sess", Namespace: "default",
			ResourceVersion: "7",
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhaseIdle,
		},
	}
	realFake := fake.NewClientBuilder().
		WithScheme(newWakeTestScheme(t)).
		WithObjects(sess).
		WithStatusSubresource(sess).
		Build()

	fixedNow := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	p := &Pipeline{K8s: realFake, Now: func() time.Time { return fixedNow }}

	// The copy the caller classified: still Running, pod believed alive.
	stale := sess.DeepCopy()
	stale.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning

	require.NoError(t, p.annotateWake(ctx, stale))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, realFake.Get(ctx, client.ObjectKey{Namespace: "default", Name: "went-idle-sess"}, &got))
	assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
		"the session is Idle NOW; without this annotation no runner respawns and the "+
			"inbound already in memory strands with no reply")
}
