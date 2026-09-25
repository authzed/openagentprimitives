package cloud

import (
	"testing"

	"github.com/stretchr/testify/assert"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func sc(provisioner, diskType string) *storagev1.StorageClass {
	s := &storagev1.StorageClass{Provisioner: provisioner}
	if diskType != "" {
		s.Parameters = map[string]string{"type": diskType}
	}
	return s
}

func scWith(provisioner string, params map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{Provisioner: provisioner, Parameters: params}
}

func TestStorageFloor(t *testing.T) {
	four := resource.MustParse("4Gi")
	tenGi := resource.MustParse("10Gi")
	oneTi := resource.MustParse("1Ti")
	cases := []struct {
		name      string
		sc        *storagev1.StorageClass
		wantFloor resource.Quantity
		wantKnown bool
	}{
		{"gke hyperdisk-balanced -> 4Gi", sc("pd.csi.storage.gke.io", "hyperdisk-balanced"), four, true},
		{"gke hyperdisk-throughput -> 4Gi (prefix)", sc("pd.csi.storage.gke.io", "hyperdisk-throughput"), four, true},
		{"gke pd-ssd -> unknown", sc("pd.csi.storage.gke.io", "pd-ssd"), resource.Quantity{}, false},
		{"azure managed disk -> 4Gi", sc("disk.csi.azure.com", "Premium_LRS"), four, true},
		{"aws ebs -> unknown", sc("ebs.csi.aws.com", "gp3"), resource.Quantity{}, false},
		{"local-path -> unknown", sc("rancher.io/local-path", ""), resource.Quantity{}, false},
		{"nil sc -> unknown", nil, resource.Quantity{}, false},
		// GKE Filestore multishare: shares are carved from a shared backing
		// instance with a 10 GiB minimum share size (a sub-10Gi request fails
		// provisioning with "less than minimum share size").
		{"filestore multishare -> 10Gi", scWith("filestore.csi.storage.gke.io", map[string]string{"multishare": "true"}), tenGi, true},
		{"filestore multishare + tier -> 10Gi", scWith("filestore.csi.storage.gke.io", map[string]string{"multishare": "true", "tier": "enterprise"}), tenGi, true},
		// GKE Filestore dedicated (non-multishare): each PVC is its own Filestore
		// instance with a 1 TiB minimum.
		{"filestore dedicated -> 1Ti", scWith("filestore.csi.storage.gke.io", map[string]string{"tier": "enterprise"}), oneTi, true},
		{"filestore multishare=false -> 1Ti", scWith("filestore.csi.storage.gke.io", map[string]string{"multishare": "false"}), oneTi, true},
		{"filestore no params -> 1Ti", scWith("filestore.csi.storage.gke.io", nil), oneTi, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := StorageFloor(tc.sc)
			assert.Equal(t, tc.wantKnown, known)
			if tc.wantKnown {
				assert.Truef(t, got.Cmp(tc.wantFloor) == 0, "floor = %s, want %s", got.String(), tc.wantFloor.String())
			}
		})
	}
}
