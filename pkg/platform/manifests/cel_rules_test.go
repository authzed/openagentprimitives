package manifests

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestCRDValidationRulesParse is the regression for a CEL rule that shipped with
// a curly quote where an empty-string literal belonged, which the apiserver
// rejected at CRD-install time with a lexer error.
//
// The cause is a toolchain interaction, not a typo, which is why a guard is
// warranted rather than a one-line correction. gofmt applies typographic
// conversion to DOC COMMENTS, and a +kubebuilder marker attached to a type is a
// doc comment: a pair of straight single quotes in a marker is rewritten to a
// curly quote by the next gofmt run. Any CEL rule spelling an empty string that
// way is silently corrupted on save. Write size(x) > 0 instead.
//
// The failure mode is what makes this worth a unit test. A CRD that will not
// install makes envtest fail to boot, and every envtest-backed suite treated an
// unbootable envtest as "skip" — so `mage test:integration`, `mage test:e2e`
// and `mage test:bronze` all exited 0 having run nothing. The unit suite, which
// touches no apiserver, stayed green throughout. Nothing anywhere went red.
//
// This PARSES rather than type-checks: `self` is bound by the apiserver's own
// CEL environment, which is not reconstructed here. Parsing alone catches the
// entire syntax class — bad quotes, stray tokens, unbalanced parens — with no
// declarations needed, and a type error in a rule surfaces the moment any
// envtest suite boots (now loudly, since an unbootable envtest fails instead of
// skipping).
func TestCRDValidationRulesParse(t *testing.T) {
	env, err := cel.NewEnv()
	require.NoError(t, err, "build CEL env")

	checked := 0
	for _, d := range allBundledDocs(t) {
		if d.GetKind() != "CustomResourceDefinition" {
			continue
		}
		for _, rule := range validationRules(t, d) {
			_, issues := env.Parse(rule.expr)
			assert.NoErrorf(t, issues.Err(),
				"CRD %s: x-kubernetes-validations rule at %s does not parse as CEL.\n"+
					"  rule: %s\n"+
					"Fix the +kubebuilder:validation:XValidation marker in pkg/apis/v1alpha1/ "+
					"and re-run `mage gen:api && mage manifests`.",
				d.GetName(), rule.path, rule.expr)
			checked++
		}
	}

	// Without this the test passes vacuously the moment the traversal stops
	// finding rules — which is the same shape of silent pass it exists to catch.
	require.NotZero(t, checked, "no x-kubernetes-validations rules found in the bundled CRDs; the traversal is broken")
	t.Logf("parsed %d CEL validation rules", checked)
}

type celRule struct {
	path string
	expr string
}

// validationRules walks a CRD's schemas and returns every x-kubernetes-validations
// rule, with a JSON-ish path for the failure message.
func validationRules(t *testing.T, crd *unstructured.Unstructured) []celRule {
	t.Helper()

	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoErrorf(t, err, "CRD %s: read spec.versions", crd.GetName())

	var out []celRule
	for _, v := range versions {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(vm, "name")
		schema, found, err := unstructured.NestedMap(vm, "schema", "openAPIV3Schema")
		require.NoErrorf(t, err, "CRD %s version %s: read schema", crd.GetName(), name)
		if !found {
			continue
		}
		out = append(out, collectRules(schema, name)...)
	}
	return out
}

// collectRules recurses a schema node, gathering rules from this node's
// x-kubernetes-validations and from every nested properties/items schema.
func collectRules(node map[string]any, path string) []celRule {
	var out []celRule

	if raw, found, _ := unstructured.NestedSlice(node, "x-kubernetes-validations"); found {
		for _, r := range raw {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			if expr, ok := rm["rule"].(string); ok {
				out = append(out, celRule{path: path, expr: expr})
			}
		}
	}

	if props, found, _ := unstructured.NestedMap(node, "properties"); found {
		for name, p := range props {
			if pm, ok := p.(map[string]any); ok {
				out = append(out, collectRules(pm, path+"."+name)...)
			}
		}
	}
	if items, found, _ := unstructured.NestedMap(node, "items"); found {
		out = append(out, collectRules(items, path+"[]")...)
	}
	return out
}
