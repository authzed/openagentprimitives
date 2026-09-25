package gke

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

const (
	// gcePDCSIProvisioner is the GKE Persistent Disk CSI driver. It provisions
	// BOTH legacy PD (type: pd-*) and Hyperdisk (type: hyperdisk-*); the `type`
	// parameter distinguishes them.
	gcePDCSIProvisioner = "pd.csi.storage.gke.io"
	// statefulHyperdiskClassName is the StorageClass oap creates (and oap clean
	// removes) when steering the bundled stateful PVCs onto Hyperdisk.
	statefulHyperdiskClassName = "ap-stateful-hyperdisk"
	// statefulInstallTier labels the created class so oap clean can find it.
	statefulInstallTier = cloud.InstallTierStatefulStorage
	// nodeInstanceTypeLabel is the well-known node label carrying the machine
	// type (e.g. "n4-standard-4"), set by GKE on Standard and Autopilot nodes.
	nodeInstanceTypeLabel = "node.kubernetes.io/instance-type"
)

// hyperdiskOnlyFamilies is the set of GCE machine families that support ONLY
// Hyperdisk, not legacy Persistent Disk. Attaching a pd-* volume to one of
// these fails with "pd-balanced disk type cannot be used by <type> machine
// type". This list is the detection TRIGGER; the provisioning probe is the
// ground-truth confirmation. A family missing here is a false negative that
// degrades to the existing install-wait FailedAttachVolume diagnostic — never
// worse — so extend conservatively as Google ships new families.
var hyperdiskOnlyFamilies = map[string]bool{
	"n4": true, "c4": true, "c4a": true, "c4d": true,
	"m4": true, "x4": true, "z3": true, "h3": true,
}

// isHyperdiskOnlyMachineType reports whether instanceType (e.g. "n4-standard-4")
// belongs to a Hyperdisk-only GCE machine family. The family is the substring
// before the first "-".
func isHyperdiskOnlyMachineType(instanceType string) bool {
	family, _, ok := strings.Cut(instanceType, "-")
	if !ok {
		return false
	}
	return hyperdiskOnlyFamilies[family]
}

// provisionsPD reports whether sc provisions legacy Persistent Disk via the GKE
// PD CSI driver (provisioner pd.csi.storage.gke.io with a pd-* type).
func provisionsPD(sc *storagev1.StorageClass) bool {
	if sc == nil || sc.Provisioner != gcePDCSIProvisioner {
		return false
	}
	return strings.HasPrefix(sc.Parameters["type"], "pd-")
}

// provisionsHyperdisk reports whether sc provisions Hyperdisk via the GKE PD CSI
// driver (provisioner pd.csi.storage.gke.io with a hyperdisk-* type).
func provisionsHyperdisk(sc *storagev1.StorageClass) bool {
	if sc == nil || sc.Provisioner != gcePDCSIProvisioner {
		return false
	}
	return strings.HasPrefix(sc.Parameters["type"], "hyperdisk-")
}

// hasHyperdiskOnlyNode reports whether any node in the cluster is a
// Hyperdisk-only machine family.
func hasHyperdiskOnlyNode(ctx context.Context, kc kubernetes.Interface) (bool, error) {
	nodes, err := kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for i := range nodes.Items {
		if isHyperdiskOnlyMachineType(nodes.Items[i].Labels[nodeInstanceTypeLabel]) {
			return true, nil
		}
	}
	return false, nil
}

// buildHyperdiskStorageClass returns the StorageClass oap creates to steer the
// bundled stateful PVCs onto Hyperdisk. It is NOT marked default — oap pins the
// two PVCs via storageClassName and leaves the cluster default untouched. The
// install-tier label lets oap clean remove it.
func buildHyperdiskStorageClass() *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:   statefulHyperdiskClassName,
			Labels: map[string]string{cloud.InstallTierLabelKey: statefulInstallTier},
		},
		Provisioner:          gcePDCSIProvisioner,
		Parameters:           map[string]string{"type": "hyperdisk-balanced"},
		VolumeBindingMode:    ptr.To(storagev1.VolumeBindingWaitForFirstConsumer),
		AllowVolumeExpansion: ptr.To(true),
		ReclaimPolicy:        ptr.To(corev1.PersistentVolumeReclaimDelete),
	}
}

// gkeStatefulStorage is the GKE cloud.StatefulStorage implementation.
type gkeStatefulStorage struct{}

// Resolve steers the bundled stateful PVCs onto Hyperdisk when (and only when)
// the cluster's default StorageClass provisions legacy Persistent Disk but the
// node pool is a Hyperdisk-only machine family — the combination that binds the
// PVC yet can never attach it. Every other case trusts the cluster default.
func (gkeStatefulStorage) Resolve(ctx context.Context, p cloud.StatefulParams) (cloud.StatefulDecision, error) {
	def, err := cloud.DefaultStorageClass(ctx, p.Clients.Typed)
	if err != nil {
		return cloud.StatefulDecision{}, fmt.Errorf("get default StorageClass: %w", err)
	}
	if !provisionsPD(def) {
		// No default, or the default already provisions Hyperdisk / a non-PD CSI.
		return cloud.StatefulDecision{}, nil
	}
	hyperdiskOnly, err := hasHyperdiskOnlyNode(ctx, p.Clients.Typed)
	if err != nil {
		return cloud.StatefulDecision{}, fmt.Errorf("inspect node machine types: %w", err)
	}
	if !hyperdiskOnly {
		// PD-provisioning default is fine for this node pool (e.g. n2/e2).
		return cloud.StatefulDecision{}, nil
	}

	// Default provisions PD but the nodes are Hyperdisk-only: pin our PVCs to a
	// Hyperdisk class. Reuse an existing one if present, else create ours.
	scs, err := p.Clients.Typed.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return cloud.StatefulDecision{}, fmt.Errorf("list StorageClasses: %w", err)
	}
	for i := range scs.Items {
		if provisionsHyperdisk(&scs.Items[i]) {
			name := scs.Items[i].Name
			return cloud.StatefulDecision{
				ClassName: name,
				Probe:     true,
				Message:   "GKE: default StorageClass " + def.Name + " provisions Persistent Disk, but this node pool is a Hyperdisk-only machine family (pd-* volumes bind but never attach). Pinning Postgres/Neo4j to existing Hyperdisk class " + name + ".",
			}, nil
		}
	}
	created := buildHyperdiskStorageClass()
	return cloud.StatefulDecision{
		ClassName:   created.Name,
		CreateClass: created,
		Probe:       true,
		Message:     "GKE: default StorageClass " + def.Name + " provisions Persistent Disk, but this node pool is a Hyperdisk-only machine family (pd-* volumes bind but never attach). Creating Hyperdisk StorageClass " + created.Name + " and pinning Postgres/Neo4j to it.",
	}, nil
}
