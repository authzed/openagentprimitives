package main

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// nilSenderProvider models a CLIENT-HOSTED channel kind (the builtin
// browser-view / web-chat): SenderFor returns (nil, nil) — the documented
// "relay drops the envelope" sentinel — which every caller MUST nil-check
// before calling Send, or it dereferences a nil interface and crash-loops
// channelsd.
type nilSenderProvider struct{}

func (nilSenderProvider) SenderFor(context.Context, *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	return nil, nil
}

func channelAttachedSession(ns, name string) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
		},
	}
}

// TestStatusWatchdog_ClientHostedSender_NoPanic reproduces the crash that took
// channelsd down: for a client-hosted session (browser view) SenderFor resolves
// to a nil sender, and the watchdog's Send call sites dereferenced it. A status
// signal (operation_activity / turn_activity / a clear) for such a session must
// DROP silently, never panic the relay's NATS callback.
func TestStatusWatchdog_ClientHostedSender_NoPanic(t *testing.T) {
	ctx := context.Background()

	t.Run("sendCaption drops on nil sender, no panic", func(t *testing.T) {
		wd := newStatusWatchdog(nil, nilSenderProvider{})
		assert.NotPanics(t, func() {
			wd.sendCaption(ctx, logr.Discard(), channelAttachedSession("ns", "s"),
				"taking longer than expected…")
		})
	})

	// The notice path resolves no sender at all — a notice is published, and
	// the relay owns the client-hosted drop — so a nil-sender provider must not
	// stop it reaching the bus. Pinned here, next to the caption's nil guard,
	// because "the two paths handle a nil sender differently" is exactly the
	// kind of asymmetry a later edit re-collapses into a panic.
	t.Run("sendNotice publishes past a nil sender, no panic", func(t *testing.T) {
		pub := &publishRecorder{}
		wd := newStatusWatchdog(nil, nilSenderProvider{})
		wd.publish = pub.publish
		assert.NotPanics(t, func() {
			wd.sendNotice(ctx, logr.Discard(), channelAttachedSession("ns", "s"),
				"notice-stalled-1", watchdogTimeoutNotice("s", 1))
		})
		assert.Len(t, pub.interactions(t, "ns", "s"), 1,
			"a nil default sender must not suppress the stall notice; only the relay decides to drop it")
	})

	t.Run("clearIndicator drops on nil sender, no panic, no error", func(t *testing.T) {
		sess := channelAttachedSession("ns", "s")
		cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).Build()
		wd := newStatusWatchdog(cli, nilSenderProvider{})
		var err error
		assert.NotPanics(t, func() { err = wd.clearIndicator(ctx, "ns", "s") })
		assert.NoError(t, err)
	})
}
