// The senderResolver builds a Sender with r.mu released — resolveDeps makes
// two API-server Gets — and takes the lock again only to write the cache
// entry. A Set* landing in that window clears the cache (invalidateSenders)
// and is then UNDONE by the in-flight resolve writing its pre-Set Sender back
// in. Nothing rebuilds it: the Set* methods are the only callers of
// invalidateSendersLocked and each runs once at startup.
//
// The window is the normal shape of channelsd boot, not a corner: run() starts
// `go wd.Run` and relay.Start — whose out.> callbacks dispatch on their own
// goroutine — well before it has a webd URL to build the minters from, so a
// backlogged envelope resolves its Sender across SetSessionViewMinter.
//
// These tests force that interleaving deterministically. The only production
// code between the deps snapshot and the cache write is the Kind's
// NewSender/SubChannelSender, so the Kind is the seam.
package main

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// hookKind is a channel kind that runs a one-shot test hook at the exact
// moment the resolver constructs a Sender — after depsSnapshot has copied the
// wiring, before the cache write re-takes r.mu — and records the Deps every
// build saw. It embeds the fake kind for the rest of the interface.
type hookKind struct {
	fakekind.Kind
	name string

	mu      sync.Mutex
	onBuild func()              // fires once, from inside the build
	built   []channelkinds.Deps // one entry per Sender constructed
}

func (k *hookKind) Name() string { return k.name }

func (k *hookKind) NewSender(deps channelkinds.Deps) channelkinds.Sender {
	k.record(deps)
	return k.Kind.NewSender(deps)
}

// SubChannelSender ignores the requested name: the fake kind has no
// session_view_offer sub-channel, and any non-nil Sender serves here — these
// tests assert on the Deps a Sender was built from, never on what it delivers.
func (k *hookKind) SubChannelSender(_ string, deps channelkinds.Deps) channelkinds.Sender {
	k.record(deps)
	return k.Kind.SubChannelSender("message", deps)
}

// record captures this build's Deps and fires the hook, which is where the
// test performs the Set* that races the cache write.
func (k *hookKind) record(deps channelkinds.Deps) {
	k.mu.Lock()
	k.built = append(k.built, deps)
	hook := k.onBuild
	k.onBuild = nil
	k.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (k *hookKind) builtDeps() []channelkinds.Deps {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.built)
}

// registerHookKind registers a hookKind under a name no other test uses. The
// registry has no unregister, and Reset would drop the fake/local kinds this
// package's other tests rely on (their init() runs once per process), so each
// caller must pass a distinct name instead of cleaning up.
func registerHookKind(t *testing.T, name string) *hookKind {
	t.Helper()
	k := &hookKind{name: name}
	registry.Register(k)
	return k
}

// newHookKindResolver builds a resolver over a session bound to a Channel of
// the given kind, with the Channel and its Secret present so resolveDeps
// reaches the Sender construction.
func newHookKindResolver(t *testing.T, kindName string) (*senderResolver, *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c1"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           kindName,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "c1-creds"},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c1-creds"},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s1"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: kindName},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(ch, sec, sess).Build()
	return newSenderResolver(cli, nil, nil), sess
}

// TestSenderForRebuildsASenderWhoseWiringChangedMidBuild: a minter wired while
// a Sender is being built must not be undone by that Sender landing in the
// cache, and the next lookup must carry the minter.
func TestSenderForRebuildsASenderWhoseWiringChangedMidBuild(t *testing.T) {
	k := registerHookKind(t, "hookkind-senderfor")
	r, sess := newHookKindResolver(t, k.name)

	// main.go's ordering, made deterministic: the Set runs after this
	// resolve snapshotted the wiring (nil minter) and before it writes the
	// cache entry.
	minter := newSessionViewMinter(func() string { return "https://webd.example" })
	k.onBuild = func() { r.SetSessionViewMinter(minter) }

	_, err := r.SenderFor(context.Background(), sess)
	require.NoError(t, err, "first resolve must succeed")

	_, err = r.SenderFor(context.Background(), sess)
	require.NoError(t, err, "second resolve must succeed")

	built := k.builtDeps()
	require.Len(t, built, 2,
		"a Sender built before the minter arrived must not be cached: writing it back into the "+
			"cache the Set just cleared strands the Channel on a nil minter for the life of the process")
	assert.Nil(t, built[0].SessionViewMinter,
		"the first build must predate the minter, or this test is not exercising the window")
	assert.Same(t, minter, built[1].SessionViewMinter,
		"the rebuild must carry the minter that arrived mid-build")

	// Caching itself must still work, or the fix has simply disabled it.
	_, err = r.SenderFor(context.Background(), sess)
	require.NoError(t, err, "third resolve must succeed")
	assert.Len(t, k.builtDeps(), 2,
		"with the wiring settled, the cached Sender must be reused rather than rebuilt per envelope")
}

