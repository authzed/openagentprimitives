package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// wakeSession builds a session in the given phase, optionally carrying an
// already-stamped wake request and the operator's last-consumed wake.
func wakeSession(t *testing.T, phase string, annotatedAt, lastWakeAt *time.Time) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", Generation: 1},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac"},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
	if annotatedAt != nil {
		s.Annotations = map[string]string{
			spiceboxv1alpha1.AnnotationWakeRequestedAt: annotatedAt.Format(time.RFC3339Nano),
		}
	}
	if lastWakeAt != nil {
		s.Status.LastWakeAt = &metav1.Time{Time: *lastWakeAt}
	}
	return s
}

// TestRequestWake covers the exiting runner's post-Idle respawn request. The
// two skips are as load-bearing as the write: annotating a live session would
// respawn a runner behind its own back, and re-annotating an already-pending
// wake can outlive the operator's status.lastWakeAt and fire a second,
// spurious respawn whose drain finds nothing to place.
func TestRequestWake(t *testing.T) {
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-time.Hour)

	cases := []struct {
		name        string
		sess        func(t *testing.T) *spiceboxv1alpha1.AgentSession
		wantWritten bool
	}{
		{
			name: "Idle with no prior request: annotation written, operator respawns",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				return wakeSession(t, spiceboxv1alpha1.AgentSessionPhaseIdle, nil, nil)
			},
			wantWritten: true,
		},
		{
			name: "Idle with an already-consumed request: re-annotated, the old stamp can no longer wake anything",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				return wakeSession(t, spiceboxv1alpha1.AgentSessionPhaseIdle, &older, &newer)
			},
			wantWritten: true,
		},
		{
			name: "Idle with a pending request: skipped, channelsd already asked for this wake",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				return wakeSession(t, spiceboxv1alpha1.AgentSessionPhaseIdle, &newer, &older)
			},
			wantWritten: false,
		},
		{
			name: "Running: skipped, a live runner must never be respawned under itself",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				return wakeSession(t, spiceboxv1alpha1.AgentSessionPhaseRunning, nil, nil)
			},
			wantWritten: false,
		},
		{
			name: "Failed: skipped, a terminal session would never respawn a runner",
			sess: func(t *testing.T) *spiceboxv1alpha1.AgentSession {
				return wakeSession(t, spiceboxv1alpha1.AgentSessionPhaseFailed, nil, nil)
			},
			wantWritten: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := tc.sess(t)
			before := sess.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt]
			c := fake.NewClientBuilder().
				WithScheme(newScheme(t)).
				WithObjects(sess).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
				Build()
			key := client.ObjectKeyFromObject(sess)
			ctx := context.Background()

			req, err := runner.NewStatusPatcher(c, key).RequestWake(ctx)
			require.NoError(t, err, "RequestWake")
			assert.Equal(t, tc.wantWritten, req.Outcome == runner.WakeStamped, "wrote the wake annotation (outcome=%s)", req.Outcome)

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, c.Get(ctx, key, &got), "Get after RequestWake")
			after := got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt]
			if tc.wantWritten {
				assert.NotEqual(t, before, after, "annotation advanced")
				assert.True(t, spiceboxv1alpha1.WakePending(&got), "the written request must be unconsumed, or the operator ignores it")
			} else {
				assert.Equal(t, before, after, "object left untouched")
			}
		})
	}
}

// TestRequestWakeDecidesFromAnUncachedRead pins the seam a stale read silently
// broke: RequestWake judges eligibility from the phase WriteIdle wrote
// microseconds earlier, so a client that reads through an informer cache hands
// back the PRE-idle phase and the wake is skipped — the fix disables itself and
// the message strands, with nothing in the unit suite to notice.
//
// The stale client below is the informer cache: writes land, reads lag.
func TestRequestWakeDecidesFromAnUncachedRead(t *testing.T) {
	newStale := func(t *testing.T) (client.Client, client.Client) {
		t.Helper()
		sess := wakeSession(t, spiceboxv1alpha1.AgentSessionPhaseIdle, nil, nil)
		fresh := fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithObjects(sess).
			WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
			Build()
		stale := fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithObjects(wakeSession(t, spiceboxv1alpha1.AgentSessionPhaseRunning, nil, nil)).
			WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
			WithInterceptorFuncs(interceptor.Funcs{
				// Writes go to the live object, exactly like a cached client:
				// only reads are stale.
				Patch: func(ctx context.Context, _ client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
					return fresh.Patch(ctx, obj, p, opts...)
				},
			}).
			Build()
		return stale, fresh
	}

	t.Run("cached reader alone: the wake is skipped and the message would strand", func(t *testing.T) {
		stale, _ := newStale(t)
		req, err := runner.NewStatusPatcher(stale, client.ObjectKey{Namespace: "default", Name: "s1"}).RequestWake(context.Background())
		require.NoError(t, err, "RequestWake")
		assert.Equal(t, runner.WakeNotParked, req.Outcome, "a cached read reports the pre-idle phase")
	})

	t.Run("direct reader installed: the wake is stamped from the live phase", func(t *testing.T) {
		stale, fresh := newStale(t)
		key := client.ObjectKey{Namespace: "default", Name: "s1"}
		req, err := runner.NewStatusPatcher(stale, key).WithDirectReader(fresh).RequestWake(context.Background())
		require.NoError(t, err, "RequestWake")
		assert.Equal(t, runner.WakeStamped, req.Outcome, "the live phase is Idle, so the respawn is requested")

		var got spiceboxv1alpha1.AgentSession
		require.NoError(t, fresh.Get(context.Background(), key, &got), "Get after RequestWake")
		assert.True(t, spiceboxv1alpha1.WakePending(&got), "the operator must see an unconsumed wake request")
	})
}

// TestRequestWakeLocalModeIsNoOp pins the kubectl-driven / in-process case:
// there is no AgentSession CR to patch and no operator to respawn anything.
func TestRequestWakeLocalModeIsNoOp(t *testing.T) {
	req, err := runner.LocalStatusPatcher().RequestWake(context.Background())
	require.NoError(t, err, "RequestWake in local mode must not error")
	assert.NotEqual(t, runner.WakeStamped, req.Outcome, "local mode writes nothing")
}
