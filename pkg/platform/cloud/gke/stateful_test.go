package gke

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func sc(name, provisioner, typ string, isDefault bool) *storagev1.StorageClass {
	s := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
	}
	if typ != "" {
		s.Parameters = map[string]string{"type": typ}
	}
	if isDefault {
		s.Annotations = map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	return s
}

func node(name, machine string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{"node.kubernetes.io/instance-type": machine},
	}}
}

func TestIsHyperdiskOnlyMachineType(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"n4-standard-4", true},
		{"c4-standard-8", true},
		{"c4a-highmem-4", true},
		{"n2-standard-4", false},
		{"e2-medium", false},
		{"n1-standard-1", false},
		{"", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, isHyperdiskOnlyMachineType(tc.in))
		})
	}
}

func TestProvisionsPDAndHyperdisk(t *testing.T) {
	pd := &storagev1.StorageClass{
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "pd-balanced"},
	}
	hd := &storagev1.StorageClass{
		Provisioner: "pd.csi.storage.gke.io",
		Parameters:  map[string]string{"type": "hyperdisk-balanced"},
	}
	other := &storagev1.StorageClass{Provisioner: "efs.csi.aws.com"}
	assert.True(t, provisionsPD(pd))
	assert.False(t, provisionsPD(hd))
	assert.False(t, provisionsPD(other))
	assert.False(t, provisionsPD(nil))
	assert.True(t, provisionsHyperdisk(hd))
	assert.False(t, provisionsHyperdisk(pd))
	assert.False(t, provisionsHyperdisk(nil))
}

func TestHasHyperdiskOnlyNode(t *testing.T) {
	cases := []struct {
		name string
		objs []runtime.Object
		want bool
	}{
		{"all n4 → true", []runtime.Object{node("a", "n4-standard-4")}, true},
		{"mixed, one n4 → true", []runtime.Object{node("a", "n2-standard-4"), node("b", "c4-standard-8")}, true},
		{"all n2 → false", []runtime.Object{node("a", "n2-standard-4")}, false},
		{"no nodes → false", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			got, err := hasHyperdiskOnlyNode(context.Background(), kc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildHyperdiskStorageClass(t *testing.T) {
	sc := buildHyperdiskStorageClass()
	assert.Equal(t, statefulHyperdiskClassName, sc.Name)
	assert.Equal(t, "pd.csi.storage.gke.io", sc.Provisioner)
	assert.Equal(t, "hyperdisk-balanced", sc.Parameters["type"])
	require.NotNil(t, sc.VolumeBindingMode)
	assert.Equal(t, storagev1.VolumeBindingWaitForFirstConsumer, *sc.VolumeBindingMode)
	assert.Equal(t, "stateful-storage", sc.Labels["agentprimitives.authzed.com/install-tier"])
}

func TestGKEStatefulResolve(t *testing.T) {
	cases := []struct {
		name       string
		objs       []runtime.Object
		wantClass  string
		wantCreate bool
		wantProbe  bool
	}{
		{
			name: "no default class → trust default (noop)",
			objs: []runtime.Object{node("a", "n4-standard-4")},
		},
		{
			name: "default not PD (hyperdisk already) → noop",
			objs: []runtime.Object{node("a", "n4-standard-4"), sc("hd", "pd.csi.storage.gke.io", "hyperdisk-balanced", true)},
		},
		{
			name: "PD default + n2 nodes → noop",
			objs: []runtime.Object{node("a", "n2-standard-4"), sc("std", "pd.csi.storage.gke.io", "pd-balanced", true)},
		},
		{
			name:       "PD default + n4 nodes + no hyperdisk class → create",
			objs:       []runtime.Object{node("a", "n4-standard-4"), sc("std", "pd.csi.storage.gke.io", "pd-balanced", true)},
			wantClass:  statefulHyperdiskClassName,
			wantCreate: true,
			wantProbe:  true,
		},
		{
			name:      "PD default + n4 nodes + existing hyperdisk class → reuse",
			objs:      []runtime.Object{node("a", "n4-standard-4"), sc("std", "pd.csi.storage.gke.io", "pd-balanced", true), sc("hd-existing", "pd.csi.storage.gke.io", "hyperdisk-balanced", false)},
			wantClass: "hd-existing",
			wantProbe: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			dec, err := gkeStatefulStorage{}.Resolve(context.Background(), cloud.StatefulParams{
				Clients:  cloud.Clients{Typed: kc},
				Reporter: cloud.NopReporter{},
			})
			require.NoError(t, err)
			assert.Equal(t, tc.wantClass, dec.ClassName)
			assert.Equal(t, tc.wantCreate, dec.CreateClass != nil)
			assert.Equal(t, tc.wantProbe, dec.Probe)
			if tc.wantCreate {
				assert.Equal(t, dec.ClassName, dec.CreateClass.Name)
			}
		})
	}
}
