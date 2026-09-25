package unmanaged

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

// Ported from pkg/platform/cloud/local/local_test.go's TestWorkspaceStorageResolve,
// retargeted at unmanagedWorkspaceStorage — Resolve was copied byte-identical
// from local into unmanaged, and unmanaged is what every on-prem/bare-metal/
// kind install actually resolves to today, so it carries the risk local no
// longer does. An internal (package unmanaged) test since
// unmanagedWorkspaceStorage is unexported.

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
			ws := unmanagedWorkspaceStorage{}
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
