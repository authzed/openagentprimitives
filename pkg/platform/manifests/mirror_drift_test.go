package manifests_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// TestMirrorDriftGuard fails if the core bundle gains a public image not in
// apimage.DependencyImages — so --mirror-dependencies never silently misses an
// image. Scans the always/commonly-applied sources (Install + the component
// groups). The cloud-aware cert-manager/envoy groups are intentionally out of
// scope (excluded from DependencyImages).
func TestMirrorDriftGuard(t *testing.T) {
	deps := map[string]bool{}
	for _, d := range apimage.DependencyImages {
		deps[d] = true
	}
	firstParty := map[string]bool{}
	for _, im := range apimage.All {
		firstParty[im.LocalRef()] = true
	}

	docBytes := [][]byte{manifests.Install}
	for _, fn := range []func() ([][]byte, error){
		manifests.ChannelsD, manifests.NATS,
		manifests.Postgres, manifests.Neo4j, manifests.Graphiti,
	} {
		grp, err := fn()
		require.NoError(t, err)
		docBytes = append(docBytes, grp...)
	}

	for _, b := range docBytes {
		objs, err := manifests.Split(b)
		require.NoError(t, err)
		for _, u := range objs {
			for _, img := range containerImages(u) {
				if firstParty[img] {
					continue
				}
				assert.Truef(t, deps[img],
					"public image %q in the bundle is not in apimage.DependencyImages — add it (or it won't be mirrored by --mirror-dependencies)", img)
			}
		}
	}
}

// containerImages returns every container/initContainer image in a workload doc.
func containerImages(u *unstructured.Unstructured) []string {
	var out []string
	for _, path := range [][]string{
		{"spec", "template", "spec", "containers"},
		{"spec", "template", "spec", "initContainers"},
	} {
		cs, found, _ := unstructured.NestedSlice(u.Object, path...)
		if !found {
			continue
		}
		for _, c := range cs {
			if cm, ok := c.(map[string]any); ok {
				if img, ok := cm["image"].(string); ok && img != "" {
					out = append(out, img)
				}
			}
		}
	}
	return out
}
