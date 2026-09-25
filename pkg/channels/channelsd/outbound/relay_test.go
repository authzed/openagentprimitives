package outbound

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/watchdog"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// snapshotInteractionCategories empties the channelinteractions registry so the
// caller can register test-only fixtures, restoring the production
// (init()-registered) set on cleanup. Use this, never a bare Reset() +
// t.Cleanup(Reset) pair: Reset alone has nothing to restore, so it leaves the
// registry permanently empty for every later test in the package.
func snapshotInteractionCategories(t *testing.T) {
	t.Helper()
	saved := channelinteractions.All()
	channelinteractions.Reset()
	t.Cleanup(func() {
		channelinteractions.Reset()
		for _, c := range saved {
			channelinteractions.Register(c)
		}
	})
}

// -----------------------------------------------------------------------------
// Test doubles
// -----------------------------------------------------------------------------

type captureSender struct {
	mu  sync.Mutex
	got []channelevents.Envelope
}

func (s *captureSender) Send(_ context.Context, _ channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, env)
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *captureSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

// envelopes returns a defensive copy of every envelope captured so far.
func (s *captureSender) envelopes() []channelevents.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelevents.Envelope(nil), s.got...)
}

type fixedResolver struct {
	s    channelkinds.Sender
	subS channelkinds.Sender          // optional sub-channel sender; defaults to s when nil
	sink channelkinds.StreamDeltaSink // optional stream-delta sink; nil = kind opts out

	// nilSubChannel, when true, makes SubChannelSenderFor return a genuine
	// nil Sender instead of falling back to s or subS — simulating a kind
	// (browser/local/bento) that doesn't implement the requested sub-channel.
	nilSubChannel bool

	mu                 sync.Mutex
	subChannelCalls    []string                           // names passed to SubChannelSenderFor
	subChannelSessions []*spiceboxv1alpha1.ChannelBinding // sess.Spec.InputChannel as seen by SubChannelSenderFor, index-aligned with subChannelCalls
	// subChannelRefs is the session IDENTITY as seen by SubChannelSenderFor,
	// index-aligned with the two above. Recorded separately from the binding
	// because a client-hosted host gates on identity while routing follows the
	// binding, and the whole undeliverable-card defect was those two describing
	// different sessions.
	subChannelRefs []spiceboxv1alpha1.NamespacedRef
}

func (r *fixedResolver) SenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	return r.s, nil
}

// SubChannelSenderFor mirrors the production resolver's own first line
// (internal/cmd/channelsd/sender_resolver.go:319): refuse when the session it
// is handed carries no InputChannel binding, rather than ignoring sess
// entirely. A double that never inspects sess cannot catch a call site that
// passes an unresolved (headless) session — which is exactly how this task's
// bug survived three layers of guards undetected.
func (r *fixedResolver) SubChannelSenderFor(_ context.Context, sess *spiceboxv1alpha1.AgentSession, name string) (channelkinds.Sender, error) {
	if sess.Spec.InputChannel == nil {
		return nil, fmt.Errorf("session %s/%s is not channel-attached", sess.Namespace, sess.Name)
	}
	r.mu.Lock()
	r.subChannelCalls = append(r.subChannelCalls, name)
	r.subChannelSessions = append(r.subChannelSessions, sess.Spec.InputChannel)
	r.subChannelRefs = append(r.subChannelRefs,
		spiceboxv1alpha1.NamespacedRef{Namespace: sess.Namespace, Name: sess.Name})
	r.mu.Unlock()
	if r.nilSubChannel {
		return nil, nil
	}
	if r.subS != nil {
		return r.subS, nil
	}
	return r.s, nil
}

func (r *fixedResolver) StreamDeltaSinkFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	return r.sink, nil
}

func (r *fixedResolver) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.subChannelCalls...)
}

// lastSubChannelBinding returns the InputChannel binding of the most recent
// session handed to SubChannelSenderFor, or nil if it was never called.
func (r *fixedResolver) lastSubChannelBinding() *spiceboxv1alpha1.ChannelBinding {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.subChannelSessions) == 0 {
		return nil
	}
	return r.subChannelSessions[len(r.subChannelSessions)-1]
}

// captureSink records OnDelta calls for assertions.
type captureSink struct {
	mu       sync.Mutex
	got      []channelevents.Envelope
	gotInfos []channelkinds.SessionInfo
}

func (s *captureSink) OnDelta(_ context.Context, info channelkinds.SessionInfo, env channelevents.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, env)
	s.gotInfos = append(s.gotInfos, info)
	return nil
}

func (s *captureSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *captureSink) firstInfo() (channelkinds.SessionInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.gotInfos) == 0 {
		return channelkinds.SessionInfo{}, false
	}
	return s.gotInfos[0], true
}

// writeBackSender returns a SubChannelSendResult.External map on every Send,
// simulating slack's first-send thread_ts capture.
type writeBackSender struct {
	external map[string]string
}

func (w *writeBackSender) Send(_ context.Context, _ channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{External: w.external}, nil
}

// failFirstSender fails the first Send (simulating a respond_to_user delivery
// that the channel rejects — e.g. over-long text) and records every envelope
// it was asked to send. Subsequent sends succeed, so the relay's degraded
// delivery-failure notice (sent on this same sender) is captured.
type failFirstSender struct {
	mu   sync.Mutex
	got  []channelevents.Envelope
	errs int // how many sends failed
}

func (s *failFirstSender) Send(_ context.Context, _ channelkinds.SessionInfo, env channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, env)
	if len(s.got) == 1 {
		s.errs++
		return channelkinds.SubChannelSendResult{}, errSendFailed
	}
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *failFirstSender) envelopes() []channelevents.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelevents.Envelope(nil), s.got...)
}

var errSendFailed = errFmt("simulated send failure")

type errFmt string

func (e errFmt) Error() string { return string(e) }

// captureSessionInfoSender records the SessionInfo passed to each Send call.
type captureSessionInfoSender struct {
	mu  sync.Mutex
	got []channelkinds.SessionInfo
}

func (s *captureSessionInfoSender) Send(_ context.Context, info channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, info)
	return channelkinds.SubChannelSendResult{}, nil
}

func (s *captureSessionInfoSender) first() (channelkinds.SessionInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return channelkinds.SessionInfo{}, false
	}
	return s.got[0], true
}

// all returns a defensive copy of every SessionInfo captured so far.
func (s *captureSessionInfoSender) all() []channelkinds.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channelkinds.SessionInfo(nil), s.got...)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func startEmbeddedNATS(t *testing.T) *server.Server {
	t.Helper()
	opts := &server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	srv := natstest.RunServer(opts)
	t.Cleanup(srv.Shutdown)
	return srv
}

// connectNATS starts an embedded server and returns a client conn with cleanup.
func connectNATS(t *testing.T) *nats.Conn {
	t.Helper()
	srv := startEmbeddedNATS(t)
	nc, err := nats.Connect(srv.ClientURL())
	require.NoError(t, err, "nats.Connect")
	t.Cleanup(nc.Close)
	return nc
}

// scheme returns a fresh runtime.Scheme with the v1alpha1 types registered.
func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// fakeClientWith builds a controller-runtime fake client with the supplied
// objects pre-loaded.
func fakeClientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).Build()
}

// startRelay wires a relay around an existing NATS conn + k8s client +
// resolver, starts it, and registers Stop in cleanup.
func startRelay(t *testing.T, nc *nats.Conn, cli client.Client, sndrs SenderResolver, opts ...func(*Relay)) *Relay {
	t.Helper()
	r := &Relay{NC: nc, K8s: cli, Senders: sndrs}
	for _, opt := range opts {
		opt(r)
	}
	require.NoError(t, r.Start(context.Background()), "Relay.Start")
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return r
}

func withApplyStatusEvent(fn func(ctx context.Context, ns, name string, ev watchdog.Event) watchdog.ApplyResult) func(*Relay) {
	return func(r *Relay) { r.ApplyStatusEvent = fn }
}

func withOnTurnActivity(fn func(ctx context.Context, ns, name string, active bool, cause string, seq uint64, uid string)) func(*Relay) {
	return func(r *Relay) { r.OnTurnActivity = fn }
}

func TestRelay_TurnActivityRoutesToHook(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWith(t, defaultSession())

	defaultCap := &captureSender{}
	subCap := &captureSender{}
	res := &fixedResolver{s: defaultCap, subS: subCap}

	var mu sync.Mutex
	var gotNS, gotName, gotCause, gotUID string
	var gotActive bool
	var gotSeq uint64
	var hits int
	startRelay(t, nc, cli, res, withOnTurnActivity(
		func(_ context.Context, ns, name string, active bool, cause string, seq uint64, uid string) {
			mu.Lock()
			defer mu.Unlock()
			gotNS, gotName, gotActive, gotCause, gotSeq, gotUID = ns, name, active, cause, seq, uid
			hits++
		}))

	env := buildEnv(t, "foo", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: false, Cause: channelevents.PauseCauseReply})
	env.Seq = 42
	env.SessionUID = "uid-1"
	publishOut(t, nc, "default", "foo", "turn_activity", env)

	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hits > 0
	})

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, hits, "OnTurnActivity must fire exactly once")
	assert.Equal(t, "default", gotNS)
	assert.Equal(t, "foo", gotName)
	assert.False(t, gotActive)
	assert.Equal(t, "awaiting_reply", gotCause)
	assert.Equal(t, uint64(42), gotSeq, "Seq must pass through to the hook")
	assert.Equal(t, "uid-1", gotUID, "SessionUID must pass through to the hook")
	assert.Equal(t, 0, defaultCap.count(), "turn_activity must NOT route to a Sender")
	assert.Equal(t, 0, subCap.count())
}

// defaultSession returns the typical fake-channel-bound AgentSession used by
// most routing tests.
func defaultSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "fake", NATSSubjectPrefix: "ap.session.default.foo"},
		},
	}
}

// cronSession returns a bento-input + slack-output session — the shape
// every cron write-back test starts from.
func cronSession(name string, slackExternal map[string]string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "bento-in", Kind: "bento", NATSSubjectPrefix: "ap.session.default." + name},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-out", Kind: "slack",
				External: slackExternal,
			},
		},
	}
}

// publishOut publishes an envelope to the well-known
// ap.session.<ns>.<name>.out.<kindSuffix> subject.
func publishOut(t *testing.T, nc *nats.Conn, ns, name, kindSuffix string, env channelevents.Envelope) {
	t.Helper()
	b, err := json.Marshal(env)
	require.NoError(t, err, "marshal envelope")
	subject := channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), channelevents.Kind(kindSuffix))
	require.NoError(t, nc.Publish(subject, b), "Publish")
	require.NoError(t, nc.Flush(), "Flush")
}

