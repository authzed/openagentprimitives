package local

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// ---- Strategy facts ----

func TestStrategyFacts(t *testing.T) {
	s := Strategy{}
	assert.Equal(t, cloud.KeyLocal, s.Key())
	assert.Equal(t, "local development cluster", s.DisplayName())
	assert.False(t, s.IsManaged())
	assert.Equal(t, "", s.ProviderIDPrefix(),
		"local must report no providerID prefix so cloud.Detect can never select it")
	assert.Nil(t, s.DNSEgressCIDRs())
	assert.Nil(t, s.GatewayBackendIngressCIDRs())
	dl, eta := s.GatewayAddressWait()
	assert.Equal(t, 8*time.Minute, dl)
	assert.Equal(t, 7*time.Minute, eta)
	assert.Equal(t, "", s.RegistryFromProviderID("anything"))
	assert.Equal(t, "", s.ProjectFromProviderID("anything"))
	assert.NotNil(t, s.TLS())
	assert.Equal(t, "cert-manager", s.TLS().Name())
	assert.NotNil(t, s.WorkspaceStorage())
	assert.Equal(t, cloud.DevProfile, s.InstallProfile())
}

// Validate's own tests live in validate_test.go, alongside the method — it
// now enforces both the managed-providerID refusal and the kubeconfig
// host-pattern heuristic as a single method, so its test table lives
// entirely in that one file rather than split across two.

// ---- workspaceStorage.Resolve ----

func nfsStorageClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: "nfs.csi.k8s.io",
	}
}

func TestWorkspaceStorageResolve(t *testing.T) {
	cases := []struct {
		name        string
		objs        []runtime.Object
		wantClass   string
		wantBundled bool
		wantVerify  cloud.WorkspaceVerify
	}{
		{
			name:        "known RWX class present → native class, ProbeBeforeUse",
			objs:        []runtime.Object{nfsStorageClass("nfs-client")},
			wantClass:   "nfs-client",
			wantBundled: false,
			wantVerify:  cloud.WorkspaceProbeBeforeUse,
		},
		{
			name:        "no RWX class → bundled NeedsBundled ProbeBeforeUse",
			objs:        nil,
			wantClass:   cloud.BundledWorkspaceStorageClass,
			wantBundled: true,
			wantVerify:  cloud.WorkspaceProbeBeforeUse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			ws := localWorkspaceStorage{}
			got, err := ws.Resolve(context.Background(), cloud.WorkspaceParams{
				Clients:  cloud.Clients{Typed: kc},
				Reporter: cloud.NopReporter{},
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantClass, got.ClassName, "ClassName")
			assert.Equal(t, tc.wantBundled, got.NeedsBundled, "NeedsBundled")
			assert.Equal(t, tc.wantVerify, got.Verify, "Verify")
			assert.False(t, got.Degraded, "should not be degraded")
		})
	}
}
