package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// pendingPod builds a Pending pod with the given PodScheduled condition (pass
// status="" to omit the condition entirely).
func pendingPod(phase corev1.PodPhase, status corev1.ConditionStatus, reason, msg string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if status != "" {
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: status, Reason: reason, Message: msg,
		}}
	}
	return p
}

func TestPodSchedulingFailureReason(t *testing.T) {
	cases := []struct {
		name       string
		pod        *corev1.Pod
		wantReason string
		wantOK     bool
	}{
		{
			name:       "Pending + PodScheduled=False with message: surfaces the scheduler message",
			pod:        pendingPod(corev1.PodPending, corev1.ConditionFalse, corev1.PodReasonUnschedulable, "0/3 nodes are available: 3 Insufficient cpu."),
			wantReason: "0/3 nodes are available: 3 Insufficient cpu.",
			wantOK:     true,
		},
		{
			name:       "Pending + PodScheduled=False, empty message: falls back to the condition reason",
			pod:        pendingPod(corev1.PodPending, corev1.ConditionFalse, corev1.PodReasonUnschedulable, ""),
			wantReason: corev1.PodReasonUnschedulable,
			wantOK:     true,
		},
		{
			name:   "Pending + PodScheduled=True: not a scheduling failure (fail-safe)",
			pod:    pendingPod(corev1.PodPending, corev1.ConditionTrue, "", ""),
			wantOK: false,
		},
		{
			name:   "Pending + no PodScheduled condition: fail-safe",
			pod:    pendingPod(corev1.PodPending, "", "", ""),
			wantOK: false,
		},
		{
			name:   "Running pod: not Pending, fail-safe",
			pod:    pendingPod(corev1.PodRunning, corev1.ConditionFalse, corev1.PodReasonUnschedulable, "x"),
			wantOK: false,
		},
		{
			name:   "nil pod: fail-safe",
			pod:    nil,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := podSchedulingFailureReason(tc.pod)
			assert.Equal(t, tc.wantOK, ok, "ok")
			if tc.wantOK {
				assert.Equal(t, tc.wantReason, reason, "reason")
			}
		})
	}
}

func TestSchedulingStallMessage(t *testing.T) {
	s := schedulingStall{
		podName: "codelike-abc",
		reason:  "0/3 nodes are available: 3 Insufficient cpu.",
	}
	msg := s.message()
	// Names the pod, carries the real reason + node count, and reads as a
	// waiting (not failed) state. The trailing period is trimmed.
	assert.Contains(t, msg, "codelike-abc")
	assert.Contains(t, msg, "unschedulable")
	assert.Contains(t, msg, "Insufficient cpu")
	assert.Contains(t, msg, "0/3 nodes")
	assert.Contains(t, msg, "waiting for capacity")
	assert.NotContains(t, msg, "cpu.;", "trailing period before the suffix is trimmed")
}

// TestUnschedulableSetsCondition covers maybeEmitUnschedulable's durable
// status-condition side effect in isolation: a stall past unschedulableGrace
// must set SandboxScheduling=False/Unschedulable with the stall's message,
// even when r.MonitoringPublish is nil (unconfigured NATS). channelsd reacts
// to the AgentSession CR's status, not to the monitoring event, so the
// condition must not depend on monitoring being wired — that was the bug in
// the naive "set it after the existing (MonitoringPublish-gated) logic"
// placement this test guards against.
func TestUnschedulableSetsCondition(t *testing.T) {
	r := &Reconciler{Now: func() time.Time { return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC) }}
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"}}
	stall := schedulingStall{
		podName: "codelike-x",
		podUID:  "uid-1",
		reason:  "0/3 nodes are available: 3 Insufficient cpu.",
		pending: 45 * time.Second, // past the 30s unschedulableGrace
	}

	// MonitoringPublish intentionally left nil.
	r.maybeEmitUnschedulable(context.Background(), sess, spiceboxv1alpha1.AgentSessionConditionRunnerReady, stall)

	c := apimeta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling)
	require.NotNil(t, c, "SandboxScheduling condition set even with MonitoringPublish unconfigured")
	assert.Equal(t, metav1.ConditionFalse, c.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonSandboxUnschedulable, c.Reason)
	assert.Equal(t, stall.message(), c.Message)
}

// TestMaybeEmitUnschedulableWithinGraceLeavesConditionUnset is the fail-safe
// twin: a stall that hasn't yet been stuck past unschedulableGrace must not
// flip SandboxScheduling to False — a transient scheduling blip should not
// flash a capacity warning at the user.
func TestMaybeEmitUnschedulableWithinGraceLeavesConditionUnset(t *testing.T) {
	r := &Reconciler{Now: func() time.Time { return time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC) }}
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"}}
	stall := schedulingStall{
		podName: "codelike-x",
		podUID:  "uid-2",
		reason:  "0/3 nodes are available: 3 Insufficient cpu.",
		pending: 5 * time.Second, // within the 30s unschedulableGrace
	}

	r.maybeEmitUnschedulable(context.Background(), sess, spiceboxv1alpha1.AgentSessionConditionRunnerReady, stall)

	c := apimeta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionSandboxScheduling)
	assert.Nil(t, c, "no verdict yet: stall hasn't been stuck past grace")
}