// buildEnv builds an envelope for default/<name> with the supplied kind and
// payload (which is JSON-marshalled inline).
func buildEnv(t *testing.T, name string, kind channelevents.Kind, payload any) channelevents.Envelope {
	t.Helper()
	pl, err := json.Marshal(payload)
	require.NoError(t, err, "marshal payload")
	return channelevents.Envelope{
		Version:     1,
		Kind:        kind,
		Session:     channelevents.SessionRef{Namespace: "default", Name: name},
		PublishedAt: time.Now().UTC(),
		Payload:     pl,
	}
}

// waitUntil polls predicate every 10ms up to timeout; returns true on success.
func waitUntil(t *testing.T, timeout time.Duration, predicate func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return predicate()
}

// -----------------------------------------------------------------------------
// Subject-is-authority tests
// -----------------------------------------------------------------------------

// channelBoundSession returns a channel-bound AgentSession named ns/name —
// the minimum shape the relay will route for.
func channelBoundSession(ns, name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "c1", Kind: "fake", NATSSubjectPrefix: "ap.session." + ns + "." + name,
			},
		},
	}
}

// TestRelay_SubjectIsRoutingAuthority is the cross-session impersonation attack,
// not a happy path. The relay subscribes cluster-wide while a runner's
// per-session NATS JWT permits publishing under one "ap.session.<ns>.<own-name>.>"
// tree, so the SUBJECT is the only identity NATS authorizes and env.Session is
// attacker-controlled JSON. Routing off the envelope would make that grant
// non-load-bearing: a publisher permitted on attacker's subject could set
// env.Session to victim and have the relay deliver into victim's channel.
//
// Each case publishes a forged envelope on the attacker's subject, then a
// legitimate attacker envelope as a barrier. nats.go serializes one
// subscription's callbacks on a single goroutine, so observing the barrier
// proves the forged envelope was already handled — no sleep required.
func TestRelay_SubjectIsRoutingAuthority(t *testing.T) {
	const (
		attacker = "attacker-sess"
		victim   = "victim-sess"
	)
	cases := []struct {
		name string
		kind channelevents.Kind
		// subjectKind is the trailing subject token; it matches the Kind for
		// every case here.
		subjectKind string
		payload     any
	}{
		{
			name:        "user_message claiming another session: dropped, victim's channel untouched",
			kind:        channelevents.KindUserMessage,
			subjectKind: "user_message",
			payload:     channelevents.OutboundUserMessagePayload{Text: "posted into a thread I was never granted"},
		},
		{
			name:        "interaction_request claiming another session: dropped, victim's channel untouched",
			kind:        channelevents.KindInteractionRequest,
			subjectKind: "interaction_request",
			payload: channelevents.InteractionRequestPayload{
				Category: "tool_approval", RequestRef: "req-forged", Lead: "Approve this?",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t,
				channelBoundSession("default", attacker),
				channelBoundSession("default", victim),
			)
			sender := &captureSessionInfoSender{}
			startRelay(t, nc, cli, &fixedResolver{s: sender, subS: sender})

			// The attack: published on the ONE subject the attacker's JWT
			// permits, but claiming the victim's session in the envelope body.
			forged := buildEnv(t, victim, tc.kind, tc.payload)
			publishOut(t, nc, "default", attacker, tc.subjectKind, forged)

			// Barrier: a legitimate publish on the attacker's own subject for
			// its own session. Ordered behind the forged one on the same
			// subscription, so its arrival means the forged one is done.
			barrier := buildEnv(t, attacker, channelevents.KindUserMessage,
				channelevents.OutboundUserMessagePayload{Text: "my own reply"})
			publishOut(t, nc, "default", attacker, "user_message", barrier)

			require.True(t, waitUntil(t, 2*time.Second, func() bool {
				return len(sender.all()) >= 1
			}), "barrier envelope never reached a Sender")

			got := sender.all()
			for _, info := range got {
				assert.NotEqual(t, victim, info.Name,
					"envelope published on %s's subject reached %s's channel: the NATS subject grant is not load-bearing",
					attacker, victim)
			}
			assert.Len(t, got, 1, "only the barrier envelope may be delivered; the forged one must be dropped")
		})
	}
}

// -----------------------------------------------------------------------------
// Routing-by-Kind tests
// -----------------------------------------------------------------------------

// TestRelay_DispatchByKind verifies the relay's routing-by-Kind contract.
// Each row sets up the appropriate test double, publishes a single envelope
// of the given Kind, then asserts the envelope landed on the expected sink
// and NOT on the others.
func TestRelay_DispatchByKind(t *testing.T) {
	type assertion struct {
		// Counts to expect on each potential delivery target.
		defaultSenderHits int
		subChannelHits    int
		sinkHits          int
		toolActivityHits  int32
		// When subChannelHits > 0, asserts the sub-channel name passed.
		wantSubChannel string
	}
	cases := []struct {
		name    string
		kind    channelevents.Kind
		subject string
		payload any
		nilSink bool // when true, resolver returns nil sink
		expect  assertion
	}{
		{
			name:    "KindUserMessage routes to default Sender",
			kind:    channelevents.KindUserMessage,
			subject: "user_message",
			payload: channelevents.OutboundUserMessagePayload{Text: "hello"},
			expect:  assertion{defaultSenderHits: 1},
		},
		{
			name:    "KindToolActivity folds into the machine (EvToolActivity), NOT a Sender",
			kind:    channelevents.KindToolActivity,
			subject: "tool_activity",
			payload: channelevents.ToolActivityPayload{Tool: "code_gh"},
			expect:  assertion{toolActivityHits: 1},
		},
		{
			// webui-live-only: consumed by the session-view page's live mirror
			// (livemirror.WatchOutbound), never a channel sender. Early-returns
			// before the AgentSession load — no Sender of either kind, no log.
			name:    "KindWidgetOffer is webui-live-only, NOT dispatched to any Sender",
			kind:    channelevents.KindWidgetOffer,
			subject: "widget_offer",
			payload: channelevents.WidgetOfferPayload{ArtifactID: "art-abc123", RendererKind: "mcpui"},
			expect:  assertion{},
		},
		{
			// Same shape, same reason: the agent-UI page's own live mirror
			// (pkg/web/webui/agentui's runActionMirror) subscribes to this
			// subject directly, and it has no channel-message representation.
			// Falling through would cost an apiserver Get plus a sender.Send
			// erroring "unsupported envelope kind" — up to four per click, on
			// every channel-attached session.
			name:    "KindUIActionUpdate is webui-live-only, NOT dispatched to any Sender",
			kind:    channelevents.KindUIActionUpdate,
			subject: "ui_action_update",
			payload: channelevents.UIActionUpdatePayload{
				RequestID: "req-1", Action: "advance", Requester: "viewer-1", State: "succeeded",
			},
			expect: assertion{},
		},
		{
			// text_delta also Touches the watchdog via touchOnDelta — both the sink
			// AND an EvToolActivity fold via ApplyStatusEvent fire.
			name:    "KindAssistantStreamDelta routes to StreamDeltaSink (and Touches watchdog)",
			kind:    channelevents.KindAssistantStreamDelta,
			subject: "assistant_stream_delta",
			payload: channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: "hello"},
			expect:  assertion{sinkHits: 1, toolActivityHits: 1},
		},
		{
			name:    "KindToolSessionDelta routes to SubChannelSender with name=tool_session",
			kind:    channelevents.KindToolSessionDelta,
			subject: "tool_session_delta",
			payload: channelevents.ToolSessionDeltaPayload{
				ToolCallRef: "alice-1-tu1", Stream: "stdout", Data: []byte("hi"),
			},
			expect: assertion{subChannelHits: 1, wantSubChannel: "tool_session"},
		},
		{
			name:    "KindToolSessionEvent routes to SubChannelSender with name=tool_session",
			kind:    channelevents.KindToolSessionEvent,
			subject: "tool_session_event",
			payload: channelevents.ToolSessionEventPayload{
				ToolCallRef: "alice-1-tu1", EventType: "text_delta", Text: "hi",
			},
			expect: assertion{subChannelHits: 1, wantSubChannel: "tool_session"},
		},
		{
			name:    "KindLiveViewOffer routes to SubChannelSender with name=live_view_offer",
			kind:    channelevents.KindLiveViewOffer,
			subject: "live_view_offer",
			payload: channelevents.LiveViewOfferPayload{
				ArtifactID:   "art-abc123",
				RendererKind: "html",
			},
			expect: assertion{subChannelHits: 1, wantSubChannel: "live_view_offer"},
		},
		{
			name:    "KindSessionViewOffer routes to SubChannelSender with name=session_view_offer",
			kind:    channelevents.KindSessionViewOffer,
			subject: "session_view_offer",
			payload: channelevents.SessionViewOfferPayload{SessionRef: "default/sess-1"},
			expect:  assertion{subChannelHits: 1, wantSubChannel: "session_view_offer"},
		},
		{
			name:    "KindAgentUIOffer routes to SubChannelSender with name=agent_ui_offer",
			kind:    channelevents.KindAgentUIOffer,
			subject: "agent_ui_offer",
			payload: channelevents.AgentUIOfferPayload{SessionRef: "default/sess-1"},
			expect:  assertion{subChannelHits: 1, wantSubChannel: "agent_ui_offer"},
		},
		{
			name:    "KindThreadTitle routes to SubChannelSender with name=thread_title",
			kind:    channelevents.KindThreadTitle,
			subject: "thread_title",
			payload: channelevents.ThreadTitlePayload{Title: "hello"},
			expect:  assertion{subChannelHits: 1, wantSubChannel: "thread_title"},
		},
		{
			name:    "KindInterruptApplied routes to SubChannelSender with name=queued_messages",
			kind:    channelevents.KindInterruptApplied,
			subject: "interrupt_applied",
			payload: channelevents.InterruptAppliedPayload{RequestID: "req-1", Outcome: "interrupted"},
			expect:  assertion{subChannelHits: 1, wantSubChannel: "queued_messages"},
		},
		{
			name:    "KindEnqueueAck routes to SubChannelSender with name=queued_messages",
			kind:    channelevents.KindEnqueueAck,
			subject: "enqueue_ack",
			payload: channelevents.EnqueueAckPayload{
				RequestID:  "req-2",
				Requester:  channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_BOB"},
				SessionRef: "default/foo",
			},
			expect: assertion{subChannelHits: 1, wantSubChannel: "queued_messages"},
		},
		{
			name:    "KindInteractionRequest routes to SubChannelSender with name=interaction",
			kind:    channelevents.KindInteractionRequest,
			subject: "interaction_request",
			payload: channelevents.InteractionRequestPayload{
				Category:   "test_category",
				RequestRef: "ireq-1",
				Lead:       "Approve this?",
				Audience: channelevents.InteractionAudience{
					Scope: channelevents.AudienceApprovers,
					Approvers: []channelevents.ExternalIdentity{
						{Kind: "fake", ExternalID: "U0X"},
					},
				},
			},
			expect: assertion{subChannelHits: 1, wantSubChannel: "interaction"},
		},
		{
			name:    "KindInteractionApplied routes to SubChannelSender with name=interaction",
			kind:    channelevents.KindInteractionApplied,
			subject: "interaction_applied",
			payload: channelevents.InteractionAppliedPayload{
				Category:   "test_category",
				RequestRef: "ireq-1",
				Outcome:    channelevents.OutcomeApproved,
			},
			expect: assertion{subChannelHits: 1, wantSubChannel: "interaction"},
		},
		{
			name:    "KindUserEcho routes to SubChannelSender with name=user_echo",
			kind:    channelevents.KindUserEcho,
			subject: "user_echo",
			payload: channelevents.UserEchoPayload{
				Text:   "hi from the view",
				Author: channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U123"},
				Via:    "artifact:foo/dash",
			},
			expect: assertion{subChannelHits: 1, wantSubChannel: "user_echo"},
		},
		{
			// nil sink ⇒ no sink delivery, but touchOnDelta still fires
			// because watchdog Touch is independent of sink existence.
			name:    "KindAssistantStreamDelta with nil sink: silently dropped (no Sender; still Touches watchdog)",
			kind:    channelevents.KindAssistantStreamDelta,
			subject: "assistant_stream_delta",
			payload: channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: "hi"},
			nilSink: true,
			expect:  assertion{toolActivityHits: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t, defaultSession())

			defaultCap := &captureSender{}
			subCap := &captureSender{}
			sink := &captureSink{}
			res := &fixedResolver{s: defaultCap, subS: subCap}
			if !tc.nilSink {
				res.sink = sink
			}
			var toolHits atomic.Int32
			var gotNS, gotName string
			startRelay(t, nc, cli, res, withApplyStatusEvent(func(_ context.Context, ns, name string, ev watchdog.Event) watchdog.ApplyResult {
				// tool_activity + stream-delta progress fold in as EvToolActivity;
				// none of the kinds in this table are EvCaption, so a single
				// counter is enough to assert the timing re-arm fired.
				if ev.Kind == watchdog.EvToolActivity {
					gotNS, gotName = ns, name
					toolHits.Add(1)
				}
				return watchdog.ApplyResult{Forward: true}
			}))

			env := buildEnv(t, "foo", tc.kind, tc.payload)
			publishOut(t, nc, "default", "foo", tc.subject, env)

			// Wait for whichever positive signal is expected; for the
			// all-zero case, just give the relay 150ms to (not) dispatch.
			if tc.expect.defaultSenderHits+tc.expect.subChannelHits+tc.expect.sinkHits > 0 || tc.expect.toolActivityHits > 0 {
				waitUntil(t, 2*time.Second, func() bool {
					return defaultCap.count()+subCap.count()+sink.count() > 0 ||
						toolHits.Load() > 0
				})
			} else {
				time.Sleep(150 * time.Millisecond)
			}

			assert.Equal(t, tc.expect.defaultSenderHits, defaultCap.count(), "default Sender hits")
			assert.Equal(t, tc.expect.subChannelHits, subCap.count(), "sub-channel Sender hits")
			assert.Equal(t, tc.expect.sinkHits, sink.count(), "StreamDeltaSink hits")
			assert.Equal(t, tc.expect.toolActivityHits, toolHits.Load(), "EvToolActivity folds")

			if tc.expect.wantSubChannel != "" {
				calls := res.calls()
				require.NotEmpty(t, calls,
					"SubChannelSenderFor was not called")
				assert.Equal(t, tc.expect.wantSubChannel, calls[0],
					"SubChannelSenderFor name")
			}
			if tc.kind == channelevents.KindToolActivity && tc.expect.toolActivityHits > 0 {
				assert.Equal(t, "default", gotNS, "EvToolActivity ns")
				assert.Equal(t, "foo", gotName, "EvToolActivity name")
			}
		})
	}
}

