package outbound

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

const demoOpening = "Picked up pull request demo-org/demo-repo#2."

func TestRelay_GenericSummaryCarriesInspectableInstructions(t *testing.T) {
	for _, threaded := range []bool{false, true} {
		t.Run(fmt.Sprint(threaded), func(t *testing.T) {
			nc := connectNATS(t)
			external := map[string]string{"channel_id": "C_OUT"}
			if threaded {
				external["thread_ts"] = "existing"
			}
			sess := webhookSession("async", "", external)
			sess.Spec.OpeningSummary = "Session created to review the nightly report"
			sess.Spec.Prompt.Inline = "  exact original instructions\n<@not-a-mention> "
			cli := fakeClientWith(t, sess)
			sndr := &threadRootSender{}
			startRelay(t, nc, cli, &fixedResolver{s: sndr})
			publishOut(t, nc, "default", "async", "plan_update", planUpdate(t, "async"))
			require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 2 }))
			envs, _ := sndr.snapshot()
			var payload channelevents.OutboundUserMessagePayload
			require.NoError(t, json.Unmarshal(envs[0].Payload, &payload))
			require.NotNil(t, payload.Opening)
			require.Equal(t, sess.Spec.OpeningSummary, payload.Opening.Summary)
			require.Equal(t, sess.Spec.Prompt.Inline, payload.Opening.Instructions)
		})
	}
}

// threadRootSender is slack's first-send shape: the very first Send lands in a
// channel with no thread yet and reports the root it created; every later Send
// threads under that root and reports nothing. It records the binding it was
// handed on each call, which is how the ordering assertions below prove the
// agent's output went INTO the thread the opening line rooted.
type threadRootSender struct {
	mu    sync.Mutex
	got   []channelevents.Envelope
	infos []channelkinds.SessionInfo
	// root is returned by the first Send only. Nil means this kind reports no
	// routing metadata back at all (every non-slack kind today).
	root map[string]string
}

func (s *threadRootSender) Send(_ context.Context, info channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := len(s.got) == 0
	s.got = append(s.got, env)
	// Snapshot the routing metadata: the binding is a live pointer the relay
	// mutates, so recording it by reference would let a later patch rewrite
	// what an earlier send is asserted to have seen.
	snapshot := info
	if info.Channel != nil {
		b := *info.Channel
		ext := map[string]string{}
		for k, v := range info.Channel.External {
			ext[k] = v
		}
		b.External = ext
		snapshot.Channel = &b
	}
	s.infos = append(s.infos, snapshot)
	if first && s.root != nil {
		return channelkinds.SubChannelSendResult{External: s.root}, nil
	}
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *threadRootSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *threadRootSender) snapshot() ([]channelevents.Envelope, []channelkinds.SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelevents.Envelope(nil), s.got...), append([]channelkinds.SessionInfo(nil), s.infos...)
}

// webhookSession is the shape the defect was observed on: an inbound that
// carried no human (a pull-request webhook), a dedicated slack output Channel
// anchored at the channel only, and the opening line the pipeline derived from
// the trigger. opening=="" is the human-initiated control — the same session
// with nobody's line to post, because the person's own message is the root.
func webhookSession(name, opening string, external map[string]string) *spiceboxv1alpha1.AgentSession {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{}},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "gh-in", Kind: "github", Key: "pr:demo-org/demo-repo#2",
				NATSSubjectPrefix: "ap.session.default." + name,
			},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: "slack", Key: "channel:C_OUT",
				External: external,
			},
		},
	}
	if opening != "" {
		sess.Annotations[spiceboxv1alpha1.AnnotationSessionOpening] = opening
	}
	return sess
}

// planUpdate is what the agent emitted first in the observed defect: a review
// plan, a checklist widget with nothing above it saying what it is about.
func planUpdate(t *testing.T, name string) channelevents.Envelope {
	t.Helper()
	return buildEnv(t, name, channelevents.KindPlanUpdate, map[string]any{"title": "Review plan"})
}

