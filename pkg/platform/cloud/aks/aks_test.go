package aks

import (
	"context"
	"testing"

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
	assert.Equal(t, "aks", s.Key())
	assert.Equal(t, "AKS", s.DisplayName())
	assert.True(t, s.IsManaged())
	assert.Equal(t, "azure://", s.ProviderIDPrefix())
	assert.Nil(t, s.DNSEgressCIDRs())
	assert.Nil(t, s.GatewayBackendIngressCIDRs())
	assert.Equal(t, "", s.RegistryFromProviderID("azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm"))
	assert.Equal(t, "", s.ProjectFromProviderID("azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm"))
	assert.NotNil(t, s.TLS())
	assert.Equal(t, "cert-manager", s.TLS().Name())
	assert.NotNil(t, s.WorkspaceStorage())
}

// ---- workspaceStorage.Resolve ----

func azureFilesStorageClass(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: "file.csi.azure.com",
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
			name:        "Azure Files CSI StorageClass present → native class, ProbeBeforeUse",
			objs:        []runtime.Object{azureFilesStorageClass("azurefile-csi")},
			wantClass:   "azurefile-csi",
			wantBundled: false,
			wantVerify:  cloud.WorkspaceProbeBeforeUse,
		},
		{
			name:        "no Azure Files class → bundled NeedsBundled ProbeBeforeUse",
			objs:        nil,
			wantClass:   cloud.BundledWorkspaceStorageClass,
			wantBundled: true,
			wantVerify:  cloud.WorkspaceProbeBeforeUse,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			ws := aksWorkspaceStorage{}
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
