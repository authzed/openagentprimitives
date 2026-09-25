package sidecartoolbox

import (
	toolscache "k8s.io/client-go/tools/cache"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// SidecarToolboxRevokeKeyScope returns the revocation key and scope for a
// SidecarToolbox CR deletion event. The key matches the originTool.Origin()
// value = "sidecartoolbox/<cr.Name>", because ResolvedSidecarToolbox.Ref is
// the SidecarToolbox CR metadata.Name (see agentsession_types.go Ref comment).
// SidecarToolbox is namespace-scoped so the scope is the CR's namespace.
// Exported so unit tests can assert the exact key/scope values directly.
func SidecarToolboxRevokeKeyScope(cr *spiceboxv1alpha1.SidecarToolbox) (key, scope string) {
	return "sidecartoolbox/" + cr.Name, cr.Namespace
}

// SidecarToolboxFromDelete extracts the SidecarToolbox from a cache delete
// notification, handling both the direct object and the
// DeletedFinalStateUnknown tombstone. Returns nil if the object is neither.
// Exported so unit tests can exercise the helper independently of the
// informer wiring.
func SidecarToolboxFromDelete(obj any) *spiceboxv1alpha1.SidecarToolbox {
	if cr, ok := obj.(*spiceboxv1alpha1.SidecarToolbox); ok {
		return cr
	}
	if tomb, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		if cr, ok := tomb.Obj.(*spiceboxv1alpha1.SidecarToolbox); ok {
			return cr
		}
	}
	return nil
}