// TestRelay_UserEcho_NilSubChannelSender_DegradesGracefully locks the contract
// that a KindUserEcho envelope routed to a kind with no "user_echo" sub-channel
// (browser/local/bento — the browser or TUI IS the view surface, so there is
// nothing to mirror to) is dropped silently rather than panicking or erroring.
// Same generic nil-sender degrade every optional sub-channel relies on (the
// "sender == nil" check in Relay.handle), pinned here for user_echo.
func TestRelay_UserEcho_NilSubChannelSender_DegradesGracefully(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWith(t, defaultSession())

	defaultCap := &captureSender{}
	res := &fixedResolver{s: defaultCap, nilSubChannel: true}
	startRelay(t, nc, cli, res)

	env := buildEnv(t, "foo", channelevents.KindUserEcho,
		channelevents.UserEchoPayload{Text: "hi from the view", Via: "artifact:foo/dash"})
	publishOut(t, nc, "default", "foo", "user_echo", env)

	// Give the relay time to (not) dispatch; there is no positive signal to
	// wait for since the whole point is that nothing gets called.
	time.Sleep(150 * time.Millisecond)

	calls := res.calls()
	require.NotEmpty(t, calls, "SubChannelSenderFor must still be consulted")
	assert.Equal(t, "user_echo", calls[0], "SubChannelSenderFor name")
	assert.Equal(t, 0, defaultCap.count(),
		"a nil user_echo sub-channel sender must NOT fall back to the default Sender")
}

// TestRelay_UserMessageSendFailure_SurfacesFallbackNotice locks the no-silent-
// drop contract: when the default Sender fails to deliver a user_message (the
// respond_to_user reply path), the relay must not just log and return — it must
// attempt a degraded plain-text delivery-failure notice on the same sender so
// the waiting user learns the reply was lost.
func TestRelay_UserMessageSendFailure_SurfacesFallbackNotice(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWith(t, defaultSession())

	sndr := &failFirstSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, "foo", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "the real reply"})
	publishOut(t, nc, "default", "foo", "user_message", env)

	// Expect TWO sends: the original (which fails) + the fallback notice.
	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		return len(sndr.envelopes()) >= 2
	}), "relay did not attempt a fallback notice after the user_message Send failed")

	got := sndr.envelopes()
	require.Len(t, got, 2, "exactly the original + one fallback notice")
	assert.Equal(t, channelevents.KindUserMessage, got[1].Kind,
		"fallback notice must be a user_message so it lands in-thread")

	var pl channelevents.OutboundUserMessagePayload
	require.NoError(t, json.Unmarshal(got[1].Payload, &pl), "unmarshal fallback payload")
	assert.NotEmpty(t, pl.Text, "fallback notice must carry user-visible text")
	assert.Empty(t, pl.Attachments,
		"fallback notice must be attachment-free so it survives an attachment-induced failure")
}

// -----------------------------------------------------------------------------
// Stream-delta watchdog tests
// -----------------------------------------------------------------------------

// TestRelay_StreamDeltaWatchdog locks the contract on whether each
// stream-delta EventType counts as "forward progress" for the silence
// watchdog, and which machine event it folds into: text_delta / tool_use_start
// fold in as EvToolActivity (timing re-arm); plan_update folds in as an ordered
// EvCaption; stop is a terminator and must NOT fold in (so post-stop silence
// stays detectable).
func TestRelay_StreamDeltaWatchdog(t *testing.T) {
	cases := []struct {
		name      string
		kind      channelevents.Kind
		subject   string
		eventType string // only used when kind == KindAssistantStreamDelta
		wantKind  watchdog.EventKind
		wantCalls int32
	}{
		{
			name:      "text_delta: tokens flowing → EvToolActivity",
			kind:      channelevents.KindAssistantStreamDelta,
			subject:   "assistant_stream_delta",
			eventType: "text_delta",
			wantKind:  watchdog.EvToolActivity,
			wantCalls: 1,
		},
		{
			name:      "tool_use_start: model picked a tool → EvToolActivity (gap before tool dispatches)",
			kind:      channelevents.KindAssistantStreamDelta,
			subject:   "assistant_stream_delta",
			eventType: "tool_use_start",
			wantKind:  watchdog.EvToolActivity,
			wantCalls: 1,
		},
		{
			name:      "plan_update: plan maintenance is an ordered caption → EvCaption",
			kind:      channelevents.KindPlanUpdate,
			subject:   "plan_update",
			wantKind:  watchdog.EvCaption,
			wantCalls: 1,
		},
		{
			name:      "stop: terminator must NOT fold in (post-stop silence stays detectable)",
			kind:      channelevents.KindAssistantStreamDelta,
			subject:   "assistant_stream_delta",
			eventType: "stop",
			wantCalls: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t, defaultSession())

			var mu sync.Mutex
			var events []watchdog.Event
			startRelay(t, nc, cli, &fixedResolver{s: &captureSender{}, sink: &captureSink{}},
				withApplyStatusEvent(func(_ context.Context, _, _ string, ev watchdog.Event) watchdog.ApplyResult {
					mu.Lock()
					events = append(events, ev)
					mu.Unlock()
					return watchdog.ApplyResult{Forward: true}
				}))

			var env channelevents.Envelope
			switch tc.kind {
			case channelevents.KindAssistantStreamDelta:
				env = buildEnv(t, "foo", tc.kind,
					channelevents.AssistantStreamDeltaPayload{EventType: tc.eventType, Text: "x"})
			case channelevents.KindPlanUpdate:
				env = buildEnv(t, "foo", tc.kind, channelevents.PlanUpdatePayload{PlanName: "main"})
			}
			publishOut(t, nc, "default", "foo", tc.subject, env)

			count := func() int32 {
				mu.Lock()
				defer mu.Unlock()
				return int32(len(events))
			}
			if tc.wantCalls > 0 {
				waitUntil(t, 2*time.Second, func() bool { return count() >= tc.wantCalls })
			} else {
				time.Sleep(150 * time.Millisecond)
			}

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tc.wantCalls, int32(len(events)), "ApplyStatusEvent calls")
			if tc.wantCalls > 0 {
				require.NotEmpty(t, events)
				assert.Equal(t, tc.wantKind, events[0].Kind, "folded event kind")
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Caption-gate tests
// -----------------------------------------------------------------------------

// TestRelay_CaptionGate locks the unified caption routing: a setStatus-bearing
// notification / plan_update is gated through the machine and reaches the Sender
// only when ApplyStatusEvent reports Forward; a suppressed (stale/yielded)
// caption is dropped; and the empty-Notification "clear my indicator" sentinel
// always reaches the Sender WITHOUT being gated.
func TestRelay_CaptionGate(t *testing.T) {
	cases := []struct {
		name      string
		kind      channelevents.Kind
		subject   string
		payload   any
		forward   bool // ApplyStatusEvent result for the EvCaption fold
		wantGated bool // was the caption gate (EvCaption) consulted?
		wantSends int  // Sender.Send calls
	}{
		{
			name: "notification (text) forwarded when machine says Forward",
			kind: channelevents.KindNotification, subject: "notification",
			payload: channelevents.NotificationPayload{Text: "working…"},
			forward: true, wantGated: true, wantSends: 1,
		},
		{
			name: "notification (text) suppressed when machine says drop (stale/yielded)",
			kind: channelevents.KindNotification, subject: "notification",
			payload: channelevents.NotificationPayload{Text: "stale caption"},
			forward: false, wantGated: true, wantSends: 0,
		},
		{
			name: "plan_update suppressed when machine says drop",
			kind: channelevents.KindPlanUpdate, subject: "plan_update",
			payload: channelevents.PlanUpdatePayload{PlanName: "main"},
			forward: false, wantGated: true, wantSends: 0,
		},
		{
			name: "empty-notification clear sentinel bypasses the gate and reaches the Sender",
			kind: channelevents.KindNotification, subject: "notification",
			payload: channelevents.NotificationPayload{}, // empty Text = clear sentinel
			forward: false, wantGated: false, wantSends: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t, defaultSession())
			sndr := &captureSender{}
			var gated atomic.Bool
			startRelay(t, nc, cli, &fixedResolver{s: sndr},
				withApplyStatusEvent(func(_ context.Context, _, _ string, ev watchdog.Event) watchdog.ApplyResult {
					if ev.Kind == watchdog.EvCaption {
						gated.Store(true)
					}
					return watchdog.ApplyResult{Forward: tc.forward}
				}))

			env := buildEnv(t, "foo", tc.kind, tc.payload)
			publishOut(t, nc, "default", "foo", tc.subject, env)

			if tc.wantSends > 0 {
				waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= tc.wantSends })
			} else {
				time.Sleep(150 * time.Millisecond)
			}
			assert.Equal(t, tc.wantSends, sndr.count(), "Sender.Send calls")
			assert.Equal(t, tc.wantGated, gated.Load(), "caption gate (EvCaption) consulted")
		})
	}
}

