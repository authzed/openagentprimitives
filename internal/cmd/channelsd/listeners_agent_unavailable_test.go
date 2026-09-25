package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register the slack kind
)

// slackChannel builds a socket-mode slack Channel referencing secret "s".
func slackChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			AgentClass:     "ac1",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
			Slack:          &spiceboxv1alpha1.SlackChannelConfig{},
		},
	}
}

// slackSecret builds the credentials Secret carrying the given keys.
func slackSecret(keys ...string) *corev1.Secret {
	data := map[string][]byte{}
	for _, k := range keys {
		data[k] = []byte("x")
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
		Data:       data,
	}
}

// TestTransportViable decides which INVALID Channels keep their listener.
//
// A Channel goes Valid=False for two very different families of reason, and
// stopping the listener for both is what produces silence: an agent whose
// credential expired takes its Channel down with it, channelsd detaches the
// socket, and a DM is dropped by the transport layer before any code that could
// answer it runs.
//
// The question that matters is not WHY the Channel is invalid but whether it
// can still SPEAK. A Channel whose binding is broken — no agent, an
// invalid agent, an unresolvable reply target — has a working socket and can
// tell the user so. A Channel whose own transport is broken — no secret, a
// spec its kind rejects, a kind that does not exist — has nothing to say it
// over, and starting a listener for it is what the Valid gate is for.
//
// These are the same three checks, in the same order, that the channel
// controller runs before it looks at any binding: kind, spec, secret.
func TestTransportViable(t *testing.T) {
	const botToken, appToken = "bot-token", "app-token"
	cases := []struct {
		name    string
		mutate  func(ch *spiceboxv1alpha1.Channel)
		secret  *corev1.Secret
		want    bool
		wantWhy string
	}{
		{
			name:   "healthy transport, broken binding: viable — it can report the outage",
			secret: slackSecret(botToken, appToken),
			want:   true,
		},
		{
			name:   "unknown kind: not viable — there is no transport to build",
			mutate: func(ch *spiceboxv1alpha1.Channel) { ch.Spec.Kind = "nope" },
			secret: slackSecret(botToken, appToken),
			want:   false,
		},
		{
			name:   "credentials Secret missing: not viable — nothing to authenticate the socket",
			secret: nil,
			want:   false,
		},
		{
			name:   "Secret present but missing a required key: not viable",
			secret: slackSecret(botToken), // no app-token, so socket mode cannot connect
			want:   false,
		},
		{
			name:   "kind rejects the spec: not viable",
			mutate: func(ch *spiceboxv1alpha1.Channel) { ch.Spec.Slack = nil },
			secret: slackSecret(botToken, appToken),
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := slackChannel()
			if tc.mutate != nil {
				tc.mutate(ch)
			}
			objs := []client.Object{ch}
			if tc.secret != nil {
				objs = append(objs, tc.secret)
			}
			cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(objs...).Build()
			assert.Equal(t, tc.want, transportViable(context.Background(), cli, ch))
		})
	}
}
