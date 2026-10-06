// pkg/controllers/agentsession/sessiongc_test.go
//
// These tests pin the session-lifetime GC: a terminal AgentSession past its
// SessionGCAfter window has its whole CR deleted (cascade-collecting the
// ToolCalls it owns), while a disabled sweep, a non-terminal session, and a
// still-within-window session are all left untouched. The reconcile-level test
// proves the sweep is actually wired into the terminal block rather than only
// callable in isolation.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// sessionDeletionRequested reports whether the fixture's AgentSession has been
// deleted — either fully gone, or (with the finalizer still attached, as the
// fixture seeds it) marked with a deletionTimestamp that routes the next
// reconcile to finalize.
func sessionDeletionRequested(t *testing.T, f reapFixture) bool {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	err := f.c.Get(context.Background(), client.ObjectKey{Namespace: f.sess.Namespace, Name: f.sess.Name}, &got)
	if apierrors.IsNotFound(err) {
		return true
	}
	require.NoError(t, err, "get session to inspect deletion state")
	return !got.DeletionTimestamp.IsZero()
}

func TestReconcileSessionGC(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name          string
		phase         string
		finishedDelta time.Duration // status.finishedAt relative to the fixture clock
		gcAfter       time.Duration
		wantDeleted   bool
		wantRequeue   bool // requeueAfter > 0 (a deadline is scheduled)
	}{
		{
			name:          "disabled (gcAfter=0): a long-dead terminal session is never collected",
			phase:         spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			finishedDelta: -60 * day, gcAfter: 0,
			wantDeleted: false, wantRequeue: false,
		},
		{
			name:          "non-terminal (Idle): never collected, even past the window",
			phase:         spiceboxv1alpha1.AgentSessionPhaseIdle,
			finishedDelta: -60 * day, gcAfter: 30 * day,
			wantDeleted: false, wantRequeue: false,
		},
		{
			name:          "terminal within window: kept, a deadline is scheduled",
			phase:         spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			finishedDelta: -1 * time.Hour, gcAfter: 30 * day,
			wantDeleted: false, wantRequeue: true,
		},
		{
			name:          "Succeeded past window: CR deleted",
			phase:         spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			finishedDelta: -31 * day, gcAfter: 30 * day,
			wantDeleted: true, wantRequeue: false,
		},
		{
			name:          "Failed past window: CR deleted",
			phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			finishedDelta: -31 * day, gcAfter: 30 * day,
			wantDeleted: true, wantRequeue: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReapFixture(t, tc.phase, time.Minute, tc.finishedDelta)
			f.r.SessionGCAfter = tc.gcAfter

			requeue, deleted, err := f.r.reconcileSessionGC(context.Background(), f.sess)
			require.NoError(t, err, "reconcileSessionGC")

			assert.Equal(t, tc.wantDeleted, deleted, "deleted result")
			if tc.wantRequeue {
				assert.Positive(t, requeue, "a within-window session must schedule its GC deadline")
			} else {
				assert.Zero(t, requeue, "no deadline expected")
			}
			assert.Equal(t, tc.wantDeleted, sessionDeletionRequested(t, f),
				"the CR is deleted iff GC fired")
		})
	}
}

// TestReconcile_TerminalSession_GCdPastRetention proves the sweep is wired into
// the terminal reconcile path, not merely callable: a full Reconcile of a
// terminal session past its GC window must request the CR's deletion.
func TestReconcile_TerminalSession_GCdPastRetention(t *testing.T) {
	const day = 24 * time.Hour
	f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Minute, -31*day)
	f.r.SessionGCAfter = 30 * day

	require.False(t, sessionDeletionRequested(t, f), "precondition: the session is live")

	f.reconcile(t)

	assert.True(t, sessionDeletionRequested(t, f),
		"a terminal session past its GC window must be deleted through Reconcile")
}