// TestRelay_TurnProgressGate pins the turn-progress gate contract: a
// KindTurnProgress envelope is folded through the silence-watchdog machine as an
// EvTurnProgress and only reaches the Sender when the machine reports Forward.
// A yielded session (the machine returns Forward=false) drops the spinner tick.
func TestRelay_TurnProgressGate(t *testing.T) {
	cases := []struct {
		name      string
		forward   bool // ApplyStatusEvent result for the EvTurnProgress fold
		wantSends int  // Sender.Send calls
	}{
		{name: "active session: turn_progress forwarded to the Sender", forward: true, wantSends: 1},
		{name: "yielded session: turn_progress dropped before the Sender", forward: false, wantSends: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t, defaultSession())
			sndr := &captureSender{}
			var sawTurnProgress atomic.Bool
			startRelay(t, nc, cli, &fixedResolver{s: sndr},
				withApplyStatusEvent(func(_ context.Context, _, _ string, ev watchdog.Event) watchdog.ApplyResult {
					if ev.Kind == watchdog.EvTurnProgress {
						sawTurnProgress.Store(true)
					}
					return watchdog.ApplyResult{Forward: tc.forward}
				}))

			env := buildEnv(t, "foo", channelevents.KindTurnProgress,
				channelevents.TurnProgressPayload{InputTokens: 1200, OutputTokens: 6400, ElapsedSeconds: 34, Seq: 3})
			publishOut(t, nc, "default", "foo", "turn_progress", env)

			if tc.wantSends > 0 {
				waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= tc.wantSends })
			} else {
				time.Sleep(150 * time.Millisecond)
			}
			assert.Equal(t, tc.wantSends, sndr.count(), "Sender.Send calls")
			assert.True(t, sawTurnProgress.Load(), "turn_progress must be folded as EvTurnProgress")
		})
	}
}

// TestRelay_ToolProgressGate pins the tool-progress gate contract: a
// KindToolProgress envelope is folded through the silence-watchdog machine as
// an EvToolProgress and only reaches the Sender when the machine reports
// Forward. A yielded session (the machine returns Forward=false) drops the
// per-tool liveness tick before it reaches the Sender.
func TestRelay_ToolProgressGate(t *testing.T) {
	cases := []struct {
		name      string
		forward   bool // ApplyStatusEvent result for the EvToolProgress fold
		wantSends int  // Sender.Send calls
	}{
		{name: "active session: tool_progress forwarded to the Sender", forward: true, wantSends: 1},
		{name: "yielded session: tool_progress dropped before the Sender", forward: false, wantSends: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t, defaultSession())
			sndr := &captureSender{}
			var sawToolProgress atomic.Bool
			startRelay(t, nc, cli, &fixedResolver{s: sndr},
				withApplyStatusEvent(func(_ context.Context, _, _ string, ev watchdog.Event) watchdog.ApplyResult {
					if ev.Kind == watchdog.EvToolProgress {
						sawToolProgress.Store(true)
					}
					return watchdog.ApplyResult{Forward: tc.forward}
				}))

			env := buildEnv(t, "foo", channelevents.KindToolProgress,
				channelevents.ToolProgressPayload{CallID: "tc1", Name: "git clone", ElapsedSeconds: 14})
			publishOut(t, nc, "default", "foo", "tool_progress", env)

			if tc.wantSends > 0 {
				waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= tc.wantSends })
			} else {
				time.Sleep(150 * time.Millisecond)
			}
			assert.Equal(t, tc.wantSends, sndr.count(), "Sender.Send calls")
			assert.True(t, sawToolProgress.Load(), "tool_progress must be folded as EvToolProgress")
		})
	}
}

// TestRelay_CaptionGate_ThreadsText locks caption-text threading: the EvCaption
// fold's watchdog.Event.Text must carry the KindNotification payload's Text so
// the machine remembers it as the revert target for a later operation-activity
// "cleared" tick.
func TestRelay_CaptionGate_ThreadsText(t *testing.T) {
	nc := connectNATS(t)
	cli := fakeClientWith(t, defaultSession())
	sndr := &captureSender{}

	var mu sync.Mutex
	var sawCaption bool
	var gotText string
	startRelay(t, nc, cli, &fixedResolver{s: sndr},
		withApplyStatusEvent(func(_ context.Context, _, _ string, ev watchdog.Event) watchdog.ApplyResult {
			if ev.Kind == watchdog.EvCaption {
				mu.Lock()
				sawCaption = true
				gotText = ev.Text
				mu.Unlock()
			}
			return watchdog.ApplyResult{Forward: true}
		}))

	env := buildEnv(t, "foo", channelevents.KindNotification,
		channelevents.NotificationPayload{Text: "working on it…"})
	publishOut(t, nc, "default", "foo", "notification", env)

	waitUntil(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return sawCaption
	})

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, sawCaption, "EvCaption fold must be consulted for a text-bearing notification")
	assert.Equal(t, "working on it…", gotText, "EvCaption Event.Text must carry the notification payload's Text")
}

// TestRelay_OperationActivityGate pins the operation-activity gate: a
// KindOperationActivity envelope folds through the silence-watchdog machine as
// an EvOperationActivity and reaches the Sender only when the machine reports
// Forward. When forwarded, the payload's CompactLine is overwritten with the
// machine's resolved EffectiveLine — which may differ from the runner-submitted
// one, e.g. reverting to a remembered caption on a cleared tick — so every
// sender renders the resolved value.
func TestRelay_OperationActivityGate(t *testing.T) {
	cases := []struct {
		name          string
		forward       bool
		effectiveLine string
		wantSends     int
	}{
		{
			name:          "active session: operation_activity forwarded with CompactLine set to the machine's EffectiveLine",
			forward:       true,
			effectiveLine: "resolved effective line",
			wantSends:     1,
		},
		{
			name:      "yielded session: operation_activity dropped before the Sender",
			forward:   false,
			wantSends: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t, defaultSession())
			sndr := &captureSender{}
			var sawOpActivity atomic.Bool
			startRelay(t, nc, cli, &fixedResolver{s: sndr},
				withApplyStatusEvent(func(_ context.Context, _, _ string, ev watchdog.Event) watchdog.ApplyResult {
					if ev.Kind == watchdog.EvOperationActivity {
						sawOpActivity.Store(true)
					}
					return watchdog.ApplyResult{Forward: tc.forward, EffectiveLine: tc.effectiveLine}
				}))

			env := buildEnv(t, "foo", channelevents.KindOperationActivity,
				channelevents.OperationActivityPayload{CompactLine: "runner-submitted line"})
			publishOut(t, nc, "default", "foo", "operation_activity", env)

			if tc.wantSends > 0 {
				waitUntil(t, 2*time.Second, func() bool { return sndr.count() >= tc.wantSends })
			} else {
				time.Sleep(150 * time.Millisecond)
			}
			assert.Equal(t, tc.wantSends, sndr.count(), "Sender.Send calls")
			assert.True(t, sawOpActivity.Load(), "operation_activity must be folded as EvOperationActivity")

			if tc.wantSends > 0 {
				got := sndr.envelopes()
				require.Len(t, got, 1)
				var pl channelevents.OperationActivityPayload
				require.NoError(t, json.Unmarshal(got[0].Payload, &pl), "unmarshal forwarded payload")
				assert.Equal(t, tc.effectiveLine, pl.CompactLine,
					"forwarded payload's CompactLine must be overwritten with the machine's EffectiveLine")
			}
		})
	}
}

// -----------------------------------------------------------------------------
// OutputChannel binding-preference tests
// -----------------------------------------------------------------------------

