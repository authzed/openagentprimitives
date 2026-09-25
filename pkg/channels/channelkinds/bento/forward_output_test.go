package bento

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/warpstreamlabs/bento/public/service"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// fakeInboundSink captures InboundMessages forwarded by the
// forward_to_pipeline output so the test can assert on them.
type fakeInboundSink struct {
	mu  sync.Mutex
	msg []InboundMessage
}

func (f *fakeInboundSink) Handle(_ context.Context, m InboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msg = append(f.msg, m)
	return nil
}

func (f *fakeInboundSink) snapshot() []InboundMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InboundMessage, len(f.msg))
	copy(out, f.msg)
	return out
}

// TestForwardOutput_GenerateProducesInboundMessages drives a real
// Bento stream with a 1-tick generate input + the forward_to_pipeline
// output and asserts the captured InboundMessage matches the
// configured authz subject and bloblang `root` assignment.
func TestForwardOutput_GenerateProducesInboundMessages(t *testing.T) {
	sink := &fakeInboundSink{}
	SetInboundSink(sink)
	t.Cleanup(func() { SetInboundSink(nil) })

	ch := &spiceboxv1alpha1.Channel{}
	ch.Namespace = "default"
	ch.Name = "test-bento"
	ch.Spec.Bento = &spiceboxv1alpha1.BentoChannelConfig{
		Generate: &spiceboxv1alpha1.BentoGenerateConfig{
			// `root = "tick"` makes the whole message a raw string;
			// `root.message = "tick"` would serialize to JSON `{"message":"tick"}`
			// which would defeat the Body assertion below.
			Mapping:  `root = "tick"`,
			Interval: "@every 200ms",
			Count:    1,
		},
	}
	ch.Spec.AuthzSubject = "service:test-bot"

	yaml, err := buildStreamYAML(ch)
	require.NoError(t, err, "buildStreamYAML")

	builder := service.NewStreamBuilder()
	require.NoError(t, builder.SetYAML(yaml), "SetYAML\n%s", yaml)

	stream, err := builder.Build()
	require.NoError(t, err, "Build")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = stream.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = stream.Stop(context.Background())

	got := sink.snapshot()
	require.NotEmpty(t, got, "no InboundMessage forwarded after 3s")

	m := got[0]
	assert.Equal(t, "tick", m.Body, "Body")
	assert.Equal(t, "service:test-bot", m.AuthzSubject, "AuthzSubject")
	assert.Equal(t, "bento", m.ChannelKind, "ChannelKind")
	assert.Equal(t, "test-bento", m.ChannelName, "ChannelName")
	assert.Equal(t, "default", m.ChannelNamespace, "ChannelNamespace")
}

// failingSink fails the first failures calls, then succeeds. failures<0 means
// "always fail".
type failingSink struct {
	mu       sync.Mutex
	calls    int
	failures int
	err      error
}

func (f *failingSink) Handle(context.Context, InboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures < 0 || f.calls <= f.failures {
		return f.err
	}
	return nil
}

func (f *failingSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// A permanently-failing sink must NOT be retried forever.
//
// Write returned the sink's error straight to Bento, which retries an errored
// output indefinitely with no delay. In a live cluster a misconfigured cron
// Channel therefore produced a session-creation attempt roughly every second
// against the apiserver — 41 failed AgentSessions in 44 seconds, plus a
// matching flood of Slack monitoring alerts. Three separate bugs each
// manifested that way; the flood was worse than any of them.
//
// A cron firing is a discrete scheduled event: replaying a stale trigger
// forever is never right. Try a bounded number of times, then give up and
// report delivered so the stream moves on.
func TestForwardOutput_PermanentFailureIsBoundedNotInfinite(t *testing.T) {
	restore := forwardRetryDelays
	forwardRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { forwardRetryDelays = restore })

	sink := &failingSink{failures: -1, err: errors.New("apiserver said no")}
	SetInboundSink(sink)
	t.Cleanup(func() { SetInboundSink(nil) })

	out := &forwardOutput{channelName: "c", channelNamespace: "default"}
	err := out.Write(context.Background(), service.NewMessage([]byte("hi")))

	require.NoError(t, err,
		"a permanently-failing message must be given up on, not returned as an error Bento retries forever")
	assert.Equal(t, len(forwardRetryDelays)+1, sink.count(),
		"must attempt exactly once plus one per configured backoff step")
}

// A transient failure must still be retried — giving up instantly would drop a
// weekly digest on a momentary blip.
func TestForwardOutput_TransientFailureRetriesThenSucceeds(t *testing.T) {
	restore := forwardRetryDelays
	forwardRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { forwardRetryDelays = restore })

	sink := &failingSink{failures: 1, err: errors.New("transient")}
	SetInboundSink(sink)
	t.Cleanup(func() { SetInboundSink(nil) })

	out := &forwardOutput{channelName: "c", channelNamespace: "default"}
	require.NoError(t, out.Write(context.Background(), service.NewMessage([]byte("hi"))))
	assert.Equal(t, 2, sink.count(), "one failure then one success")
}

// A cancelled context must abort the retry loop promptly rather than sleeping
// through the remaining backoff — a shutting-down channelsd must not be held
// open by a doomed message.
func TestForwardOutput_ContextCancellationAbortsRetries(t *testing.T) {
	restore := forwardRetryDelays
	forwardRetryDelays = []time.Duration{time.Hour, time.Hour}
	t.Cleanup(func() { forwardRetryDelays = restore })

	sink := &failingSink{failures: -1, err: errors.New("nope")}
	SetInboundSink(sink)
	t.Cleanup(func() { SetInboundSink(nil) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := &forwardOutput{channelName: "c", channelNamespace: "default"}
	done := make(chan error, 1)
	go func() { done <- out.Write(ctx, service.NewMessage([]byte("hi"))) }()

	select {
	case <-done:
		assert.Equal(t, 1, sink.count(), "must not keep retrying after cancellation")
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked on backoff after context cancellation")
	}
}