// TestSubChannelSenderForRebuildsASenderWhoseWiringChangedMidBuild is the
// sub-channel twin: session_view_offer is cached under its own key and reaches
// the same window.
func TestSubChannelSenderForRebuildsASenderWhoseWiringChangedMidBuild(t *testing.T) {
	k := registerHookKind(t, "hookkind-subchannel")
	r, sess := newHookKindResolver(t, k.name)

	minter := newSessionViewMinter(func() string { return "https://webd.example" })
	k.onBuild = func() { r.SetSessionViewMinter(minter) }

	_, err := r.SubChannelSenderFor(context.Background(), sess, "session_view_offer")
	require.NoError(t, err, "first resolve must succeed")

	_, err = r.SubChannelSenderFor(context.Background(), sess, "session_view_offer")
	require.NoError(t, err, "second resolve must succeed")

	built := k.builtDeps()
	require.Len(t, built, 2,
		"the session_view_offer Sender built before the minter arrived must not be cached, or "+
			"every Open-interactive-view offer on this Channel stays unconfigured")
	assert.Nil(t, built[0].SessionViewMinter,
		"the first build must predate the minter, or this test is not exercising the window")
	assert.Same(t, minter, built[1].SessionViewMinter,
		"the rebuild must carry the minter that arrived mid-build")
}

// TestInvalidateSendersBumpsTheGenerationOnAnEmptyCache pins the half that is
// invisible from the cache alone: at boot the cache is empty precisely because
// the in-flight resolve has not written its entry yet, so "nothing cached"
// must not be read as "nothing to invalidate".
func TestInvalidateSendersBumpsTheGenerationOnAnEmptyCache(t *testing.T) {
	r := newSenderResolver(nil, nil, nil)

	r.mu.Lock()
	require.Empty(t, r.cache, "precondition: the boot-window cache is empty")
	before := r.wiringGen
	r.mu.Unlock()

	r.SetSessionViewMinter(newSessionViewMinter(func() string { return "https://webd.example" }))

	r.mu.Lock()
	defer r.mu.Unlock()
	assert.NotEqual(t, before, r.wiringGen,
		"an empty cache still has an in-flight build to invalidate")
}

// TestSubChannelSenderForRebuildsAnAgentUISenderWhoseWiringChangedMidBuild is
// the agent-UI twin of the session-view test above. It is a separate test, not
// an extra assertion on that one, because the two minters are wired by
// separate Set* methods: a SetAgentUIMinter that forgot
// invalidateSendersLocked would leave every Channel resolved during the boot
// window stranded on a nil minter for the life of the process — the agent's
// "open your dashboard" button reporting not-configured on that Channel
// forever, while every sibling Channel works. Both senders do log and notify
// per attempt, so the failure is visible; what makes it worth a test is that
// it is permanent and scoped to whichever Channels lost the boot race.
func TestSubChannelSenderForRebuildsAnAgentUISenderWhoseWiringChangedMidBuild(t *testing.T) {
	k := registerHookKind(t, "hookkind-agentui-subchannel")
	r, sess := newHookKindResolver(t, k.name)

	minter := newAgentUIMinter(func() string { return "https://webd.example" })
	k.onBuild = func() { r.SetAgentUIMinter(minter) }

	_, err := r.SubChannelSenderFor(context.Background(), sess, channelkinds.SubChannelAgentUIOffer)
	require.NoError(t, err, "first resolve must succeed")

	_, err = r.SubChannelSenderFor(context.Background(), sess, channelkinds.SubChannelAgentUIOffer)
	require.NoError(t, err, "second resolve must succeed")

	built := k.builtDeps()
	require.Len(t, built, 2,
		"the agent_ui_offer Sender built before the minter arrived must not be cached, or every "+
			"agent-UI handoff on this Channel stays unconfigured")
	// Both facts are asserted, and with assert rather than require, because
	// they fail to two DIFFERENT mutations: a forgotten invalidation strands
	// the second build on the nil minter, while a depsSnapshot that never
	// copies r.agentUIMinter leaves BOTH builds nil — and the first assertion
	// alone is satisfied by a field that is simply never populated.
	assert.Nil(t, built[0].AgentUIMinter,
		"the first build must predate the minter, or this test is not exercising the window")
	// NotNil before Same, both as asserts: assert.Same on a nil interface
	// reports only "Both arguments must be pointers", which names neither the
	// field nor the consequence. Neither aborts, so a wrong instance and a
	// missing one are still distinguishable in one run.
	assert.NotNil(t, built[1].AgentUIMinter,
		"the rebuild must carry a minter at all — a nil here means depsSnapshot never copied it")
	assert.Same(t, minter, built[1].AgentUIMinter,
		"the rebuild must carry the minter that arrived mid-build")
}
