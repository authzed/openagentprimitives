package v1alpha1

// Names on the SpiceDB `agentclass` definition (pkg/authz/spicedb/schema/schema.zed).
// Mirrored here, not imported, because this package depends on nothing.
const (
	// AgentClassPermissionStartSession = starter + platform->start_session.
	AgentClassPermissionStartSession = "start_session"
	// AgentClassPermissionStartExplicit = starter. The platform-admin arm is
	// absent by design: an explicit gate is one admins list themselves on.
	AgentClassPermissionStartExplicit = "start_explicit"
	// AgentClassRelationStarter is the relation AllowedStarters is written to.
	AgentClassRelationStarter = "starter"
)

// StartGatePermission names the agentclass permission a would-be starter must
// hold on this class, or "" when the class declares no allowlist and so has no
// start gate. Pure over spec.
func (ac *AgentClass) StartGatePermission() string {
	if ac == nil {
		return ""
	}
	s := ac.Spec.GetAuthz().GetSession()
	if len(s.AllowedStarters) == 0 {
		return ""
	}
	if s.PlatformAdminsMayStart != nil && !*s.PlatformAdminsMayStart {
		return AgentClassPermissionStartExplicit
	}
	return AgentClassPermissionStartSession
}

// StarterSubjectSet is the SpiceDB subject-set naming this class's explicit
// starters, "agentclass:<ns>/<name>#starter" — the value OnlyStartersInteract
// applies as the session interact permission. Computed from the object's own
// name so a renamed (bundle-prefixed) class still names itself.
func (ac *AgentClass) StarterSubjectSet() string {
	return "agentclass:" + ac.Namespace + "/" + ac.Name + "#" + AgentClassRelationStarter
}
