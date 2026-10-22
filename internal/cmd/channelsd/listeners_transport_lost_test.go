package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/flowcontrol"
	clocktesting "k8s.io/utils/clock/testing"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

const lostKindName = "transportlosttest"

var registerLostKindOnce sync.Once

// lostKind is the fake kind with a listener that hands its Deps back to the
// test, so the test can fire Deps.TransportLost the way a real transport would.
type lostKind struct{ channelkinds.Kind }

func (*lostKind) Name() string { return lostKindName }

func (*lostKind) NewListener(deps channelkinds.Deps) channelkinds.Listener {
	l := &lostListener{deps: deps}
	lostListeners.Lock()
	lostListeners.all = append(lostListeners.all, l)
	lostListeners.Unlock()
	return l
}

// lostListenerHook, when set, runs inside Start — before startListener has
// registered the listener — to exercise a transport that dies immediately.
// Its return value is Start's.
var lostListenerHook func(deps channelkinds.Deps) error

var lostListeners struct {
	sync.Mutex
	all []*lostListener
}

type lostListener struct{ deps channelkinds.Deps }

func (l *lostListener) Start(context.Context) error {
	if lostListenerHook != nil {
		return lostListenerHook(l.deps)
	}
	return nil
}

func (*lostListener) Stop(context.Context) error { return nil }

func registerLostKind(t *testing.T) {
	t.Helper()
	base, ok := registry.Get("fake")
	require.True(t, ok, "the fake kind must be registered to embed it")
	registerLostKindOnce.Do(func() { registry.Register(&lostKind{Kind: base}) })
	lostListeners.Lock()
	lostListeners.all = nil
	lostListeners.Unlock()
	t.Cleanup(func() { lostListenerHook = nil })
}

func lostListenerCount() int {
	lostListeners.Lock()
	defer lostListeners.Unlock()
	return len(lostListeners.all)
}

func lastLostListener(t *testing.T) *lostListener {
	t.Helper()
	lostListeners.Lock()
	defer lostListeners.Unlock()
	require.NotEmpty(t, lostListeners.all, "no listener was constructed")
	return lostListeners.all[len(lostListeners.all)-1]
}

func newLostManager(t *testing.T) (*channelManager, *spiceboxv1alpha1.Channel, func() *metav1.Condition) {
	t.Helper()
	registerLostKind(t)
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-channel"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           lostKindName,
			AgentClass:     "demo-agent",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-creds"},
		},
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-creds"}}
	cli := newClobberK8s(t, ch, sec)
	m := &channelManager{cli: cli, listeners: map[string]channelkinds.Listener{}, cancels: map[string]context.CancelFunc{}}
	t.Cleanup(func() { m.Stop(logr.Discard()) })
	connected := func() *metav1.Condition {
		return channelCondition(t, cli, "demo-ns", "demo-channel", spiceboxv1alpha1.ChannelConditionConnected)
	}
	return m, ch, connected
}

const lostKey = "demo-ns/demo-channel"

func TestTransportLostMarksDisconnectedAndNextTickRestarts(t *testing.T) {
	m, ch, connected := newLostManager(t)
	ctx := context.Background()

	m.startListener(ctx, lostKey, ch)
	first := lastLostListener(t)
	require.Equal(t, metav1.ConditionTrue, connected().Status, "precondition: listener attached")

	first.deps.TransportLost(errors.New("slack socket mode: invalid_auth"))

	got := connected()
	assert.Equal(t, metav1.ConditionFalse, got.Status)
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelTransportLost, got.Reason)
	assert.Contains(t, got.Message, "invalid_auth", "the cause must reach the condition (and so the monitoring event)")

	// The next reconcile tick's startListener must replace the dead listener,
	// not early-return because the key is still in the map.
	m.startListener(ctx, lostKey, ch)
	assert.NotSame(t, first, lastLostListener(t), "the dead listener must be replaced")
	assert.Equal(t, metav1.ConditionTrue, connected().Status, "a successful restart re-attaches")

	// A further tick with a healthy listener is a no-op.
	second := lastLostListener(t)
	m.startListener(ctx, lostKey, ch)
	assert.Same(t, second, lastLostListener(t), "a healthy listener must not be restarted")
}

