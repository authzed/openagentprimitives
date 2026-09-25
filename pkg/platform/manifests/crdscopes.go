package manifests

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CRDScope records the REST-shape facts a server-side-apply client needs about
// a CRD kind: its API group, plural resource name, and whether it is
// namespaced. All three are read straight from the CRD's own
// spec.{group,names.plural,scope} in the embedded install bundle.
type CRDScope struct {
	Group      string
	Plural     string
	Namespaced bool
}

// CRDScopes parses the embedded install bundle and returns a kind→CRDScope map
// for every CustomResourceDefinition it contains, keyed by spec.names.kind.
//
// It is the single source of truth for the namespaced/cluster-scoped
// classification of the project's own CRs. Apply clients (cmd/oap's kube.Apply)
// consult it instead of hand-maintaining a parallel scope table — a table that
// silently drifts the moment a new cluster-scoped CRD ships, which is exactly
// what broke `oap init` when the cluster-scoped SpiceboxToolchain landed.
func CRDScopes() (map[string]CRDScope, error) {
	docs, err := Split(Install)
	if err != nil {
		return nil, fmt.Errorf("manifests.CRDScopes: split install bundle: %w", err)
	}
	out := map[string]CRDScope{}
	for _, d := range docs {
		if d.GetKind() != "CustomResourceDefinition" {
			continue
		}
		group, _, err := unstructured.NestedString(d.Object, "spec", "group")
		if err != nil {
			return nil, fmt.Errorf("manifests.CRDScopes: %s spec.group: %w", d.GetName(), err)
		}
		kind, _, err := unstructured.NestedString(d.Object, "spec", "names", "kind")
		if err != nil {
			return nil, fmt.Errorf("manifests.CRDScopes: %s spec.names.kind: %w", d.GetName(), err)
		}
		plural, _, err := unstructured.NestedString(d.Object, "spec", "names", "plural")
		if err != nil {
			return nil, fmt.Errorf("manifests.CRDScopes: %s spec.names.plural: %w", d.GetName(), err)
		}
		scope, _, err := unstructured.NestedString(d.Object, "spec", "scope")
		if err != nil {
			return nil, fmt.Errorf("manifests.CRDScopes: %s spec.scope: %w", d.GetName(), err)
		}
		if group == "" || kind == "" || plural == "" || scope == "" {
			return nil, fmt.Errorf("manifests.CRDScopes: %s missing group/kind/plural/scope (group=%q kind=%q plural=%q scope=%q)",
				d.GetName(), group, kind, plural, scope)
		}
		// The apiserver only knows two scopes; anything else is a malformed CRD.
		if scope != "Namespaced" && scope != "Cluster" {
			return nil, fmt.Errorf("manifests.CRDScopes: %s has unknown spec.scope %q (want Namespaced or Cluster)", d.GetName(), scope)
		}
		out[kind] = CRDScope{Group: group, Plural: plural, Namespaced: scope == "Namespaced"}
	}
	return out, nil
}
