package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
)

// --- test fakes ------------------------------------------------------------

// fakeDeps is a minimal chat.Deps for registry tests: only K8s()/Logger()
// are exercised when the injected sessionBuilderFunc never touches
// NATS()/Authz()/OperatorURL() (which is the point of the builder seam —
// registry lifecycle tests never need a real NATS/SpiceDB/operator).
type fakeDeps struct {
	k8s client.Client
	// nats, when set, is returned by NATS() so a test can start the Registry's
	// real process-wide outbound relay (startRelay) against an embedded server.
	// Zero value (nil) keeps every other test's builder-seam behavior, which
	// never reaches NATS().
	nats              *nats.Conn
	operatorURL       string
	memoryToken       string
	minter            channelkinds.ArtifactViewMinter
	sessionViewMinter channelkinds.SessionViewMinter
	// authz, when set, is returned by Authz() so a test can drive
	// browsersession.Create past (or into a failure at) its started_by SpiceDB
	// write. Zero value (nil) keeps the pre-existing behavior for every test
	// whose injected builder never touches Authz.
	authz pipeline.Authz
	// logger, when its sink is set, overrides the default Discard logger so a
	// test can assert on what was logged (e.g. a surfaced rollback-delete
	// failure). Zero value (nil sink) keeps the silent Discard behavior every
	// other test relies on.
	logger logr.Logger
	// checkInteract, when set, backs CheckInteract() so a test can script
	// allow/deny/error per (ns, name, subject) call. A nil func defaults to
	// "always allow" — the multiplayer-equivalent of the old owner-only
	// behavior most existing tests rely on without caring about
	// authorization; only a test that cares about standing sets this.
	checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
}

func (d *fakeDeps) K8s() client.Client    { return d.k8s }
func (d *fakeDeps) NATS() *nats.Conn      { return d.nats }
func (d *fakeDeps) Authz() pipeline.Authz { return d.authz }
func (d *fakeDeps) OperatorURL() string   { return d.operatorURL }
func (d *fakeDeps) MemoryToken() string   { return d.memoryToken }
func (d *fakeDeps) TrustedOrigin() string { return "https://trusted.example" }
func (d *fakeDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	if d.checkInteract != nil {
		return d.checkInteract(ctx, ns, name, subject)
	}
	return true, nil
}
func (d *fakeDeps) ArtifactViewMinter() channelkinds.ArtifactViewMinter {
	return d.minter
}
func (d *fakeDeps) SessionViewMinter() channelkinds.SessionViewMinter {
	return d.sessionViewMinter
}
func (d *fakeDeps) Logger() logr.Logger {
	if d.logger.GetSink() != nil {
		return d.logger
	}
	return logr.Discard()
}

func newFakeK8sClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// newFakeInterceptedK8sClient is newFakeK8sClient with interceptor funcs so a
// test can inject Get/Delete/… failures — e.g. to drive a create's
// rollback-cleanup path and assert the failed delete is surfaced.
func newFakeInterceptedK8sClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func validAgentClass(name string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: newChatSessionNamespace},
	}
	ac.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.AgentClassConditionValid, Status: metav1.ConditionTrue, Reason: "Valid"},
	}
	return ac
}

// fakeListener is a scriptable sessionListener.
type fakeListener struct {
	mu            sync.Mutex
	decision      channelkinds.InboundDecision
	err           error
	calls         []string                        // texts SubmitUserMessage was called with
	callExts      []channelkinds.ExternalIdentity // the ext SubmitUserMessage was called with, index-paired with calls
	interruptErr  error
	interrupts    []string                        // requestIDs SubmitInterrupt was called with
	interruptExts []channelkinds.ExternalIdentity // the ext SubmitInterrupt was called with, index-paired with interrupts
	decisionErr   error
	decisions     []decisionCall // (category, requestRef, actionID) tuples SubmitDecision was called with
	resurfaceErr  error
	resurfaces    int // times SubmitResurface was called
}

// decisionCall records one fakeListener.SubmitDecision invocation, INCLUDING
// the ext identity it was called with — the C1 regression bar: a test must be
// able to see which subject's identity reached the Decider, not just that a
// decision was submitted.
type decisionCall struct {
	ext        channelkinds.ExternalIdentity
	category   string
	requestRef string
	actionID   string
}

func (f *fakeListener) SubmitUserMessage(_ context.Context, ext channelkinds.ExternalIdentity, text, _ string) (channelkinds.InboundDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, text)
	f.callExts = append(f.callExts, ext)
	return f.decision, f.err
}

func (f *fakeListener) SubmitInterrupt(_ context.Context, ext channelkinds.ExternalIdentity, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts = append(f.interrupts, requestID)
	f.interruptExts = append(f.interruptExts, ext)
	return f.interruptErr
}

func (f *fakeListener) SubmitDecision(_ context.Context, ext channelkinds.ExternalIdentity, category, requestRef, actionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decisions = append(f.decisions, decisionCall{ext: ext, category: category, requestRef: requestRef, actionID: actionID})
	return f.decisionErr
}

