// Package conditions wraps k8s.io/apimachinery/pkg/api/meta condition
// helpers with the project-wide convention of always stamping
// ObservedGeneration from the owning object's Generation. Every
// reconciler in pkg/controllers/* uses this; do not call
// meta.SetStatusCondition directly outside this package.
package conditions

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Generationer is satisfied by every CR with a Generation field.
type Generationer interface {
	GetGeneration() int64
}

// Set applies a condition to conds with ObservedGeneration filled in
// from obj.GetGeneration(). Wraps meta.SetStatusCondition so the
// transition timestamp + dedupe-on-equal-state semantics are
// preserved.
func Set(obj Generationer, conds *[]metav1.Condition, c metav1.Condition) {
	c.ObservedGeneration = obj.GetGeneration()
	meta.SetStatusCondition(conds, c)
}

// SetTrue is shorthand for Set with Status=True.
func SetTrue(obj Generationer, conds *[]metav1.Condition, condType, reason string) {
	Set(obj, conds, metav1.Condition{
		Type: condType, Status: metav1.ConditionTrue, Reason: reason,
	})
}

// SetFalse is shorthand for Set with Status=False + a message.
func SetFalse(obj Generationer, conds *[]metav1.Condition, condType, reason, message string) {
	Set(obj, conds, metav1.Condition{
		Type: condType, Status: metav1.ConditionFalse,
		Reason: reason, Message: message,
	})
}

// Find returns the named condition or nil.
func Find(conds []metav1.Condition, condType string) *metav1.Condition {
	return meta.FindStatusCondition(conds, condType)
}

// IsTrue reports whether the condition exists and has Status=True.
func IsTrue(conds []metav1.Condition, condType string) bool {
	c := Find(conds, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}