func TestTransportLostDuringStartIsNotOverwrittenByConnectedTrue(t *testing.T) {
	m, ch, connected := newLostManager(t)
	ctx := context.Background()
	lostListenerHook = func(deps channelkinds.Deps) error {
		deps.TransportLost(errors.New("token_revoked"))
		return nil
	}

	m.startListener(ctx, lostKey, ch)

	got := connected()
	require.NotNil(t, got)
	assert.Equal(t, metav1.ConditionFalse, got.Status, "startListener must not stamp True over a loss reported during Start")
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelTransportLost, got.Reason)

	lostListenerHook = nil
	first := lastLostListener(t)
	m.startListener(ctx, lostKey, ch)
	assert.NotSame(t, first, lastLostListener(t), "the next tick must still replace it")
	assert.Equal(t, metav1.ConditionTrue, connected().Status)
}

// TestFailedListenersRestartWithExponentialBackoff pins that a fatal failure
// is retried — never given up on — but paced: 5s, then 10s, for both a lost
// transport and a Start that errors.
func TestFailedListenersRestartWithExponentialBackoff(t *testing.T) {
	m, ch, connected := newLostManager(t)
	clk := clocktesting.NewFakeClock(time.Now())
	m.restarts = flowcontrol.NewFakeBackOff(listenerRestartInitial, listenerRestartMax, clk)
	ctx := context.Background()

	m.startListener(ctx, lostKey, ch)
	lastLostListener(t).deps.TransportLost(errors.New("invalid_auth"))
	assert.Contains(t, connected().Message, "in about 5s", "the condition says when the retry comes")

	built := lostListenerCount()
	m.startListener(ctx, lostKey, ch)
	assert.Equal(t, built, lostListenerCount(), "no restart inside the first backoff window")

	clk.Step(listenerRestartInitial)
	m.startListener(ctx, lostKey, ch)
	require.Equal(t, built+1, lostListenerCount(), "restart once the first window elapses")
	assert.Equal(t, metav1.ConditionTrue, connected().Status)

	// Second consecutive failure — this time Start itself errors on the retry.
	lastLostListener(t).deps.TransportLost(errors.New("invalid_auth"))
	clk.Step(listenerRestartInitial)
	built = lostListenerCount()
	m.startListener(ctx, lostKey, ch)
	assert.Equal(t, built, lostListenerCount(), "the window doubled to 10s: 5s is too early")

	clk.Step(listenerRestartInitial)
	lostListenerHook = func(channelkinds.Deps) error { return errors.New("auth.test: invalid_auth") }
	m.startListener(ctx, lostKey, ch)
	require.Equal(t, built+1, lostListenerCount(), "retry at 10s")
	assert.Equal(t, spiceboxv1alpha1.ReasonChannelListenerStartFailed, connected().Reason)

	lostListenerHook = nil
	built = lostListenerCount()
	clk.Step(listenerRestartInitial * 2)
	m.startListener(ctx, lostKey, ch)
	assert.Equal(t, built, lostListenerCount(), "a failed Start doubles the window again, to 20s")

	clk.Step(listenerRestartInitial * 2)
	m.startListener(ctx, lostKey, ch)
	assert.Equal(t, built+1, lostListenerCount(), "retry at 20s")
	assert.Equal(t, metav1.ConditionTrue, connected().Status, "and it recovers")
}

func TestTransportLostAfterStopIsIgnored(t *testing.T) {
	m, ch, connected := newLostManager(t)
	ctx := context.Background()

	m.startListener(ctx, lostKey, ch)
	l := lastLostListener(t)
	m.stopListener(logr.Discard(), lostKey)

	l.deps.TransportLost(errors.New("use of closed network connection"))

	assert.Equal(t, metav1.ConditionTrue, connected().Status,
		"a transport that dies because channelsd stopped it is not a loss")
}
