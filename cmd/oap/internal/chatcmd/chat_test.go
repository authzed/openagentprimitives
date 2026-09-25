package chatcmd

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	local "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink/viewlink"
)

// TestNewChatOutboundRelay_ScopesToTheChatSession guards the pre-Get half of
// the cross-session scope. outbound.Relay subscribes the cluster-wide
// "ap.session.*.*.out.>" for every consumer and the CLI's NATS grant is "ap.>",
// so without an Accept predicate this relay is handed — and pays a rate-limited
// AgentSession Get for — every Slack-, cron- and browser-driven session in the
// cluster, on the single goroutine the subscription's callbacks are serialized
// on, directly in front of the chat user's own reply.
//
// Accept must come from the Host itself (host.Accepts) so it and the resolver
// methods answer from one (namespace, session) pair and can never disagree.
func TestNewChatOutboundRelay_ScopesToTheChatSession(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent-a1b2c3-chan"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "local"},
	}
	host, err := local.NewHost(local.HostConfig{
		Deps:        channelkinds.Deps{Channel: ch},
		Sink:        &local.RecordingSink{},
		User:        chatExternalIdentity(clilogin.LocalUser()),
		Namespace:   "default",
		SessionName: "demo-agent-a1b2c3",
	})
	require.NoError(t, err, "build local host")

	// nil NC / K8s: nothing is started here, only the wiring is inspected.
	relay := newChatOutboundRelay(nil, nil, host)
	require.NotNil(t, relay.Accept,
		"the chat relay must carry an Accept predicate; without one every session in the cluster is delivered into this TUI")
	assert.True(t, relay.Accept("default", "demo-agent-a1b2c3"),
		"the chat's own session must be accepted")
	assert.False(t, relay.Accept("default", "someone-elses-session"),
		"another session's outbound envelopes must be rejected before the AgentSession is even loaded")
	assert.False(t, relay.Accept("other-ns", "demo-agent-a1b2c3"),
		"the same session name in another namespace must be rejected")
}

func TestBuildChatChannel_HasLocalKindAndOwnerRef(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent", UID: "ac-uid"},
	}
	ch := buildChatChannel("default", "demo-agent-a1b2c3", "demo-agent", ac)
	assert.Equal(t, "local", ch.Spec.Kind)
	assert.Equal(t, "demo-agent", ch.Spec.AgentClass)
	require.Len(t, ch.OwnerReferences, 1, "ephemeral Channel is owned by the AgentClass for cleanup")
	assert.Equal(t, "AgentClass", ch.OwnerReferences[0].Kind)
}

func TestBuildChatSession_HasCorrelationLabelsAndChannelBinding(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent-a1b2c3-chan"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "local"},
	}
	sess := buildChatSession("default", "demo-agent-a1b2c3", "demo-agent", ch, "prompt text", "local-user", "")
	// The pipeline correlates inbound→session via channel.name +
	// sha256(channelKey) labels; oap must stamp them so a typed message
	// routes to this pre-created session.
	assert.Equal(t, ch.Name, sess.Labels[spiceboxv1alpha1.LabelChannelName])
	wantKeyHash := channelkey.LabelValue(local.ChannelKey("default", ch.Name))
	assert.Equal(t, wantKeyHash, sess.Labels[spiceboxv1alpha1.LabelChannelKey])
	assert.Equal(t, local.KindName, sess.Labels[spiceboxv1alpha1.LabelChannelKind])
	require.NotNil(t, sess.Spec.InputChannel)
	assert.Equal(t, ch.Name, sess.Spec.InputChannel.Name)
	assert.Equal(t, "local", sess.Spec.InputChannel.Kind)
	assert.Equal(t, local.ChannelKey("default", ch.Name), sess.Spec.InputChannel.Key)
}

