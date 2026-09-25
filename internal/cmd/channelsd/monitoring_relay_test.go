package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register slack kind so "a-broken" resolves
)

func TestMonitoringRelay_FansOutToMonitoringChannels(t *testing.T) {
	fakekind.ResetAllDrivers()
	t.Cleanup(fakekind.ResetAllDrivers)

	monCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "mon-creds"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
	// A non-monitoring channel that must NOT receive the event.
	plainCh := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "plain"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:     "agent",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "mon-creds"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mon-creds"}}

	cli := fake.NewClientBuilder().
		WithScheme(makeScheme()).
		WithObjects(monCh, plainCh, secret).
		Build()

	relay := &monitoringRelay{cli: cli}

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
		Condition:  "Refresh",
	}
	data, err := json.Marshal(ev)
	require.NoError(t, err)

	relay.handle(context.Background(), &nats.Msg{Data: data})

	got := fakekind.DriverFor("default", "mon").MonitoringEvents()
	require.Len(t, got, 1, "the monitoring channel must receive the event")
	assert.Equal(t, "github-bot", got[0].Source.Name)

	assert.Nil(t, fakekind.DriverFor("default", "plain"),
		"a non-monitoring channel must not be touched")
}

func TestMonitoringRelay_DropsMalformedMessage(t *testing.T) {
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).Build()
	relay := &monitoringRelay{cli: cli}
	// Must not panic.
	relay.handle(context.Background(), &nats.Msg{Data: []byte("{not json")})
}

// TestMonitoringRelay_SendFailureDoesNotAbortFanOut verifies that a per-channel
// send failure (or sender-resolution failure) for one monitoring Channel does
// not prevent the remaining monitoring Channels from receiving the event.
//
// "a-broken" is kind=slack but its Secret has no bot-token key, so
// newSlackAPIClient returns nil and SendMonitoring returns an error.
// "b-fake" is kind=fake and must still receive the event.
func TestMonitoringRelay_SendFailureDoesNotAbortFanOut(t *testing.T) {
	fakekind.ResetAllDrivers()
	t.Cleanup(fakekind.ResetAllDrivers)

	// broken: kind=slack, Secret intentionally lacks "bot-token" →
	// newSlackAPIClient returns nil → SendMonitoring returns an error.
	broken := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "a-broken"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "empty-creds"},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C123"},
			},
		},
	}
	// healthy: kind=fake — always succeeds; records via its Driver.
	healthy := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "b-fake"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			Role:           spiceboxv1alpha1.ChannelRoleMonitoring,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "empty-creds"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
	// Secret with no keys — slack kind reads "bot-token" from Data; absent ⇒ nil client.
	emptySecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "empty-creds"}}

	cli := fake.NewClientBuilder().WithScheme(makeScheme()).
		WithObjects(broken, healthy, emptySecret).Build()
	relay := &monitoringRelay{cli: cli}

	ev := channelevents.MonitoringEvent{
		Level:      channelevents.MonitoringLevelError,
		Category:   "credential",
		Transition: channelevents.MonitoringTransitionFailed,
		Source:     channelevents.MonitoringSourceRef{Kind: "AgentIdentity", Namespace: "default", Name: "github-bot"},
		Condition:  "Refresh",
	}
	data, err := json.Marshal(ev)
	require.NoError(t, err)

	relay.handle(context.Background(), &nats.Msg{Data: data})

	got := fakekind.DriverFor("default", "b-fake").MonitoringEvents()
	require.Len(t, got, 1, "the healthy monitoring channel must still receive the event after the broken one failed")
	assert.Equal(t, "github-bot", got[0].Source.Name)
}
