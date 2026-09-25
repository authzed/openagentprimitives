package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// session_watcher_notice_delivery_test.go pins WHERE a notice goes.
//
// Every user-facing notice channelsd emits — the session's failure, a denied
// continuation, the watchdog's stall report — is a KindInteractionRequest, and
// an interaction_request is rendered by exactly one thing: the channel kind's
// "interaction" sub-channel sender, which only the outbound relay resolves.
// Handing one to the DEFAULT sender reaches slackSender.Send's `default:` arm
// and comes back "unsupported envelope kind" — the notice is built, addressed,
// dropped, and the user sees silence. So these watchers must PUBLISH, exactly
// as every other notice publisher in channelsd does (credential_request,
// credential_update, credential_linked).
//
// The reason that regression could ship is that every fake sender in this
// package accepts any envelope it is given. defaultKindSender does not.

// defaultKindSender mirrors the envelope switch of a real default channel
// Sender (pkg/channels/channelkinds/slack's slackSender.Send): it renders the kinds a
// default sender implements and REJECTS everything else, recording the
// rejection so a mis-routed envelope is observable instead of invisible.
type defaultKindSender struct {
	mu       sync.Mutex
	accepted []channelevents.Kind
	rejected []channelevents.Kind
}

func (s *defaultKindSender) Send(_ context.Context, _ channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch env.Kind {
	case channelevents.KindNotification, channelevents.KindUserMessage,
		channelevents.KindTurnProgress, channelevents.KindToolProgress,
		channelevents.KindOperationActivity, channelevents.KindPlanUpdate:
		s.accepted = append(s.accepted, env.Kind)
		return channelkinds.SubChannelSendResult{}, nil
	default:
		s.rejected = append(s.rejected, env.Kind)
		return channelkinds.SubChannelSendResult{},
			errors.New("default sender: unsupported envelope kind " + string(env.Kind))
	}
}

func (s *defaultKindSender) rejectedKinds() []channelevents.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelevents.Kind(nil), s.rejected...)
}

func (s *defaultKindSender) acceptedKinds() []channelevents.Kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelevents.Kind(nil), s.accepted...)
}

// publishRecorder captures what a watcher put on the bus.
type publishRecorder struct {
	mu   sync.Mutex
	sent []recordedPublish
	err  error
}

type recordedPublish struct {
	Subject string
	Env     channelevents.Envelope
}

func (p *publishRecorder) publish(subject string, data []byte) error {
	if p.err != nil {
		return p.err
	}
	var env channelevents.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, recordedPublish{Subject: subject, Env: env})
	return nil
}

// interactions returns the decoded interaction_request payloads published for
// ns/name, asserting each landed on that session's own OUT subject.
func (p *publishRecorder) interactions(t *testing.T, ns, name string) []channelevents.InteractionRequestPayload {
	t.Helper()
	want := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindInteractionRequest)
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []channelevents.InteractionRequestPayload
	for _, rec := range p.sent {
		if rec.Env.Kind != channelevents.KindInteractionRequest {
			continue
		}
		assert.Equal(t, want, rec.Subject,
			"an interaction_request must go out on its own session's OUT subject, where the relay picks it up")
		var pl channelevents.InteractionRequestPayload
		require.NoError(t, json.Unmarshal(rec.Env.Payload, &pl))
		out = append(out, pl)
	}
	return out
}

// TestSessionWatcherPublishesFailureNotice is the blocker's RED test: a Failed
// session's notice must reach the bus (and from there the relay's "interaction"
// sub-channel sender), NOT the default sender that cannot render it.
func TestSessionWatcherPublishesFailureNotice(t *testing.T) {
	sess := failedChannelSession("fk", "fake", "fake")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &defaultKindSender{}
	pub := &publishRecorder{}
	w := newSessionWatcher(cli, &staticSenderResolver{sender: sender}, nil)
	w.publish = pub.publish

	w.reconcile(context.Background(), testr.New(t))

	got := pub.interactions(t, "default", "fk")
	require.Len(t, got, 1,
		"a Failed session's notice must be published for the relay to route to the interaction sender")
	assert.Equal(t, categories.AgentFailed, got[0].Category)
	assert.Empty(t, sender.rejectedKinds(),
		"the notice must never be handed to the default sender, which rejects interaction_request")
}