// TestBuildChatSession_ChannelKeyLabelMatchesPipelineLookup is the
// regression guard for bug #5. The channelsd inbound pipeline's Deliver
// correlates an inbound message to its pre-created AgentSession by
// listing sessions whose LabelChannelKey equals
// channelkey.LabelValue(ev.ChannelKey). If buildChatSession
// stamps that label with any other value, the lookup misses, the
// pipeline falls through to its new-session path, and the follow-up
// message lands on a bogus session that the per-session memory token
// does not authorize → 403.
//
// This test calls BOTH sides of the correlation — the value
// buildChatSession stamps and the value the pipeline computes for the
// exact same channel key the local listener will send — and asserts
// they are equal. A divergence between the two functions fails here.
func TestBuildChatSession_ChannelKeyLabelMatchesPipelineLookup(t *testing.T) {
	const (
		ns          = "default"
		sessionName = "demo-agent-a1b2c3"
	)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sessionName + "-chan"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "local"},
	}
	sess := buildChatSession(ns, sessionName, "demo-agent", ch, "prompt text", "local-user", "")

	// The channel key the local listener stamps on every InboundEvent is
	// local.ChannelKey(namespace, channelName) — see local.Host /
	// localListener. The pipeline's Deliver hashes ev.ChannelKey with
	// channelkey.LabelValue to build its correlation lookup.
	listenerKey := local.ChannelKey(ns, ch.Name)
	pipelineLookupValue := channelkey.LabelValue(listenerKey)

	require.Equal(t, listenerKey, sess.Spec.InputChannel.Key,
		"the InputChannel.Key buildChatSession sets must equal the key the local listener will send as ev.ChannelKey")
	assert.Equal(t, pipelineLookupValue, sess.Labels[spiceboxv1alpha1.LabelChannelKey],
		"the LabelChannelKey buildChatSession stamps must equal channelkey.LabelValue(channelKey) — "+
			"otherwise the pipeline's correlation lookup misses and the follow-up routes to a bogus session (bug #5)")
}

// TestBuildChatSession_StampsStartedByExternalIDAnnotation is the
// regression guard for the started_by annotation: channelsd writes
// AnnotationStartedByExternalID on every session it creates, but `oap
// agent chat` pre-creates the session itself, so buildChatSession must
// stamp it. Without it, approverIsStartedBy (pkg/channels/channelsd/pipeline/
// decision.go) rejects every approval click on a chat session.
func TestBuildChatSession_StampsStartedByExternalIDAnnotation(t *testing.T) {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent-a1b2c3-chan"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "local"},
	}
	const wantExternalID = "bob@example.com"
	const wantEmail = "bob@example.com"
	sess := buildChatSession("default", "demo-agent-a1b2c3", "demo-agent", ch, "prompt text", wantExternalID, wantEmail)
	got := sess.Annotations[spiceboxv1alpha1.AnnotationStartedByExternalID]
	require.NotEmpty(t, got,
		"buildChatSession must stamp the started-by annotation so approval clicks pass approverIsStartedBy")
	assert.Equal(t, wantExternalID, got,
		"the annotation must carry the caller-supplied external ID — the same value an approval click is matched against")
	// The verified email must be stamped too, or identity_choice's
	// DecideRequester check can't canonicalize the CLI initiator and every
	// identity decision on a chat session is rejected fail-closed.
	assert.Equal(t, wantEmail, sess.Annotations[spiceboxv1alpha1.AnnotationStartedByEmail],
		"buildChatSession must stamp the verified email for identity_choice's requester canonicalization")
}

// TestChatSessionStarter_Submit_FollowUpRequestsChannelsdExactlyOnce
// verifies that a SUBSEQUENT message (the non-first submit) reaches
// channelsd via the NATS view_message request-reply exactly once. This is
// the counterpart to the double-injection fix: the first message reaches
// the agent only as spec.prompt.inline (startSession no longer also calls
// SubmitUserMessage), while every later message is submitted through
// host.Listener().SubmitUserMessage → channelkinds.RequestViewMessage →
// deps.NATSRequest — and does so just once, not twice.
//
// Replaces the former
// TestChatSessionStarter_Submit_FollowUpRoutesThroughPipelineExactlyOnce
// (and its recordingInbound fake), which asserted routing through
// Deps.Inbound. That path was already dead by the time this task started:
// pkg/channels/channelkinds/local's SubmitUserMessage (refactored to call
// channelkinds.RequestViewMessage) stopped reading deps.Inbound entirely, so
// the old test's recordingInbound fake was never invoked and its
// require.Eventually(... inb.count() == 1 ...) timed out red on every run.
// This version drives the same follow-up-submit path but records
// deps.NATSRequest calls instead, which is what SubmitUserMessage actually
// calls now.
func TestChatSessionStarter_Submit_FollowUpRequestsChannelsdExactlyOnce(t *testing.T) {
	var calls int32
	var mu sync.Mutex
	var gotSubject string
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent-a1b2c3-chan"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "local"},
	}
	host, err := local.NewHost(local.HostConfig{
		Deps: channelkinds.Deps{
			Channel: ch,
			NATSRequest: func(subject string, _ []byte, _ time.Duration) ([]byte, error) {
				atomic.AddInt32(&calls, 1)
				mu.Lock()
				gotSubject = subject
				mu.Unlock()
				return json.Marshal(channelevents.ViewMessageResultPayload{
					Outcome: channelkinds.OutcomeRouted.String(),
				})
			},
		},
		Sink: &local.RecordingSink{},
		// Use the local fallback identity (no IdP in the test environment).
		User:        chatExternalIdentity(clilogin.LocalUser()),
		Namespace:   "default",
		SessionName: "demo-agent-a1b2c3",
	})
	require.NoError(t, err, "build local host")

	// started=true + host set models the post-first-message state: the
	// first submit already created the session, so this call takes the
	// follow-up branch.
	s := &chatSessionStarter{started: true, host: host}
	p := tea.NewProgram(newChatModel("demo-agent", "demo-agent-a1b2c3", true))
	s.setProgram(p)

	s.submit("follow-up message")

	require.Eventually(t, func() bool { return atomic.LoadInt32(&calls) == 1 }, 2*time.Second, 5*time.Millisecond,
		"a follow-up submit must reach channelsd via NATS request-reply exactly once")
	// Give any erroneous extra request a chance to land, then re-assert.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"a follow-up message must be requested exactly once — never double-sent")
	mu.Lock()
	gotSubjectSnapshot := gotSubject
	mu.Unlock()
	assert.Equal(t, "ap.session.default.demo-agent-a1b2c3.in.view_message", gotSubjectSnapshot)
}