// TestRelay_SessionOpening_RootsTheThreadBeforeTheFirstOutput is the defect:
// with no inbound message of its own, a webhook-started session's thread was
// rooted by whatever the agent emitted first — here a plan checklist — leaving
// the first thing in the thread saying nothing about what it was for.
func TestRelay_SessionOpening_RootsTheThreadBeforeTheFirstOutput(t *testing.T) {
	nc := connectNATS(t)
	sess := webhookSession("gh-session", demoOpening, map[string]string{"channel_id": "C_OUT"})
	cli := fakeClientWith(t, sess)

	sndr := &threadRootSender{root: map[string]string{"thread_ts": "1111.2222", "channel_id": "C_OUT"}}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	publishOut(t, nc, "default", "gh-session", "plan_update", planUpdate(t, "gh-session"))

	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 2 }),
		"the opening line and the agent's plan must both be sent")

	envs, infos := sndr.snapshot()
	require.Len(t, envs, 2, "exactly the opening line plus the agent's output")

	// (1) Ordering: the opening line goes out FIRST, so it is what Slack roots
	// the thread on. This is the whole defect.
	assert.Equal(t, channelevents.KindUserMessage, envs[0].Kind, "the opening line is sent first")
	var pl channelevents.OutboundUserMessagePayload
	require.NoError(t, json.Unmarshal(envs[0].Payload, &pl), "unmarshal the opening payload")
	assert.Equal(t, demoOpening, pl.Text, "the opening line says what the session is about")
	assert.Equal(t, channelevents.KindPlanUpdate, envs[1].Kind, "the agent's plan follows it")

	// (2) The opening posts at channel level (no thread yet); the agent's plan
	// posts INTO the thread that send rooted. Without the in-memory refresh the
	// plan would open a second, parallel thread.
	assert.Empty(t, infos[0].Channel.External["thread_ts"], "the opening line roots the thread")
	assert.Equal(t, "1111.2222", infos[1].Channel.External["thread_ts"],
		"the agent's output must land inside the thread the opening rooted")

	// (3) Durably anchored, so later turns and inbound thread replies find the
	// same session.
	const wantKey = "thread:C_OUT:1111.2222"
	var patched spiceboxv1alpha1.AgentSession
	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		_ = cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "gh-session"}, &patched)
		return patched.Spec.OutputChannel != nil && patched.Spec.OutputChannel.Key == wantKey
	}), "outputChannel was not anchored on the opening line's thread root")
	assert.Equal(t, "1111.2222", patched.Spec.OutputChannel.External["thread_ts"])
	assert.Equal(t, channelkey.LabelValue(wantKey), patched.Labels[spiceboxv1alpha1.LabelOutputChannelKey])

	// (4) Retained: the opening text is kept for channelsd to re-render the
	// pinned status from, not cleared once posted. The thread_ts guard above is
	// what stops a second post, not the annotation's presence.
	assert.NotEmpty(t, patched.Annotations[spiceboxv1alpha1.AnnotationSessionOpening],
		"the opening text is kept for the pinned-status renderer")
	assert.NotEmpty(t, patched.Spec.OutputChannel.External["thread_ts"], "the thread is anchored")

	// (5) A second relay pass does not re-post: the guard at the top of
	// openOutboundThread short-circuits once thread_ts is set, regardless of
	// the (now-retained) annotation.
	publishOut(t, nc, "default", "gh-session", "plan_update", planUpdate(t, "gh-session"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 3 }),
		"the barrier envelope must still be delivered")
	envsAfter, _ := sndr.snapshot()
	require.Len(t, envsAfter, 3, "no additional opening was posted on the second pass")
	assert.Equal(t, channelevents.KindPlanUpdate, envsAfter[2].Kind,
		"the third send is the agent's second output, not a re-posted opening")
}

// TestRelay_SessionOpening_HumanInitiatedSessionIsUntouched is the regression
// direction and the one that matters most. A session a person started already
// has a root — their own message — and already says what it is about. Nothing
// may be synthesized in front of it.
func TestRelay_SessionOpening_HumanInitiatedSessionIsUntouched(t *testing.T) {
	nc := connectNATS(t)
	sess := webhookSession("human-session", "", map[string]string{"channel_id": "C_OUT"})
	cli := fakeClientWith(t, sess)

	sndr := &threadRootSender{root: map[string]string{"thread_ts": "3333.4444", "channel_id": "C_OUT"}}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	publishOut(t, nc, "default", "human-session", "plan_update", planUpdate(t, "human-session"))

	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 1 }),
		"the agent's output must still be delivered")
	// A barrier the relay handles after the first envelope: nats.go serializes
	// one subscription's callbacks, so seeing the second send proves no opening
	// was interleaved before it rather than merely not-yet-sent.
	publishOut(t, nc, "default", "human-session", "plan_update", planUpdate(t, "human-session"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 2 }), "barrier")

	envs, _ := sndr.snapshot()
	require.Len(t, envs, 2, "only the agent's own output, never a synthesized line")
	for i, env := range envs {
		assert.Equal(t, channelevents.KindPlanUpdate, env.Kind, "envelope %d", i)
	}
}

