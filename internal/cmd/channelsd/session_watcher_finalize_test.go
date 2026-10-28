package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/watchdog"
)

// finalizingSender is a capturingSender that also implements the optional
// channelkinds.SessionFinalizer, recording each call (and optionally failing).
type finalizingSender struct {
	capturingSender
	fmu       sync.Mutex
	finalized []bool // the failed flag of each FinalizeSession call
	err       error
}

func (s *finalizingSender) FinalizeSession(_ context.Context, _ channelkinds.SessionInfo, failed bool) error {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	s.finalized = append(s.finalized, failed)
	return s.err
}

func (s *finalizingSender) calls() []bool {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	return append([]bool(nil), s.finalized...)
}

type finalizingResolver struct {
	fakeSenderResolver
	sender *finalizingSender
}

func (r *finalizingResolver) SenderFor(context.Context, *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	return r.sender, nil
}

func terminalSession(phase string, finishedAgo time.Duration) *spiceboxv1alpha1.AgentSession {
	sess := failedChannelSession("t1", "fake", "fake")
	sess.Status.Phase = phase
	fin := metav1.NewTime(time.Now().Add(-finishedAgo))
	sess.Status.FinishedAt = &fin
	return sess
}

// A session the operator ended — never closed out by its runner — has its
// working indicators taken down once: the clear sentinel AND the kind's
// finalizer, on both failure and success, and not again on the next tick.
func TestSessionWatcher_TerminalCleanup(t *testing.T) {
	cases := []struct {
		name          string
		phase         string
		finishedAgo   time.Duration
		wantClears    int
		wantFinalized []bool
	}{
		{"recently failed: one clear, finalized as failed", spiceboxv1alpha1.AgentSessionPhaseFailed, time.Minute, 1, []bool{true}},
		{"recently succeeded: one clear, finalized as not failed", spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Minute, 1, []bool{false}},
		{"ended long ago: left alone (no restart storm)", spiceboxv1alpha1.AgentSessionPhaseFailed, 2 * time.Hour, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := terminalSession(tc.phase, tc.finishedAgo)
			cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
				WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
			sender := &finalizingSender{}
			res := &finalizingResolver{sender: sender}
			w := newSessionWatcher(cli, res, newStatusWatchdog(cli, res))
			w.publish = (&publishRecorder{}).publish

			w.reconcile(context.Background(), testr.New(t))
			w.reconcile(context.Background(), testr.New(t))

			var clears int
			for _, n := range sender.captured() {
				if n.Payload.Text == "" {
					clears++
				}
			}
			assert.Equal(t, tc.wantClears, clears, "clear sentinel count across two ticks")
			assert.Equal(t, tc.wantFinalized, sender.calls(), "finalizer calls across two ticks")
		})
	}
}

// A failed finalize is not memoized: the next tick tries again.
func TestSessionWatcher_TerminalCleanupRetriesAfterFailure(t *testing.T) {
	sess := terminalSession(spiceboxv1alpha1.AgentSessionPhaseFailed, time.Minute)
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &finalizingSender{err: errors.New("slack: ratelimited")}
	res := &finalizingResolver{sender: sender}
	w := newSessionWatcher(cli, res, newStatusWatchdog(cli, res))
	w.publish = (&publishRecorder{}).publish

	w.reconcile(context.Background(), testr.New(t))
	sender.fmu.Lock()
	sender.err = nil
	sender.fmu.Unlock()
	w.reconcile(context.Background(), testr.New(t))
	w.reconcile(context.Background(), testr.New(t))

	assert.Equal(t, []bool{true, true}, sender.calls(), "retried once after the failure, then done")
}

// A session HELD for capacity gets one durable "queued" notice per hold
// episode — published (so the relay opens a triggered session's thread), not
// handed to the default sender — and is not re-posted on the next tick.
func TestSessionWatcher_CapacityHoldNotice(t *testing.T) {
	sess := sessionWithSchedulingCondition("h1", "uid-h1", metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable)
	sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionCapacityHold, Status: metav1.ConditionTrue,
		Reason:             spiceboxv1alpha1.ReasonCapacityHoldWaiting,
		Message:            "The cluster is out of room right now; this will start on its own once capacity frees up (gives up at 17:23 UTC).",
		LastTransitionTime: metav1.Now(),
	})
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{}
	w := newSessionWatcher(cli, &fakeSenderResolver{sender: &capturingSender{}}, nil)
	w.publish = pub.publish

	w.reconcile(context.Background(), testr.New(t))
	w.reconcile(context.Background(), testr.New(t))

	got := pub.interactions(t, "default", "h1")
	require.Len(t, got, 1, "one queued notice for the hold episode")
	assert.Equal(t, categories.SessionQueued, got[0].Category)
	assert.Contains(t, got[0].Body, "gives up at 17:23 UTC")

	var stored spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &stored))
	assert.NotEmpty(t, stored.Annotations[capacityHoldNoticeAnnotation],
		"the episode is recorded durably so a channelsd restart does not repost")
}

// While held, silence is expected — the watchdog must not warn "taking longer".
func TestWaitFromStatus_CapacityHoldIsVisible(t *testing.T) {
	sess := sessionWithSchedulingCondition("h1", "uid-h1", metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable)
	assert.Equal(t, watchdog.WaitActive, waitFromStatus(sess), "a plain scheduling stall stays an active wait")
	sess.Status.Conditions = append(sess.Status.Conditions, metav1.Condition{
		Type: spiceboxv1alpha1.AgentSessionConditionCapacityHold, Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonCapacityHoldWaiting,
	})
	assert.Equal(t, watchdog.WaitVisible, waitFromStatus(sess), "a held session is a visible wait")
}