func TestStdinIsInteractive_DetectsPipe(t *testing.T) {
	// A non-character-device stdin (a pipe) is non-interactive.
	r, w, err := osPipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	assert.False(t, apcmd.StdinIsInteractive(r), "a pipe is not a TTY")
}

func osPipe() (*os.File, *os.File, error) { return os.Pipe() }

func newAgentChatScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, fn := range []func(*runtime.Scheme) error{corev1.AddToScheme, spiceboxv1alpha1.AddToScheme} {
		require.NoError(t, fn(s), "AddToScheme")
	}
	return s
}

// TestSessionFailureDetail_PrefersConditionReasonAndMessage covers the
// helper the phase watcher uses to turn a Failed AgentSession into an
// operator-facing reason + message.
func TestSessionFailureDetail_PrefersConditionReasonAndMessage(t *testing.T) {
	cases := []struct {
		name        string
		status      spiceboxv1alpha1.AgentSessionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name: "Failed condition present: reason+message come from the condition",
			status: spiceboxv1alpha1.AgentSessionStatus{
				FailureReason: "RunnerCrashed",
				Conditions: []metav1.Condition{{
					Type:    spiceboxv1alpha1.AgentSessionConditionFailed,
					Status:  metav1.ConditionTrue,
					Reason:  "RunnerCrashed",
					Message: "runner restarted 5 times",
				}},
			},
			wantReason:  "RunnerCrashed",
			wantMessage: "runner restarted 5 times",
		},
		{
			name: "no condition: falls back to Status.FailureReason, empty message",
			status: spiceboxv1alpha1.AgentSessionStatus{
				FailureReason: "BudgetExhausted",
			},
			wantReason:  "BudgetExhausted",
			wantMessage: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &spiceboxv1alpha1.AgentSession{Status: tc.status}
			reason, message := sessionFailureDetail(s)
			assert.Equal(t, tc.wantReason, reason)
			assert.Equal(t, tc.wantMessage, message)
		})
	}
}

// TestBuildWiredChatModel_HasSubmitAndDecideWired is the regression
// guard for the value-copy wiring bug: chatModel is a value type, so
// tea.NewProgram(model) boxes a COPY — any submit/decideInteraction assigned
// to the local model AFTER tea.NewProgram never reaches the program's
// copy, leaving the program's callbacks nil and "type a message →
// nothing happens". buildWiredChatModel must return a model whose
// submit/decideInteraction are ALREADY non-nil so the value handed to
// tea.NewProgram carries them.
func TestBuildWiredChatModel_HasSubmitAndDecideWired(t *testing.T) {
	starter := &chatSessionStarter{}
	model := buildWiredChatModel("demo-agent", "demo-agent-a1b2c3", true, starter)

	require.NotNil(t, model.submit, "submit must be wired before tea.NewProgram copies the model")
	require.NotNil(t, model.decideInteraction, "decideInteraction must be wired before tea.NewProgram copies the model")

	// The callbacks are method values bound to the *chatSessionStarter
	// POINTER, so a starter.program backfill done AFTER buildWiredChatModel
	// (mirroring runAgentChat's ordering) is visible to them. Backfill,
	// then prove the model's submit reaches the same program: submit's
	// first claim emits msgSessionStarting via s.send → program.Send.
	p := tea.NewProgram(model)
	starter.setProgram(p)
	assert.Same(t, p, starter.prog(),
		"starter.program backfilled after buildWiredChatModel is visible to the wired callbacks")
}

