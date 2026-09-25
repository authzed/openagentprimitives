// Package crdenumtest reads the Channel CRD's shipped spec enums out of the
// embedded install bundle, so a test can pin a Go-side list against the CRD
// itself instead of against a hand-maintained literal: "every channel kind
// this binary must register" (spec.kind) and "every role a Channel may
// declare" (spec.role, mirrored by v1alpha1.AllChannelRoles).
//
// Test-only, by convention (matching pkg/controllers/testfixtures,
// pkg/platform/identity/credkind/registry/registrytest, and the rest of this
// repo's *test support packages): it imports pkg/platform/manifests, whose
// Install embeds the ~1MB generated install.yaml via go:embed. Importing it
// from a production main.go would bake that bundle into a server binary that
// never needs it. Nothing here enforces the boundary beyond the name and
// this comment — do not import crdenumtest outside a _test.go file.
package crdenumtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// ChannelKindEnum returns spec.kind's +kubebuilder:validation:Enum list from
// the shipped Channel CRD, as rendered by controller-gen.
func ChannelKindEnum(t *testing.T) []string {
	t.Helper()
	return channelSpecEnum(t, "kind")
}

// ChannelRoleEnum returns spec.role's +kubebuilder:validation:Enum list from
// the shipped Channel CRD. It is what pins
// spiceboxv1alpha1.AllChannelRoles() — a hand-maintained Go list of the same
// four values — to the schema the apiserver will actually enforce.
func ChannelRoleEnum(t *testing.T) []string {
	t.Helper()
	return channelSpecEnum(t, "role")
}

// channelSpecEnum walks the embedded, shipped install bundle for the Channel
// CRD and returns the named spec field's enum list. It splits
// manifests.Install on YAML document separators, unmarshals each document, and
// selects the one whose kind is CustomResourceDefinition and whose name is
// channels.agentprimitives.authzed.com.
//
// t.Helper() + require.NoError so a bundle whose shape has changed
// unexpectedly (a renamed field, a restructured schema) fails loud at the
// call site, rather than this function returning a silently empty slice a
// caller might not check.
func channelSpecEnum(t *testing.T, field string) []string {
	t.Helper()

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

		if u.GetKind() != "CustomResourceDefinition" {
			continue
		}
		if u.GetName() != "channels.agentprimitives.authzed.com" {
			continue
		}

		versions, _, err := unstructured.NestedSlice(u.Object, "spec", "versions")
		require.NoError(t, err, "spec.versions was not a slice")
		require.NotEmpty(t, versions, "spec.versions was empty")

		version, ok := versions[0].(map[string]interface{})
		require.True(t, ok, "spec.versions[0] was not an object")

		enum, found, err := unstructured.NestedStringSlice(version,
			"schema", "openAPIV3Schema", "properties", "spec", "properties", field, "enum")
		require.NoErrorf(t, err,
			"spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.%s.enum was not a string slice", field)
		// A field that lost its +kubebuilder:validation:Enum marker would
		// otherwise hand the caller an empty slice, and an ElementsMatch
		// against empty is the kind of assertion that passes while checking
		// nothing.
		require.Truef(t, found, "the shipped Channel CRD declares no enum for spec.%s", field)
		return enum
	}

	return nil
}