func (f *fakeListener) SubmitResurface(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resurfaces++
	return f.resurfaceErr
}

func (f *fakeListener) resurfaceCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resurfaces
}

// testSink is a fake emitter recording every Emit + whether Close was called.
type testSink struct {
	mu     sync.Mutex
	events []any
	closed bool
}

func (s *testSink) Emit(msg any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, msg)
}

func (s *testSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func (s *testSink) snapshot() ([]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.events...), s.closed
}

// fakeBuilder returns a sessionBuilderFunc that never touches k8s/NATS/
// SpiceDB — it builds a bare sessionEntry wrapping a fakeListener, and
// records every onTerminal callback + stop/deleteK8s invocation so tests can
// assert on registry-level lifecycle behavior in isolation.
type fakeBuilder struct {
	mu          sync.Mutex
	buildErr    error
	built       []string // session names successfully built
	stopped     []string
	deletedK8s  []string
	onTerminals map[string]func(terminalNotice) // captured per session, for tests that want to fire it manually
	listener    *fakeListener

	// beforeReturn, when set, is invoked synchronously just before build
	// returns its entry — the seam
	// TestRegistry_AdoptSession_ClosedDuringWiring_StopsInProcessButPreservesCR
	// uses to simulate a Shutdown that lands while an Adopt is still in
	// flight, deterministically (no goroutine/timing race needed).
	beforeReturn func()
}

func newFakeBuilder() *fakeBuilder {
	return &fakeBuilder{onTerminals: map[string]func(terminalNotice){}, listener: &fakeListener{}}
}

func (b *fakeBuilder) build(
	_ context.Context, _ Deps, key sessionKey, subject string, onTerminal func(terminalNotice),
) (*sessionEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buildErr != nil {
		return nil, b.buildErr
	}
	b.built = append(b.built, key.Name)
	b.onTerminals[key.Name] = onTerminal
	entry := newSessionEntry(key.Namespace, key.Name, subject)
	entry.listener = b.listener
	entry.stop = func() {
		b.mu.Lock()
		b.stopped = append(b.stopped, key.Name)
		b.mu.Unlock()
	}
	entry.deleteK8s = func(context.Context) {
		b.mu.Lock()
		b.deletedK8s = append(b.deletedK8s, key.Name)
		b.mu.Unlock()
	}
	if b.beforeReturn != nil {
		b.beforeReturn()
	}
	return entry, nil
}

// putTestEntry publishes entry into reg's table directly, bypassing the
// reserve/wire/publish ladder. It is for fixtures that need a live entry
// present and are testing something else entirely (fan-out scoping, frame
// shapes); anything asserting on the ladder itself must go through
// adoptNewTestSession so it exercises the real bookkeeping.
//
// The slot's cap subject is the entry's own owner, which is what the adopt
// path records for a session its starter just began.
func putTestEntry(reg *Registry, key sessionKey, entry *sessionEntry) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.sessions[key] = &liveSlot{subject: entry.owner, entry: entry}
}

// putTestReservation installs a slot that is reserved but not yet wired — the
// state a key is in while its in-process half is being built. r.lookup must
// report it as absent.
func putTestReservation(reg *Registry, key sessionKey, subject string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.sessions[key] = &liveSlot{subject: subject}
}

// liveSlotCount reports how many keys reg holds, reserved or live. Reads under
// the registry's own lock so a test never races the reaper.
func liveSlotCount(reg *Registry) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return len(reg.sessions)
}

// adoptNewTestSession stands in for the create-then-adopt shape production's
// authorized start route takes, with the CREATE half faked out: it mints a
// session name and adopts it through the registry's injected builder, so every
// registry test exercises the real reserve/wire/publish bookkeeping.
//
// text is the opening message the sidebar label is derived from — stamped here
// rather than by the fake builder because production reads it from the
// AgentSession's own spec (wireExistingSession), which no fake creates.
func (r *Registry) adoptNewTestSession(ctx context.Context, agentClass, text, subject string) (string, error) {
	name := browsersession.NewSessionName(agentClass)
	if err := r.Adopt(ctx, newChatSessionNamespace, name, subject); err != nil {
		return "", err
	}
	if e, ok := r.lookup(sessionKey{Namespace: newChatSessionNamespace, Name: name}); ok {
		e.agentClass = agentClass
		e.title = sessionTitle(text, agentClass, e.createdAt)
	}
	return name, nil
}

func (b *fakeBuilder) fireTerminal(name string, notice terminalNotice) {
	b.mu.Lock()
	fn := b.onTerminals[name]
	b.mu.Unlock()
	if fn != nil {
		fn(notice)
	}
}

func (b *fakeBuilder) wasStopped(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.stopped {
		if n == name {
			return true
		}
	}
	return false
}

func (b *fakeBuilder) wasDeletedK8s(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.deletedK8s {
		if n == name {
			return true
		}
	}
	return false
}

