package runner

// coldStartEligible reports whether the runner should drive the cold-start flow
// for this session's initial prompt: a genuinely-new (non-forked) channel-driven
// session whose AgentClass enables scope with a coldStart mode other than "off",
// and which has the publish hook + memory facade + engine wired. The ForkedFrom
// guard keeps a forked session — whose child memory is seeded with the parent
// transcript — from re-running cold start; the !hadInitialPrompt gate at the
// call site already excludes resumes. Anything unwired (kubectl-driven sessions,
// tests without NATS) places the initial prompt verbatim as turn 0.
func (l *Loop) coldStartEligible() bool {
	ok, _ := l.coldStartEligibility()
	return ok
}

// coldStartEligibility reports whether cold-start should run and, when it should
// NOT, a human-readable reason naming the failed precondition. The reason is
// logged at the call site so an operator can tell from logs why a scope-enabled
// session did not run cold-start.
func (l *Loop) coldStartEligibility() (bool, string) {
	switch {
	case l.AgentClass == nil:
		return false, "AgentClass is nil"
	case l.AgentSession != nil && l.AgentSession.Spec.ForkedFrom != "":
		return false, "session is forked (ForkedFrom set)"
	case !l.AgentClass.Spec.GetScope().Enabled:
		return false, "AgentClass authz.scope.enabled is false"
	case l.AgentClass.Spec.GetScope().ColdStart == "off":
		return false, "AgentClass authz.scope.coldStart is off"
	case l.ColdStartRequestPublish == nil:
		return false, "ColdStartRequestPublish not wired (not channel-attached or NATS unavailable)"
	case l.Mem == nil:
		return false, "Mem (memory querier) not wired"
	case l.Engine == nil:
		return false, "Engine (LLM) not wired"
	}
	return true, ""
}