// TestRelay_PrefersOutputChannelBinding verifies that when a session has
// both InputChannel (e.g. bento) and OutputChannel (e.g. slack), the
// Sender and the StreamDeltaSink both receive OutputChannel as
// SessionInfo.Channel. Legacy single-channel sessions (no OutputChannel)
// fall back to InputChannel.
func TestRelay_PrefersOutputChannelBinding(t *testing.T) {
	const slackChannelID = "C0ABCDEF"
	const slackThread = "111.222"

	cronSess := cronSession("cron-sess", map[string]string{"channel_id": slackChannelID})
	legacySess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-delta", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-eng", Kind: "slack",
				NATSSubjectPrefix: "ap.session.default.legacy-delta",
				External:          map[string]string{"channel_id": "CENG", "thread_ts": slackThread},
			},
		},
	}

	type want struct {
		channelKind string
		channelName string
		channelID   string
	}
	cases := []struct {
		name    string
		sess    *spiceboxv1alpha1.AgentSession
		kind    channelevents.Kind
		subject string
		payload any
		// captureFn extracts the SessionInfo observed by the appropriate sink.
		setup func(t *testing.T, nc *nats.Conn, cli client.Client) func() (channelkinds.SessionInfo, bool)
		want  want
	}{
		{
			name:    "Sender path: cron session (bento+slack) → OutputChannel = slack",
			sess:    cronSess,
			kind:    channelevents.KindUserMessage,
			subject: "user_message",
			payload: channelevents.OutboundUserMessagePayload{Text: "cron says hi"},
			setup: func(t *testing.T, nc *nats.Conn, cli client.Client) func() (channelkinds.SessionInfo, bool) {
				sndr := &captureSessionInfoSender{}
				startRelay(t, nc, cli, &fixedResolver{s: sndr})
				return sndr.first
			},
			want: want{channelKind: "slack", channelName: "slack-out", channelID: slackChannelID},
		},
		{
			name:    "StreamDeltaSink path: cron session → OutputChannel = slack",
			sess:    cronSession("cron-delta", map[string]string{"channel_id": "C0", "thread_ts": ""}),
			kind:    channelevents.KindAssistantStreamDelta,
			subject: "assistant_stream_delta",
			payload: channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: "hello from cron"},
			setup: func(t *testing.T, nc *nats.Conn, cli client.Client) func() (channelkinds.SessionInfo, bool) {
				sink := &captureSink{}
				startRelay(t, nc, cli, &fixedResolver{s: &captureSender{}, sink: sink})
				return sink.firstInfo
			},
			want: want{channelKind: "slack", channelName: "slack-out", channelID: "C0"},
		},
		{
			name:    "StreamDeltaSink path: legacy session (slack input only, no OutputChannel) → InputChannel fallback",
			sess:    legacySess,
			kind:    channelevents.KindAssistantStreamDelta,
			subject: "assistant_stream_delta",
			payload: channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: "legacy streaming reply"},
			setup: func(t *testing.T, nc *nats.Conn, cli client.Client) func() (channelkinds.SessionInfo, bool) {
				sink := &captureSink{}
				startRelay(t, nc, cli, &fixedResolver{s: &captureSender{}, sink: sink})
				return sink.firstInfo
			},
			want: want{channelKind: "slack", channelName: "slack-eng", channelID: "CENG"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := connectNATS(t)
			cli := fakeClientWith(t, tc.sess)
			getInfo := tc.setup(t, nc, cli)

			env := buildEnv(t, tc.sess.Name, tc.kind, tc.payload)
			publishOut(t, nc, "default", tc.sess.Name, tc.subject, env)

			waitUntil(t, 2*time.Second, func() bool {
				_, ok := getInfo()
				return ok
			})
			info, ok := getInfo()
			require.True(t, ok, "expected callback was never invoked")
			require.NotNil(t, info.Channel, "SessionInfo.Channel is nil")
			assert.Equal(t, tc.want.channelKind, info.Channel.Kind, "Channel.Kind")
			assert.Equal(t, tc.want.channelName, info.Channel.Name, "Channel.Name")
			assert.Equal(t, tc.want.channelID, info.Channel.External["channel_id"], "Channel.External[channel_id]")
		})
	}
}

// -----------------------------------------------------------------------------
// Headless delegated child: release-card delivery through the lineage
// -----------------------------------------------------------------------------

// captureLogSink returns a logr.Logger that records every formatted Info/Error
// call, and a joined() accessor over what has been recorded so far. Needed
// only where a test must distinguish WHICH drop line fired (a failed lineage
// walk vs. a legitimately absent binding), not just whether a Sender did.
func captureLogSink(t *testing.T) (logr.Logger, func() string) {
	t.Helper()
	var mu sync.Mutex
	var lines []string
	rec := funcr.New(func(_, args string) {
		mu.Lock()
		lines = append(lines, args)
		mu.Unlock()
	}, funcr.Options{})
	return rec, func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

// startRelayWithLogger is startRelay's counterpart for tests that must assert
// WHICH line the relay logged: it carries logger on the subscription's
// context so log.FromContext inside Relay.handle resolves to it instead of
// controller-runtime's discard-by-default root logger.
func startRelayWithLogger(t *testing.T, nc *nats.Conn, cli client.Client, sndrs SenderResolver, logger logr.Logger) *Relay {
	t.Helper()
	r := &Relay{NC: nc, K8s: cli, Senders: sndrs}
	ctx := log.IntoContext(context.Background(), logger)
	require.NoError(t, r.Start(ctx), "Relay.Start")
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return r
}

// headlessLineageRootSession is a channel-bound root — the shape a released
// hold's card must actually land in.
func headlessLineageRootSession(name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "root-channel", Kind: "fake", NATSSubjectPrefix: "ap.session.default." + name,
			},
		},
	}
}

// headlessDelegatedChildSession is a delegated child: headless by
// construction (no InputChannel, no OutputChannel — the shape
// pkg/controllers/subagentrequest/controller.go's buildChild produces),
// parented to parentName.
func headlessDelegatedChildSession(name, parentName string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "ac1",
			Parent: &spiceboxv1alpha1.NamespacedRef{Namespace: "default", Name: parentName},
		},
	}
}

// interactionRequestEnvelope builds a valid KindInteractionRequest envelope
// for sessName — the forensic-hold release card's own kind, the one kind this
// task routes through the lineage.
func interactionRequestEnvelope(t *testing.T, sessName string) channelevents.Envelope {
	t.Helper()
	return buildEnv(t, sessName, channelevents.KindInteractionRequest, channelevents.InteractionRequestPayload{
		Category:   "tool_approval",
		RequestRef: "hold-release-1",
		Lead:       "Release this held session?",
		Audience: channelevents.InteractionAudience{
			Scope:     channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "fake", ExternalID: "U0REVIEWER"}},
		},
	})
}

// TestRelay_HeadlessChildReleaseCard covers Task 8's fix: a delegated child is
// headless by construction (no spec.inputChannel) and a held session can
// never gain one, so an interaction request about a child — the forensic-hold
// release card — must resolve its delivery binding through the lineage
// closure or the hold can never be released. Every other envelope kind, and
// every session shape the walk cannot help, must still drop exactly as
// before: this task moves ONE envelope kind, not a child's whole
// conversation.
func TestRelay_HeadlessChildReleaseCard(t *testing.T) {
	t.Run("interaction request for a headless child routes to the root's binding, still addressed to the child", func(t *testing.T) {
		root := headlessLineageRootSession("root-sess")
		child := headlessDelegatedChildSession("child-sess", root.Name)
		nc := connectNATS(t)
		cli := fakeClientWith(t, root, child)
		sndr := &captureSessionInfoSender{}
		res := &fixedResolver{subS: sndr}
		startRelay(t, nc, cli, res)

		env := interactionRequestEnvelope(t, child.Name)
		publishOut(t, nc, "default", child.Name, "interaction_request", env)

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			_, ok := sndr.first()
			return ok
		}), "sender.Send was never called")

		got, ok := sndr.first()
		require.True(t, ok)
		require.NotNil(t, got.Channel, "SessionInfo.Channel is nil")
		// Delivered through the ROOT's binding — getting this backwards (the
		// child's own, nonexistent, binding) is exactly the one-way door this
		// task closes.
		assert.Equal(t, root.Spec.InputChannel.Name, got.Channel.Name, "must route to the root's Channel, not drop")
		assert.Equal(t, root.Spec.InputChannel.Kind, got.Channel.Kind, "must route to the root's Channel kind")
		// ...but SessionInfo still names the CHILD: the card is ABOUT the
		// child, delivered in the root's thread. Reversed, this would address
		// the release card at the wrong session and release the wrong hold.
		assert.Equal(t, child.Namespace, got.Namespace)
		assert.Equal(t, child.Name, got.Name)
		// The session the RESOLVER was handed (not just the eventually-sent
		// SessionInfo) must itself carry the root's binding — this is Task 8a's
		// own fix. Without it, this whole subtest still passed on Task 8's
		// unmodified call site, because the old fixedResolver ignored the
		// session it was given entirely.
		lookupBinding := res.lastSubChannelBinding()
		require.NotNil(t, lookupBinding, "SubChannelSenderFor must have been called with a non-nil binding")
		assert.Equal(t, root.Spec.InputChannel.Name, lookupBinding.Name, "resolver must see the root's Channel name, not the child's (nil) own")
		assert.Equal(t, root.Spec.InputChannel.Kind, lookupBinding.Kind, "resolver must see the root's Channel kind")
	})

	t.Run("a non-interaction envelope for the same headless child still drops", func(t *testing.T) {
		root := headlessLineageRootSession("root-sess2")
		child := headlessDelegatedChildSession("child-sess2", root.Name)
		nc := connectNATS(t)
		cli := fakeClientWith(t, root, child)
		sndr := &captureSessionInfoSender{}
		res := &fixedResolver{s: sndr, subS: sndr}
		startRelay(t, nc, cli, res)

		// An ordinary reply from the child: must drop, exactly as before this
		// task. Routing a child's ordinary output into the root's thread is a
		// conversational-mode feature (Track 1b), not this one — this is the
		// blast-radius assertion that keeps the fix from silently becoming
		// "route all child output upward".
		ordinary := buildEnv(t, child.Name, channelevents.KindUserMessage,
			channelevents.OutboundUserMessagePayload{Text: "ordinary child output"})
		publishOut(t, nc, "default", child.Name, "user_message", ordinary)

		// Barrier: a real interaction request on the SAME child subject, which
		// (per the case above) DOES route once handled, on the SAME sender.
		// Its arrival proves the ordinary envelope published before it was
		// already handled — nats.go serializes one subscription's callbacks on
		// a single goroutine.
		barrier := interactionRequestEnvelope(t, child.Name)
		publishOut(t, nc, "default", child.Name, "interaction_request", barrier)

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			return len(sndr.all()) >= 1
		}), "barrier interaction request never reached the sender")

		got := sndr.all()
		assert.Len(t, got, 1, "only the barrier's interaction request may be delivered; the ordinary reply must be dropped")
		// The ordinary envelope must never reach the resolver at all — it is
		// dropped in the pre-switch gate (relay.go's outBinding==nil check),
		// before SubChannelSenderFor is ever consulted. If the resolver saw two
		// calls here, the ordinary reply would have reached sender construction
		// (whether or not it went on to deliver), which is exactly the
		// conversational-mode routing this task must NOT introduce as a
		// side effect.
		assert.Len(t, res.calls(), 1, "only the barrier's interaction request may reach SubChannelSenderFor")
	})

	t.Run("a genuinely channel-less root drops, unchanged", func(t *testing.T) {
		// A kubectl-driven session: no parent, no binding of its own. The walk
		// runs (this IS a KindInteractionRequest) but legitimately finds
		// nothing — a real, expected outcome, not an error.
		root := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: "kubectl-root", Namespace: "default"},
			Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
		}
		nc := connectNATS(t)
		cli := fakeClientWith(t, root)
		sndr := &captureSessionInfoSender{}
		logger, joined := captureLogSink(t)
		startRelayWithLogger(t, nc, cli, &fixedResolver{subS: sndr}, logger)

		env := interactionRequestEnvelope(t, root.Name)
		publishOut(t, nc, "default", root.Name, "interaction_request", env)

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			return strings.Contains(joined(), "no human-readable channel binding anywhere in the lineage")
		}), "expected drop log line never appeared")

		assert.Empty(t, sndr.all(), "a channel-less root's own interaction request must still drop")
	})

	t.Run("a broken lineage link surfaces distinctly from a legitimate no-binding root", func(t *testing.T) {
		// The parent this child names does not exist — a broken link, not a
		// legitimate root. Per no-silent-errors this must be distinguishable in
		// the logs from "no binding anywhere", which is a different
		// operational problem (one is data corruption; the other is normal).
		child := headlessDelegatedChildSession("orphan-child", "does-not-exist")
		nc := connectNATS(t)
		cli := fakeClientWith(t, child) // the parent is deliberately absent
		sndr := &captureSessionInfoSender{}
		logger, joined := captureLogSink(t)
		startRelayWithLogger(t, nc, cli, &fixedResolver{subS: sndr}, logger)

		env := interactionRequestEnvelope(t, child.Name)
		publishOut(t, nc, "default", child.Name, "interaction_request", env)

		require.True(t, waitUntil(t, 2*time.Second, func() bool {
			return strings.Contains(joined(), "lineage walk failed")
		}), "expected error log line never appeared")

		assert.Empty(t, sndr.all(), "a broken lineage link must drop, not deliver")
		assert.NotContains(t, joined(), "no human-readable channel binding anywhere in the lineage",
			"a broken link must not be indistinguishable from a legitimate no-binding root")
	})
}