// --- OnTurnActivity ----------------------------------------------------

func TestRegistry_OnTurnActivity_EmitsToOwningSessionSink(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	entry, ok := reg.lookup(newChatKey(name))
	require.True(t, ok)
	sink := &testSink{}
	require.True(t, entry.attach(sink))

	// The active⇄paused transition for the owning session reaches its sink as
	// a browser.MsgTurnActivity render event — the path the browser uses to
	// clear its "working" throbber on turn completion.
	reg.onTurnActivity(context.Background(), newChatSessionNamespace, name, false, "awaiting_user_message", 0, "")

	events, _ := sink.snapshot()
	require.Len(t, events, 1)
	m, ok := events[0].(browser.MsgTurnActivity)
	require.True(t, ok, "want browser.MsgTurnActivity, got %T", events[0])
	assert.False(t, m.Active)
	assert.Equal(t, "awaiting_user_message", m.Cause)
	assert.Equal(t, name, m.Session.Name)
}

func TestRegistry_OnTurnActivity_ForeignOrUnknownSessionDropped(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	entry, ok := reg.lookup(newChatKey(name))
	require.True(t, ok)
	sink := &testSink{}
	require.True(t, entry.attach(sink))

	// A foreign namespace and an unknown session name must both drop — the
	// signal can never cross into another conversation's sink (leak-safe
	// scoping, mirroring the SenderResolver methods).
	reg.onTurnActivity(context.Background(), "other-ns", name, true, "", 0, "")
	reg.onTurnActivity(context.Background(), newChatSessionNamespace, "no-such-session", true, "", 0, "")

	events, _ := sink.snapshot()
	assert.Empty(t, events, "foreign-namespace and unknown-session signals must not reach any sink")
}

// --- Adopt -------------------------------------------------------------

func TestRegistry_Adopt_HappyPath(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)

	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hello there", "user:abc123")
	require.NoError(t, err)
	assert.Contains(t, name, "demo-agent-")

	entry, ok := reg.lookup(newChatKey(name))
	require.True(t, ok, "an adopted session must be findable")
	assert.Equal(t, "user:abc123", entry.owner)
	assert.Contains(t, fb.built, name)
}

// TestRegistry_Adopt_WiringError_ReleasesSlot: a wiring failure must leave no
// reservation behind, or the key is unusable for the rest of the process's
// life and it silently costs the subject one of their sixteen slots.
func TestRegistry_Adopt_WiringError_ReleasesSlot(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	fb.buildErr = errors.New("boom: pipeline wiring failed")
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)

	_, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Empty(t, fb.built, "a failed wiring must not be recorded as built")
	assert.Zero(t, liveSlotCount(reg), "a failed wiring must release the reserved slot")
}

// TestRegistry_Adopt_IsIdempotent: adopting a key this process already holds a
// live entry for must be a no-op. Two entries for one conversation would mean
// two Hosts and two health watchers racing each other's teardown.
func TestRegistry_Adopt_IsIdempotent(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)

	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	first, ok := reg.lookup(newChatKey(name))
	require.True(t, ok)

	require.NoError(t, reg.Adopt(context.Background(), newChatSessionNamespace, name, "user:owner"))

	second, ok := reg.lookup(newChatKey(name))
	require.True(t, ok)
	assert.Same(t, first, second, "a second Adopt must reuse the live entry, not wire a second one")
	assert.Len(t, fb.built, 1, "a second Adopt must not run the builder again")
}

// --- the two caps ------------------------------------------------------

// TestRegistry_Caps is the two-cap model that replaced one process-wide cap.
// The row that matters most is "a second subject still succeeds": a renamed
// process-wide cap would pass every other row in this table and fail only
// that one.
func TestRegistry_Caps(t *testing.T) {
	cases := []struct {
		name string
		// fill seeds the registry before the call under test.
		fill func(t *testing.T, reg *Registry)
		// subject is who then tries to reserve.
		subject string
		wantErr error
	}{
		{
			name: "the subject is at their own limit: refused as too-many-for-you, not as server-full",
			fill: func(t *testing.T, reg *Registry) {
				fillSlots(t, reg, "user:crowded", limits.perSubject)
			},
			subject: "user:crowded",
			wantErr: ErrTooManySessionsForSubject,
		},
		{
			name: "a SECOND subject still succeeds while the first is at their limit — the cap is per-subject, not process-wide",
			fill: func(t *testing.T, reg *Registry) {
				fillSlots(t, reg, "user:crowded", limits.perSubject)
			},
			subject: "user:newcomer",
			wantErr: nil,
		},
		{
			name: "the process is at capacity across many subjects: refused as server-full, never as the viewer's own fault",
			fill: func(t *testing.T, reg *Registry) {
				// Spread across enough subjects that no single one is anywhere
				// near limits.perSubject — so a refusal here can only
				// come from the process ceiling.
				per := limits.perSubject - 1
				for i := 0; i*per < limits.total; i++ {
					fillSlotsFrom(t, reg, fmt.Sprintf("user:filler-%d", i), i*per, min(per, limits.total-i*per))
				}
			},
			subject: "user:newcomer",
			wantErr: ErrServerAtCapacity,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newFakeBuilder()
			reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, fb.build)
			tc.fill(t, reg)
			before := liveSlotCount(reg)

			release, err := reg.Reserve(newChatSessionNamespace, "wanted-session", tc.subject)

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Nil(t, release)
				assert.Equal(t, before, liveSlotCount(reg), "a refused reservation must add no slot")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, release)
			assert.Equal(t, before+1, liveSlotCount(reg))
			release()
			assert.Equal(t, before, liveSlotCount(reg), "release must drop the reservation")
		})
	}
}

