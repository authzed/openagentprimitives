// Package crdschematest reads field schemas out of the SHIPPED install
// bundle, so a test asserting a CRD default or enum derives it from
// generated YAML rather than from the Go marker it is supposed to be
// checking.
//
// Test-only, by convention (matching
// pkg/channels/channelkinds/registry/crdenumtest and the rest of this
// repo's *test support packages): it imports pkg/platform/manifests, whose
// Install embeds the ~1MB generated install.yaml via go:embed. Importing it
// from a production main.go would bake that bundle into a server binary
// that never needs it. Nothing here enforces the boundary beyond the name
// and this comment — do not import crdschematest outside a _test.go file.
package crdschematest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// FieldSchema returns the OpenAPI schema at jsonPath within the named CRD's
// first served version, as shipped in the embedded install bundle
// (manifests.Install) — the exact bundle `oap install` applies.
//
// crdPlural is the CRD's plural resource name (e.g. "spiceboxclasses"); the
// CRD is looked up as "<crdPlural>.agentprimitives.authzed.com".
//
// jsonPath is a "."-separated walk of field names, e.g.
// "spec.mounts.items.properties.format":
//   - "items" descends into an array schema's item schema.
//   - "properties" is a no-op separator: every other segment already
//     resolves against the current schema's Properties map, so an explicit
//     "properties" segment (mirroring the raw OpenAPI structure) is
//     consumed without moving.
//   - any other segment is looked up in the current schema's Properties map.
//
// Fails the test via require when the CRD, its first served version, or any
// path segment is missing, so a renamed field or a misspelled path breaks
// the test loudly instead of silently returning a zero-value
// JSONSchemaProps a caller might not notice.
func FieldSchema(t *testing.T, crdPlural, jsonPath string) apiextensionsv1.JSONSchemaProps {
	t.Helper()

	crdName := crdPlural + ".agentprimitives.authzed.com"

	docs := strings.Split(string(manifests.Install), "\n---")
	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			// Not every document in the bundle is well-formed on its own once
			// split on a bare "\n---" (a value that legitimately contains the
			// separator sequence would break here too); skip and keep looking
			// rather than fail the whole walk on an unrelated document.
			continue
		}

		if u.GetKind() != "CustomResourceDefinition" || u.GetName() != crdName {
			continue
		}

		versions, _, err := unstructured.NestedSlice(u.Object, "spec", "versions")
		require.NoError(t, err, "%s: spec.versions was not a slice", crdName)
		require.NotEmpty(t, versions, "%s: spec.versions was empty", crdName)

		version, ok := versions[0].(map[string]interface{})
		require.True(t, ok, "%s: spec.versions[0] was not an object", crdName)

		node, _, err := unstructured.NestedMap(version, "schema", "openAPIV3Schema")
		require.NoError(t, err, "%s: spec.versions[0].schema.openAPIV3Schema was not an object", crdName)
		require.NotNil(t, node, "%s: spec.versions[0].schema.openAPIV3Schema was missing", crdName)

		for _, seg := range strings.Split(jsonPath, ".") {
			switch seg {
			case "properties":
				continue
			case "items":
				items, ok := node["items"].(map[string]interface{})
				require.True(t, ok, "%s: jsonPath %q: no items schema at segment %q", crdName, jsonPath, seg)
				node = items
			default:
				props, ok := node["properties"].(map[string]interface{})
				require.True(t, ok, "%s: jsonPath %q: current schema has no properties at segment %q", crdName, jsonPath, seg)
				next, ok := props[seg].(map[string]interface{})
				require.True(t, ok, "%s: jsonPath %q: field %q not found", crdName, jsonPath, seg)
				node = next
			}
		}

		raw, err := json.Marshal(node)
		require.NoError(t, err, "%s: jsonPath %q: marshalling resolved schema node", crdName, jsonPath)

		var result apiextensionsv1.JSONSchemaProps
		require.NoError(t, json.Unmarshal(raw, &result), "%s: jsonPath %q: unmarshalling into JSONSchemaProps", crdName, jsonPath)
		return result
	}

	require.Failf(t, "CRD not found in embedded install bundle", "crd=%q", crdName)
	return apiextensionsv1.JSONSchemaProps{}
}
