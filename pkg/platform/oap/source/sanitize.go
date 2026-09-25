package source

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// lastAppliedConfigAnnotation is the kubectl client-side-apply annotation;
// it captures a prior applied state and must not be re-applied verbatim.
const lastAppliedConfigAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// Sanitize strips server-managed fields from u in place so the object can be
// safely re-applied on install: the whole status subtree, the server-populated
// metadata fields (resourceVersion, uid, generation, creationTimestamp,
// managedFields, ownerReferences, selfLink, finalizers), and the kubectl
// last-applied-configuration annotation (dropping the annotations map entirely
// if that was its only entry). apiVersion, kind, spec, and the rest of metadata
// are left untouched.
//
// finalizers are stripped because re-applying a CR with a foreign finalizer
// into a cluster whose controller is absent would wedge the object in
// Terminating on delete. Each controller re-adds its own on the next reconcile,
// so dropping it on export is safe.
func Sanitize(u *unstructured.Unstructured) {
	unstructured.RemoveNestedField(u.Object, "status")

	for _, field := range []string{
		"resourceVersion",
		"uid",
		"generation",
		"creationTimestamp",
		"managedFields",
		"ownerReferences",
		"selfLink",
		"finalizers",
	} {
		unstructured.RemoveNestedField(u.Object, "metadata", field)
	}

	annotations, found, err := unstructured.NestedStringMap(u.Object, "metadata", "annotations")
	if err != nil || !found {
		// Malformed (non-string-valued) or absent annotations: nothing to
		// sanitize here, and Sanitize has no error path to report it through.
		return
	}
	if _, ok := annotations[lastAppliedConfigAnnotation]; !ok {
		return
	}
	delete(annotations, lastAppliedConfigAnnotation)
	if len(annotations) == 0 {
		unstructured.RemoveNestedField(u.Object, "metadata", "annotations")
		return
	}
	if err := unstructured.SetNestedStringMap(u.Object, annotations, "metadata", "annotations"); err != nil {
		// Unreachable in practice: metadata.annotations was just read back
		// above as a map[string]interface{} of strings, so writing the
		// trimmed map to the same path cannot fail. Guarded rather than
		// silently discarding a real error, per repo convention.
		return
	}
}
