package apcmd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ToolCallPhase condenses a ToolCall's conditions into the single word the
// list and operations tables print.
//
// The order is the point: a terminal condition (Failed / Timeout / Canceled)
// wins over Succeeded, which wins over Running, because a call can carry more
// than one True condition and the worst outcome is the one an operator needs
// to see. "Pending" is the answer when nothing has been asserted yet.
func ToolCallPhase(tc *spiceboxv1alpha1.ToolCall) string {
	for _, c := range tc.Status.Conditions {
		switch c.Type {
		case spiceboxv1alpha1.ToolCallConditionFailed,
			spiceboxv1alpha1.ToolCallConditionTimeout,
			spiceboxv1alpha1.ToolCallConditionCanceled:
			if c.Status == metav1.ConditionTrue {
				return string(c.Type)
			}
		}
	}
	for _, c := range tc.Status.Conditions {
		if c.Type == spiceboxv1alpha1.ToolCallConditionSucceeded && c.Status == metav1.ConditionTrue {
			return "Succeeded"
		}
	}
	for _, c := range tc.Status.Conditions {
		if c.Type == spiceboxv1alpha1.ToolCallConditionRunning && c.Status == metav1.ConditionTrue {
			return "Running"
		}
	}
	return "Pending"
}

// AgentClassIsValid reports whether the class's Valid condition is True.
//
// A class with no Valid condition yet is NOT valid: the reconciler has not
// reached it, so nothing has vouched for the tools, identities and channels it
// names. Every command that starts a session off a class gates on this, so the
// answer has to be the same one in all of them.
func AgentClassIsValid(ac *spiceboxv1alpha1.AgentClass) bool {
	for _, c := range ac.Status.Conditions {
		if c.Type == spiceboxv1alpha1.AgentClassConditionValid {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