// -----------------------------------------------------------------------------
// SessionInfo.Annotations propagation
// -----------------------------------------------------------------------------

// TestRelay_SessionInfo_CarriesSessionAnnotations proves the relay copies the
// AgentSession's annotations onto SessionInfo for BOTH Send and OnDelta. Kept
// generic: the relay must not know any kind's annotation keys.
func TestRelay_SessionInfo_CarriesSessionAnnotations(t *testing.T) {
	const key, val = "example.test/anchor", "1700000000.000100"

	t.Run("Send path copies annotations", func(t *testing.T) {
		sess := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "annot-send", Namespace: "default",
				Annotations: map[string]string{key: val},
			},
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				Class:        "ac1",
				InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "fake", NATSSubjectPrefix: "ap.session.default.annot-send"},
			},
		}
		nc := connectNATS(t)
		cli := fakeClientWith(t, sess)
		sndr := &captureSessionInfoSender{}
		startRelay(t, nc, cli, &fixedResolver{s: sndr})

		env := buildEnv(t, sess.Name, channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "hi"})
		publishOut(t, nc, "default", sess.Name, "user_message", env)

		waitUntil(t, 2*time.Second, func() bool {
			_, ok := sndr.first()
			return ok
		})
		got, ok := sndr.first()
		require.True(t, ok, "sender.Send must have been called")
		assert.Equal(t, val, got.Annotations[key], "annotations must reach SessionInfo (Send)")
	})

	t.Run("OnDelta path copies annotations", func(t *testing.T) {
		sess := &spiceboxv1alpha1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{
				Name: "annot-delta", Namespace: "default",
				Annotations: map[string]string{key: val},
			},
			Spec: spiceboxv1alpha1.AgentSessionSpec{
				Class:        "ac1",
				InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "fake", NATSSubjectPrefix: "ap.session.default.annot-delta"},
			},
		}
		nc := connectNATS(t)
		cli := fakeClientWith(t, sess)
		sink := &captureSink{}
		startRelay(t, nc, cli, &fixedResolver{s: &captureSender{}, sink: sink})

		env := buildEnv(t, sess.Name, channelevents.KindAssistantStreamDelta,
			channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: "hello"})
		publishOut(t, nc, "default", sess.Name, "assistant_stream_delta", env)

		waitUntil(t, 2*time.Second, func() bool {
			_, ok := sink.firstInfo()
			return ok
		})
		got, ok := sink.firstInfo()
		require.True(t, ok, "sink.OnDelta must have been called")
		assert.Equal(t, val, got.Annotations[key], "annotations must reach SessionInfo (OnDelta)")
	})
}

// TestRelay_SessionInfo_CarriesSessionInitiator proves the relay populates
// SessionInfo.SessionInitiator from AnnotationStartedByCanonicalID. Out-of-band
// applied senders (slack's credential_linked notice) carry no recipient in the
// payload and resolve it from this field — without it the "connected, revoke if
// not you" detective control silently fails to deliver.
func TestRelay_SessionInfo_CarriesSessionInitiator(t *testing.T) {
	// The annotation stores the PREFIXED subject form; SessionInfo.SessionInitiator
	// is BARE (identity.CanonicalUserID) — the relay must strip "user:" on write.
	const initiator = "user:dXNlckBleGFtcGxlLnRlc3Q="
	var wantBareInitiator = identity.CanonicalFromTrusted("dXNlckBleGFtcGxlLnRlc3Q=", "test fixture")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: "initiator-send", Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: initiator,
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "fake", NATSSubjectPrefix: "ap.session.default.initiator-send"},
		},
	}
	nc := connectNATS(t)
	cli := fakeClientWith(t, sess)
	sndr := &captureSessionInfoSender{}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, sess.Name, channelevents.KindUserMessage, channelevents.OutboundUserMessagePayload{Text: "hi"})
	publishOut(t, nc, "default", sess.Name, "user_message", env)

	waitUntil(t, 2*time.Second, func() bool {
		_, ok := sndr.first()
		return ok
	})
	got, ok := sndr.first()
	require.True(t, ok, "sender.Send must have been called")
	assert.Equal(t, wantBareInitiator, got.SessionInitiator, "SessionInitiator must reach SessionInfo (Send), bare (no \"user:\" prefix)")
}

// -----------------------------------------------------------------------------
// First-send write-back / patch tests
// -----------------------------------------------------------------------------

// TestRelay_FirstSend_WriteBackPatch covers what the relay does when the Sender
// returns routing metadata on its first reply. Three observables — the patched
// outputChannel key, the index label, and the SessionAttached publish — share
// one fixture and are asserted together rather than as a table whose rows would
// each check something completely different. The negative (a legacy session must
// NOT be patched) stays separate, in
// TestRelay_FirstSend_NoOutputChannel_SkipsPatch.
func TestRelay_FirstSend_WriteBackPatch(t *testing.T) {
	nc := connectNATS(t)
	sess := cronSession("cron-session", map[string]string{"channel_id": "C01ABCDEF"})
	sess.UID = "cron-uid-1"
	cli := fakeClientWith(t, sess)

	// Subscribe to SessionAttached BEFORE starting the relay so we don't
	// miss the event.
	got := make(chan channelevents.SessionAttached, 1)
	sub, err := nc.Subscribe(channelevents.SessionAttachedSubject, func(m *nats.Msg) {
		var ev channelevents.SessionAttached
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			return
		}
		select {
		case got <- ev:
		default:
		}
	})
	require.NoError(t, err, "subscribe SessionAttached")
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	sndr := &writeBackSender{external: map[string]string{
		"thread_ts":  "1234.5678",
		"channel_id": "C01ABCDEF",
	}}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, "cron-session", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "first post"})
	publishOut(t, nc, "default", "cron-session", "user_message", env)

	const wantKey = "thread:C01ABCDEF:1234.5678"
	wantLabel := channelkey.LabelValue(wantKey)

	// Poll for the patch to land on the fake client.
	var patched spiceboxv1alpha1.AgentSession
	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		_ = cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cron-session"}, &patched)
		return patched.Spec.OutputChannel != nil &&
			patched.Spec.OutputChannel.External["thread_ts"] == "1234.5678" &&
			patched.Labels[spiceboxv1alpha1.LabelOutputChannelKey] == wantLabel
	}), "OutputChannel + LabelOutputChannelKey did not land within 2s")

	// (1) spec.outputChannel.External fields preserved + Key derived.
	require.NotNil(t, patched.Spec.OutputChannel, "OutputChannel was cleared")
	assert.Equal(t, "1234.5678", patched.Spec.OutputChannel.External["thread_ts"])
	assert.Equal(t, "C01ABCDEF", patched.Spec.OutputChannel.External["channel_id"])
	assert.Equal(t, wantKey, patched.Spec.OutputChannel.Key, "OutputChannel.Key")

	// (2) LabelOutputChannelKey stamped in the same operation — without
	// this label the inbound LabelOutputChannelKey fallback can't find
	// the cron-spawned session and human thread replies silently drop.
	assert.Equal(t, wantLabel, patched.Labels[spiceboxv1alpha1.LabelOutputChannelKey],
		"labels[%s] (sha256 of %q)", spiceboxv1alpha1.LabelOutputChannelKey, wantKey)

	// (3) SessionAttached event published so threadIndex updates in real
	// time without a channelsd restart.
	select {
	case ev := <-got:
		assert.Equal(t, "default", ev.Namespace)
		assert.Equal(t, "cron-session", ev.SessionName)
		assert.Equal(t, "cron-uid-1", string(ev.SessionUID))
		assert.Equal(t, "slack-out", ev.OutputChannelName)
		assert.Equal(t, "slack", ev.OutputChannelKind)
		assert.Equal(t, wantKey, ev.OutputChannelKey)
		assert.Equal(t, "1234.5678", ev.External["thread_ts"])
		assert.Equal(t, "C01ABCDEF", ev.External["channel_id"])
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SessionAttached event after OutputChannel write-back")
	}
}

// TestRelay_FirstSend_NoOutputChannel_SkipsPatch verifies legacy single-
// Channel sessions (InputChannel only, no OutputChannel) are NOT patched
// — even when the Sender hands back routing metadata. Legacy slack
// sessions get their thread root from the listener's inbound stamp, not
// from the outbound write-back path.
func TestRelay_FirstSend_NoOutputChannel_SkipsPatch(t *testing.T) {
	nc := connectNATS(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "slack-eng", Kind: "slack", NATSSubjectPrefix: "ap.session.default.legacy"},
		},
	}
	cli := fakeClientWith(t, sess)

	sndr := &writeBackSender{external: map[string]string{
		"thread_ts":  "9999.0000",
		"channel_id": "C99",
	}}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, "legacy", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "legacy reply"})
	publishOut(t, nc, "default", "legacy", "user_message", env)

	// Give the relay time to (not) patch. Polling for absence is
	// awkward; a brief settle is sufficient because we don't expect
	// the relay to act on this path at all.
	time.Sleep(200 * time.Millisecond)
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "legacy"}, &got), "Get")
	assert.Nil(t, got.Spec.OutputChannel,
		"OutputChannel was unexpectedly populated: %+v", got.Spec.OutputChannel)
}

