package channelevents

// KindRevoked is the unified operator-to-runner in-flight revocation event:
// "this resource is no longer permitted; invalidate any live use." One subject
// (ap.revocation) carries every revocable kind; the runner dispatches by
// RevokedPayload.Kind to a registered revocation.Invalidator.
const KindRevoked Kind = "revoked"

// RevokedPayload identifies one revoked resource.
type RevokedPayload struct {
	// Kind is the registered revocable-kind name ("tool-origin", "credential").
	Kind string `json:"kind"`
	// Key is the kind-specific identity: an Origin() string for tool-origin
	// (e.g. "mcpserver/linear"), or "<secretNamespace>/<secretName>" for
	// credential.
	Key string `json:"key"`
	// Scope is the namespace the revocation applies to; "" = cluster-wide.
	Scope string `json:"scope"`
}
