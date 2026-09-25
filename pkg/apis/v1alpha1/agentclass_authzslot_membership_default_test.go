package v1alpha1_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	. "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestAuthzSlotMembershipDefault_MatchesGeneratedCRD pins
// AuthzSlotMembershipDefault against the CRD kubebuilder actually generated
// for AuthzSlot.Membership's `+kubebuilder:default` marker, so the two cannot
// drift apart. install.Install reads the constant to complete a bundle's
// slots before the SSA-apply loop (see pkg/platform/oap/install/apply.go): if
// the marker ever changed without the constant following, the installer
// would write a value the apiserver no longer defaults to, and a re-install
// would stop being a true SSA no-op again.
//
// loadCRD is defined in relationshipsource_validation_test.go (same package);
// parsing the structured CRD rather than grepping a line keeps this robust to
// the generated file's own reflow.
func TestAuthzSlotMembershipDefault_MatchesGeneratedCRD(t *testing.T) {
	crd := loadCRD(t, "agentclasses")
	require.NotEmpty(t, crd.Spec.Versions, "CRD must declare at least one version")

	slots := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].Properties["authz"].Properties["slots"]
	require.NotNil(t, slots.Items, "slots must declare an items schema")
	require.NotNil(t, slots.Items.Schema, "slots.items must be a single schema, not a tuple")

	prop := slots.Items.Schema.Properties["membership"]
	require.NotNil(t, prop.Default, "generated CRD must carry a default for AuthzSlot.Membership")

	var got string
	require.NoError(t, json.Unmarshal(prop.Default.Raw, &got), "decode membership default")

	assert.Equal(t, AuthzSlotMembershipDefault, got,
		"AuthzSlotMembershipDefault must mirror AuthzSlot.Membership's +kubebuilder:default marker")
}