// TestRelay_FirstSend_RestartForkChild_PatchesInputChannel verifies that a
// restart-fork child session (InputChannel only, no OutputChannel, but with the
// AnnotationForkedFromThread annotation) gets its InputChannel.External["thread_ts"]
// and LabelChannelKey patched after first-send — enabling inbound routing to find
// the child session when the user replies in the new thread.
func TestRelay_FirstSend_RestartForkChild_PatchesInputChannel(t *testing.T) {
	nc := connectNATS(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "child-fk",
			Namespace: "default",
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationForkedFromThread: "T99:C-PARENT:parent.1111",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-eng", Kind: "slack",
				NATSSubjectPrefix: "ap.session.default.child-fk",
				External: map[string]string{
					"channel_id": "C-CHILD",
					"team_id":    "T99",
					// thread_ts intentionally absent — cleared at fork time.
				},
			},
		},
	}
	cli := fakeClientWith(t, sess)

	sndr := &writeBackSender{external: map[string]string{
		"thread_ts":  "child-root.2222",
		"channel_id": "C-CHILD",
	}}
	startRelay(t, nc, cli, &fixedResolver{s: sndr})

	env := buildEnv(t, "child-fk", channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "restart first reply"})
	publishOut(t, nc, "default", "child-fk", "user_message", env)

	// Poll until the relay patches InputChannel (or timeout).
	var got spiceboxv1alpha1.AgentSession
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, cli.Get(context.Background(),
			client.ObjectKey{Namespace: "default", Name: "child-fk"}, &got))
		if got.Spec.InputChannel != nil && got.Spec.InputChannel.External["thread_ts"] != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	require.NotNil(t, got.Spec.InputChannel)
	assert.Equal(t, "child-root.2222", got.Spec.InputChannel.External["thread_ts"],
		"InputChannel.External[thread_ts] must be patched with the new thread root")
	assert.Equal(t, "C-CHILD", got.Spec.InputChannel.External["channel_id"])

	expectedKey := "thread:C-CHILD:child-root.2222"
	assert.Equal(t, channelkey.LabelValue(expectedKey), got.Labels[spiceboxv1alpha1.LabelChannelKey],
		"LabelChannelKey must be patched with the new thread hash")

	// OutputChannel must remain nil — only InputChannel is relevant here.
	assert.Nil(t, got.Spec.OutputChannel)
}

// TestRelay_FirstSend_HumanDirectedOwnBinding_PatchesInputChannel covers the
// positive direction of the write-back gate (`outBinding == ownBinding` in
// relay.go, just above patchOutputChannel/patchInputChannelThreadTS): a
// HUMAN-DIRECTED envelope — not KindUserMessage — on a session whose own
// binding IS the human-directed walk's answer must still get its write-back.
//
// Every other writeBackSender test in this file (TestRelay_FirstSend_*) uses
// KindUserMessage, which never calls ResolveHumanDirectedBinding at all —
// outBinding is just ownBinding by construction. This is the only test that
// exercises the gate's true positive: the walk runs, returns sess's own
// binding (a slack-bound root has no parent, so the walk stops at sess), and
// the identity check must still pass. The gate rests on POINTER identity
// (relay.go's own comment: "the walk returns sess's own binding pointer when
// it stops at sess") — a refactor that made ResolveHumanDirectedBinding (or
// WalkAncestors) return a copy instead of the field pointer would break this
// silently: no crash, just a lost thread root on every human-directed
// envelope through a session's own channel.
func TestRelay_FirstSend_HumanDirectedOwnBinding_PatchesInputChannel(t *testing.T) {
	nc := connectNATS(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "root-hd",
			Namespace: "default",
			Annotations: map[string]string{
				// Needed to reach the InputChannel-only write-back branch
				// (relay.go patches OutputChannel when set, else InputChannel
				// only for a restart-fork session) — this session carries no
				// OutputChannel, only InputChannel, same shape as
				// TestRelay_FirstSend_RestartForkChild_PatchesInputChannel.
				spiceboxv1alpha1.AnnotationForkedFromThread: "T99:C-ROOT:root.0000",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			// No Spec.Parent: a root session, so ResolveHumanDirectedBinding's
			// walk stops at sess itself and returns sess's OWN binding pointer
			// — the exact case that makes outBinding == ownBinding true for a
			// human-directed envelope.
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "slack-eng", Kind: "slack",
				NATSSubjectPrefix: "ap.session.default.root-hd",
				External: map[string]string{
					"channel_id": "C-ROOT",
					// thread_ts intentionally absent — this is the card's
					// first send.
				},
			},
		},
	}
	cli := fakeClientWith(t, sess)

	sndr := &writeBackSender{external: map[string]string{
		"thread_ts":  "hd-root.3333",
		"channel_id": "C-ROOT",
	}}
	// subS, not s: KindInteractionRequest routes through SubChannelSenderFor
	// ("interaction"), never SenderFor.
	startRelay(t, nc, cli, &fixedResolver{subS: sndr})

	env := interactionRequestEnvelope(t, "root-hd")
	publishOut(t, nc, "default", "root-hd", "interaction_request", env)

	// Poll until the relay patches InputChannel (or timeout).
	var got spiceboxv1alpha1.AgentSession
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, cli.Get(context.Background(),
			client.ObjectKey{Namespace: "default", Name: "root-hd"}, &got))
		if got.Spec.InputChannel != nil && got.Spec.InputChannel.External["thread_ts"] != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	require.NotNil(t, got.Spec.InputChannel)
	assert.Equal(t, "hd-root.3333", got.Spec.InputChannel.External["thread_ts"],
		"a human-directed envelope through the session's own binding must still be write-back patched")
	assert.Equal(t, "C-ROOT", got.Spec.InputChannel.External["channel_id"])
}

// TestRelayNotesPendingApprovalRequests pins that notePendingPrompt records only
// KindInteractionRequest — the kind every re-surfaceable prompt rides. Applied
// and non-prompt kinds are ignored.
func TestRelayNotesPendingApprovalRequests(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	cases := []struct {
		name string
		kind channelevents.Kind
	}{
		{"interaction_applied is not noted", channelevents.KindInteractionApplied},
		{"notification is not noted", channelevents.KindNotification},
		{"enqueue ack is not noted", channelevents.KindEnqueueAck},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem := memory.NewLocal(inmem.NewBackend())
			raw, _ := json.Marshal(map[string]string{"requestID": "r1"})
			env := channelevents.Envelope{Kind: tc.kind, Payload: raw}
			notePendingPrompt(ctx, mem, "ns", "s", env) // helper under test
			got, err := parkedprompt.Outstanding(ctx, mem, memory.Scope{Kind: "session", ID: "ns/s"})
			require.NoError(t, err)
			assert.Empty(t, got, "only KindInteractionRequest is recorded")
		})
	}
}

// TestNotePendingPromptRecordsOnlyCachedCategories pins WHICH categories reach
// durable storage. ResurfaceCached is recorded; ResurfaceNone and unregistered
// categories are not — and critically neither is ResurfaceRegenerate, whose
// prompt carries a freshly-minted signed link that must never be persisted in
// replayable form (it is rebuilt from the session's phase instead).
func TestNotePendingPromptRecordsOnlyCachedCategories(t *testing.T) {
	snapshotInteractionCategories(t)
	channelinteractions.Register(channelinteractions.Category{
		Name: "cacheable_cat", Park: spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		Tone:     channelinteractions.ToneRoutine,
		Deciders: channelinteractions.DecideApprovers, Resurface: channelinteractions.ResurfaceCached})
	channelinteractions.Register(channelinteractions.Category{
		Name: "nocache_cat", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideRequester,
		Resurface: channelinteractions.ResurfaceNone})
	channelinteractions.Register(channelinteractions.Category{
		Name: "regen_cat", Park: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials,
		Tone:     channelinteractions.ToneRoutine,
		Deciders: channelinteractions.DecideRequester, Resurface: channelinteractions.ResurfaceRegenerate})

	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	mk := func(category, ref string, interruptible bool) channelevents.Envelope {
		pl := channelevents.InteractionRequestPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
			Category:        category, RequestRef: ref, Lead: "L", Interruptible: interruptible,
			Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers,
				Approvers: []channelevents.ExternalIdentity{{Kind: "fake", ExternalID: "U0ALICE"}}},
		}
		b, err := json.Marshal(pl)
		require.NoError(t, err)
		return channelevents.Envelope{Version: 1, Kind: channelevents.KindInteractionRequest,
			Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"}, Payload: b}
	}

	notePendingPrompt(ctx, mem, "default", "demo-session", mk("cacheable_cat", "req-1", true))
	notePendingPrompt(ctx, mem, "default", "demo-session", mk("nocache_cat", "req-2", false))
	notePendingPrompt(ctx, mem, "default", "demo-session", mk("never_registered", "req-3", false))
	notePendingPrompt(ctx, mem, "default", "demo-session", mk("regen_cat", "req-4", false))

	got, err := parkedprompt.Outstanding(ctx, mem, memory.Scope{Kind: "session", ID: "default/demo-session"})
	require.NoError(t, err)
	require.Len(t, got, 1, "only the ResurfaceCached registered category is recorded")
	assert.Equal(t, "req-1", got[0].RequestRef)
	assert.True(t, got[0].Interruptible, "interruptibility comes from the payload, not the kind")
	assert.Equal(t, "cacheable_cat", got[0].Category)
}

// TestNotePendingPromptDoesNotResurrectAResolvedPrompt covers the relay's leg of
// the cross-restart idempotency contract. The SAME prompt envelope can reach the
// relay again after its decision landed (the resurface path republishes one it
// read before the tombstone was written; the runner re-emits outstanding
// requests on restart), and the relay notes every OUT interaction_request.
// Re-noting must leave the tombstone standing: it is the only already-resolved
// verdict that outlives the process, so clearing it would let a second click
// re-run the bound handler and write a second grant.
func TestNotePendingPromptDoesNotResurrectAResolvedPrompt(t *testing.T) {
	snapshotInteractionCategories(t)
	channelinteractions.Register(channelinteractions.Category{
		Name: "cacheable_cat", Park: spiceboxv1alpha1.AgentSessionPhaseAwaitingDecision,
		Tone:     channelinteractions.ToneRoutine,
		Deciders: channelinteractions.DecideApprovers, Resurface: channelinteractions.ResurfaceCached})

	ctx := memory.WithSystemApproval(context.Background(), "test")
	mem := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/demo-session"}

	pl := channelevents.InteractionRequestPayload{
		AgentSessionRef: channelevents.SessionRef{Namespace: "default", Name: "demo-session"},
		Category:        "cacheable_cat", RequestRef: "req-1", Lead: "L",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceApprovers,
			Approvers: []channelevents.ExternalIdentity{{Kind: "fake", ExternalID: "U0ALICE"}}},
	}
	raw, err := json.Marshal(pl)
	require.NoError(t, err)
	env := channelevents.Envelope{Version: 1, Kind: channelevents.KindInteractionRequest,
		Session: channelevents.SessionRef{Namespace: "default", Name: "demo-session"}, Payload: raw}

	notePendingPrompt(ctx, mem, "default", "demo-session", env)
	require.NoError(t, parkedprompt.Resolve(ctx, mem, scope, "req-1"), "the decision lands")

	notePendingPrompt(ctx, mem, "default", "demo-session", env) // the republish

	outstanding, err := parkedprompt.Outstanding(ctx, mem, scope)
	require.NoError(t, err)
	assert.Empty(t, outstanding, "a decided prompt must not become re-surfaceable again")

	res, err := mem.Query(ctx, memory.Query{Scope: scope, Kinds: []string{parkedprompt.KindName}})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1, "the tombstone must remain: absent reads as never-decided")
	var stored parkedprompt.Content
	require.NoError(t, json.Unmarshal(res.Entries[0].Content, &stored))
	assert.True(t, stored.Resolved, "the durable already-resolved verdict must survive the republish")
}

