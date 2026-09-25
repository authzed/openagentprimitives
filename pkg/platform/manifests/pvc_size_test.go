package manifests

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// minBundledPVCSize is the floor every bundled PVC request must clear. GKE
// hyperdisk-balanced (and Azure managed disks) reject a sub-4Gi disk with a
// non-retryable InvalidArgument, which leaves an unbound PVC Pending forever and
// stalls `oap install` on any cluster whose default StorageClass sits on such a
// disk type. Keeping every bundled PVC at or above 4Gi keeps it provisionable on
// every supported cloud's default class.
var minBundledPVCSize = resource.MustParse("4Gi")

// TestBundledPVCsMeetDiskFloor is the regression for the spicebox-operator-memory
// PVC shipping at 1Gi — below hyperdisk-balanced's 4GB floor — which wedged the
// operator Deployment (Pending/Pending) and stalled install on GKE. Every PVC and
// volumeClaimTemplate across the embedded install bundle and the stateful
// component bundles must request >= 4Gi.
func TestBundledPVCsMeetDiskFloor(t *testing.T) {
	docs := allBundledDocs(t)

	checked := 0
	assertSize := func(name, raw string) {
		q, err := resource.ParseQuantity(raw)
		require.NoErrorf(t, err, "PVC %s: unparseable storage request %q", name, raw)
		assert.GreaterOrEqualf(t, q.Cmp(minBundledPVCSize), 0,
			"bundled PVC %s requests %s, below the %s cloud disk floor (hyperdisk-balanced rejects sub-4Gi and the PVC never binds); "+
				"raise it in config/ (or the component yaml) and run `mage manifests`",
			name, raw, minBundledPVCSize.String())
		checked++
	}

	for _, d := range docs {
		switch d.GetKind() {
		case "PersistentVolumeClaim":
			if raw, found, err := nestedString(d.Object, "spec", "resources", "requests", "storage"); err == nil && found {
				assertSize(d.GetName(), raw)
			}
		case "StatefulSet":
			tmpls, found, err := nestedSlice(d.Object, "spec", "volumeClaimTemplates")
			if err != nil || !found {
				continue
			}
			for _, raw := range tmpls {
				tmpl, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name := d.GetName()
				if n, f, _ := nestedString(tmpl, "metadata", "name"); f {
					name = d.GetName() + "/" + n
				}
				if sz, found, err := nestedString(tmpl, "spec", "resources", "requests", "storage"); err == nil && found {
					assertSize(name, sz)
				}
			}
		}
	}

	assert.Positive(t, checked, "expected at least one bundled PVC/volumeClaimTemplate to check; did the bundle structure change?")
}

// allBundledDocs returns every object across the base install bundle and the
// stateful component bundles that oap install applies (the ones that carry PVCs).
func allBundledDocs(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	var all []*unstructured.Unstructured

	base, err := Split(Install)
	require.NoError(t, err)
	all = append(all, base...)

	for _, comp := range []func() ([][]byte, error){Postgres, Neo4j} {
		parts, err := comp()
		require.NoError(t, err)
		for _, b := range parts {
			docs, err := Split(b)
			require.NoError(t, err)
			all = append(all, docs...)
		}
	}
	return all
}
