package revocation

// AllNamespaces is passed as myNamespace by a cluster-wide consumer — one whose
// invalidatable state spans every namespace, such as the operator, whose token
// broker caches credentials resolved on behalf of sessions in all namespaces. It
// matches every scope, including namespace-scoped revocations.
//
// "*" cannot collide with a real namespace: Kubernetes namespace names are DNS
// labels (RFC 1123), so "*" is not a legal name.
//
// This exists so a cluster-wide consumer states its intent, rather than relying
// on the accident that a particular kind's revocations happen to be published
// cluster-wide today.
const AllNamespaces = "*"

// Applies reports whether a revocation scoped to scope affects a consumer whose
// session is in myNamespace. scope "" means cluster-wide (applies to all), and
// myNamespace == AllNamespaces means the consumer accepts every scope.
//
// Load-bearing for tool-origin correctness: Origin keys are not namespace-
// qualified, so the same name in two namespaces is two distinct resources; the
// scope gate prevents a revoke in one namespace from denying the other's.
func Applies(scope, myNamespace string) bool {
	return scope == "" || myNamespace == AllNamespaces || scope == myNamespace
}
