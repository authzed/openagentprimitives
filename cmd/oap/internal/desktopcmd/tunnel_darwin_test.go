//go:build darwin

package desktopcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/publicendpoint"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// desktopWrittenConfigMap is the ConfigMap as writeWebdExternalURL leaves it:
// the runtime-chosen loopback port, which is where the endpoint this hook
// creates takes its spec.localURL from.
func desktopWrittenConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
			Namespace: cloud.WebdServiceNamespace,
		},
		Data: map[string]string{
			spiceboxv1alpha1.WebdTrustedURLKey: desktopURL,
			spiceboxv1alpha1.WebdSandboxURLKey: desktopURL,
		},
	}
}

func channelOfKind(name, kind string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: kind},
	}
}

// TestEnsureWebhookTunnel_OpensOneOnlyForAChannelThatReceivesWebhooks is the
// boot half of the on-demand policy: `oap agent install` opens a tunnel for a
// channel it DECLARES, and this opens one for a channel that is already wired —
// by `oap channel create`, or by an earlier run whose cluster has since been
// rebuilt.
func TestEnsureWebhookTunnel_OpensOneOnlyForAChannelThatReceivesWebhooks(t *testing.T) {
	cases := []struct {
		name     string
		channels []client.Object
		want     bool
	}{
		{
			name:     "a github Channel: this cluster needs an address to receive at",
			channels: []client.Object{channelOfKind("demo-agent-gh", "github")},
			want:     true,
		},
		{
			name:     "a slack Channel: the transport dials out, so nothing is opened",
			channels: []client.Object{channelOfKind("demo-agent-slack", "slack")},
			want:     false,
		},
		{
			name: "one of several: the tunnel is opened for the one that needs it",
			channels: []client.Object{
				channelOfKind("demo-agent-slack", "slack"),
				channelOfKind("demo-agent-gh", "github"),
			},
			want: true,
		},
		{
			name:     "no Channels at all: a fresh desktop opens nothing",
			channels: nil,
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			objs := append([]client.Object{desktopWrittenConfigMap()}, tc.channels...)
			c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(objs...).Build()

			require.NoError(t, stateForTest(t).ensureWebhookTunnel(ctx, c))

			var pe spiceboxv1alpha1.PublicEndpoint
			err := c.Get(ctx, types.NamespacedName{Name: publicendpoint.WebdName}, &pe)
			if !tc.want {
				assert.True(t, apierrors.IsNotFound(err), "no webhook channel means no tunnel")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, desktopURL, pe.Spec.LocalURL,
				"the endpoint's local URL is the loopback address the desktop actually bound, "+
					"read from the ConfigMap writeWebdExternalURL wrote — a guess would 404 every route")
		})
	}
}

// TestEnsureWebhookTunnel_SurfacesAListFailureRatherThanOpeningNothingQuietly.
// "I could not tell whether a channel needs an address" must not read as "none
// does": the caller logs what comes back, and a swallowed error there is a
// desktop that silently never receives a delivery again.
func TestEnsureWebhookTunnel_SurfacesAListFailureRatherThanOpeningNothingQuietly(t *testing.T) {
	boom := errors.New("apiserver said no")
	c := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()

	err := stateForTest(t).ensureWebhookTunnel(context.Background(), c)

	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "webhooks", "the message must say what the list was for")
}
