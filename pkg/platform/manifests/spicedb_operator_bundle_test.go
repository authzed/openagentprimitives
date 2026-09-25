package manifests_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// TestInstallBundleIncludesSpiceDBOperator asserts the vendored operator bundle
// made it into install.yaml (CRD + controller Deployment) and that its image is
// registered for --mirror-dependencies.
func TestInstallBundleIncludesSpiceDBOperator(t *testing.T) {
	objs, err := manifests.Split(manifests.Install)
	require.NoError(t, err)

	var haveCRD, haveDeploy bool
	for _, u := range objs {
		if u.GetKind() == "CustomResourceDefinition" && u.GetName() == "spicedbclusters.authzed.com" {
			haveCRD = true
		}
		if u.GetKind() == "Deployment" && u.GetName() == "spicedb-operator" {
			haveDeploy = true
		}
	}
	assert.True(t, haveCRD, "install.yaml must include the spicedbclusters.authzed.com CRD")
	assert.True(t, haveDeploy, "install.yaml must include the spicedb-operator Deployment")

	var listed bool
	for _, d := range apimage.DependencyImages {
		if strings.Contains(d, "spicedb-operator") {
			listed = true
		}
	}
	assert.True(t, listed, "apimage.DependencyImages must include the spicedb-operator image")
}
