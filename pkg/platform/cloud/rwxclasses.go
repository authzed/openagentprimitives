package cloud

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// bundledWorkspaceProvisioner is the provisioner name of the bundled local-path
// workspace provisioner (see config/workspace-provisioner/). Its StorageClass
// is named BundledWorkspaceStorageClass; it hands out ReadWriteOnce,
// node-pinned volumes despite the "-rwx" in that name.
const bundledWorkspaceProvisioner = "cluster.local/ap-workspace-provisioner"

// filestoreProvisionerName is the GKE Filestore CSI driver — the managed,
// genuinely cross-node RWX backend.
const filestoreProvisionerName = "filestore.csi.storage.gke.io"

// RWXClassInfo describes one StorageClass the workspace picker may offer, with
// the classification the picker needs to label it. It is derived purely from
// the StorageClass's provisioner and parameters — no live provisioning is done.
type RWXClassInfo struct {
	Name        string
	Provisioner string
	// Bundled is the bundled local-path provisioner: RWO and node-pinned, so a
	// session's pods can only ever schedule on the one node its PVs landed on.
	// Included because it is the workspace default, NOT because it is truly RWX.
	Bundled bool
	// Filestore is a GKE Filestore CSI class: managed, cross-node RWX.
	Filestore bool
	// Multishare is a Filestore class with parameters.multishare=true: one
	// Filestore instance shared across many PVCs. A non-multishare Filestore
	// class provisions a dedicated ≥1 TiB instance per claim.
	Multishare bool
}

// ListRWXClasses returns every StorageClass on the cluster that can back a
// shared workspace: any class whose provisioner is a known RWX driver, plus the
// bundled local-path class (included as the workspace default even though it is
// node-pinned). Block-storage classes (pd.csi, gce-pd, …) are excluded because
// they cannot be mounted ReadWriteMany. The result preserves the API's list
// order so the picker's numbering is stable.
func ListRWXClasses(ctx context.Context, kc kubernetes.Interface) ([]RWXClassInfo, error) {
	scs, err := kc.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list storage classes: %w", err)
	}
	var out []RWXClassInfo
	for i := range scs.Items {
		sc := &scs.Items[i]
		bundled := sc.Provisioner == bundledWorkspaceProvisioner
		if !bundled && !knownRWXProvisioners[sc.Provisioner] {
			continue
		}
		info := RWXClassInfo{
			Name:        sc.Name,
			Provisioner: sc.Provisioner,
			Bundled:     bundled,
			Filestore:   sc.Provisioner == filestoreProvisionerName,
		}
		if info.Filestore && sc.Parameters["multishare"] == "true" {
			info.Multishare = true
		}
		out = append(out, info)
	}
	return out, nil
}
