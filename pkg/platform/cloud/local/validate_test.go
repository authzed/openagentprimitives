package local_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
)

func nodeWithProviderID(id string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       corev1.NodeSpec{ProviderID: id},
	}
}

func TestLocalValidateAcceptsLocalClusters(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		context string
	}{
		{"loopback IPv4: accepted", "https://127.0.0.1:6443", "whatever"},
		{"loopback IPv6: accepted", "https://[::1]:6443", "whatever"},
		{"localhost: accepted", "https://localhost:6443", "whatever"},
		{"docker-desktop well-known host: accepted", "https://kubernetes.docker.internal:6443", "x"},
		{"*.local mDNS host: accepted", "https://my-box.local:6443", "x"},
		{"kind- context prefix: accepted", "https://10.0.0.5:6443", "kind-ap"},
		{"k3d- context prefix: accepted", "https://10.0.0.5:6443", "k3d-ap"},
		{"minikube context: accepted", "https://10.0.0.5:6443", "minikube"},
		{"minikube- context prefix: accepted", "https://10.0.0.5:6443", "minikube-extras"},
		{"docker-desktop context: accepted", "https://10.0.0.5:6443", "docker-desktop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cloud.MustFor(cloud.KeyLocal).Validate(context.Background(), cloud.ValidateParams{
				Clients:     cloud.Clients{Typed: fake.NewSimpleClientset()},
				RESTConfig:  &rest.Config{Host: tc.host},
				ContextName: tc.context,
			})
			assert.NoError(t, err)
		})
	}
}

// TestLocalValidateAcceptsNilClientset pins pkg/platform/cloud/validate.go's
// DetectedManagedKey guard (`p.Clients.Typed == nil` -> ("", nil), "nothing
// to inspect; not an inconsistency"): a caller that supplies no typed
// clientset at all must not be refused on the managed-cloud axis — there is
// nothing to compare against, so nothing to refuse. The host still has to
// look local on its own merits (this does not relax the host heuristic).
func TestLocalValidateAcceptsNilClientset(t *testing.T) {
	err := cloud.MustFor(cloud.KeyLocal).Validate(context.Background(), cloud.ValidateParams{
		Clients:     cloud.Clients{}, // Typed is a genuine nil interface, not a fake clientset
		RESTConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "whatever",
	})
	assert.NoError(t, err, "nil Typed means nothing to compare against, not a managed-cloud match")
}

func TestLocalValidateRefusesNonLocalClusters(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		context string
		nodes   []runtime.Object
		wantErr string
	}{
		{
			name:    "public host, unrecognized context: refused with the override hint",
			host:    "https://api.prod.example.com:6443",
			context: "prod",
			wantErr: "doesn't match a local-cluster pattern",
		},
		{
			name:    "empty server URL: refused",
			host:    "",
			context: "x",
			wantErr: "no cluster configured",
		},
		{
			name:    "malformed URL: refused",
			host:    "://not-a-url",
			context: "x",
			wantErr: "couldn't parse server URL",
		},
		{
			name:    "GKE providerID on a loopback host: refused despite passing the host heuristic",
			host:    "https://127.0.0.1:6443",
			context: "kind-ap",
			nodes:   []runtime.Object{nodeWithProviderID("gce://p/us-central1-a/n1")},
			wantErr: "gke",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cloud.MustFor(cloud.KeyLocal).Validate(context.Background(), cloud.ValidateParams{
				Clients:     cloud.Clients{Typed: fake.NewSimpleClientset(tc.nodes...)},
				RESTConfig:  &rest.Config{Host: tc.host},
				ContextName: tc.context,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestLocalValidateAllowOverrideRelaxesTheHostHeuristicOnly(t *testing.T) {
	base := cloud.ValidateParams{
		Clients:       cloud.Clients{Typed: fake.NewSimpleClientset()},
		RESTConfig:    &rest.Config{Host: "https://api.corp-dev.example.com:6443"},
		ContextName:   "corp-dev",
		AllowOverride: true,
	}
	assert.NoError(t, cloud.MustFor(cloud.KeyLocal).Validate(context.Background(), base),
		"--allow-non-local-cluster exists for corporate dev clusters on a VPN")

	// But it must NOT relax the managed-cloud refusal: the local profile installs
	// sqlite + in-memory SpiceDB + a file:// PVC, which is wrong on a real cloud
	// regardless of how the user reached it.
	managed := base
	managed.Clients = cloud.Clients{Typed: fake.NewSimpleClientset(nodeWithProviderID("gce://p/us-central1-a/n1"))}
	err := cloud.MustFor(cloud.KeyLocal).Validate(context.Background(), managed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gke")
}