// TestRegistry_Reserve_ReleaseAfterAdoptIsANoOp: a caller that reserves,
// creates and adopts successfully may still run a deferred release. That must
// not tear down the live conversation it just handed the user.
func TestRegistry_Reserve_ReleaseAfterAdoptIsANoOp(t *testing.T) {
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, fb.build)

	release, err := reg.Reserve(newChatSessionNamespace, "demo-agent-1111", "user:owner")
	require.NoError(t, err)
	require.NoError(t, reg.Adopt(context.Background(), newChatSessionNamespace, "demo-agent-1111", "user:owner"))

	release()
	release() // twice: release must be safe to call more than once

	_, ok := reg.lookup(newChatKey("demo-agent-1111"))
	assert.True(t, ok, "releasing a reservation that has already been adopted must not evict the live entry")
}

// fillSlots seeds n live slots owned by subject.
func fillSlots(t *testing.T, reg *Registry, subject string, n int) {
	t.Helper()
	fillSlotsFrom(t, reg, subject, 0, n)
}

// fillSlotsFrom seeds n live slots owned by subject, named from an offset so
// two callers never collide on a key.
func fillSlotsFrom(t *testing.T, reg *Registry, subject string, offset, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("filler-session-%d", offset+i)
		putTestEntry(reg, newChatKey(name), newSessionEntry(newChatSessionNamespace, name, subject))
	}
}

// checkInteractOnlyFor returns a fakeDeps.checkInteract func that admits only
// the named subject — the CheckInteract-era replacement for the old
// owner-string comparison, for a test that specifically asserts a subject
// with no standing is refused.
func checkInteractOnlyFor(subject string) func(ctx context.Context, ns, name, s string) (bool, error) {
	return func(_ context.Context, _, _, s string) (bool, error) {
		return s == subject, nil
	}
}

// checkInteractOnlyForObject returns a fakeDeps.checkInteract func that
// admits any subject but ONLY when asked about the named (ns, name) object —
// the object-discriminating counterpart to checkInteractOnlyFor(subject).
// Every other scripted fake in this package ignores its ns/name arguments
// (checkInteractOnlyFor scripts purely by subject), so nothing else proves
// the gate is actually consulted about the URL's own session rather than
// some other one it happens to already hold an answer for.
func checkInteractOnlyForObject(ns, name string) func(ctx context.Context, ns2, name2, subject string) (bool, error) {
	return func(_ context.Context, ns2, name2, _ string) (bool, error) {
		return ns2 == ns && name2 == name, nil
	}
}

// newChatKey returns the sessionKey for a session adoptNewTestSession built —
// it places every fixture session in newChatSessionNamespace, so tests
// exercising the sessionKey-taking Registry methods against such a fixture
// pair that name with this namespace.
func newChatKey(name string) sessionKey {
	return sessionKey{Namespace: newChatSessionNamespace, Name: name}
}

// liveSessionCount reports how many live sessions the registry holds, taking
// r.mu so the read is race-free.
func liveSessionCount(reg *Registry) int { return liveSlotCount(reg) }

// --- SubmitMessage -----------------------------------------------------

func TestRegistry_SubmitMessage(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	fb.listener.decision = channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:owner")}, fb.build)

	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	t.Run("owner can submit; routed to the listener", func(t *testing.T) {
		dec, err := reg.SubmitMessage(context.Background(), newChatKey(name), "user:owner", "follow up", "")
		require.NoError(t, err)
		assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
		assert.Contains(t, fb.listener.calls, "follow up")
	})

	t.Run("a different subject is forbidden", func(t *testing.T) {
		_, err := reg.SubmitMessage(context.Background(), newChatKey(name), "user:someone-else", "hijack", "")
		assert.ErrorIs(t, err, ErrForbidden)
	})

	t.Run("unknown session id", func(t *testing.T) {
		_, err := reg.SubmitMessage(context.Background(), newChatKey("no-such-session"), "user:owner", "hi", "")
		assert.ErrorIs(t, err, ErrSessionNotFound)
	})
}

// --- SubmitInterrupt -----------------------------------------------------

