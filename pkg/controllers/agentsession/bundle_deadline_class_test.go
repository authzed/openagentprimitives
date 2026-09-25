package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A Filestore-backed workspace has a slow first-provision (the backing
// multishare instance takes ~5-10 min to create), which can exceed the default
// 8-min bundle deadline and fail a healthy session on timing alone. The
// deadline must stretch for such classes; a fast class (or none) keeps the
// default.
func TestEffectiveBundleReadyDeadline(t *testing.T) {
	cases := []struct {
		name        string
		class       string
		provisioner string
		want        time.Duration
	}{
		{"filestore multishare gets the extended deadline", "ws", "filestore.csi.storage.gke.io", filestoreBundleReadyDeadline},
		{"a fast class keeps the default", "ws", "pd.csi.storage.gke.io", bundleReadyDeadline},
		{"no configured class keeps the default", "", "", bundleReadyDeadline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(floorScheme(t))
			if tc.class != "" {
				b = b.WithObjects(storageClass(tc.class, tc.provisioner, nil))
			}
			r := &Reconciler{Client: b.Build(), WorkspaceStorageClass: tc.class}
			assert.Equal(t, tc.want, r.effectiveBundleReadyDeadline(context.Background()))
		})
	}
}
