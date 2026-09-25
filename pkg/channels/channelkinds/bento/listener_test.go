package bento

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestListener_StartReconcileStop(t *testing.T) {
	sink := &fakeInboundSink{}
	SetInboundSink(sink)
	t.Cleanup(func() { SetInboundSink(nil) })

	l := newListener(channelkinds.Deps{}).(*listener)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := &spiceboxv1alpha1.Channel{}
	ch.Namespace = "default"
	ch.Name = "tickr"
	ch.Spec.Bento = &spiceboxv1alpha1.BentoChannelConfig{
		Generate: &spiceboxv1alpha1.BentoGenerateConfig{
			Mapping:  `root = "tick"`,
			Interval: "@every 100ms",
			Count:    0,
		},
	}
	ch.Spec.AuthzSubject = "service:tickr-bot"

	require.NoError(t, l.Reconcile(ctx, ch), "Reconcile")
	t.Cleanup(func() { _ = l.Stop(context.Background()) })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, len(sink.snapshot()), 2, "expected >= 2 messages after 2s")

	// Reconcile a NEW spec → previous stream is torn down + new one started.
	ch.Spec.Bento.Generate.Mapping = `root = "tick2"`
	require.NoError(t, l.Reconcile(ctx, ch), "Reconcile (update)")

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := sink.snapshot()
		if len(got) > 0 && got[len(got)-1].Body == "tick2" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error(`did not observe "tick2" after Reconcile update`)
}