func TestRegistry_SubmitInterrupt(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:owner")}, fb.build)

	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	t.Run("owner can request an interrupt; forwarded to the listener", func(t *testing.T) {
		err := reg.SubmitInterrupt(context.Background(), newChatKey(name), "user:owner", "req-1")
		require.NoError(t, err)
		assert.Contains(t, fb.listener.interrupts, "req-1")
	})

	t.Run("a different subject is forbidden", func(t *testing.T) {
		err := reg.SubmitInterrupt(context.Background(), newChatKey(name), "user:someone-else", "req-2")
		assert.ErrorIs(t, err, ErrForbidden)
	})

	t.Run("unknown session id", func(t *testing.T) {
		err := reg.SubmitInterrupt(context.Background(), newChatKey("no-such-session"), "user:owner", "req-3")
		assert.ErrorIs(t, err, ErrSessionNotFound)
	})
}

// --- C1 regression: a shared live entry's Submit* stamp the CALLING
// subject's identity, not whichever subject the entry happened to be built
// for -------------------------------------------------------------------

// TestRegistry_SubmitMessage_StampsCallingSubjectNotEntryBuilder is the
// Critical-finding regression bar: build ONE entry as if Bob (a participant)
// re-attached first, then have Alice — a DIFFERENT subject CheckInteract also
// admits — submit through that SAME entry, and assert the identity reaching
// the listener is Alice's own, not Bob's. Before the fix, Registry.SubmitMessage
// forwarded no identity at all: the listener's own construction-time Ext
// (Bob's, baked in when the entry was wired) was what got stamped regardless
// of who actually called Submit* — unreachable before CheckInteract replaced
// the started-by comparison (exactly one subject could ever hold an entry),
// live the moment a participant, not only the starter, can attach.
func TestRegistry_SubmitMessage_StampsCallingSubjectNotEntryBuilder(t *testing.T) {
	const bob = "user:bob@example.com"
	const alice = "user:alice@example.com"
	fl := &fakeListener{decision: channelkinds.InboundDecision{Outcome: channelkinds.OutcomeRouted}}
	// The entry itself is owned by "bob" — exactly what rehydrate would stamp
	// if Bob were the first to re-attach a session Alice actually started.
	entry := newSessionEntry(newChatSessionNamespace, "shared-1111", bob)
	entry.listener = fl
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t), checkInteract: func(_ context.Context, _, _, subject string) (bool, error) {
		return subject == bob || subject == alice, nil
	}}, newFakeBuilder().build)
	putTestEntry(reg, newChatKey("shared-1111"), entry)

	_, err := reg.SubmitMessage(context.Background(), newChatKey("shared-1111"), alice, "alice's message", "")
	require.NoError(t, err)

	require.Len(t, fl.callExts, 1)
	assert.Equal(t, "user:alice@example.com", fl.callExts[0].Email.String(),
		"the identity stamped on Alice's own message must be Alice's, not Bob's — even though Bob's identity is what the shared entry was built/owned with")
}

// TestRegistry_SubmitDecision_StampsCallingSubjectNotEntryBuilder is C1's
// other half: DeciderPolicy validates standing against the Decider a
// decision carries, so stamping the wrong subject there is an authorization
// defect on the append-only audit surface, not a display bug.
func TestRegistry_SubmitDecision_StampsCallingSubjectNotEntryBuilder(t *testing.T) {
	const bob = "user:bob@example.com"
	const alice = "user:alice@example.com"
	fl := &fakeListener{}
	entry := newSessionEntry(newChatSessionNamespace, "shared-2222", bob)
	entry.listener = fl
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t), checkInteract: func(_ context.Context, _, _, subject string) (bool, error) {
		return subject == bob || subject == alice, nil
	}}, newFakeBuilder().build)
	putTestEntry(reg, newChatKey("shared-2222"), entry)

	err := reg.SubmitDecision(context.Background(), newChatKey("shared-2222"), alice, "identity_choice", "req-1", "agent")
	require.NoError(t, err)

	require.Len(t, fl.decisions, 1)
	assert.Equal(t, "user:alice@example.com", fl.decisions[0].ext.Email.String(),
		"the Decider must be Alice's own identity, not Bob's — evaluating a decision under the wrong subject's DeciderPolicy standing is the C1 defect")
}

// TestRegistry_SubmitInterrupt_StampsCallingSubjectNotEntryBuilder is C1's
// third leg, found during the round-1 sweep: fakeListener.SubmitInterrupt
// used to discard its ext argument entirely (`_ channelkinds.ExternalIdentity`),
// so no Registry-level test — including the round-1 Message/Decision pair
// above — could have caught a wrong identity reaching
// InterruptRequestPayload.Requester. fakeListener now records it like the
// other two Submit* methods.
func TestRegistry_SubmitInterrupt_StampsCallingSubjectNotEntryBuilder(t *testing.T) {
	const bob = "user:bob@example.com"
	const alice = "user:alice@example.com"
	fl := &fakeListener{}
	entry := newSessionEntry(newChatSessionNamespace, "shared-3333", bob)
	entry.listener = fl
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t), checkInteract: func(_ context.Context, _, _, subject string) (bool, error) {
		return subject == bob || subject == alice, nil
	}}, newFakeBuilder().build)
	putTestEntry(reg, newChatKey("shared-3333"), entry)

	err := reg.SubmitInterrupt(context.Background(), newChatKey("shared-3333"), alice, "req-1")
	require.NoError(t, err)

	require.Len(t, fl.interruptExts, 1)
	assert.Equal(t, "user:alice@example.com", fl.interruptExts[0].Email.String(),
		"the Requester must be Alice's own identity, not Bob's — even though Bob's identity is what the shared entry was built/owned with")
}

