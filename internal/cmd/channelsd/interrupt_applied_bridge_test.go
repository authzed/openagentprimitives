package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
)

// recordingPublish captures every (subject, envelope) pair a bridge publishes,
// so tests can assert both the count (zero for the double-fire guard) and the
// round-tripped payload.
type recordingPublish struct {
	subjects []string
	envs     []channelevents.Envelope
}

func (r *recordingPublish) Publish(subj string, data []byte) error {
	r.subjects = append(r.subjects, subj)
	var env channelevents.Envelope
	_ = json.Unmarshal(data, &env)
	r.envs = append(r.envs, env)
	return nil
}

// sessionWithInputKind builds a fake K8s client holding one AgentSession whose
// InputChannel.Kind is `kind` (empty ⇒ no InputChannel binding at all).
func sessionWithInputKind(t *testing.T, ns, name, kind string) *interruptAppliedBridge {
	t.Helper()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
	if kind != "" {
		sess.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: kind}
	}
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).Build()
	return &interruptAppliedBridge{cli: cli}
}

// interruptAppliedMsg marshals an interrupt_applied envelope into a NATS msg
// published HONESTLY — on the very session's own out subject, exactly as
// PublishOut would. The forgery case (subject and envelope disagreeing) is
// TestInterruptAppliedBridge_RoutesOffTheSubject's business.
func interruptAppliedMsg(t *testing.T, ns, name string, ip channelevents.InterruptAppliedPayload) *nats.Msg {
	t.Helper()
	return interruptAppliedMsgOn(t,
		channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.KindInterruptApplied),
		ns, name, ip)
}

// interruptAppliedMsgOn is interruptAppliedMsg with the subject decoupled from
// the envelope's claimed session, so a test can publish one session's envelope
// on another session's subject.
func interruptAppliedMsgOn(t *testing.T, subject, claimNS, claimName string, ip channelevents.InterruptAppliedPayload) *nats.Msg {
	t.Helper()
	env, err := channelevents.BuildEnvelope(claimNS, claimName, channelevents.KindInterruptApplied, ip)
	require.NoError(t, err, "build interrupt_applied envelope")
	data, err := json.Marshal(env)
	require.NoError(t, err, "marshal envelope")
	return &nats.Msg{Subject: subject, Data: data}
}

// TestInterruptAppliedBridge_RoutesOffTheSubject: the bridge holds its OWN
// cluster-wide subscription ("ap.session.*.*.out.interrupt_applied"), separate
// from the outbound relay's, so it needs the relay's subject-authority rule for
// itself. A runner authorized on session A's out subject must not be able to
// name session B in the envelope and have the bridge render an interruption
// card — carrying its own attacker-chosen Reason text — into B's Slack thread.
func TestInterruptAppliedBridge_RoutesOffTheSubject(t *testing.T) {
	cases := []struct {
		name          string
		subject       string
		wantPublished bool
	}{
		{
			name: "forged: published on another session's subject, claims the victim — dropped",
			subject: channelevents.SubjectOut(
				channelevents.SubjectPrefix("default", "attacker"), channelevents.KindInterruptApplied),
			wantPublished: false,
		},
		{
			name:          "unparseable subject: dropped rather than trusted to the envelope",
			subject:       "nonsense",
			wantPublished: false,
		},
		{
			name: "honest: subject and envelope agree — republished",
			subject: channelevents.SubjectOut(
				channelevents.SubjectPrefix("default", "sess1"), channelevents.KindInterruptApplied),
			wantPublished: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := sessionWithInputKind(t, "default", "sess1", "slack")
			rec := &recordingPublish{}
			b.publish = rec.Publish

			b.handle(context.Background(), interruptAppliedMsgOn(t, tc.subject, "default", "sess1",
				channelevents.InterruptAppliedPayload{
					RequestID: "req-1", Outcome: "rejected", Reason: "<!channel> pwned",
				}))

			if tc.wantPublished {
				require.Len(t, rec.subjects, 1, "an honest interrupt_applied must still be republished")
				return
			}
			assert.Empty(t, rec.subjects,
				"the bridge must never render into a session the publisher was not authorized to publish for")
		})
	}
}

