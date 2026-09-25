package v1alpha1_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// loadCRD reads the generated CRD YAML for the given plural resource name
// (e.g. "relationshipsources") from config/crds and decodes it into the real
// CustomResourceDefinition type. There is no existing shared helper of this
// shape in the repo — other tests in this package (e.g.
// TestChannelCRD_KindEnum_IncludesLocal in channel_types_test.go) assert on
// the raw file text instead. A structured decode is cheap here (apiextensionsv1
// and sigs.k8s.io/yaml are both already in go.mod) and lets the assertion
// below walk the exact schema path rather than hoping a substring match landed
// on the right property.
func loadCRD(t *testing.T, plural string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	path := fmt.Sprintf("../../../config/crds/agentprimitives.authzed.com_%s.yaml", plural)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "generated CRD must exist at %s (run mage gen:api)", path)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd), "decode generated CRD YAML at %s", path)
	return &crd
}

// resumeAfter is a ScopeID and nothing else. It is controller-owned status,
// but the constraint is defence in depth: a malformed value must be refused
// rather than handed to a kind as a resume point.
func TestRelationshipSourceStatus_ResumeAfterIsConstrained(t *testing.T) {
	crd := loadCRD(t, "relationshipsources")
	require.NotEmpty(t, crd.Spec.Versions, "CRD must declare at least one version")

	prop := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["status"].Properties["sync"].Properties["resumeAfter"]

	assert.Equal(t, "string", prop.Type)
	require.NotNil(t, prop.MaxLength, "resumeAfter must be length-bounded")
	assert.LessOrEqual(t, *prop.MaxLength, int64(256))
}
