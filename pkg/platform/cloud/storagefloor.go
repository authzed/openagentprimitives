package cloud

import (
	"strings"

	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// StorageFloor returns the known minimum provisionable disk size for sc's
// backend, and whether a floor is known. Only backends we are confident about
// return true; everything else returns (zero, false) and MUST NOT be used to
// block — we fail or clamp a PVC only when a request is genuinely infeasible.
//
// Confirmed floors: GKE hyperdisk-* rejects sub-4Gi disks at the GCE API level;
// Azure managed disks round up to 4Gi. The hyperdisk- prefix match is a
// conservative LOWER bound — other hyperdisk types have floors >= 4Gi, so this
// catches sub-4Gi requests but may under-detect exotic types with much larger
// minimums (accepted: no false positives).
//
// GKE Filestore (filestore.csi.storage.gke.io) has two shapes, distinguished by
// the class's multishare parameter:
//   - multishare=true: PVCs are carved as shares from a shared backing instance,
//     with a 10 GiB minimum share size — a sub-10Gi request fails provisioning
//     with "Request bytes … is less than minimum share size bytes 10737418240".
//   - otherwise (dedicated): each PVC is its own Filestore instance, which has a
//     1 TiB minimum. We report 1Ti so an undersized request fails loudly at
//     validation rather than after a slow provisioning rejection.
func StorageFloor(sc *storagev1.StorageClass) (resource.Quantity, bool) {
	if sc == nil {
		return resource.Quantity{}, false
	}
	four := resource.MustParse("4Gi")
	switch sc.Provisioner {
	case "pd.csi.storage.gke.io":
		if strings.HasPrefix(sc.Parameters["type"], "hyperdisk-") {
			return four, true
		}
	case "disk.csi.azure.com":
		return four, true
	case "filestore.csi.storage.gke.io":
		if sc.Parameters["multishare"] == "true" {
			return resource.MustParse("10Gi"), true
		}
		return resource.MustParse("1Ti"), true
	}
	return resource.Quantity{}, false
}