// TestChatSessionStarter_Prog_IsRaceFreeAndDefensive covers the
// program accessor: prog returns nil before setProgram (so send is a
// safe no-op rather than a nil-deref) and the backfilled value after.
func TestChatSessionStarter_Prog_IsRaceFreeAndDefensive(t *testing.T) {
	s := &chatSessionStarter{}
	assert.Nil(t, s.prog(), "program is nil before setProgram backfill")
	// send before backfill must not panic (defensive no-op).
	assert.NotPanics(t, func() { s.send(msgSessionStarting{}) },
		"send before the program is wired is a no-op, not a panic")

	p := tea.NewProgram(newChatModel("a", "b", true))
	s.setProgram(p)
	assert.Same(t, p, s.prog(), "prog returns the backfilled program")
}

// TestChatSessionStarter_ClaimFirst_OnlyFirstSubmitWins verifies the
// first-vs-subsequent submit decision: exactly one caller sees
// first==true, and a fast concurrent double-submit cannot create two
// sessions.
func TestChatSessionStarter_ClaimFirst_OnlyFirstSubmitWins(t *testing.T) {
	t.Run("sequential: first call wins, later calls do not", func(t *testing.T) {
		s := &chatSessionStarter{}
		first1, _ := s.claimFirst()
		assert.True(t, first1, "the first submit claims the session")
		first2, _ := s.claimFirst()
		assert.False(t, first2, "the second submit does not re-claim")
		first3, _ := s.claimFirst()
		assert.False(t, first3, "every later submit routes instead of claiming")
	})

	t.Run("concurrent double-submit: exactly one caller claims first", func(t *testing.T) {
		s := &chatSessionStarter{}
		const n = 32
		results := make(chan bool, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				first, _ := s.claimFirst()
				results <- first
			}()
		}
		wg.Wait()
		close(results)
		wins := 0
		for first := range results {
			if first {
				wins++
			}
		}
		assert.Equal(t, 1, wins, "a fast double-submit must not create two sessions")
	})
}

// TestChatSessionStarter_Teardown_SkipsSessionDeleteWhenNotCreated
// verifies the deferred teardown deletes the AgentSession only when one
// was actually created — the user may quit before typing anything.
func TestChatSessionStarter_Teardown_SkipsSessionDeleteWhenNotCreated(t *testing.T) {
	scheme := newAgentChatScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s1"},
	}

	t.Run("no session created: teardown is a no-op, session absent stays absent", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).Build()
		s := &chatSessionStarter{b: &kube.Bundle{Controller: c, Namespace: "ns"}}
		// started never flipped, sess nil — teardown must not panic and
		// must not attempt a delete.
		s.teardown()
		var got spiceboxv1alpha1.AgentSession
		err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "s1"}, &got)
		assert.True(t, apierrors.IsNotFound(err), "no session was ever created")
	})

	t.Run("session created: teardown deletes it", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess.DeepCopy()).Build()
		s := &chatSessionStarter{
			b:    &kube.Bundle{Controller: c, Namespace: "ns"},
			sess: sess.DeepCopy(),
		}
		s.teardown()
		var got spiceboxv1alpha1.AgentSession
		err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "s1"}, &got)
		assert.True(t, apierrors.IsNotFound(err), "teardown deleted the created session")
	})
}

// TestWaitForSessionReady_ParkedAwaitingUserPhase_IsReady pins the CLI's copy
// of the readiness gate to the same contract as webd's: a session that parks
// awaiting the user (AwaitingCredentials / AwaitingIdentityChoice) has, by
// design, no runner and thus no <name>-memory-token Secret, yet the TUI must
// proceed to render the prompt rather than block or error. Erroring here makes
// startup return, which fires the deferred teardown and DELETES the parked
// session — the exact "prompt never shows, then it's gone" bug, on `oap agent
// chat`. The already-cancelled context proves the phase is consulted before the
// context (wait.Until polls fn before selecting on Done).
func TestWaitForSessionReady_ParkedAwaitingUserPhase_IsReady(t *testing.T) {
	scheme := newAgentChatScheme(t)
	cases := []struct {
		name  string
		phase string
	}{
		{name: "AwaitingCredentials is ready", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials},
		{name: "AwaitingIdentityChoice is ready", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingIdentityChoice},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s1"},
			}
			sess.Status.Phase = tc.phase
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()
			b := &kube.Bundle{Controller: c, Namespace: "ns"}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			require.NoError(t, waitForSessionReady(ctx, b, "s1"),
				"a started-but-parked session must be ready even with no memory-token Secret")
		})
	}
}

