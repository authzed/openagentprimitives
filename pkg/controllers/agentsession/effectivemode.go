package agentsession

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ResolveEffectiveIdentityMode is the ONE place the operator decides which
// identity mode a session actually runs under. It returns "agent" or
// "userPassthrough" — never "", never "ask"/"dynamic".
//
// The class is authoritative for the three STATIC modes (agent,
// userPassthrough, and unset → agent). status.effectiveIdentityMode is
// consulted only for the two INTERACTIVE modes (ask, dynamic), where the class
// deliberately defers the decision and the operator mirrors the runner's
// signed IdentityChoiceResolved event onto status; before that choice lands the
// session boots on a provisional agent identity.
//
// Why the class wins for static modes, as a security property rather than a
// stylistic one: a session runner holds `patch` on its own
// agentsessions/status (rbac.go), so status.effectiveIdentityMode is a field
// the agent process can write. If a static agent-mode class could be promoted
// to userPassthrough by that field, reconcilePassthroughIdentity would apply
// BuildPassthroughSecretRBAC — a Role granting the session's runner
// ServiceAccount get+update on the human starter's OAuth master Secrets — on a
// class designed never to see them. The admission webhook
// (pkg/controllers/webhooks/agentsession) refuses runner writes to the field as the outer
// door; this function is the inner one, and neither depends on the other.
//
// internal/cmd/runner performs the identical resolution at boot (its own switch, over
// the same two inputs) so the operator and the runner cannot disagree about
// which identity a session is running as.
func ResolveEffectiveIdentityMode(sess *spiceboxv1alpha1.AgentSession, ac *spiceboxv1alpha1.AgentClass) string {
	switch ac.Spec.IdentityMode {
	case spiceboxv1alpha1.IdentityModeAsk, spiceboxv1alpha1.IdentityModeDynamic:
		if sess != nil && sess.Status.EffectiveIdentityMode != "" {
			return sess.Status.EffectiveIdentityMode
		}
		// Choice not made yet: boot provisionally as the agent.
		return spiceboxv1alpha1.IdentityModeAgent
	case spiceboxv1alpha1.IdentityModeUserPassthrough:
		return spiceboxv1alpha1.IdentityModeUserPassthrough
	default:
		// Static agent, or unset (which defaults to agent downstream).
		return spiceboxv1alpha1.IdentityModeAgent
	}
}
