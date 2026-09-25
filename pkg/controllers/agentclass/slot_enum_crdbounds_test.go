package agentclass

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// loadAgentClassCRD reads the GENERATED CRD YAML (config/crds/…, produced by
// `mage gen:api` from the kubebuilder markers on AuthzSlot) and decodes it
// into the real CustomResourceDefinition type — the same pattern
// pkg/controllers/relationshipsource/crdbounds_test.go uses: the marker is
// the source of truth, the generated file is what the apiserver actually
// enforces, and only the generated file proves a hand-typed Go mirror still
// agrees with it.
func loadAgentClassCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	path := "../../../config/crds/agentprimitives.authzed.com_agentclasses.yaml"
	data, err := os.ReadFile(path)
	require.NoError(t, err, "generated CRD must exist at %s (run mage gen:api)", path)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd), "decode generated CRD YAML at %s", path)
	return &crd
}

// slotItemSchema walks to spec.authz.slots.items' schema — the single
// AuthzSlot definition every enum below hangs off. AuthzSlot is ALSO inlined
// a second time under the deprecated spec.boundEntities (CRDs cannot $ref
// share a definition across two fields), but spec.authz.slots is the live
// field and the one every enum map in slot_declaration.go actually gates.
func slotItemSchema(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.JSONSchemaProps {
	t.Helper()
	require.NotEmpty(t, crd.Spec.Versions, "CRD must declare at least one version")
	slots := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.
		Properties["spec"].Properties["authz"].Properties["slots"]
	require.NotNil(t, slots.Items, "spec.authz.slots.items must be set")
	require.NotNil(t, slots.Items.Schema, "spec.authz.slots.items must be a single schema, not an array of schemas")
	return slots.Items.Schema
}

// listEnumValues reads a LIST field's items.enum (fillFrom, autoGrantFrom —
// both `+listType=atomic` string arrays) as plain strings.
func listEnumValues(t *testing.T, slotSchema *apiextensionsv1.JSONSchemaProps, field string) []string {
	t.Helper()
	prop, ok := slotSchema.Properties[field]
	require.True(t, ok, "spec.authz.slots.items.properties[%q] must be declared", field)
	require.NotNil(t, prop.Items, "%s must be a list with an enum on its items", field)
	require.NotNil(t, prop.Items.Schema, "%s.items must be a single schema, not an array of schemas", field)
	return enumStrings(t, prop.Items.Schema.Enum)
}

// scalarEnumValues reads a SCALAR field's own enum (membership — a single
// string, not a list).
func scalarEnumValues(t *testing.T, slotSchema *apiextensionsv1.JSONSchemaProps, field string) []string {
	t.Helper()
	prop, ok := slotSchema.Properties[field]
	require.True(t, ok, "spec.authz.slots.items.properties[%q] must be declared", field)
	return enumStrings(t, prop.Enum)
}

// enumStrings decodes a JSONSchemaProps.Enum — each entry an
// apiextensionsv1.JSON carrying the raw JSON bytes of one enum value — into
// plain Go strings.
func enumStrings(t *testing.T, enum []apiextensionsv1.JSON) []string {
	t.Helper()
	out := make([]string, 0, len(enum))
	for _, e := range enum {
		var s string
		require.NoError(t, json.Unmarshal(e.Raw, &s), "enum value %q must decode as a JSON string", e.Raw)
		out = append(out, s)
	}
	return out
}

// mapKeys returns a set's members as a slice, for ElementsMatch comparison
// against a CRD enum (which cares about the value set, not the map itself).
func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSlotEnumMirrors_MatchTheGeneratedCRD proves the three reconciler-side
// enum maps in slot_declaration.go — validFillFrom, validAutoGrantFrom,
// validMembership — carry EXACTLY the values the CRD's kubebuilder markers
// on AuthzSlot admit, no more and no fewer.
//
// Each is a deliberate defense-in-depth duplicate of a CRD enum (see
// slot_declaration.go's own comment on why the reconciler checks again after
// the apiserver already has), and nothing before this test ever compared
// them against the generated schema. validFillFrom desynced from the CRD
// marker inside the very fix wave this test belongs to: the marker gained
// "trigger", the reconciler's hand-typed map did not, and a class naming it
// was refused at every reconcile with no path to Valid=True — invisible to
// `go test ./...`, which never touches the live apiserver this test reads
// its expectation back from. validFillFrom is now BUILT from
// authz.FillFromVocabulary() rather than hand-typed, so this test is really
// pinning that exported vocabulary (and the two still-hand-typed maps)
// against the CRD, not re-typing a third copy of any of them.
func TestSlotEnumMirrors_MatchTheGeneratedCRD(t *testing.T) {
	crd := loadAgentClassCRD(t)
	slotSchema := slotItemSchema(t, crd)

	assert.ElementsMatch(t, listEnumValues(t, slotSchema, "fillFrom"), mapKeys(validFillFrom),
		"validFillFrom (slot_declaration.go, built from authz.FillFromVocabulary()) must carry "+
			"exactly the CRD's fillFrom enum")
	assert.ElementsMatch(t, listEnumValues(t, slotSchema, "autoGrantFrom"), mapKeys(validAutoGrantFrom),
		"validAutoGrantFrom (slot_declaration.go) must carry exactly the CRD's autoGrantFrom enum")
	assert.ElementsMatch(t, scalarEnumValues(t, slotSchema, "membership"), mapKeys(validMembership),
		"validMembership (slot_declaration.go) must carry exactly the CRD's membership enum")
}
