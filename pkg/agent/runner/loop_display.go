package runner

// servedModelDisplay composes the uniform "<provider>/<model>" display id for
// the model that actually served a turn, preferring the provider-reported
// served model (differs per turn under OpenRouter auto-routing/fallback) and
// falling back to the configured model when the provider does not report one.
func servedModelDisplay(provider, respModel, configured string) string {
	m := respModel
	if m == "" {
		m = configured
	}
	if provider == "" {
		return m
	}
	return provider + "/" + m
}

// agentDisplayName returns AgentName, or "agent" if unset.
func (l *Loop) agentDisplayName() string {
	if l.AgentName != "" {
		return l.AgentName
	}
	return "agent"
}

// agentClassDisplayName returns the AgentClass's human-friendly label
// for end-user-facing surfaces (approval prompts, etc.). Falls back to
// the AgentClass metadata.name when DisplayName is unset; empty string
// when nothing is wired (callers render their own fallback).
func (l *Loop) agentClassDisplayName() string {
	if l.AgentClass != nil && l.AgentClass.Spec.DisplayName != "" {
		return l.AgentClass.Spec.DisplayName
	}
	return l.AgentName
}
