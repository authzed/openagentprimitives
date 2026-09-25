//go:build darwin

package desktopcmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

const (
	installSeedURL = "http://localhost:8080"
	desktopURL     = "http://127.0.0.1:17080"
	desktopPort    = 17080
)

// seededExternalURLConfigMap is what `oap install` leaves behind on a desktop:
// its own loopback seed, which names the wrong host AND the wrong port for a
// desktop VM. claimed=true adds the PublicEndpoint controller's field-manager
// entry, i.e. the controller has actually applied the two keys.
func seededExternalURLConfigMap(claimed bool) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spiceboxv1alpha1.WebdExternalURLConfigMap,
			Namespace: cloud.WebdServiceNamespace,
		},
		Data: map[string]string{
			spiceboxv1alpha1.WebdTrustedURLKey: installSeedURL,
			spiceboxv1alpha1.WebdSandboxURLKey: installSeedURL,
		},
	}
	if claimed {
		cm.ManagedFields = []metav1.ManagedFieldsEntry{{
			Manager:   spiceboxv1alpha1.WebdExternalURLFieldOwner,
			Operation: metav1.ManagedFieldsOperationApply,
		}}
	}
	return cm
}

func endpointTargeting(name, namespace, service string) *spiceboxv1alpha1.PublicEndpoint {
	return &spiceboxv1alpha1.PublicEndpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: spiceboxv1alpha1.PublicEndpointSpec{
			Target:   spiceboxv1alpha1.PublicEndpointTarget{Namespace: namespace, Service: service, Port: 8080},
			Provider: "ngrok",
			LocalURL: desktopURL,
		},
	}
}

func stateForTest(t *testing.T) *desktopState {
	t.Helper()
	return &desktopState{out: &bytes.Buffer{}}
}

// TestWriteWebdExternalURL_StandsDownOnlyAfterTheControllerHasClaimedTheKeys is
// the handover between the desktop's own loopback write and the PublicEndpoint
// controller. It fails in opposite directions and both are damaging: writing
// after the handover flaps a live public URL back to loopback, while not
// writing before it leaves the desktop on install's localhost:8080 seed, which
// is wrong on both host and port and 404s every route.
//
// The condition is the controller's field manager on the ConfigMap AND an
// endpoint still targeting webd — "a CR exists" is not enough (a Failed first
// reconcile writes nothing) and "the claim exists" is not enough either (a
// field-manager entry outlives the CR that made it).
func TestWriteWebdExternalURL_StandsDownOnlyAfterTheControllerHasClaimedTheKeys(t *testing.T) {
	webdEndpoint := endpointTargeting("webd", cloud.WebdServiceNamespace, cloud.WebdServiceName)

	cases := []struct {
		name      string
		claimed   bool
		endpoints []client.Object
		wantURL   string
	}{
		{
			name:    "no endpoint and no claim: the desktop writes its own loopback address",
			wantURL: desktopURL,
		},
		{
			name:      "endpoint claimed the keys: the controller owns them, so the desktop stands down",
			claimed:   true,
			endpoints: []client.Object{webdEndpoint},
			wantURL:   installSeedURL,
		},
		{
			name:      "endpoint exists but has never written: the desktop must still write, or nothing does",
			claimed:   false,
			endpoints: []client.Object{webdEndpoint},
			wantURL:   desktopURL,
		},
		{
			name:      "a stale claim from a deleted endpoint: the desktop takes the value back",
			claimed:   true,
			endpoints: nil,
			wantURL:   desktopURL,
		},
		{
			name:      "a claim plus an endpoint targeting some other Service: not webd's, so the desktop writes",
			claimed:   true,
			endpoints: []client.Object{endpointTargeting("dashboard", cloud.WebdServiceNamespace, "some-other-service")},
			wantURL:   desktopURL,
		},
		{
			name:      "a claim plus webd's Service name in another namespace: still not webd's",
			claimed:   true,
			endpoints: []client.Object{endpointTargeting("elsewhere", "other-namespace", cloud.WebdServiceName)},
			wantURL:   desktopURL,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ctrl := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(tc.endpoints...).Build()
			typed := typedfake.NewSimpleClientset(seededExternalURLConfigMap(tc.claimed))

			require.NoError(t, stateForTest(t).writeWebdExternalURL(ctx, ctrl, typed, desktopPort))

			cm, err := typed.CoreV1().ConfigMaps(cloud.WebdServiceNamespace).
				Get(ctx, spiceboxv1alpha1.WebdExternalURLConfigMap, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, tc.wantURL, cm.Data[spiceboxv1alpha1.WebdTrustedURLKey])
			assert.Equal(t, tc.wantURL, cm.Data[spiceboxv1alpha1.WebdSandboxURLKey],
				"the desktop has one origin; both keys move together or neither does")
		})
	}
}

// TestWriteWebdExternalURL_SurfacesAListFailureRatherThanWriting: "I could not
// tell who owns this" must not read as "nobody does", because that answer is
// the one that writes over a live public URL. Only reachable once the claim is
// present, since that is the only branch that asks.
func TestWriteWebdExternalURL_SurfacesAListFailureRatherThanWriting(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("apiserver said no")
	ctrl := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()
	typed := typedfake.NewSimpleClientset(seededExternalURLConfigMap(true))

	err := stateForTest(t).writeWebdExternalURL(ctx, ctrl, typed, desktopPort)
	require.Error(t, err)
	assert.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), spiceboxv1alpha1.WebdExternalURLConfigMap,
		"the error must name what could not be decided")

	cm, cmErr := typed.CoreV1().ConfigMaps(cloud.WebdServiceNamespace).
		Get(ctx, spiceboxv1alpha1.WebdExternalURLConfigMap, metav1.GetOptions{})
	require.NoError(t, cmErr)
	assert.Equal(t, installSeedURL, cm.Data[spiceboxv1alpha1.WebdTrustedURLKey],
		"an undecidable owner must leave the value untouched")
}

// TestWriteWebdExternalURL_CreatesTheConfigMapWhenInstallLeftNone keeps the
// self-contained get-or-create branch honest: a desktop whose install did not
// seed the ConfigMap must still end up with a reachable address.
func TestWriteWebdExternalURL_CreatesTheConfigMapWhenInstallLeftNone(t *testing.T) {
	ctx := context.Background()
	ctrl := ctrlfake.NewClientBuilder().WithScheme(kube.Scheme).Build()
	var typed kubernetes.Interface = typedfake.NewSimpleClientset()

	require.NoError(t, stateForTest(t).writeWebdExternalURL(ctx, ctrl, typed, desktopPort))

	cm, err := typed.CoreV1().ConfigMaps(cloud.WebdServiceNamespace).
		Get(ctx, spiceboxv1alpha1.WebdExternalURLConfigMap, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, desktopURL, cm.Data[spiceboxv1alpha1.WebdTrustedURLKey])
}
