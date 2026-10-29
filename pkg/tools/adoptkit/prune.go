package adoptkit

import (
	"context"
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PruneOwners removes, in ONE write, every ownerKind adoption recorded on obj
// whose owner stale reports as no longer needing it: the owner annotation and
// the field manager that applied it. It returns how many owners it removed.
//
// Adoptions otherwise only ever accumulate. Nothing releases one when its owner
// goes away, so a Secret shared by a short-lived kind collects one annotation
// and one field manager per instance, forever, and every server-side apply to
// it is merged against all of them. That is how one credential Secret reached
// 866 owners and seven seconds per apply.
//
// Why one JSON patch rather than an empty apply per owner: an empty apply under
// an owner's field manager would drop exactly what that owner applied, but it
// is one round trip per owner against an object that is slow BECAUSE it has so
// many. Here the stale annotations are removed and the stale managers' entries
// dropped from metadata.managedFields together, which the API server accepts
// as written when a request sets managedFields itself.
//
// What it never does:
//
//   - Remove the adoption label. A stale owner's entry may have been the only
//     one owning it, but the label stays on the object regardless, merely
//     unowned. Un-adopting is not this function's call: a live reader may
//     depend on the label, and the cost being fixed is the per-owner entries,
//     not the one label.
//   - Drop a field manager a live owner shares. Manager names carry the owner's
//     kind and name but not its namespace, so two owners with one name in two
//     namespaces share a manager; it is kept while either is live.
//   - Write over a newer object. The patch tests obj's resourceVersion first,
//     so an obj read from a stale cache fails the patch rather than reverting
//     someone's newer metadata. The caller retries on its next pass.
//
// obj is only read. writer receives a JSON patch against obj's key.
func PruneOwners(ctx context.Context, writer client.Writer, obj client.Object, ownerKind string, stale func(types.NamespacedName) bool) (int, error) {
	prefix := ownerAnnotationPrefix + ownerKind + "-"
	staleKeys := []string{}
	staleManagers := map[string]bool{}
	liveManagers := map[string]bool{}
	for k, v := range obj.GetAnnotations() {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		owner, ok := parseOwnerValue(v)
		if !ok {
			continue // not a value this package wrote; leave it alone
		}
		if stale(owner) {
			staleKeys = append(staleKeys, k)
			staleManagers[fieldManager(ownerKind, owner)] = true
		} else {
			liveManagers[fieldManager(ownerKind, owner)] = true
		}
	}
	if len(staleKeys) == 0 {
		return 0, nil
	}

	ops := []map[string]any{{"op": "test", "path": "/metadata/resourceVersion", "value": obj.GetResourceVersion()}}
	for _, k := range staleKeys {
		ops = append(ops, map[string]any{"op": "remove", "path": "/metadata/annotations/" + jsonPointerEscape(k)})
	}
	kept := make([]metav1.ManagedFieldsEntry, 0, len(obj.GetManagedFields()))
	for _, mf := range obj.GetManagedFields() {
		if mf.Operation == metav1.ManagedFieldsOperationApply && staleManagers[mf.Manager] && !liveManagers[mf.Manager] {
			continue
		}
		kept = append(kept, mf)
	}
	// An empty list is how a client asks the API server to strip managedFields
	// entirely, which would take every OTHER manager's ownership with it. Only
	// rewrite the list when something remains; the annotations go either way.
	if len(kept) > 0 && len(kept) < len(obj.GetManagedFields()) {
		ops = append(ops, map[string]any{"op": "replace", "path": "/metadata/managedFields", "value": kept})
	}
	body, err := json.Marshal(ops)
	if err != nil {
		return 0, err
	}

	target, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return 0, nil
	}
	if err := writer.Patch(ctx, target, client.RawPatch(types.JSONPatchType, body)); err != nil {
		return 0, err
	}
	return len(staleKeys), nil
}

// parseOwnerValue reads the "namespace/name" value Adopt writes for an owner.
// A cluster-scoped owner is written with an empty namespace, as "/name".
func parseOwnerValue(v string) (types.NamespacedName, bool) {
	ns, name, ok := strings.Cut(v, "/")
	if !ok || name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: ns, Name: name}, true
}

// jsonPointerEscape escapes one JSON Pointer reference token (RFC 6901): "~"
// first, then "/", which every annotation key with a prefix contains.
func jsonPointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
