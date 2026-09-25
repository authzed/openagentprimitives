package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// session_watcher_restart_denied_test.go pins the user-visible half of a denied
// thread continuation.
//
// The bug: when a user replies in the thread of a terminal session, channelsd
// acks with "…I'm picking it up in a new session — same thread, full history."
// and hands the fork to the operator. If the operator's SessionFork gate DENIES
// it, the operator recorded a RestartDenied condition and published a notice to
// ap.session.<ns>.<name>.out.metaagent_notice — a raw-JSON subject that the
// wildcard outbound relay (ap.session.*.*.out.>) also consumes and rejects with
// "unsupported envelope version 0 (want 1)". The user saw the ack, then
// silence, forever. That violates the no-silent-errors rule: the user was
// actively waiting on the thing that failed.
//
// The durable RestartDenied condition is the signal; the session watcher — the
// same surface that already relays phase=Failed to the thread — must relay it.

// deniedWatcher returns a watcher whose notices go where a notice is actually
// deliverable: the bus, from which the outbound relay renders it through the
// channel kind's "interaction" sub-channel sender.
//
// A fake DEFAULT sender accepting KindInteractionRequest would be a trap: no
// real default sender implements that kind, so such a test stays green while
// every denial is dropped in production. Routing is pinned in
// session_watcher_notice_delivery_test.go; what is under test HERE is the
// dedup/repost semantics on top of it.
func deniedWatcher(cli client.Client, pub *publishRecorder) *sessionWatcher {
	w := newSessionWatcher(cli, &staticSenderResolver{sender: &defaultKindSender{}}, nil)
	w.publish = pub.publish
	return w
}

// deniedSession builds the exact production shape: a channel-attached session
// that already Failed and already had its failure reported (the annotation is
// present), which the operator has since marked RestartDenied because the fork
// gate refused the continuation.
func deniedSession(name string, uid types.UID, reason, message string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       uid,
			Labels:    map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
			// Already reported the Failed message — the denial is a SEPARATE
			// event and must not be swallowed by the failure dedup.
			Annotations: map[string]string{failureReportedAnnotation: "2026-07-28T02:29:10Z"},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "agent-a",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			FailureReason: spiceboxv1alpha1.ReasonAgentSessionBundleFail,
			Conditions: []metav1.Condition{{
				Type:               spiceboxv1alpha1.AgentSessionConditionRestartDenied,
				Status:             metav1.ConditionTrue,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// bumpRestartDenied re-stamps the RestartDenied condition with a new
// LastTransitionTime, mirroring a second, later denial on the same session.
func bumpRestartDenied(sess *spiceboxv1alpha1.AgentSession) {
	for i := range sess.Status.Conditions {
		if sess.Status.Conditions[i].Type == spiceboxv1alpha1.AgentSessionConditionRestartDenied {
			sess.Status.Conditions[i].LastTransitionTime = metav1.NewTime(metav1.Now().Add(time.Minute))
			return
		}
	}
}

func TestSessionWatcherReportsRestartDenied(t *testing.T) {
	const reasonDetail = "Only the session owner can continue this conversation."
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, reasonDetail)
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{}

	deniedWatcher(cli, pub).reconcile(context.Background(), testr.New(t))

	got := pub.interactions(t, "default", "s1")
	require.Len(t, got, 1,
		"a denied continuation MUST be reported to the thread — the user was acked with "+
			"'picking it up in a new session' and is waiting on it")
	// The refusal reason is controller-generated text, so it rides the
	// Excerpt, which every surface renders inert.
	require.NotNil(t, got[0].Excerpt,
		"the notice must carry the reason the continuation was refused")
	assert.Contains(t, got[0].Excerpt.Content, reasonDetail)
	assert.NotEmpty(t, got[0].NextStep,
		"a degraded notice must tell the user what to do instead")
}

func TestSessionWatcherDedupsRestartDenied(t *testing.T) {
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, "nope")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{}
	w := deniedWatcher(cli, pub)

	w.reconcile(context.Background(), testr.New(t))
	w.reconcile(context.Background(), testr.New(t))

	assert.Len(t, pub.interactions(t, "default", "s1"), 1,
		"the 5s poll must not repost the same denial every tick")
}

func TestSessionWatcherRepostsLaterRestartDenial(t *testing.T) {
	ctx := context.Background()
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, "nope")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{}
	w := deniedWatcher(cli, pub)

	w.reconcile(ctx, testr.New(t))
	require.Len(t, pub.interactions(t, "default", "s1"), 1)

	// The user tries again; the gate denies again. A new denial is new
	// information and must be surfaced, not swallowed by the first one's dedup.
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sess), sess))
	bumpRestartDenied(sess)
	require.NoError(t, cli.Status().Update(ctx, sess))
	w.reconcile(ctx, testr.New(t))

	assert.Len(t, pub.interactions(t, "default", "s1"), 2, "a second, later denial must post again")
}