func TestWaitForSessionReady_PendingNoToken_StillNotReady(t *testing.T) {
	// The phase-aware readiness must NOT short-circuit a genuinely not-yet-
	// started session: Pending with no token is still "wait", proven by the
	// cancelled context surfacing rather than a false ready.
	scheme := newAgentChatScheme(t)
	sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "s1"}}
	sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhasePending
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()
	b := &kube.Bundle{Controller: c, Namespace: "ns"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.Error(t, waitForSessionReady(ctx, b, "s1"), "a Pending session with no token is not ready")
}

// TestChatExternalIdentity_Shapes covers both output shapes of
// chatExternalIdentity: verified IdP identity → kind=idp, email used as
// ExternalID; unverified fallback → kind=local, OS-username ExternalID.
func TestChatExternalIdentity_Shapes(t *testing.T) {
	t.Run("verified (idp login): kind=idp, Email=ExternalID=email", func(t *testing.T) {
		p := identity.VerifiedEmail("alice@example.com", "Alice")
		ext := chatExternalIdentity(p)
		assert.Equal(t, "idp", ext.Kind.String())
		assert.Equal(t, "alice@example.com", ext.Email.String())
		assert.Equal(t, "alice@example.com", ext.ExternalID.String())
		assert.Empty(t, ext.TeamScope)
	})

	t.Run("unverified (local fallback): kind=local, ExternalID=OS username, no email", func(t *testing.T) {
		// clilogin.LocalUser() returns the same principal clilogin.EnsureIdentity returns
		// when no ClusterIdentityProvider is configured (legacy path).
		p := clilogin.LocalUser()
		ext := chatExternalIdentity(p)
		assert.Equal(t, "local", ext.Kind.String())
		assert.Equal(t, p.ExternalID(), ext.ExternalID, "ExternalID must equal the OS username in the Principal")
		assert.Empty(t, ext.Email, "the unverified fallback carries no email")
		assert.Empty(t, ext.TeamScope)
	})
}

// TestChatExternalIdentity_CanonicalEquality is the invariant test: the
// canonical the pipeline derives from chatExternalIdentity(p) must equal
// p.Canonical() for BOTH branches. The pipeline does
// identity.FromExternal(ext.Kind, ext.TeamScope, ext.ExternalID, ext.Email).Canonical();
// if that diverges from p.Canonical() the started_by write and the pipeline
// check disagree — causing a "session not found / permission denied" on
// the first follow-up message.
func TestChatExternalIdentity_CanonicalEquality(t *testing.T) {
	cases := []struct {
		name string
		p    identity.Principal
	}{
		{
			name: "verified idp principal: canonical = base64(email) for both sides",
			p:    identity.VerifiedEmail("alice@example.com", "Alice"),
		},
		{
			name: "local fallback principal: canonical = base64(local::username) for both sides",
			p:    clilogin.LocalUser(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext := chatExternalIdentity(tc.p)
			pipelinePrincipal := identity.FromExternal(identity.Kind(ext.Kind), identity.TeamScope(ext.TeamScope), identity.RawExternalID(ext.ExternalID), identity.Email(ext.Email))
			if ext.Email == "" {
				// The local fallback branch is email-less — its intended
				// subject is the synthetic kind:teamScope:externalID, so it
				// opts in (the idp branch carries a verified email and must not).
				pipelinePrincipal = pipelinePrincipal.AllowSynthetic()
			}
			pipelineCanonical, err := pipelinePrincipal.Canonical()
			require.NoError(t, err)
			wantCanonical, err := tc.p.Canonical()
			require.NoError(t, err)
			assert.Equal(t, wantCanonical, pipelineCanonical,
				"pipeline canonical (from ExternalIdentity) must equal p.Canonical() — "+
					"any divergence breaks started_by ↔ CheckInteract agreement")
		})
	}
}

