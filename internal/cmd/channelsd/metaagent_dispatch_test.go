// The generic half of the metaagent flow: locate the session, resolve its
// kind, hand the raw payload to that kind's sub-channel Sender. Everything
// kind-specific is asserted in pkg/channels/channelkinds/slack.
package main

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake" // a kind with no metaagent sub-channel
)

// metaagentFixture builds a session on the fake kind — a registered kind that
// returns nil for both metaagent sub-channels, i.e. the "this kind cannot
// render it" case the handler has to survive.
func metaagentFixture(t *testing.T) *metaagentHandlers {
	t.Helper()
	scm := makeScheme()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c1"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "fake",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "c1-creds"},
			Fake:           &spiceboxv1alpha1.FakeChannelConfig{},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "c1-creds"},
		Data:       map[string][]byte{"k": []byte("v")},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s1"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "c1", Kind: "fake"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scm).WithObjects(ch, sec, sess).Build()
	return &metaagentHandlers{cli: cli}
}

// TestMetaagentDispatchSurvivesEveryDropPath walks each way a metaagent
// message can fail to reach a human. None may panic, and none may take the
// binary down — the handler runs on a NATS subscription goroutine.
//
// The fake kind returns nil from SubChannelSender for both metaagent names —
// the documented "this kind does not implement that sub-channel" answer. The
// drop must stay kind-agnostic: resolving a Slack client here and logging "no
// Slack API client" for a session that was never on Slack is the failure mode
// this pins against.
func TestMetaagentDispatchSurvivesEveryDropPath(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		data    string
	}{
		{
			name:    "malformed subject: dropped without dispatch",
			subject: "ap.session.only-three",
			data:    `{"requestId":"r"}`,
		},
		{
			name:    "payload is not JSON: dropped before the kind sees it",
			subject: "ap.session.default.s1.out.metaagent_scope_approval",
			data:    `not json at all`,
		},
		{
			name:    "unknown session: dropped",
			subject: "ap.session.default.nosuch.out.metaagent_scope_approval",
			data:    `{"requestId":"r"}`,
		},
		{
			name:    "kind implements no scope-approval sub-channel: dropped, loudly",
			subject: "ap.session.default.s1.out.metaagent_scope_approval",
			data:    `{"requestId":"r","requester":"U1"}`,
		},
		{
			name:    "kind implements no notice sub-channel: dropped, loudly",
			subject: "ap.session.default.s1.out.metaagent_notice",
			data:    `{"requester":"U1","body":"hi"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := metaagentFixture(t)
			msg := &nats.Msg{Subject: tc.subject, Data: []byte(tc.data)}
			assert.NotPanics(t, func() { h.handleScopeApproval(context.Background(), msg) })
			assert.NotPanics(t, func() { h.handleNotice(context.Background(), msg) })
		})
	}
}

// TestFakeKindDeclinesTheMetaagentSubChannels documents WHY the drop paths
// above are reachable, so the table above cannot quietly stop testing the
// no-sender branch if the fake kind ever grows one.
func TestFakeKindDeclinesTheMetaagentSubChannels(t *testing.T) {
	k, ok := registry.Get("fake")
	require.True(t, ok)
	assert.Nil(t, k.SubChannelSender(channelkinds.SubChannelMetaagentScopeApproval, channelkinds.Deps{}))
	assert.Nil(t, k.SubChannelSender(channelkinds.SubChannelMetaagentNotice, channelkinds.Deps{}))
}