// TestRelay_SessionOpening_EstablishedThreadIsNotReopened: once the binding
// records a thread root, the thread already has a first message. Posting the
// opening again would drop it in the middle of an ongoing conversation.
func TestRelay_SessionOpening_EstablishedThreadIsNotReopened(t *testing.T) {
	nc := connectNATS(t)
	sess := webhookSession("established", demoOpening,
		map[string]string{"channel_id": "C_OUT", "thread_ts": "9999.0000"})
	cli := fakeClientWith(t, sess)

	sndr := &threadRootSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	publishOut(t, nc, "default", "established", "plan_update", planUpdate(t, "established"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 1 }), "output delivered")
	publishOut(t, nc, "default", "established", "plan_update", planUpdate(t, "established"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 2 }), "barrier")

	envs, _ := sndr.snapshot()
	require.Len(t, envs, 2)
	for i, env := range envs {
		assert.Equal(t, channelevents.KindPlanUpdate, env.Kind, "envelope %d", i)
	}
}

// TestRelay_SessionOpening_PostedOnceForAKindThatReportsNoRoot covers the kind
// whose Send hands back no routing metadata: the binding never gains a thread
// root, so "is this thread already open" cannot be answered from thread_ts.
// The opening text itself is retained (for the pinned-status renderer), so
// AnnotationSessionOpeningSent is what stops a second opening here — without
// it the line would be re-posted ahead of every single envelope for the
// session's life.
func TestRelay_SessionOpening_PostedOnceForAKindThatReportsNoRoot(t *testing.T) {
	nc := connectNATS(t)
	sess := webhookSession("no-root", demoOpening, map[string]string{"channel_id": "C_OUT"})
	cli := fakeClientWith(t, sess)

	sndr := &threadRootSender{} // reports nothing back
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	publishOut(t, nc, "default", "no-root", "plan_update", planUpdate(t, "no-root"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 2 }), "opening + output")
	publishOut(t, nc, "default", "no-root", "plan_update", planUpdate(t, "no-root"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= 3 }), "barrier")

	envs, _ := sndr.snapshot()
	require.Len(t, envs, 3)
	openings := 0
	for _, env := range envs {
		if env.Kind == channelevents.KindUserMessage {
			openings++
		}
	}
	assert.Equal(t, 1, openings, "the opening line is posted exactly once per session")
}

// TestRelay_SessionOpening_SendFailureDegradesAndRetries: the opening is a
// framing message, never a gate. When it cannot be posted the agent's output
// still goes out (the thread roots on it, exactly as it does today) and the
// annotation is left in place so the next envelope tries again.
func TestRelay_SessionOpening_SendFailureDegradesAndRetries(t *testing.T) {
	nc := connectNATS(t)
	sess := webhookSession("opening-fails", demoOpening, map[string]string{"channel_id": "C_OUT"})
	cli := fakeClientWith(t, sess)

	sndr := &failFirstSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	publishOut(t, nc, "default", "opening-fails", "plan_update", planUpdate(t, "opening-fails"))

	require.True(t, waitUntil(t, 2*time.Second, func() bool { return len(sndr.envelopes()) >= 2 }),
		"a failed opening must not swallow the agent's output")
	envs := sndr.envelopes()
	assert.Equal(t, channelevents.KindUserMessage, envs[0].Kind, "the opening was attempted")
	assert.Equal(t, channelevents.KindPlanUpdate, envs[1].Kind, "the agent's output went out anyway")

	var after spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "opening-fails"}, &after))
	assert.Equal(t, demoOpening, after.Annotations[spiceboxv1alpha1.AnnotationSessionOpening],
		"an unposted opening must survive for the next envelope to retry")
}
