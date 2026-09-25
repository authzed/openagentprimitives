package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
)

func TestSenderResolver_SkipsClientHostedKind(t *testing.T) {
	// A fake-kind session resolves to a non-nil Sender, proving the resolver
	// still works for channelsd-hosted kinds alongside the skip. The
	// client-hosted (RelayedByChannelsd==false) case is NOT covered here.
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
	r := newSenderResolver(cli, nil, nil)

	got, err := r.SenderFor(context.Background(), sess)
	require.NoError(t, err)
	assert.NotNil(t, got, "channelsd-hosted kind resolves to a Sender")
}

func TestSenderResolver_ClientHostedKind_ResolvesNil(t *testing.T) {
	scm := makeScheme()
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "lc"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "local",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "lc-creds"},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "lc-creds"},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ls"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "lc", Kind: "local"},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scm).WithObjects(ch, sec, sess).Build()
	r := newSenderResolver(cli, nil, nil)

	got, err := r.SenderFor(context.Background(), sess)
	require.NoError(t, err, "client-hosted kind must NOT error — relay drops silently")
	assert.Nil(t, got, "client-hosted kind resolves to a nil Sender")
}