func TestSessionWatcherDoesNotRepostRestartDeniedAfterProcessRestart(t *testing.T) {
	ctx := context.Background()
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, "nope")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{}

	deniedWatcher(cli, pub).reconcile(ctx, testr.New(t))
	require.Len(t, pub.interactions(t, "default", "s1"), 1)

	// channelsd is redeployed. The in-memory dedup map is gone, but
	// RestartDenied is written True exactly once and NEVER cleared, so the
	// stale condition is still sitting on the CR. Without durable dedup every
	// redeploy re-posts the denial to the thread, forever.
	deniedWatcher(cli, pub).reconcile(ctx, testr.New(t))

	assert.Len(t, pub.interactions(t, "default", "s1"), 1,
		"a channelsd restart must not re-post a denial the user already saw")
}

func TestSessionWatcherRepostsLaterRestartDenialAfterProcessRestart(t *testing.T) {
	ctx := context.Background()
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, "nope")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{}

	deniedWatcher(cli, pub).reconcile(ctx, testr.New(t))
	require.Len(t, pub.interactions(t, "default", "s1"), 1)

	// A denial the user has NOT seen must survive the restart in the other
	// direction: durable dedup keyed on the condition's LastTransitionTime, not
	// on mere presence, so the next denial still reaches the thread.
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sess), sess))
	bumpRestartDenied(sess)
	require.NoError(t, cli.Status().Update(ctx, sess))
	deniedWatcher(cli, pub).reconcile(ctx, testr.New(t))

	assert.Len(t, pub.interactions(t, "default", "s1"), 2,
		"a second, later denial is new information and must post even across a restart")
}

// A denial the user never received must not be marked reported: neither the
// in-memory map nor the durable annotation may record it, or the next tick
// skips the session and the denial is lost for good.
func TestSessionWatcherRetriesRestartDeniedAfterPublishError(t *testing.T) {
	ctx := context.Background()
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, "nope")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{err: errors.New("nats down")}
	w := deniedWatcher(cli, pub)

	w.reconcile(ctx, testr.New(t))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sess), &got))
	assert.Empty(t, got.Annotations[restartDeniedReportedAnnotation],
		"a denial the user never received must not be marked as reported")

	pub.err = nil
	w.reconcile(ctx, testr.New(t))
	assert.Len(t, pub.interactions(t, "default", "s1"), 1,
		"the retry must deliver exactly one denial")
}

func TestSessionWatcherIgnoresFalseRestartDenied(t *testing.T) {
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, "nope")
	sess.Status.Conditions[0].Status = metav1.ConditionFalse
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{}

	deniedWatcher(cli, pub).reconcile(context.Background(), testr.New(t))

	assert.Empty(t, pub.interactions(t, "default", "s1"),
		"a non-True RestartDenied condition posts nothing")
}

// staticSenderResolver returns one shared sender for every session.
type staticSenderResolver struct {
	sender     channelkinds.Sender
	resolveErr error
}

func (r *staticSenderResolver) SenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	return r.sender, nil
}

func (r *staticSenderResolver) SubChannelSenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession, _ string) (channelkinds.Sender, error) {
	return nil, nil
}

func (r *staticSenderResolver) StreamDeltaSinkFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	return nil, nil
}