// TestSessionWatcherPublishesRestartDeniedNotice covers the sibling site: the
// denial the user is actively waiting on after being told "I'm picking it up in
// a new session".
func TestSessionWatcherPublishesRestartDeniedNotice(t *testing.T) {
	sess := deniedSession("s1", "uid-1", spiceboxv1alpha1.ReasonForkNotAuthorized, "nope")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	sender := &defaultKindSender{}
	pub := &publishRecorder{}
	w := newSessionWatcher(cli, &staticSenderResolver{sender: sender}, nil)
	w.publish = pub.publish

	w.reconcile(context.Background(), testr.New(t))

	got := pub.interactions(t, "default", "s1")
	require.Len(t, got, 1, "a denied continuation must be published, not handed to the default sender")
	assert.Equal(t, categories.ContinuationDenied, got[0].Category)
	assert.Empty(t, sender.rejectedKinds())
}

// TestStatusWatchdogPublishesStallNotice covers the third site. The watchdog's
// stall report is the message that explains an otherwise silent session, so a
// dropped one is the worst of the three.
func TestStatusWatchdogPublishesStallNotice(t *testing.T) {
	sess := channelAttachedSession("ns", "s")
	pub := &publishRecorder{}
	sender := &defaultKindSender{}
	wd := newStatusWatchdog(nil, &staticSenderResolver{sender: sender})
	wd.publish = pub.publish

	wd.sendNotice(context.Background(), logr.Discard(), sess, "notice-stalled-1",
		watchdogTimeoutNotice("s", 1))

	got := pub.interactions(t, "ns", "s")
	require.Len(t, got, 1, "the stall notice must be published for the relay to render")
	assert.Equal(t, categories.AgentStalled, got[0].Category)
	assert.Empty(t, sender.rejectedKinds(),
		"the stall notice must never be handed to the default sender, which rejects interaction_request")
}

// A failed publish must NOT be recorded as reported: the user never saw the
// message, so the next tick has to try again. (With routing fixed, a failure is
// transient — a NATS blip — rather than the permanent, every-5s-forever
// re-failure the mis-routed send produced.)
func TestSessionWatcherRetriesFailureNoticeAfterPublishError(t *testing.T) {
	ctx := context.Background()
	sess := failedChannelSession("fk", "fake", "fake")
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	pub := &publishRecorder{err: errors.New("nats down")}
	w := newSessionWatcher(cli, &staticSenderResolver{sender: &defaultKindSender{}}, nil)
	w.publish = pub.publish

	w.reconcile(ctx, testr.New(t))

	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(sess), &got))
	assert.Empty(t, got.Annotations[failureReportedAnnotation],
		"a notice the user never received must not be marked as reported")

	// The bus comes back; the next tick delivers.
	pub.err = nil
	w.reconcile(ctx, testr.New(t))
	assert.Len(t, pub.interactions(t, "default", "fk"), 1,
		"the retry must deliver exactly one notice")
}

// TestSessionWatcherAddressesOutboundBindingForNotifications pins the
// split-channel (cron/bento-triggered) case for the two sites that legitimately
// keep the default sender: a session whose INPUT binding is a bento trigger and
// whose OUTPUT binding is the Slack channel must be addressed by its OUTPUT
// binding. Addressing the input binding hands a Slack sender a binding with no
// channel_id, which it refuses — the failure mode OutboundBinding exists to
// prevent, and one that never shows up on a same-channel session.
func TestSessionWatcherAddressesOutboundBindingForNotifications(t *testing.T) {
	sess := failedChannelSession("cron", "fake", "fake")
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
	sess.Status.FailureReason = ""
	sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "trigger", Kind: "fake"}
	sess.Spec.OutputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: "room", Kind: "fake", External: map[string]string{"channel_id": "C123"},
	}
	sess.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentSessionConditionSandboxScheduling,
		Status:             metav1.ConditionFalse,
		Reason:             "Unschedulable",
		Message:            "no capacity",
		LastTransitionTime: metav1.Now(),
	}}
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	rec := &bindingRecordingSender{}
	w := newSessionWatcher(cli, &staticSenderResolver{sender: rec}, nil)

	w.reconcile(context.Background(), testr.New(t))

	got := rec.bindings()
	require.Len(t, got, 1, "the capacity notice must be sent once")
	require.NotNil(t, got[0])
	assert.Equal(t, "room", got[0].Name,
		"an outbound notice must be addressed to the session's OUTPUT binding")
}

// bindingRecordingSender records the ChannelBinding each send was addressed to.
type bindingRecordingSender struct {
	mu   sync.Mutex
	seen []*spiceboxv1alpha1.ChannelBinding
}

func (s *bindingRecordingSender) Send(_ context.Context, sess channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, sess.Channel)
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *bindingRecordingSender) bindings() []*spiceboxv1alpha1.ChannelBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*spiceboxv1alpha1.ChannelBinding(nil), s.seen...)
}