// --- audit-forgery regression guard ---------------------------------------
//
// `oap agent chat` used to read the per-session Ed25519 audit-signing seed
// out of the <name>-memory-token Secret and sign inbound turns as the
// session publisher — meaning a laptop-resident process could forge a
// session's tamper-evident audit chain. These two tests are a durable
// guarantee that `oap` can never sign again.

// TestAgentChatNeverReadsTheAuditSigningKey guards against this package
// regaining a reference to the Secret's `audit-signing-key` data key. It reads
// the sources as text rather than exercising behavior, so a reintroduction is
// caught even if no test happens to exercise that path.
//
// It scans every non-test file in the package rather than naming one: the chat
// command is split across several, and a guard pinned to a single filename
// stops guarding the moment the code it watches moves next door.
func TestAgentChatNeverReadsTheAuditSigningKey(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "read the package directory")

	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		require.NoError(t, err, "read %s", name)
		assert.NotContains(t, string(b), "audit-signing-key",
			"%s: oap runs on a laptop; it must not be able to forge a session's audit chain", name)
		scanned++
	}
	require.NotZero(t, scanned, "the scan must have found the package's sources")
}

// TestChatMemoryIsGone asserts chat_memory.go — the pipeline.Memory
// implementation that wrapped writes in a provenance.SigningMemory — is
// deleted, not merely unreferenced. An unreferenced-but-present file is one
// import away from being wired back in.
//
// Both this package and the cmd/oap root are checked: the chat command lives
// here, but the file's original home was the root, and a reintroduction could
// land in either.
func TestChatMemoryIsGone(t *testing.T) {
	for _, path := range []string{"chat_memory.go", "../../chat_memory.go"} {
		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err), "%s must be deleted, not merely unused", path)
	}
}

// TestHostDepsCarriesEveryMinter pins the three minter assignments in the
// Deps the TUI's local host is built from. Each is best-effort and each has a
// separate builder, so an omitted assignment produces no error anywhere: the
// sender simply reports that its page is unconfigured, on a surface only a
// human running `oap agent chat` ever sees.
//
// Distinct instances per field, asserted by identity, because the three
// minters are handed to hostDeps in positional order and a transposition is
// the mistake this shape invites.
func TestHostDepsCarriesEveryMinter(t *testing.T) {
	s := &chatSessionStarter{
		ch: &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-channel"}},
		b:  &kube.Bundle{Controller: fake.NewClientBuilder().WithScheme(newAgentChatScheme(t)).Build()},
	}
	artifact := &viewlink.Minter{}
	sessionView := &tuiSessionViewMinter{webdBaseURL: "https://webd.example.test"}
	agentUI := &tuiAgentUIMinter{webdBaseURL: "https://webd.example.test"}

	deps := s.hostDeps(artifact, sessionView, agentUI)

	for _, f := range []struct {
		field string
		want  any
		got   any
	}{
		{"ArtifactViewMinter", artifact, deps.ArtifactViewMinter},
		{"SessionViewMinter", sessionView, deps.SessionViewMinter},
		{"AgentUIMinter", agentUI, deps.AgentUIMinter},
	} {
		// NotNil first: assert.Same against a nil interface reports only
		// "Both arguments must be pointers", which names neither the field
		// nor what its absence costs.
		if assert.NotNil(t, f.got, "Deps.%s is nil: that minter never reached the host", f.field) {
			assert.Same(t, f.want, f.got, "Deps.%s must carry the instance built at startup", f.field)
		}
	}
	assert.Same(t, s.ch, deps.Channel, "the host must serve the chat session's own Channel")
	assert.NotNil(t, deps.K8sClient, "senders read the cluster through Deps, not a package global")
}

// TestHostDepsLeavesAnAbsentMinterATrueNilInterface: every builder can decline
// (webd unreachable), and the Deps fields are INTERFACES. A decline must
// arrive as a true nil interface so the senders' own `!= nil` guards hold —
// a typed-nil pointer would pass every guard and panic on the first mint.
func TestHostDepsLeavesAnAbsentMinterATrueNilInterface(t *testing.T) {
	s := &chatSessionStarter{
		ch: &spiceboxv1alpha1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-channel"}},
		b:  &kube.Bundle{Controller: fake.NewClientBuilder().WithScheme(newAgentChatScheme(t)).Build()},
	}

	deps := s.hostDeps(nil, nil, nil)

	assert.Nil(t, deps.ArtifactViewMinter)
	assert.Nil(t, deps.SessionViewMinter)
	assert.Nil(t, deps.AgentUIMinter)
}