// --- Attach / Detach -----------------------------------------------------

func TestRegistry_AttachDetach(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	sink := &testSink{}
	entry, err := reg.Attach(context.Background(), newChatKey(name), "user:owner", sink)
	require.NoError(t, err)

	entry.Emit("anything") // unrecognized type is fine at this layer — fan-out doesn't inspect it
	events, _ := sink.snapshot()
	assert.Len(t, events, 1, "attached sink must receive broadcast Emits")

	reg.Detach(entry, sink)
	entry.Emit("more")
	events, _ = sink.snapshot()
	assert.Len(t, events, 1, "a detached sink must not receive further Emits")
}

func TestRegistry_Attach_WrongOwnerForbidden(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, checkInteract: checkInteractOnlyFor("user:owner")}, newFakeBuilder().build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	_, err = reg.Attach(context.Background(), newChatKey(name), "user:intruder", &testSink{})
	assert.ErrorIs(t, err, ErrForbidden)
}

func TestRegistry_Attach_UnknownSession(t *testing.T) {
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	_, err := reg.Attach(context.Background(), newChatKey("nope"), "user:owner", &testSink{})
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

func TestRegistry_Attach_AfterTeardown(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	reg.teardown(newChatKey(name), terminalNotice{Reason: "succeeded"})

	_, err = reg.Attach(context.Background(), newChatKey(name), "user:owner", &testSink{})
	assert.ErrorIs(t, err, ErrSessionNotFound, "a torn-down session must reject a racing attach rather than silently accept it")
}

// --- teardown -----------------------------------------------------------

func TestRegistry_Teardown_NotifiesAndClosesSinksAndCleansUp(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	sink1, sink2 := &testSink{}, &testSink{}
	_, err = reg.Attach(context.Background(), newChatKey(name), "user:owner", sink1)
	require.NoError(t, err)
	_, err = reg.Attach(context.Background(), newChatKey(name), "user:owner", sink2)
	require.NoError(t, err)

	reg.teardown(newChatKey(name), terminalNotice{Reason: "failed", FailureReason: "budget", FailureMessage: "ran out of tokens"})

	_, stillThere := reg.lookup(newChatKey(name))
	assert.False(t, stillThere, "torn-down session must be removed from the registry")
	assert.True(t, fb.wasStopped(name), "teardown must call the session's stop() (relay + watcher shutdown)")
	// teardown is IN-PROCESS ONLY — it must NOT delete the AgentSession CR.
	// A terminal (Failed/Succeeded) or idle session stays inspectable + resumable;
	// the operator owns CR lifecycle (idle-reap keeps it, expiration deletes it,
	// with the audit preserved via the finalizer's ephemeral-only cleanup). Hard-
	// deleting here is what wiped every ended conversation's logs.
	assert.False(t, fb.wasDeletedK8s(name), "teardown must NOT delete the AgentSession CR (operator owns CR lifecycle; keeps logs inspectable)")

	for _, s := range []*testSink{sink1, sink2} {
		events, closed := s.snapshot()
		require.Len(t, events, 1, "each attached sink must get exactly one session_ended notice")
		assert.True(t, closed, "each attached sink's connection must be closed")
		ended, ok := events[0].(sessionEndedMsg)
		require.True(t, ok)
		assert.Equal(t, "failed", ended.Reason)
		assert.Equal(t, "budget", ended.FailureReason)
	}
}

func TestRegistry_Teardown_IdempotentUnderConcurrentCallers(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	name, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.teardown(newChatKey(name), terminalNotice{Reason: "idle_timeout"})
		}()
	}
	wg.Wait()

	fb.mu.Lock()
	stopCount := 0
	for _, n := range fb.stopped {
		if n == name {
			stopCount++
		}
	}
	fb.mu.Unlock()
	assert.Equal(t, 1, stopCount, "concurrent teardown callers must converge on exactly one real teardown")
}

// --- reaper ---------------------------------------------------------------

