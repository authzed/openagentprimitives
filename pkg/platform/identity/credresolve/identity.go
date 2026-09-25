// identity.go — the runtime view of an identity's credentials, kind-agnostic.
// Both AgentIdentity (agent-scoped, namespaced) and SessionUserIdentity
// (per-session, narrowed from a UserIdentity) project into RuntimeIdentity;
// the shared resolver in this package consumes only that shape.
package credresolve

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// RuntimeIdentity is the runtime view of an identity's credentials —
// whatever its kind. Both AgentIdentity (agent-scoped, namespaced) and
// SessionUserIdentity (per-session, narrowed from a UserIdentity)
// project into this shape. The descriptor builders consume only this.
type RuntimeIdentity struct {
	// Credentials are the named credentials available for resolution.
	Credentials []spiceboxv1alpha1.AgentCredential
	// Namespace is where each credential's Secret refs resolve by default
	// (and, when StaticProjection is nil, for type=static too).
	Namespace string
	// Label is a short human-readable identifier of where this came
	// from — used only in error messages (e.g. "AgentIdentity foo" /
	// "SessionUserIdentity bar").
	Label string
	// StaticProjection, when non-nil, redirects type=static credential
	// sources to a per-session projected Secret (keyed by credential name)
	// in the session namespace, instead of the master Secret in Namespace.
	// type=oauth / type=federated credentials still resolve from Namespace so
	// their JIT refresh / ID-JAG mint stay anchored on the master / IdP Secret.
	// Set only by the userPassthrough flow; nil for AgentIdentity.
	StaticProjection *StaticCredentialProjection
}

// StaticCredentialProjection points type=static credential sources at a
// per-session projected Secret in the session namespace. The operator
// materializes this Secret (see PassthroughCredentialSecretName) so a sandbox
// ToolCall's credential source stays within the ToolCall's own namespace.
type StaticCredentialProjection struct {
	// Namespace is the session namespace the projected Secret lives in.
	Namespace string
	// SecretName is the per-session projected Secret's name.
	SecretName string
}

// RuntimeIdentityFromAgentIdentity projects an AgentIdentity into its
// runtime view. A nil ai yields a zero-valued RuntimeIdentity with a
// "<none>" label.
func RuntimeIdentityFromAgentIdentity(ai *spiceboxv1alpha1.AgentIdentity) RuntimeIdentity {
	if ai == nil {
		return RuntimeIdentity{Label: "<none>"}
	}
	return RuntimeIdentity{
		Credentials: ai.Spec.Credentials,
		Namespace:   ai.Namespace,
		Label:       "AgentIdentity " + ai.Name,
	}
}

// RuntimeIdentityFromSessionUserIdentity projects a SessionUserIdentity
// (the per-session narrowed view of a user's catalog) into its runtime
// view. type=oauth / type=federated master/IdP Secrets live in
// IdentitiesNamespace (Namespace), and JIT refresh / ID-JAG mint stay anchored
// there. type=static credential VALUES are projected by the operator into a
// per-session Secret in the session namespace (StaticProjection): their
// descriptor sources then resolve WITHIN the session namespace so a sandbox
// ToolCall passes ValidateCredentialSourceNamespaces.
func RuntimeIdentityFromSessionUserIdentity(suid *spiceboxv1alpha1.SessionUserIdentity) RuntimeIdentity {
	if suid == nil {
		return RuntimeIdentity{Label: "<none>"}
	}
	return RuntimeIdentity{
		Credentials: suid.Spec.Credentials,
		Namespace:   spiceboxv1alpha1.IdentitiesNamespace,
		Label:       "SessionUserIdentity " + suid.Name,
		StaticProjection: &StaticCredentialProjection{
			Namespace:  suid.Namespace,
			SecretName: spiceboxv1alpha1.PassthroughCredentialSecretName(suid.Name),
		},
	}
}
