package spiceboxtoolkit

import (
	toolscache "k8s.io/client-go/tools/cache"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// SpiceboxToolkitRevokeKeyScope returns the revocation key and scope for a
// SpiceboxToolkit CR deletion event. The key matches SandboxTool.Origin() =
// "toolkit/" + toolkit.Toolkit.Name, where toolkit.Toolkit.Name is
// SpiceboxToolkitSpec.Name (NOT the CR metadata.Name — they differ; the spec
// Name is the logical tool name, the CR name is often "<name>-<revision>").
// SpiceboxToolkit is cluster-scoped so the scope is "" (empty string).
// Exported so unit tests can assert the exact key/scope values directly.
func SpiceboxToolkitRevokeKeyScope(cr *spiceboxv1alpha1.SpiceboxToolkit) (key, scope string) {
	return "toolkit/" + cr.Spec.Name, ""
}

// SpiceboxToolkitFromDelete extracts the SpiceboxToolkit from a cache delete
// notification, handling both the direct object and the
// DeletedFinalStateUnknown tombstone. Returns nil if the object is neither.
// Exported so unit tests can exercise the helper independently of the
// informer wiring.
func SpiceboxToolkitFromDelete(obj any) *spiceboxv1alpha1.SpiceboxToolkit {
	if cr, ok := obj.(*spiceboxv1alpha1.SpiceboxToolkit); ok {
		return cr
	}
	if tomb, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		if cr, ok := tomb.Obj.(*spiceboxv1alpha1.SpiceboxToolkit); ok {
			return cr
		}
	}
	return nil
}