func TestRegistry_ReapIdle_TearsDownStaleSessionsOnly(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)

	staleName, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	freshName, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	attachedName, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	staleEntry, _ := reg.lookup(newChatKey(staleName))
	staleEntry.mu.Lock()
	staleEntry.lastActivity = time.Now().Add(-2 * sessionIdleTTL)
	staleEntry.mu.Unlock()

	attachedEntry, err := reg.Attach(context.Background(), newChatKey(attachedName), "user:owner", &testSink{})
	require.NoError(t, err)
	attachedEntry.mu.Lock()
	attachedEntry.lastActivity = time.Now().Add(-2 * sessionIdleTTL)
	attachedEntry.mu.Unlock()

	reg.reapIdle()

	_, staleStillThere := reg.lookup(newChatKey(staleName))
	assert.False(t, staleStillThere, "an idle session with no attached sink must be reaped")

	_, freshStillThere := reg.lookup(newChatKey(freshName))
	assert.True(t, freshStillThere, "a recently-active session must not be reaped")

	_, attachedStillThere := reg.lookup(newChatKey(attachedName))
	assert.True(t, attachedStillThere, "a session with an attached sink must never be reaped, regardless of lastActivity")
}

// TestReapIdle_ReclaimsAReservationNothingPublishedInto covers the slot shape
// the reaper used to skip entirely: reserved, never published into.
//
// Two ways a slot gets there, both now reachable on every cluster kind: a start
// whose cluster objects were created but whose adopt failed (the reservation is
// KEPT on purpose — see browserstart.Start — because those objects exist and
// must keep counting against the viewer), and a panic between Reserve and the
// return, which net/http recovers per connection. Before this, either one held
// the slot for the process's lifetime: reapIdle skipped every `entry == nil`
// slot, and sixteen of them locked a viewer out permanently.
//
// The fresh reservation in the same table is the discriminating half — a reaper
// that dropped every entry-less slot would break the ordinary reserve → create
// → adopt window, where a slot is legitimately entry-less for seconds.
func TestReapIdle_ReclaimsAReservationNothingPublishedInto(t *testing.T) {
	var logged []string
	reg := newRegistryWithBuilder(&fakeDeps{
		k8s: newFakeK8sClient(t),
		logger: funcr.New(func(_, args string) {
			logged = append(logged, args)
		}, funcr.Options{}),
	}, newFakeBuilder().build)

	now := time.Now()
	reg.clock = func() time.Time { return now }

	abandoned := sessionKey{Namespace: "demo-ns", Name: "abandoned-1111"}
	inFlight := sessionKey{Namespace: "demo-ns", Name: "in-flight-2222"}
	_, err := reg.Reserve(abandoned.Namespace, abandoned.Name, "user:owner")
	require.NoError(t, err)
	_, err = reg.Reserve(inFlight.Namespace, inFlight.Name, "user:owner")
	require.NoError(t, err)

	// Only the first one ages past the TTL.
	reg.mu.Lock()
	reg.sessions[abandoned].reservedAt = now.Add(-2 * reservationTTL)
	reg.mu.Unlock()

	reg.reapIdle()

	reg.mu.Lock()
	_, abandonedStillThere := reg.sessions[abandoned]
	_, inFlightStillThere := reg.sessions[inFlight]
	reg.mu.Unlock()

	assert.False(t, abandonedStillThere,
		"a reservation nothing ever published into must be reclaimed, or it charges a viewer forever")
	assert.True(t, inFlightStillThere,
		"a reservation still inside its window must survive — the ordinary start path holds one for seconds")
	assert.Contains(t, strings.Join(logged, "\n"), "reclaimed a reservation",
		"reclaiming a viewer's slot must be visible to an operator, not silent")
}

// --- sessionEntry: attach-vs-teardown atomicity ---------------------------

func TestSessionEntry_AttachFailsAfterMarkTorndown(t *testing.T) {
	e := newSessionEntry(newChatSessionNamespace, "s1", "user:owner")
	sinks, first := e.markTorndown()
	assert.Empty(t, sinks)
	assert.True(t, first)

	ok := e.attach(&testSink{})
	assert.False(t, ok, "attach must fail once the entry is torn down — the caller is responsible for closing the connection it just upgraded")

	_, second := e.markTorndown()
	assert.False(t, second, "markTorndown must be idempotent")
}

func TestSessionEntry_IdleForPausedWhileSinkAttached(t *testing.T) {
	e := newSessionEntry(newChatSessionNamespace, "s1", "user:owner")
	e.lastActivity = time.Now().Add(-time.Hour)
	d, isIdle := e.idleFor()
	assert.True(t, isIdle)
	assert.GreaterOrEqual(t, d, time.Hour)

	require.True(t, e.attach(&testSink{}))
	_, isIdle = e.idleFor()
	assert.False(t, isIdle, "the idle clock must pause while any sink is attached")
}

// --- Shutdown / ResetForTest ----------------------------------------------

func TestRegistry_Shutdown_TearsDownEverySession(t *testing.T) {
	k8s := newFakeK8sClient(t, validAgentClass("demo-agent"))
	fb := newFakeBuilder()
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s}, fb.build)
	reg.startReaper()

	n1, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)
	n2, err := reg.adoptNewTestSession(context.Background(), "demo-agent", "hi", "user:owner")
	require.NoError(t, err)

	reg.shutdown(context.Background())

	assert.True(t, fb.wasStopped(n1))
	assert.True(t, fb.wasStopped(n2))
	assert.Empty(t, reg.sessions)
}