// -----------------------------------------------------------------------------
// Stream-delta cost tests — head-of-line blocking of the primary path
// -----------------------------------------------------------------------------

// slowGetClient charges a fixed delay to every Get, standing in for client-go's
// default rate limiter on the uncached clients webd and channelsd hand the relay
// (5 QPS / burst 10 when no QPS override is set). It also counts the Gets so a
// failure message can say what the wall-clock was spent on.
type slowGetClient struct {
	client.Client
	delay time.Duration
	gets  atomic.Int64
}

func (c *slowGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets.Add(1)
	time.Sleep(c.delay)
	return c.Client.Get(ctx, key, obj, opts...)
}

// classGatedResolver models the production stream-delta chain: resolving a sink
// costs a second live API Get of the AgentClass, because
// channelkinds.ShowsAssistantStream reads spec.channels.showAssistantStream
// (pkg/channels/channelkinds/streaming.go, reached from
// browser.Host.StreamDeltaSinkFor and internal/cmd/channelsd/sender_resolver.go alike).
// A nil sink means the class has streaming OFF — the shipped default — so the
// Get is pure waste: the relay pays it and discards the delta.
type classGatedResolver struct {
	s   channelkinds.Sender
	cli client.Client

	// sinkMu guards sink so a test can flip the class's streaming flag while
	// the relay's delivery goroutine is resolving against it.
	sinkMu sync.Mutex
	sink   channelkinds.StreamDeltaSink

	sinkCalls atomic.Int64
}

// setSink stands in for an operator flipping spec.channels.showAssistantStream.
func (r *classGatedResolver) setSink(s channelkinds.StreamDeltaSink) {
	r.sinkMu.Lock()
	defer r.sinkMu.Unlock()
	r.sink = s
}

func (r *classGatedResolver) SenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	return r.s, nil
}

func (r *classGatedResolver) SubChannelSenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession, _ string) (channelkinds.Sender, error) {
	return r.s, nil
}

func (r *classGatedResolver) StreamDeltaSinkFor(ctx context.Context, sess *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	r.sinkCalls.Add(1)
	var class spiceboxv1alpha1.AgentClass
	if err := r.cli.Get(ctx, client.ObjectKey{Namespace: sess.Namespace, Name: sess.Spec.Class}, &class); err != nil {
		return nil, nil
	}
	r.sinkMu.Lock()
	defer r.sinkMu.Unlock()
	return r.sink, nil
}

// streamDeltaEnv builds one text_delta envelope for default/foo.
func streamDeltaEnv(t *testing.T, text string) channelevents.Envelope {
	t.Helper()
	return buildEnv(t, "foo", channelevents.KindAssistantStreamDelta,
		channelevents.AssistantStreamDeltaPayload{EventType: "text_delta", Text: text})
}

// TestRelay_StreamDeltasWithStreamingOff_DoNotBlockFinalMessage pins the
// CONSEQUENCE of per-token API lookups, not the Get count: nats.go dispatches
// one subscription's callbacks serially on a single goroutine, so every round
// trip a stream delta makes sits directly in front of the user's final reply,
// the approval prompts, and the plan updates behind it.
//
// The runner publishes one delta per SSE text_delta UNCONDITIONALLY
// (internal/cmd/runner/main.go's buildLoopOnStreamEvent gates only on channel
// attachment), so with showAssistantStream OFF — the shipped default — an
// unmemoized relay would pay a session Get plus a class Get per token and throw
// the token away. Sixty tokens are enough to bury the reply itself.
func TestRelay_StreamDeltasWithStreamingOff_DoNotBlockFinalMessage(t *testing.T) {
	const (
		deltas   = 60
		getDelay = 10 * time.Millisecond
		// An unmemoized relay costs deltas*2*getDelay = 1.2s before the final
		// message is even looked at. Memoizing the decline pays one session+class
		// Get pair for the first delta, then nothing, then one Get for the final
		// message: ~30ms. 400ms sits an order of magnitude either side.
		budget = 400 * time.Millisecond
	)

	nc := connectNATS(t)
	class := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"}}
	cli := &slowGetClient{Client: fakeClientWith(t, defaultSession(), class), delay: getDelay}

	final := &captureSender{}
	// sink nil ⇒ the class has showAssistantStream off; the relay must drop the
	// deltas — the only question is what it spends doing so.
	res := &classGatedResolver{s: final, cli: cli}
	startRelay(t, nc, cli, res)

	start := time.Now()
	for i := 0; i < deltas; i++ {
		publishOut(t, nc, "default", "foo", "assistant_stream_delta", streamDeltaEnv(t, "tok"))
	}
	publishOut(t, nc, "default", "foo", "user_message",
		buildEnv(t, "foo", channelevents.KindUserMessage,
			channelevents.OutboundUserMessagePayload{Text: "the reply the user is waiting for"}))

	require.True(t, waitUntil(t, 10*time.Second, func() bool { return final.count() == 1 }),
		"the final user_message must be delivered")
	elapsed := time.Since(start)

	assert.Less(t, elapsed, budget,
		"the user's final reply was queued behind %d stream deltas' API lookups (%d Gets, %s elapsed) — "+
			"deltas the class had streaming turned off for and that were discarded anyway",
		deltas, cli.gets.Load(), elapsed)
	assert.LessOrEqual(t, cli.gets.Load(), int64(4),
		"a run of deltas the kind declines must re-check the live class gate a bounded number of times, not once per token")
}

// TestRelay_StreamDeltaGate_PermittedSinkIsNeverCached is the fail-closed half
// of the decline memo. channelkinds.ShowsAssistantStream is a disclosure gate —
// the raw model narrative "must never leak to a channel ... unless an AgentClass
// explicitly turns it on" — so only the DECLINING answer may be remembered,
// whose staleness withholds. A class that permits streaming is re-asked on every
// delta, leaving no window in which a freshly-disabled flag keeps streaming.
func TestRelay_StreamDeltaGate_PermittedSinkIsNeverCached(t *testing.T) {
	const deltas = 5

	nc := connectNATS(t)
	class := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"}}
	cli := fakeClientWith(t, defaultSession(), class)

	sink := &captureSink{}
	res := &classGatedResolver{s: &captureSender{}, cli: cli, sink: sink}
	startRelay(t, nc, cli, res)

	for i := 0; i < deltas; i++ {
		publishOut(t, nc, "default", "foo", "assistant_stream_delta", streamDeltaEnv(t, "tok"))
	}

	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sink.count() == deltas }),
		"every delta must reach the sink when the class permits streaming")
	assert.Equal(t, int64(deltas), res.sinkCalls.Load(),
		"the live class gate must be consulted once per delta while streaming is permitted — never memoized")
}

func withStreamDeclineTTL(d time.Duration) func(*Relay) {
	return func(r *Relay) { r.StreamDeclineTTL = d }
}

// TestRelay_StreamDeltaGate_DeclineExpiresAndRechecks bounds the staleness of
// the decline memo in the only direction it can be stale: after the window
// lapses the relay re-asks, so turning showAssistantStream ON mid-reply starts
// rendering rather than staying dark for the rest of the session.
func TestRelay_StreamDeltaGate_DeclineExpiresAndRechecks(t *testing.T) {
	nc := connectNATS(t)
	class := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"}}
	cli := fakeClientWith(t, defaultSession(), class)

	sink := &captureSink{}
	res := &classGatedResolver{s: &captureSender{}, cli: cli}
	startRelay(t, nc, cli, res, withStreamDeclineTTL(20*time.Millisecond))

	publishOut(t, nc, "default", "foo", "assistant_stream_delta", streamDeltaEnv(t, "before"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return res.sinkCalls.Load() >= 1 }),
		"the first delta must resolve the gate live")
	assert.Equal(t, 0, sink.count(), "nothing renders while the class declines")

	// The operator flips spec.channels.showAssistantStream on.
	res.setSink(sink)
	time.Sleep(30 * time.Millisecond)

	publishOut(t, nc, "default", "foo", "assistant_stream_delta", streamDeltaEnv(t, "after"))
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return sink.count() == 1 }),
		"once the decline window lapses the relay must re-ask the gate and start rendering")
}

// TestRelay_StreamDeltaGate_DeclinedDeltasStillReArmTheWatchdog guards the one
// thing a declined delta still contributes. Tokens flowing IS forward progress,
// and folding that in is what stops a long, tool-free reply from tripping the
// silence watchdog's "agent appears stuck" warning. The fold runs ahead of sink
// resolution, so short-circuiting the API lookups must not take it with them.
func TestRelay_StreamDeltaGate_DeclinedDeltasStillReArmTheWatchdog(t *testing.T) {
	const deltas = 10

	nc := connectNATS(t)
	class := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"}}
	cli := fakeClientWith(t, defaultSession(), class)

	var mu sync.Mutex
	var events []watchdog.Event
	res := &classGatedResolver{s: &captureSender{}, cli: cli} // sink nil ⇒ declines
	startRelay(t, nc, cli, res, withApplyStatusEvent(
		func(_ context.Context, _, _ string, ev watchdog.Event) watchdog.ApplyResult {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
			return watchdog.ApplyResult{Forward: true}
		}))

	for i := 0; i < deltas; i++ {
		publishOut(t, nc, "default", "foo", "assistant_stream_delta", streamDeltaEnv(t, "tok"))
	}

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(events)
	}
	require.True(t, waitUntil(t, 2*time.Second, func() bool { return count() == deltas }),
		"every declined delta must still re-arm the silence watchdog")

	mu.Lock()
	defer mu.Unlock()
	for i, ev := range events {
		assert.Equal(t, watchdog.EvToolActivity, ev.Kind, "event %d folds in as a progress re-arm", i)
	}
}

// lastSubChannelRef returns the session IDENTITY of the most recent session
// handed to SubChannelSenderFor, and whether it was ever called.
func (r *fixedResolver) lastSubChannelRef() (spiceboxv1alpha1.NamespacedRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.subChannelRefs) == 0 {
		return spiceboxv1alpha1.NamespacedRef{}, false
	}
	return r.subChannelRefs[len(r.subChannelRefs)-1], true
}
