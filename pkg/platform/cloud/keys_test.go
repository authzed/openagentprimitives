package cloud

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeyConstantsAreDistinctAndStable(t *testing.T) {
	keys := []string{KeyLocal, KeyDesktop, KeyDefault, KeyGKE, KeyEKS, KeyAKS}
	seen := map[string]bool{}
	for _, k := range keys {
		assert.NotEmpty(t, k, "no kind key may be empty: the empty key is reserved as an error")
		assert.False(t, seen[k], "duplicate kind key %q", k)
		seen[k] = true
	}
	// The wire values are stamped into AP_CLUSTER_KIND on live Deployments and
	// read back by the operator/webd, so changing one is a breaking change to
	// an installed cluster. Pin them.
	assert.Equal(t, "local", KeyLocal)
	assert.Equal(t, "desktop", KeyDesktop)
	assert.Equal(t, "default", KeyDefault)
	assert.Equal(t, "gke", KeyGKE)
	assert.Equal(t, "eks", KeyEKS)
	assert.Equal(t, "aks", KeyAKS)
}
