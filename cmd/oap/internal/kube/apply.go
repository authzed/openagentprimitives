package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// apGroup is the API group for the project's own CRs. Their scope (namespaced
// vs cluster) and plural resource name are read from the embedded CRDs rather
// than hardcoded here — see agentPrimitivesScopes.
const apGroup = "agentprimitives.authzed.com"

// agentPrimitivesScopes returns the CRD-derived scope map for the project's own
// CR group, built once from the embedded install bundle. Consulting the CRDs
// themselves means resolveGVR can never drift out of sync with a newly-shipped
// cluster-scoped CRD (the bug that broke `oap init` on SpiceboxToolchain).
var agentPrimitivesScopes = sync.OnceValues(manifests.CRDScopes)

// schemaGVK / schemaGVR are tiny aliases so this file doesn't have to
// repeat the full schema package path in every switch arm.
type schemaGVK = schema.GroupVersionKind
type schemaGVR = schema.GroupVersionResource

const (
	// InstalledByAnnotation marks every resource oap applies, so `oap clean` can
	// distinguish what oap installed (e.g. an offered cert-manager / Envoy
	// Gateway) from pre-existing cluster infrastructure it must NOT remove.
	InstalledByAnnotation = "agentprimitives.authzed.com/installed-by"
	// InstalledByValue is the annotation value stamped by Apply.
	InstalledByValue = "ap"
)

// Apply does a server-side apply via the dynamic client. For missing objects,
// we fall back to Create, as the fake dynamic client returns "not found" when
// ApplyPatch is used on missing objects in tests.
func Apply(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured, fieldManager string) error {
	gvk := obj.GroupVersionKind()
	gvr, namespaced, err := resolveGVR(gvk)
	if err != nil {
		return fmt.Errorf("apply %s/%s: resolve GVR: %w", gvk.Kind, obj.GetName(), err)
	}

	// Stamp provenance so `oap clean` can tell oap-installed resources (incl.
	// offered components like cert-manager) from pre-existing cluster infra.
	anns := obj.GetAnnotations()
	if anns == nil {
		anns = map[string]string{}
	}
	anns[InstalledByAnnotation] = InstalledByValue
	obj.SetAnnotations(anns)

	data, err := json.Marshal(obj.Object)
	if err != nil {
		return fmt.Errorf("apply %s/%s: marshal: %w", gvk.Kind, obj.GetName(), err)
	}

	patchOpts := metav1.PatchOptions{
		FieldManager: fieldManager,
		Force:        boolPtr(true),
	}

	var ri dynamic.ResourceInterface
	if namespaced {
		// A namespaced resource with no metadata.namespace would
		// be sent to the cluster-scope path of a namespaced
		// resource — the apiserver returns 405 with the cryptic
		// "the server does not allow this method on the requested
		// resource". Catch it here with a message that names the
		// offender so operators can fix the manifest.
		if obj.GetNamespace() == "" {
			return fmt.Errorf("apply %s/%s: missing metadata.namespace on a namespaced resource (apiserver would 405); set metadata.namespace in the manifest",
				gvk.Kind, obj.GetName())
		}
		ri = dyn.Resource(gvr).Namespace(obj.GetNamespace())
	} else {
		ri = dyn.Resource(gvr)
	}

	_, patchErr := ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, patchOpts)
	if patchErr == nil {
		return nil
	}

	// The fake dynamic client returns "not found" for ApplyPatch on missing objects
	// and other errors for existing ones. Fall back to Create-or-Update for tests.
	if apierrors.IsNotFound(patchErr) {
		_, err = ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return fmt.Errorf("apply %s/%s: create: %w", gvk.Kind, obj.GetName(), err)
		}
		return nil
	}

	if !applyUpdateFallback {
		return fmt.Errorf("apply %s/%s: %w", gvk.Kind, obj.GetName(), patchErr)
	}

	// TEST-ONLY (see applyUpdateFallback): the dynamic fake rejects apply patches
	// on existing objects, so tests fall back to a plain Update.
	_, getErr := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if getErr != nil {
		// Object doesn't exist — forward the original patch error.
		return fmt.Errorf("apply %s/%s: %w", gvk.Kind, obj.GetName(), patchErr)
	}
	_, err = ri.Update(ctx, obj, metav1.UpdateOptions{FieldManager: fieldManager})
	if err != nil {
		return fmt.Errorf("apply %s/%s: update: %w", gvk.Kind, obj.GetName(), err)
	}
	return nil
}

// applyUpdateFallback enables a Get+Update fallback after a failed server-side
// apply. It is FALSE in production and must stay that way: that fallback
// replaces the live object with the manifest, with no resourceVersion and no
// managedFields, discarding controller-added finalizers, ownerReferences, and
// every field another manager owns — and then returns nil, so the caller is
// told "applied". It is reachable on the `oap install` upgrade path, where the
// apiserver can reject a patch against an object whose stored state no longer
// matches an upgraded CRD schema.
//
// It exists only because client-go's dynamic fake cannot serve an apply patch
// for *unstructured.Unstructured, so fake-client tests need this path. The only
// switch that sets it is SetApplyUpdateFallbackForTest, which lives in
// export_test.go — so no production build can turn it on.
var applyUpdateFallback bool