// --- Blocker #8: the chat relay must not pay an API Get for foreign traffic --

// TestRegistry_Relay_ForeignSessionEnvelope_CostsNoAPIGet pins the pre-Get
// scoping of the Registry's single process-wide relay.
//
// startRelay subscribes to the literal cluster-wide "ap.session.*.*.out.>", so
// webd sees every Slack- and CLI-driven session's outbound envelopes too — none
// of which it can ever deliver, because the Registry resolves senders from its
// own live-session table. Loading each of those AgentSessions from the API
// server to discover that costs a rate-limited round-trip on the SAME serialized
// NATS delivery goroutine the chat's own replies arrive on, so unrelated
// cluster traffic delays the desktop user's reply.
//
// The Registry already knows the answer without the API server: the session is
// either in its table or it is not.
func TestRegistry_Relay_ForeignSessionEnvelope_CostsNoAPIGet(t *testing.T) {
	const owned, ownedChan = "sess-owned", "chan-owned"
	const foreign, foreignChan = "slack-driven-session", "chan-foreign"

	var gets atomic.Int64
	k8s := newFakeInterceptedK8sClient(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	}, chatAgentSession(owned, ownedChan), chatAgentSession(foreign, foreignChan))

	entry := newSessionEntry(newChatSessionNamespace, owned, "user:owner")
	entry.senders = newTestBrowserHost(t, nil, ownedChan, owned, entry)
	sink := &testSink{}
	require.True(t, entry.attach(sink))

	nc := connectTestNATS(t)
	reg := newRegistryWithBuilder(&fakeDeps{k8s: k8s, nats: nc}, newFakeBuilder().build)
	putTestEntry(reg, newChatKey(owned), entry)
	require.NoError(t, reg.startRelay(), "startRelay")
	t.Cleanup(func() {
		reg.relayCancel()
		_ = reg.relay.Stop(context.Background())
	})

	foreignEnv, err := channelevents.BuildEnvelope(newChatSessionNamespace, foreign, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "a reply bound for Slack"})
	require.NoError(t, err)
	for i := 0; i < 20; i++ {
		publishChatOut(t, nc, foreign, foreignEnv)
	}

	// An owned envelope published last is the ordering probe: the relay's
	// callbacks are serialized, so once this one has been delivered every
	// foreign envelope ahead of it has already been handled.
	ownedEnv, err := channelevents.BuildEnvelope(newChatSessionNamespace, owned, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "the chat user's reply"})
	require.NoError(t, err)
	publishChatOut(t, nc, owned, ownedEnv)

	require.Eventually(t, func() bool {
		events, _ := sink.snapshot()
		return len(events) >= 1
	}, 2*time.Second, 10*time.Millisecond, "the owned session's envelope must still be delivered")

	assert.LessOrEqual(t, gets.Load(), int64(1),
		"only the owned session's envelope may cost an API Get; envelopes for sessions this registry does not own must be dropped before any lookup")

	events, _ := sink.snapshot()
	assert.Len(t, events, 1, "the owned sink must receive exactly its own envelope")
}

// TestSetLimits covers the operator knob for the two caps, and one property
// that matters more than the knob: an unset flag must not disable a limit.
//
// internal/cmd/webd passes its flag values straight through, and a zero from an
// unset int flag reaching a naive setter would mean "no bound" on a
// browser-facing surface — the opposite of what an operator who set nothing
// asked for.
func TestSetLimits(t *testing.T) {
	perSubject, total := limits.perSubject, limits.total
	t.Cleanup(func() { limits.perSubject, limits.total = perSubject, total })

	SetLimits(3, 9)
	assert.Equal(t, 3, MaxLiveSessionsPerSubject())
	assert.Equal(t, 9, MaxLiveSessions())

	SetLimits(0, 0)
	assert.Equal(t, 3, MaxLiveSessionsPerSubject(), "an unset flag must leave the limit alone, never remove it")
	assert.Equal(t, 9, MaxLiveSessions(), "an unset flag must leave the limit alone, never remove it")

	SetLimits(-1, -1)
	assert.Equal(t, 3, MaxLiveSessionsPerSubject(), "a negative value is not 'unlimited' either")
	assert.Equal(t, 9, MaxLiveSessions())

	// The caps are what claimSlot actually enforces, not just what the getters
	// report — a setter that updated a second copy would pass everything above.
	reg := newRegistryWithBuilder(&fakeDeps{k8s: newFakeK8sClient(t)}, newFakeBuilder().build)
	SetLimits(1, 9)
	_, err := reg.Reserve("demo-ns", "first", "user:owner")
	require.NoError(t, err)
	_, err = reg.Reserve("demo-ns", "second", "user:owner")
	assert.ErrorIs(t, err, ErrTooManySessionsForSubject,
		"the limit claimSlot enforces must be the one SetLimits wrote, not a second copy")
}
