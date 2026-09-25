package cloud

import "fmt"

// CheckPublicEndpointAllowed reports whether a PublicEndpoint may be created
// against this cluster kind at all. It is the CREATION-side half of the same
// answer the PublicEndpoint reconciler enforces at reconcile time, and both
// halves are needed:
//
//   - The reconciler's gate cannot stop the CR from being written. By the time
//     it runs, the object exists, an operator has been told a tunnel is coming,
//     and the only thing left is a Failed condition nobody is watching.
//   - This check cannot stop a CR that was created some other way — by hand, by
//     an older build, or before AP_CLUSTER_KIND was stamped. That is what the
//     reconciler's gate is for.
//
// So every path that creates a PublicEndpoint calls this FIRST, before it
// writes anything. A tunnel opened on a cluster with real ingress is an
// unannounced outbound path out of production infrastructure — a security
// surprise, not a convenience — and refusing is cheaper than explaining.
//
// The refusal names the kind because the kind is the thing the operator got
// wrong: AP_CLUSTER_KIND is stamped once at install and read everywhere after,
// so "which kind answered this" is the only actionable fact in the message.
func CheckPublicEndpointAllowed(s Strategy) error {
	// A nil Strategy means the caller never resolved a cluster kind. Refuse
	// rather than dereference: fail-closed matches For()'s own refusal on an
	// empty key, and the alternative is a nil-interface panic at the exact
	// gate that exists to prevent surprises.
	if s == nil {
		return fmt.Errorf("no cluster kind resolved: a PublicEndpoint may only be created once the " +
			"cluster kind is known, because the kind decides whether a project-managed tunnel is " +
			"permitted at all")
	}
	policy := s.InstallProfile().PublicEndpointPolicy()
	if policy.AllowsTunnel() {
		return nil
	}
	return fmt.Errorf("cluster kind %q does not allow a project-managed tunnel (policy %s): this "+
		"cluster reaches the Internet through its own ingress, and webd's external URL is already "+
		"set from it. Configure external access with `oap install --trusted-hostname` instead of a "+
		"PublicEndpoint", s.Key(), policy)
}

// IsWebdTarget reports whether a namespace+service pair names webd's own
// Service — the ONLY PublicEndpoint target whose address may be published as
// webd's external URL, and therefore the one that tells a would-be second
// writer of that ConfigMap to stand down.
//
// Name and namespace only; a port is deliberately not part of the question.
// The published value's port is the tunnel's or the host port-forward's, never
// the Service's, so a target naming webd on a different Service port is still
// webd.
func IsWebdTarget(namespace, service string) bool {
	return namespace == WebdServiceNamespace && service == WebdServiceName
}
