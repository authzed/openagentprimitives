//go:build !integration && !e2e

package credkind_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers every credkind in THIS test binary — see enum_invariant_test.go's
	// import comment for why each test also asserts the registry is non-empty.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// TestSecretRefPathAgreesWithSecretRef is the invariant that makes
// SecretRefPath safe to rewrite through: for every registered kind, the path
// it declares must be exactly where its own SecretRef reads the Secret name
// from.
//
// It is asserted BY CONSTRUCTION rather than by comparing two hand-written
// lists: a value is planted at the declared path in an unstructured
// credential, converted to the typed AgentCredential, and handed to
// SecretRef. If the path names a different field — a renamed block, a typo, a
// path copied from a sibling kind — SecretRef reads nothing back and this
// fails.
//
// Without the invariant, `oap agent install --name` would rewrite a field
// nothing reads and leave the real reference untouched: precisely the
// dangling-Secret failure that a hardcoded two-path rewrite already caused
// once for githubApp, restored in a shape that looks registry-driven.
func TestSecretRefPathAgreesWithSecretRef(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the registry must be populated or every assertion below is vacuous")

	const planted = "planted-secret-name"
	for _, k := range kinds {
		t.Run(k.Type(), func(t *testing.T) {
			path := k.SecretRefPath()

			obj := map[string]any{"name": "cred", "type": k.Type()}
			if len(path) > 0 {
				require.NoError(t, unstructured.SetNestedField(obj, planted, path...),
					"the declared path must be settable on a credential object")
			}

			var cred spiceboxv1alpha1.AgentCredential
			require.NoError(t, unstructuredToCredential(obj, &cred),
				"the declared path must map onto the typed AgentCredential")

			ref := k.SecretRef(cred)
			if len(path) == 0 {
				assert.Nil(t, ref,
					"a kind that declares no SecretRefPath must have no backing Secret at all; "+
						"one that does would be skipped by every rewrite")
				return
			}
			require.NotNilf(t, ref, "SecretRef read nothing back from %v — the declared path is not where this kind stores its Secret name", path)
			assert.Equalf(t, planted, ref.Name,
				"SecretRefPath %v and SecretRef disagree about where the Secret name lives", path)
		})
	}
}

// TestEveryKindWithABackingSecretDeclaresItsPath is the half
// TestSecretRefPathAgreesWithSecretRef structurally cannot cover: that test
// plants a value at the DECLARED path, so a kind declaring nothing simply has
// nothing planted, its SecretRef reads nil, and it passes — which is exactly
// how a kind whose ref never gets rewritten would slip through, silently
// leaving a dangling Secret name in every --name install.
//
// So the expectation is derived from the CRD type instead of from the kind.
// Each credential type's union block is the AgentCredential field whose json
// tag IS the type name; if that block carries a `secretRef`, the kind owns a
// backing Secret and MUST declare the path into it. If it does not (federated
// carries only idpSecretRef, which is deliberately not this credential's own
// store), the kind must declare nothing.
func TestEveryKindWithABackingSecretDeclaresItsPath(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds)

	credType := reflect.TypeOf(spiceboxv1alpha1.AgentCredential{})
	for _, k := range kinds {
		t.Run(k.Type(), func(t *testing.T) {
			block, ok := fieldByJSONTag(credType, k.Type())
			require.Truef(t, ok, "AgentCredential has no %q block; the union field's json tag must match the registry type", k.Type())

			blockType := block.Type
			for blockType.Kind() == reflect.Ptr {
				blockType = blockType.Elem()
			}
			_, hasSecretRef := fieldByJSONTag(blockType, "secretRef")

			if !hasSecretRef {
				assert.Empty(t, k.SecretRefPath(),
					"this type's block carries no secretRef, so there is nothing for a rewrite to repoint")
				return
			}
			assert.Equalf(t, []string{k.Type(), "secretRef", "name"}, k.SecretRefPath(),
				"%s.secretRef exists on AgentCredential, so this kind must declare the path to it — "+
					"an undeclared path means `oap agent install --name` leaves the reference naming a Secret it never creates",
				k.Type())
		})
	}
}

// fieldByJSONTag finds the struct field whose json tag's first component is
// name. Matching on the tag rather than the Go field name is what ties the
// assertion to the wire shape the unstructured rewrite actually walks.
func fieldByJSONTag(t reflect.Type, name string) (reflect.StructField, bool) {
	if t.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// TestSecretRefPathIsRelativeToOneCredential pins the contract callers rely
// on: the path starts INSIDE a spec.credentials element (at the type block),
// never at the CR root. A path that began with "spec" would be written at the
// wrong depth by every caller.
func TestSecretRefPathIsRelativeToOneCredential(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds)

	for _, k := range kinds {
		path := k.SecretRefPath()
		if len(path) == 0 {
			continue
		}
		require.Lenf(t, path, 3, "%s: expected a <block>.secretRef.name shaped path, got %v", k.Type(), path)
		assert.NotEqualf(t, "spec", path[0],
			"%s: SecretRefPath is relative to one spec.credentials element, not to the CR root", k.Type())
		assert.NotEqualf(t, "credentials", path[0],
			"%s: SecretRefPath is relative to one spec.credentials element, not to the list", k.Type())
		assert.Equalf(t, "name", path[len(path)-1],
			"%s: SecretRefPath must address the Secret NAME field", k.Type())
	}
}

// unstructuredToCredential decodes one spec.credentials element into the typed
// AgentCredential, so a test can plant a value at a declared path and then ask
// the typed accessor to read it back.
func unstructuredToCredential(obj map[string]any, out *spiceboxv1alpha1.AgentCredential) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(obj, out)
}