// TestInterruptAppliedBridge_SlackSession_RepublishesAsInteractionApplied is the
// KEY test: for a Slack-input session the bridge republishes the runner's
// interrupt_applied as exactly ONE interaction_applied(queued_messages) on the
// OUT subject (and ZERO on IN — the runner already handled the interrupt), with
// the RequestID / ResponseURL round-tripped and the interrupted/rejected outcome
// mapped to Resolved/Denied + the matching OutcomeText.
func TestInterruptAppliedBridge_SlackSession_RepublishesAsInteractionApplied(t *testing.T) {
	cases := []struct {
		name        string
		outcome     string
		reason      string
		wantOutcome string
		wantText    string
	}{
		{
			name:        "interrupted → Resolved, 'Interrupting…' text",
			outcome:     "interrupted",
			wantOutcome: channelevents.OutcomeResolved,
			wantText:    "Interrupting — sending your queued message(s) now.",
		},
		{
			name:        "rejected with reason → Denied, 'Couldn't interrupt — <reason>'",
			outcome:     "rejected",
			reason:      "no turn in flight",
			wantOutcome: channelevents.OutcomeDenied,
			wantText:    "Couldn't interrupt — no turn in flight",
		},
		{
			name:        "rejected without reason → Denied, sensible default",
			outcome:     "rejected",
			wantOutcome: channelevents.OutcomeDenied,
			wantText:    "Couldn't interrupt — the runner declined.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := sessionWithInputKind(t, "default", "sess1", "slack")
			rec := &recordingPublish{}
			b.publish = rec.Publish

			b.handle(context.Background(), interruptAppliedMsg(t, "default", "sess1", channelevents.InterruptAppliedPayload{
				RequestID:   "queued-req-1",
				Outcome:     tc.outcome,
				Reason:      tc.reason,
				ResponseURL: "https://hooks.slack.com/actions/T/1/interrupt",
			}))

			require.Len(t, rec.subjects, 1, "the bridge must publish exactly one interaction_applied for a Slack session")
			assert.Equal(t, "ap.session.default.sess1.out.interaction_applied", rec.subjects[0],
				"republished on the OUT subject ONLY (the runner already consumed the interrupt on IN)")

			var pl channelevents.InteractionAppliedPayload
			require.NoError(t, json.Unmarshal(rec.envs[0].Payload, &pl), "unmarshal interaction_applied payload")
			assert.Equal(t, categories.QueuedMessages, pl.Category, "Category")
			assert.Equal(t, "queued-req-1", pl.RequestRef, "RequestRef round-trips the interrupt RequestID")
			assert.Equal(t, tc.wantOutcome, pl.Outcome, "Outcome mapping")
			assert.Equal(t, tc.wantText, pl.OutcomeText, "OutcomeText")
			assert.Equal(t, "https://hooks.slack.com/actions/T/1/interrupt", pl.ResponseRef,
				"ResponseRef round-trips the response_url so the applied edit targets the clicker's ephemeral")
		})
	}
}

// TestInterruptAppliedBridge_NonSlackSession_NoOp is the double-fire guard: a
// browser-input session consumes KindInterruptApplied directly on its
// queued_messages sub-channel sender, so the bridge MUST publish NOTHING for it —
// republishing an interaction_applied would double-render.
func TestInterruptAppliedBridge_NonSlackSession_NoOp(t *testing.T) {
	for _, kind := range []string{browser.KindName, "local", "bento", ""} {
		t.Run("input kind="+kind+" (no-op)", func(t *testing.T) {
			b := sessionWithInputKind(t, "default", "sess1", kind)
			rec := &recordingPublish{}
			b.publish = rec.Publish

			b.handle(context.Background(), interruptAppliedMsg(t, "default", "sess1", channelevents.InterruptAppliedPayload{
				RequestID: "queued-req-1",
				Outcome:   "interrupted",
			}))

			assert.Empty(t, rec.subjects,
				"a non-Slack session must get ZERO interaction_applied from the bridge (it consumes interrupt_applied directly — no double-fire)")
		})
	}
}

// TestInterruptAppliedBridge_MissingSession_NoOp: a session lookup miss (already
// GC'd) is a logged no-op, not a publish and not a panic.
func TestInterruptAppliedBridge_MissingSession_NoOp(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).Build() // no session
	b := &interruptAppliedBridge{cli: cli}
	rec := &recordingPublish{}
	b.publish = rec.Publish

	b.handle(context.Background(), interruptAppliedMsg(t, "default", "gone", channelevents.InterruptAppliedPayload{
		RequestID: "queued-req-1", Outcome: "interrupted",
	}))
	assert.Empty(t, rec.subjects, "no publish when the session no longer exists")
}

// TestInterruptAppliedBridge_MalformedEnvelope_NoOp: a malformed message is a
// logged no-op, not a panic.
func TestInterruptAppliedBridge_MalformedEnvelope_NoOp(t *testing.T) {
	b := sessionWithInputKind(t, "default", "sess1", "slack")
	rec := &recordingPublish{}
	b.publish = rec.Publish
	b.handle(context.Background(), &nats.Msg{Data: []byte("{not json")})
	assert.Empty(t, rec.subjects, "no publish for a malformed envelope")
}
