// The metaagent OUT subjects have their own dedicated subscriber
// (internal/cmd/channelsd/metaagent_handlers.go), which resolves the kind's
// metaagent_scope_approval / metaagent_notice SUB-CHANNEL sender. They also
// match this relay's cluster-wide "ap.session.*.*.out.>" subscription, so the
// relay sees every one and must leave them entirely alone: routing one would
// send an approval prompt to the MAIN sender (the switch's default arm) on top
// of the correct delivery, and decoding one as an Envelope logs a spurious
// validation drop for traffic that is not an envelope at all.

package outbound

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// capturingSink is a logr.LogSink that records every Info/Error message body,
// so a test can assert on what the relay did or did not say. logr's testing
// helpers only forward to *testing.T; we need the text back.
type capturingSink struct {
	mu   sync.Mutex
	msgs []string
}

func (s *capturingSink) Init(logr.RuntimeInfo)               {}
func (s *capturingSink) Enabled(int) bool                    { return true }
func (s *capturingSink) WithValues(...any) logr.LogSink      { return s }
func (s *capturingSink) WithName(string) logr.LogSink        { return s }
func (s *capturingSink) Info(_ int, msg string, _ ...any)    { s.record(msg) }
func (s *capturingSink) Error(_ error, msg string, _ ...any) { s.record(msg) }

func (s *capturingSink) record(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msg)
}

// contains reports whether any captured message contains needle.
func (s *capturingSink) contains(needle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

func (s *capturingSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

// TestRelaySkipsMetaagentSubjects drives handle() directly (no NATS round-trip)
// so the assertion is about the relay's decision, not about delivery timing.
// Both wire shapes are covered on purpose: the bare payload authzd and the
// operator publish today, and the enveloped form a runner authorized on this
// session's OUT subject could hand-craft. Neither may reach a Sender, and
// neither may be logged as a malformed envelope.
func TestRelaySkipsMetaagentSubjects(t *testing.T) {
	sess := channelBoundSession("default", "s1")
	cli := fakeClientWith(t, sess)

	// Both bodies are marshalled from the canonical payload structs rather than
	// spelled as JSON literals, so a renamed field breaks this test at COMPILE
	// time instead of leaving it passing against a shape nothing publishes.
	approval := scope.MetaagentApprovalPayload{
		RequestID: "req-1", Requester: "U1", ApproverSummary: "widen",
	}
	approvalBytes, err := json.Marshal(approval)
	require.NoError(t, err, "marshal a bare scope approval")

	noticeBytes, err := json.Marshal(channelkinds.MetaagentNoticePayload{
		Requester: "U1", Body: "applied",
	})
	require.NoError(t, err, "marshal a bare notice")

	approvalEnv, err := channelevents.BuildEnvelope("default", "s1",
		channelevents.KindMetaagentScopeApproval, approval)
	require.NoError(t, err, "build an enveloped scope approval")
	approvalEnvBytes, err := json.Marshal(approvalEnv)
	require.NoError(t, err)

	cases := []struct {
		name    string
		subject string
		data    []byte
	}{
		{
			name:    "bare scope approval (today's authzd wire shape): skipped silently",
			subject: "ap.session.default.s1.out.metaagent_scope_approval",
			data:    approvalBytes,
		},
		{
			name:    "bare notice (today's authzd + operator wire shape): skipped silently",
			subject: "ap.session.default.s1.out.metaagent_notice",
			data:    noticeBytes,
		},
		{
			name:    "enveloped scope approval (post-flip wire shape): still skipped, never routed",
			subject: "ap.session.default.s1.out.metaagent_scope_approval",
			data:    approvalEnvBytes,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &capturingSink{}
			ctx := log.IntoContext(context.Background(), logr.New(sink))

			sender := &captureSender{}
			r := &Relay{K8s: cli, Senders: &fixedResolver{s: sender}}
			r.handle(ctx, &nats.Msg{Subject: tc.subject, Data: tc.data})

			assert.Zero(t, sender.count(),
				"the relay must not deliver a metaagent surface; its dedicated subscriber owns it")
			assert.False(t, sink.contains("envelope validation failed"),
				"the relay must not report a metaagent payload as a malformed envelope; logged: %v", sink.all())
			assert.False(t, sink.contains("malformed envelope"),
				"same, for the unmarshal-failure branch; logged: %v", sink.all())
		})
	}
}

// TestRelayStillHandlesNonMetaagentOutSubjects is the mutation guard for the
// skip above: a skip keyed too broadly (on the "ap.session.*.*.out." prefix
// rather than on the kind token) would silently stop relaying everything.
func TestRelayStillHandlesNonMetaagentOutSubjects(t *testing.T) {
	sess := channelBoundSession("default", "s1")
	cli := fakeClientWith(t, sess)

	sender := &captureSender{}
	r := &Relay{K8s: cli, Senders: &fixedResolver{s: sender}}

	env := buildEnv(t, "s1", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "hello"})
	raw, err := json.Marshal(env)
	require.NoError(t, err)

	r.handle(context.Background(), &nats.Msg{
		Subject: channelevents.SubjectOut(channelevents.SubjectPrefix("default", "s1"), channelevents.KindUserMessage),
		Data:    raw,
	})
	assert.Equal(t, 1, sender.count(), "an ordinary out.user_message must still be delivered")
}
