package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// sessionWithSchedulingCondition builds a channel-attached, Pending
// AgentSession carrying a single SandboxScheduling condition. Mirrors the
// shape session_watcher's reconcile loop requires to reach the scheduling
// handler: LabelChannelName (the fake client's List(HasLabels{...}) selector)
// + Spec.InputChannel (the "channel-attached" gate) + a live/pending phase.
func sessionWithSchedulingCondition(name string, uid types.UID, status metav1.ConditionStatus, reason string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       uid,
			Labels:    map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "agent-a",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhasePending,
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
				Status:             status,
				Reason:             reason,
				Message:            "test stall",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// setSchedulingCondition overwrites sess's SandboxScheduling condition
// in-place, bumping LastTransitionTime — mirrors what the operator's
// conditions.Set does on a real status flip.
func setSchedulingCondition(sess *spiceboxv1alpha1.AgentSession, status metav1.ConditionStatus, reason string) {
	for i := range sess.Status.Conditions {
		if sess.Status.Conditions[i].Type == spiceboxv1alpha1.AgentSessionConditionSandboxScheduling {
			sess.Status.Conditions[i].Status = status
			sess.Status.Conditions[i].Reason = reason
			sess.Status.Conditions[i].LastTransitionTime = metav1.Now()
			return
		}
	}
}

// recordedNotification pairs a captured KindNotification payload with the
// session it was sent for, so tests can assert both content and target.
type recordedNotification struct {
	Namespace, Name string
	Payload         channelevents.NotificationPayload
}

// capturingSender is a channelkinds.Sender that decodes KindNotification
// envelopes into notifications. Any other envelope kind is an error — this
// double is scoped to exactly what session_watcher's scheduling handler
// sends.
type capturingSender struct {
	mu            sync.Mutex
	notifications []recordedNotification
}

func (s *capturingSender) Send(_ context.Context, sess channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	if env.Kind != channelevents.KindNotification {
		return channelkinds.SubChannelSendResult{}, errors.New("capturingSender: unexpected envelope kind " + string(env.Kind))
	}
	var pl channelevents.NotificationPayload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return channelkinds.SubChannelSendResult{}, err
	}
	s.mu.Lock()
	s.notifications = append(s.notifications, recordedNotification{Namespace: sess.Namespace, Name: sess.Name, Payload: pl})
	s.mu.Unlock()
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *capturingSender) captured() []recordedNotification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedNotification(nil), s.notifications...)
}

// fakeSenderResolver implements outbound.SenderResolver, returning a single
// shared capturingSender for every session (or resolveErr, if set, to
// exercise the "no sender available" graceful-skip path). The two other
// interface methods are unused by session_watcher and stubbed out.
type fakeSenderResolver struct {
	sender     *capturingSender
	resolveErr error
}

func (r *fakeSenderResolver) SenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	return r.sender, nil
}

func (r *fakeSenderResolver) SubChannelSenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession, _ string) (channelkinds.Sender, error) {
	return nil, nil
}

func (r *fakeSenderResolver) StreamDeltaSinkFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	return nil, nil
}

func TestSessionWatcherPostsWaitingForCapacity(t *testing.T) {
	sess := sessionWithSchedulingCondition("s1", "uid-1", metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable)
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &capturingSender{}
	w := newSessionWatcher(cli, &fakeSenderResolver{sender: sender}, nil)

	w.reconcile(context.Background(), testr.New(t))

	got := sender.captured()
	require.Len(t, got, 1, "one notification posted for the stuck episode")
	assert.Contains(t, got[0].Payload.Text, "Waiting for capacity")
}

func TestSessionWatcherDedupsWaitingForCapacity(t *testing.T) {
	sess := sessionWithSchedulingCondition("s1", "uid-1", metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable)
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &capturingSender{}
	w := newSessionWatcher(cli, &fakeSenderResolver{sender: sender}, nil)

	w.reconcile(context.Background(), testr.New(t))
	w.reconcile(context.Background(), testr.New(t))

	assert.Len(t, sender.captured(), 1, "second reconcile while still False must NOT repost")
}

func TestSessionWatcherClearsOnScheduled(t *testing.T) {
	sess := sessionWithSchedulingCondition("s1", "uid-1", metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable)
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &capturingSender{}
	w := newSessionWatcher(cli, &fakeSenderResolver{sender: sender}, nil)

	// First stuck episode: posts.
	w.reconcile(context.Background(), testr.New(t))
	require.Len(t, sender.captured(), 1)

	// Flip to Scheduled: clears (empty-Text sentinel).
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), sess))
	setSchedulingCondition(sess, metav1.ConditionTrue, spiceboxv1alpha1.ReasonSandboxScheduled)
	require.NoError(t, cli.Status().Update(context.Background(), sess))
	w.reconcile(context.Background(), testr.New(t))

	got := sender.captured()
	require.Len(t, got, 2, "the True transition must send exactly one clear")
	assert.Equal(t, "", got[len(got)-1].Payload.Text, "clear is the empty-Text sentinel")

	// A second True reconcile must NOT re-clear (nothing posted to clear).
	w.reconcile(context.Background(), testr.New(t))
	assert.Len(t, sender.captured(), 2, "no prior post outstanding; True is a no-op")

	// A fresh stuck episode after the clear must post again (episode reset).
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), sess))
	setSchedulingCondition(sess, metav1.ConditionFalse, spiceboxv1alpha1.ReasonSandboxUnschedulable)
	require.NoError(t, cli.Status().Update(context.Background(), sess))
	w.reconcile(context.Background(), testr.New(t))

	got = sender.captured()
	require.Len(t, got, 3, "a later stuck episode must post again")
	assert.Contains(t, got[len(got)-1].Payload.Text, "Waiting for capacity")
}

func TestSessionWatcherAbsentConditionNoop(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "s1",
			Namespace: "default",
			UID:       "uid-1",
			Labels:    map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "agent-a",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase: spiceboxv1alpha1.AgentSessionPhasePending,
			// No SandboxScheduling condition at all — nothing ever stalled.
		},
	}
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &capturingSender{}
	w := newSessionWatcher(cli, &fakeSenderResolver{sender: sender}, nil)

	w.reconcile(context.Background(), testr.New(t))

	assert.Empty(t, sender.captured(), "absent condition must not post anything")
}
