package main

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// session_watcher_markers_test.go covers the watcher's per-session dedup
// markers: WHEN one is written (only after the action it dedups succeeded) and
// WHEN it is dropped (once the session it belongs to is gone).

// idleSession builds a channel-attached session parked in Phase=Idle with a
// LastIdleAt stamp — the shape maybeClearIdle keys its dedup on.
func idleSession(name string) *spiceboxv1alpha1.AgentSession {
	now := metav1.Now()
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: name, UID: types.UID("uid-" + name),
			Labels: map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch", Kind: "fake"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:      spiceboxv1alpha1.AgentSessionPhaseIdle,
			LastIdleAt: &now,
		},
	}
}

// A failed idle cleanup must be retried. LastIdleAt does not move while the
// session sits Idle, so marking the dedup BEFORE the Clear succeeded pinned the
// key forever and no later tick ever retried — leaving the "⚠️ Taking longer"
// indicator up on a session that had stopped working, which is the exact thing
// this cleanup exists to take down.
func TestSessionWatcherRetriesIdleClearAfterFailure(t *testing.T) {
	ctx := context.Background()
	sess := idleSession("s1")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &defaultKindSender{}
	res := &staticSenderResolver{sender: sender, resolveErr: errors.New("channel unreachable")}
	w := newSessionWatcher(cli, res, newStatusWatchdog(cli, res))

	w.reconcile(ctx, testr.New(t))
	require.Empty(t, sender.acceptedKinds(), "the clear could not be sent while the sender was unresolvable")
	w.mu.Lock()
	_, marked := w.idleHandled[string(sess.UID)]
	w.mu.Unlock()
	assert.False(t, marked, "a Clear that failed must not be recorded as handled")

	// The channel comes back.
	res.resolveErr = nil
	w.reconcile(ctx, testr.New(t))
	assert.Len(t, sender.acceptedKinds(), 1, "the retry must send the clear exactly once")

	// …and now that it landed, the 5s poll must stop re-clearing.
	w.reconcile(ctx, testr.New(t))
	assert.Len(t, sender.acceptedKinds(), 1, "a successful clear is deduped on the next tick")
}

// The marker maps are keyed by session UID and only ever written at their post
// sites, so without a prune a long-lived channelsd accumulates one entry per
// session it ever saw, for the life of the process.
func TestSessionWatcherPrunesMarkersForDeletedSessions(t *testing.T) {
	ctx := context.Background()
	sess := idleSession("s1")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	res := &staticSenderResolver{sender: &defaultKindSender{}}
	w := newSessionWatcher(cli, res, newStatusWatchdog(cli, res))

	w.reconcile(ctx, testr.New(t))
	w.mu.Lock()
	_, marked := w.idleHandled[string(sess.UID)]
	w.mu.Unlock()
	require.True(t, marked, "the idle cleanup must have recorded its marker")

	require.NoError(t, cli.Delete(ctx, sess))
	w.reconcile(ctx, testr.New(t))

	w.mu.Lock()
	defer w.mu.Unlock()
	assert.Empty(t, w.idleHandled, "a deleted session's marker must not outlive it")
}
