// pkg/controllers/relationshipsource/crdbounds_test.go
package relationshipsource

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// loadRelationshipSourceCRD reads the GENERATED CRD YAML (config/crds/…,
// produced by `mage gen:api` from the +kubebuilder:validation markers on
// RelationshipSourceScopeError) and decodes it into the real
// CustomResourceDefinition type, the same pattern
// pkg/apis/v1alpha1/relationshipsource_validation_test.go's loadCRD and
// pkg/platform/manifests use for asserting against generated manifests rather
// than regexing the Go source: the marker is the source of truth, the
// generated file is what the API server actually enforces, and only the
// generated file proves the two still agree.
func loadRelationshipSourceCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	path := "../../../config/crds/agentprimitives.authzed.com_relationshipsources.yaml"
	data, err := os.ReadFile(path)
	require.NoError(t, err, "generated CRD must exist at %s (run mage gen:api)", path)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd), "decode generated CRD YAML at %s", path)
	return &crd
}

// TestCRDLengthConstantsMatchGeneratedMarkers proves crdScopeMaxLength and
// crdMessageMaxLength (scopeerrors.go) still mirror the
// +kubebuilder:validation:MaxLength markers on RelationshipSourceScopeError's
// Scope and Message fields, by reading them back out of the GENERATED CRD
// rather than the Go struct tags themselves — regex-parsing the marker
// comment out of relationshipsource_types.go was rejected for the same reason
// every other mirrored-constant check in this repo reads the generated
// artifact: the generated file is what the API server enforces, and a
// constant that silently drifted from the marker (this shipped once, and was
// caught only in review) makes every status write that reaches the old,
// larger bound rejected WHOLE by the API server, which retry-loops the
// controller against the upstream it just synced. See scopeerrors.go's own
// comment on the two constants for the failure mode.
func TestCRDLengthConstantsMatchGeneratedMarkers(t *testing.T) {
	crd := loadRelationshipSourceCRD(t)
	require.NotEmpty(t, crd.Spec.Versions, "CRD must declare at least one version")

	items := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["status"].Properties["sync"].Properties["lastPass"].Properties["scopeErrorSamples"].Items
	require.NotNil(t, items, "status.sync.lastPass.scopeErrorSamples.items must be set")
	require.NotNil(t, items.Schema, "scopeErrorSamples.items must be a single schema, not an array of schemas")

	scopeProp := items.Schema.Properties["scope"]
	msgProp := items.Schema.Properties["message"]

	require.NotNil(t, scopeProp.MaxLength, "RelationshipSourceScopeError.Scope must carry +kubebuilder:validation:MaxLength")
	require.NotNil(t, msgProp.MaxLength, "RelationshipSourceScopeError.Message must carry +kubebuilder:validation:MaxLength")

	assert.Equal(t, int64(crdScopeMaxLength), *scopeProp.MaxLength,
		fmt.Sprintf("crdScopeMaxLength (scopeerrors.go) must mirror RelationshipSourceScopeError.Scope's "+
			"+kubebuilder:validation:MaxLength marker (currently %d in the generated CRD); update the constant "+
			"and re-run `mage gen:api && mage manifests` if the marker changed", *scopeProp.MaxLength))
	assert.Equal(t, int64(crdMessageMaxLength), *msgProp.MaxLength,
		fmt.Sprintf("crdMessageMaxLength (scopeerrors.go) must mirror RelationshipSourceScopeError.Message's "+
			"+kubebuilder:validation:MaxLength marker (currently %d in the generated CRD); update the constant "+
			"and re-run `mage gen:api && mage manifests` if the marker changed", *msgProp.MaxLength))
}
