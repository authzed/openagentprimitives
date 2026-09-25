package main

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// watcherListener is a Listener that also implements SessionWatcher, recording
// what it was handed.
//
// Mutex-guarded: the subscription-wiring test drives SessionUpdated from the
// NATS callback goroutine while the test goroutine polls, so an unguarded
// slice is a real data race (the race detector fails the whole package, which
// is how this was caught).
type watcherListener struct {
	mu  sync.Mutex
	got []*spiceboxv1alpha1.AgentSession
}

func (w *watcherListener) Start(context.Context) error { return nil }
func (w *watcherListener) Stop(context.Context) error  { return nil }
func (w *watcherListener) SessionUpdated(_ context.Context, sess *spiceboxv1alpha1.AgentSession) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, sess)
}

// seen returns a snapshot of the sessions handed to this listener.
func (w *watcherListener) seen() []*spiceboxv1alpha1.AgentSession {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]*spiceboxv1alpha1.AgentSession, len(w.got))
	copy(out, w.got)
	return out
}

// plainListener implements Listener but NOT SessionWatcher.
type plainListener struct{}

func (plainListener) Start(context.Context) error { return nil }
func (plainListener) Stop(context.Context) error  { return nil }

func attachScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func cronSession(name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "test-output",
				Kind: "slack",
				Key:  "thread:C1:1.0",
				External: map[string]string{
					"channel_id": "C1",
					"thread_ts":  "1.0",
				},
			},
		},
	}
}

// The live thread-index path: the outbound relay patches
// spec.outputChannel.{Key,External}, publishes SessionAttached on NATS, and
// this dispatcher must hand the FRESH session to the listener bound to the
// OUTPUT Channel. Without it a cron thread only enters the slack listener's
// threadIndex on the next channelsd restart's startup walk, and every human
// reply in the meantime is dropped at the threadIndex gate as "unowned
// thread" — observed in a live cluster.
//
// The whole chain was untested at this seam: the slack listener's
// SessionUpdated has tests, the relay's publish has tests, this dispatcher
// between them had none.
func TestHandleSessionAttached_DispatchesToOutputChannelListener(t *testing.T) {
	sess := cronSession("cron-1")
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(sess).Build()
	m := newChannelManager(cli, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	w := &watcherListener{}
	m.listeners["default/test-output"] = w

	m.HandleSessionAttached(context.Background(), channelevents.SessionAttached{
		Namespace:         "default",
		SessionName:       "cron-1",
		OutputChannelName: "test-output",
		OutputChannelKind: "slack",
		OutputChannelKey:  "thread:C1:1.0",
		External:          map[string]string{"channel_id": "C1", "thread_ts": "1.0"},
	})

	require.Len(t, w.seen(), 1, "the output Channel's listener must receive the session")
	assert.Equal(t, "cron-1", w.seen()[0].Name)
	// Freshly fetched from the API, not reconstructed from the envelope — the
	// listener must see current state including fields the fixed-shape NATS
	// payload cannot carry.
	require.NotNil(t, w.seen()[0].Spec.OutputChannel)
	assert.Equal(t, "C1", w.seen()[0].Spec.OutputChannel.External["channel_id"])
	assert.Equal(t, "1.0", w.seen()[0].Spec.OutputChannel.External["thread_ts"])
}

// A session-attached event naming a Channel with no running listener, or one
// whose listener is not a SessionWatcher, must not panic — and must not be
// silent. These branches returning bare made it impossible to tell from logs
// which step of the chain dropped the event.
func TestHandleSessionAttached_UnroutableEventsAreSafe(t *testing.T) {
	cases := []struct {
		name     string
		register func(m *channelManager)
		evName   string
	}{
		{
			name:     "no listener for the output Channel",
			register: func(*channelManager) {},
			evName:   "test-output",
		},
		{
			name: "listener does not implement SessionWatcher",
			register: func(m *channelManager) {
				m.listeners["default/test-output"] = plainListener{}
			},
			evName: "test-output",
		},
		{
			name: "listener registered under a different Channel name",
			register: func(m *channelManager) {
				m.listeners["default/other-output"] = &watcherListener{}
			},
			evName: "test-output",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := cronSession("cron-1")
			cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).WithObjects(sess).Build()
			m := newChannelManager(cli, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			tc.register(m)

			assert.NotPanics(t, func() {
				m.HandleSessionAttached(context.Background(), channelevents.SessionAttached{
					Namespace:         "default",
					SessionName:       "cron-1",
					OutputChannelName: tc.evName,
					OutputChannelKind: "slack",
				})
			})
		})
	}
}

// A missing session must not reach the listener: SessionUpdated is called only
// with a real, freshly-read AgentSession.
func TestHandleSessionAttached_MissingSessionIsNotDispatched(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(attachScheme(t)).Build() // no objects
	m := newChannelManager(cli, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	w := &watcherListener{}
	m.listeners["default/test-output"] = w

	m.HandleSessionAttached(context.Background(), channelevents.SessionAttached{
		Namespace:         "default",
		SessionName:       "gone",
		OutputChannelName: "test-output",
		OutputChannelKind: "slack",
	})

	assert.Empty(t, w.seen(), "a session that cannot be read must not be dispatched")
}

// Compile-time: the fakes satisfy the interfaces the dispatcher asserts on.
var (
	_ channelkinds.Listener       = (*watcherListener)(nil)
	_ channelkinds.SessionWatcher = (*watcherListener)(nil)
	_ channelkinds.Listener       = plainListener{}
	_ client.Client               = (client.Client)(nil)
)
