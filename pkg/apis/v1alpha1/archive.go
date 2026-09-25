package v1alpha1

import "k8s.io/apimachinery/pkg/api/meta"

// ArchivedBySweep reports whether Phase==Succeeded was reached by the operator's
// long-idle archive sweep rather than by the agent finishing. The sweep clears
// FailureReason and records itself only as the Idle condition's reason, so this
// is the sole runtime signal distinguishing "parked" from "done".
//
// A superseded parent is excluded even when it still carries the Archived
// condition: SupersedeParent stamps Phase=Succeeded without clearing it, and a
// resumed parent would contend with the very child that replaced it.
func ArchivedBySweep(sess *AgentSession) bool {
	if sess == nil || sess.Status.SupersededBy != "" {
		return false
	}
	c := meta.FindStatusCondition(sess.Status.Conditions, AgentSessionConditionIdle)
	return c != nil && c.Reason == ReasonAgentSessionArchived
}