// Delete removes the resource described by obj, resolving its GVR the same way
// Apply does. A missing resource is not an error. Mirrors Apply for teardown —
// e.g. `oap clean` removing an oap-installed component's embedded manifests.
func Delete(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	gvr, namespaced, err := resolveGVR(gvk)
	if err != nil {
		return fmt.Errorf("delete %s/%s: resolve GVR: %w", gvk.Kind, obj.GetName(), err)
	}
	var ri dynamic.ResourceInterface
	if namespaced {
		ri = dyn.Resource(gvr).Namespace(obj.GetNamespace())
	} else {
		ri = dyn.Resource(gvr)
	}
	if err := ri.Delete(ctx, obj.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s/%s: %w", gvk.Kind, obj.GetName(), err)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

// resolveGVR maps a GVK to its plural resource and reports whether it's namespaced.
// We hardcode a static mapping for the well-known core kinds we expect to apply
// via oap install. This avoids round-tripping discovery for well-known kinds.
func resolveGVR(gvk schemaGVK) (schemaGVR, bool, error) {
	switch gvk.Kind {
	case "Namespace":
		return schemaGVR{Group: "", Version: "v1", Resource: "namespaces"}, false, nil
	case "ServiceAccount":
		return schemaGVR{Group: "", Version: "v1", Resource: "serviceaccounts"}, true, nil
	case "Secret":
		return schemaGVR{Group: "", Version: "v1", Resource: "secrets"}, true, nil
	case "ConfigMap":
		return schemaGVR{Group: "", Version: "v1", Resource: "configmaps"}, true, nil
	case "Service":
		return schemaGVR{Group: "", Version: "v1", Resource: "services"}, true, nil
	case "Deployment":
		return schemaGVR{Group: "apps", Version: "v1", Resource: "deployments"}, true, nil
	case "StatefulSet":
		return schemaGVR{Group: "apps", Version: "v1", Resource: "statefulsets"}, true, nil
	case "Role":
		return schemaGVR{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, true, nil
	case "RoleBinding":
		return schemaGVR{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, true, nil
	case "ClusterRole":
		return schemaGVR{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, false, nil
	case "ClusterRoleBinding":
		return schemaGVR{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, false, nil
	case "CustomResourceDefinition":
		return schemaGVR{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, false, nil
	case "ValidatingWebhookConfiguration":
		return schemaGVR{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}, false, nil
	case "MutatingWebhookConfiguration":
		return schemaGVR{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}, false, nil
	case "StorageClass":
		return schemaGVR{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}, false, nil
	}

	// The project's own CRs get their scope + plural straight from the
	// embedded CRDs, so a new cluster-scoped CRD can never again make
	// oap install/init wrongly demand metadata.namespace on it.
	if gvk.Group == apGroup {
		scopes, err := agentPrimitivesScopes()
		if err != nil {
			return schemaGVR{}, false, fmt.Errorf("resolve %s: read CRD scopes: %w", gvk.Kind, err)
		}
		if s, ok := scopes[gvk.Kind]; ok {
			return schemaGVR{Group: gvk.Group, Version: gvk.Version, Resource: s.Plural}, s.Namespaced, nil
		}
	}

	// Fall back to a heuristic plural for less-common kinds (rare for us
	// but useful when oap apply is called on a non-AP CR YAML).
	return mapperFallback(gvk)
}

// mapperFallback constructs a heuristic plural by lowercasing+pluralizing the
// kind and assumes the kind is NAMESPACED. It is the last resort for kinds that
// are neither well-known core kinds (handled by the switch in resolveGVR) nor
// the project's own CRs (whose scope + plural come from the embedded CRDs, so
// their cluster-scoped members are classified correctly regardless of this
// namespaced assumption). It exists mainly for `oap apply` on a third-party
// namespaced CR YAML.
func mapperFallback(gvk schemaGVK) (schemaGVR, bool, error) {
	plural := pluralize(gvk.Kind)
	return schemaGVR{Group: gvk.Group, Version: gvk.Version, Resource: plural}, true, nil
}

func pluralize(kind string) string {
	lower := strings.ToLower(kind)
	switch {
	case len(lower) == 0:
		return lower
	case lower[len(lower)-1] == 's':
		return lower + "es"
	case lower[len(lower)-1] == 'y':
		return lower[:len(lower)-1] + "ies"
	default:
		return lower + "s"
	}
}

// PluralizeForTest is exported for unit tests only.
func PluralizeForTest(kind string) string { return pluralize(kind) }

// ResolveNamespacedForTest reports whether resolveGVR classifies the given
// group+kind as namespaced. The group matters for the project's own CRs, whose
// scope is read from the embedded CRDs; it is ignored for built-in kinds
// matched by the switch. Exported for unit tests only — guards the
// cluster-scoped classification of install-bundle kinds.
func ResolveNamespacedForTest(group, kind string) (bool, error) {
	_, namespaced, err := resolveGVR(schemaGVK{Group: group, Kind: kind})
	return namespaced, err
}
